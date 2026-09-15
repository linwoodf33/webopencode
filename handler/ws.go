package handler

import (
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

// NewWs 处理 /ws 端点，支持两种接入：
//
//  1. 新会话：校验 ?token= 一次性 token，取出绑定的用户名并再次兜底校验
//     （正则 + 非root + 系统存在），然后以该用户 sudo 启动会话。
//  2. 恢复会话：?session=<id>&username=<user> 命中会话仓库中仍存活的 pty 会话
//     （页面刷新后自动重连），重新附着到同一终端，正在运行的进程不丢失。
//
// 无论哪种方式，升级为 WebSocket 后都附着（attach）到 hub 中的会话；连接断开
// （刷新/断线）时仅分离（detach）而不销毁会话，保留 sessionTTL 等待重连。
func NewWs(authr *auth.Authenticator, opencodeCfg *auth.OpencodeConfig, sandboxCfg *auth.SandboxConfig, hub *sessionHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 尝试恢复已存在的会话（页面刷新后自动重连）。
		sid := r.URL.Query().Get("session")
		username := r.URL.Query().Get("username")
		resuming := sid != ""
		var hs *hubSession
		if resuming {
			hs = hub.get(sid)
			if hs == nil || hs.username != username || hs.isClosed() {
				log.Printf("ws: resume failed for session %q user %q", sid, username)
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "session not found or expired"})
				return
			}
		}

		// 2. 升级为 WebSocket。
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}

		// 3. 新会话：校验一次性 token 并启动会话。
		if !resuming {
			token := r.URL.Query().Get("token")
			uname, homeDir, verr := authr.ValidateAndConsume(token)
			if verr != nil {
				log.Printf("ws: token validation failed: %v", verr)
				payload, _ := protocol.CloseMessage("invalid or expired token").Marshal()
				_ = conn.WriteMessage(websocket.TextMessage, payload)
				_ = conn.Close()
				return
			}
			username = uname

			// 根据 ?mode= 决定本次会话进入 opencode 还是 bash。
			// 同步失败只 log、不阻塞登录（仍继续启动 opencode）。
			mode := r.URL.Query().Get("mode")
			var effCfg *auth.OpencodeConfig
			if opencodeCfg != nil && opencodeCfg.Enabled && mode == "opencode" {
				effCfg = opencodeCfg
				if err := pty.SyncOpencodeConfig(username, homeDir, opencodeCfg, 15*time.Second); err != nil {
					log.Printf("ws: sync opencode config for %q failed (non-fatal): %v", username, err)
				}
			}

			// 先生成会话 ID（与沙箱 cgroup 命名一致），再创建 pty 会话。
			sessionID := newSessionID()

			sess, serr := pty.NewSessionForUserMode(username, homeDir, effCfg, sandboxCfg, sessionID)
			if serr != nil {
				log.Printf("pty session create failed: %v", serr)
				payload, _ := protocol.CloseMessage("failed to start shell: " + serr.Error()).Marshal()
				_ = conn.WriteMessage(websocket.TextMessage, payload)
				_ = conn.Close()
				return
			}
			hs = hub.register(username, sess, sessionID)
		}

		// 4. 附着到会话：本协程读取客户端输入，pump goroutine 负责输出。
		ac := &attachedConn{conn: conn}
		hs.attach(ac)
		hs.readLoop(ac)
		hs.detach(ac)
	}
}
