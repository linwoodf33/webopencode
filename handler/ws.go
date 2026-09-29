package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"webshell/auth"
	"webshell/protocol"
	"webshell/pty"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 允许所有来源（演示用途）。
		return true
	},
}

// wsClose 向连接发送 close 消息并关闭。
func wsClose(conn *websocket.Conn, reason string) {
	payload, _ := protocol.CloseMessage(reason).Marshal()
	_ = conn.WriteMessage(websocket.TextMessage, payload)
	_ = conn.Close()
}

// NewWs 处理 /ws?ticket=<t> 端点。
//
// 统一接入：先消费一次性 ticket（action=create|attach），再升级 WebSocket。
//   - action=create：用 ticket 内已校验的 Username/HomeDir/Mode/Type/Hours 创建
//     pty 会话并附着；
//   - action=attach：附着到 ticket 指定的会话（服务端再兜底校验存活/未过期）。
//
// ticket 无效/过期/重放，或仅凭 sessionID（无 ticket）→ 401（升级前拒绝）。
func NewWs(mgr *SessionManager, tickets *auth.TicketStore, cfg *auth.Config) http.HandlerFunc {
	opencodeCfg := &cfg.Opencode
	codexCfg := &cfg.Codex
	sandboxCfg := &cfg.Sandbox

	return func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("ticket")
		purpose, ok := tickets.Consume(raw)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid or expired ticket"})
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}

		switch purpose.Action {
		case "attach":
			ls := mgr.GetLocal(purpose.Session)
			if ls == nil || ls.isClosed() {
				// TOCTOU 兜底：ticket 签发后会话可能已关闭/过期。
				wsClose(conn, "session not found or expired")
				return
			}
			info, ok := mgr.index.Get(purpose.Session)
			if !ok || info.expired(mgr.clock()) {
				wsClose(conn, "session not found or expired")
				return
			}
			ac := &attachedConn{conn: conn}
			if !mgr.Attach(ls, ac) {
				return
			}
			ls.readLoop(ac)
			mgr.Detach(ls, ac)

		case "create":
			username := purpose.Username
			homeDir := purpose.HomeDir
			mode := purpose.Mode

			// 根据 ticket 内的 mode 决定本次会话进入 opencode / codex 还是 bash。
			// 同步失败只 log、不阻塞登录（仍继续启动对应代理）。
			var effCfg *auth.OpencodeConfig
			var effCodexCfg *auth.CodexConfig
			if opencodeCfg != nil && opencodeCfg.Enabled && mode == "opencode" {
				effCfg = opencodeCfg
				if err := pty.SyncOpencodeConfig(username, homeDir, opencodeCfg, 15*time.Second); err != nil {
					log.Printf("ws: sync opencode config for %q failed (non-fatal): %v", username, err)
				}
			} else if codexCfg != nil && codexCfg.Enabled && mode == "codex" {
				effCodexCfg = codexCfg
				if err := pty.SyncCodexConfig(username, homeDir, codexCfg, 15*time.Second); err != nil {
					log.Printf("ws: sync codex config for %q failed (non-fatal): %v", username, err)
				}
			}

			// 先生成会话 ID（与沙箱 cgroup 命名一致），再创建 pty 会话。
			sessionID := newSessionID()

			// opencode / codex 模式：启动前补全配置引用的 api_key。
			// 此时 WebSocket 已 Upgrade、会话尚未登记（readLoop 未启动），
			// auth flow 直接读写 conn。缺失 key 时下发 auth-request 弹窗等待用户
			// 输入，用户取消/超时则发送 close 消息并结束会话。
			var authEnv map[string]string
			var aerr error
			if effCfg != nil {
				authTimeout := defaultAuthTimeout
				if opencodeCfg.AuthTimeout > 0 {
					authTimeout = time.Duration(opencodeCfg.AuthTimeout) * time.Second
				}
				authEnv, aerr = resolveAuthEnv(conn, username, homeDir, auth.RequiredEnvVars, authTimeout)
			} else if effCodexCfg != nil {
				authTimeout := defaultAuthTimeout
				if codexCfg.AuthTimeout > 0 {
					authTimeout = time.Duration(codexCfg.AuthTimeout) * time.Second
				}
				authEnv, aerr = resolveAuthEnv(conn, username, homeDir, auth.RequiredCodexEnvVars, authTimeout)
			}
			if aerr != nil {
				log.Printf("ws: auth env for %q failed: %v", username, aerr)
				wsClose(conn, "api key required but canceled or timed out")
				return
			}

			sess, serr := pty.NewSessionForUserMode(username, homeDir, effCfg, effCodexCfg, sandboxCfg, sessionID, authEnv)
			if serr != nil {
				log.Printf("pty session create failed: %v", serr)
				wsClose(conn, "failed to start shell: "+serr.Error())
				return
			}

			ls, cerr := mgr.Create(CreateParams{
				ID:        sessionID,
				Username:  username,
				Mode:      mode,
				LongLived: purpose.Type == "long",
				Hours:     purpose.Hours,
				MaxLong:   cfg.MaxLongN,
				MaxTotal:  cfg.MaxTotalN,
			}, sess)
			if cerr != nil {
				reason := "failed to create session"
				switch {
				case errors.Is(cerr, ErrLongLimit):
					reason = msgLongLimit
				case errors.Is(cerr, ErrTotalLimit):
					reason = msgTotalLimit
				}
				wsClose(conn, reason)
				return
			}

			ac := &attachedConn{conn: conn}
			mgr.Attach(ls, ac)
			ls.readLoop(ac)
			mgr.Detach(ls, ac)

		default:
			wsClose(conn, "invalid ticket action")
		}
	}
}
