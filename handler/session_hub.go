package handler

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"webshell/protocol"
	"webshell/pty"
)

// 会话保留时间：无 WebSocket 连接（页面刷新/短暂断线）时，pty 会话在该时间内
// 保持存活，前端用同一 sessionID 重连即可恢复同一个终端；超时后由巡检 goroutine 关闭。
const (
	sessionTTL      = 5 * time.Minute
	reaperInterval  = 30 * time.Second
	pendingMaxBytes = 1 << 20 // 分离期间积压输出的上限（1MB），超出丢弃最早数据
)

// attachedConn 封装一个已附着（attach）到会话的 WebSocket 连接。
// 使用独立的 writeMu 与 hubSession.mu 分开，避免持会话锁做网络写。
type attachedConn struct {
	writeMu sync.Mutex
	conn    *websocket.Conn
}

// hubSession 表示一个可被刷新后恢复的 pty 会话。
//
// 生命周期：register 后启动 pump goroutine 独立读取 pty 输出；一个 WebSocket
// 连接可通过 attach/detach 反复附着/分离（页面刷新时分离，重连时重新附着），
// 分离期间输出积压到 pending，重连后冲刷给新连接。会话在 shell 退出或超时后关闭。
type hubSession struct {
	hub      *sessionHub
	id       string
	username string
	sess     *pty.Session

	mu       sync.Mutex
	closed   bool
	expireAt time.Time
	conn     *attachedConn
	pending  []byte
}

// sendOutput 把 pty 输出分发给当前附着的连接；若无连接则积压等待重连后冲刷。
func (h *hubSession) sendOutput(data []byte) {
	h.mu.Lock()
	if h.conn != nil {
		ac := h.conn
		h.mu.Unlock()
		payload, err := protocol.OutputMessage(data).Marshal()
		if err != nil {
			return
		}
		ac.writeMu.Lock()
		_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
		ac.writeMu.Unlock()
		return
	}
	h.pending = append(h.pending, data...)
	if len(h.pending) > pendingMaxBytes {
		h.pending = h.pending[len(h.pending)-pendingMaxBytes:]
	}
	h.mu.Unlock()
}

// attach 将一个新的连接附着到会话，冲刷分离期间积压的输出，并通知客户端会话标识。
func (h *hubSession) attach(ac *attachedConn) {
	h.mu.Lock()
	old := h.conn
	h.conn = ac
	pending := h.pending
	h.pending = nil
	h.mu.Unlock()

	// 若已存在旧连接（如刷新时新旧短暂重叠），关闭旧连接，避免重复写。
	if old != nil && old != ac {
		payload, _ := protocol.CloseMessage("replaced by new connection").Marshal()
		old.writeMu.Lock()
		_ = old.conn.WriteMessage(websocket.TextMessage, payload)
		_ = old.conn.Close()
		old.writeMu.Unlock()
	}

	// 冲刷分离期间积压的输出。
	for len(pending) > 0 {
		n := len(pending)
		if n > 4096 {
			n = 4096
		}
		h.sendOutput(pending[:n])
		pending = pending[n:]
	}

	// 通知客户端本次会话标识（用于刷新后恢复）。
	payload, _ := protocol.SessionMessage(h.id).Marshal()
	ac.writeMu.Lock()
	_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
	ac.writeMu.Unlock()
}

// detach 移除指定连接（仅当它仍是当前附着连接时生效），并延长会话存活时间，
// 为可能的刷新重连留出窗口。ac 来自旧连接的 readLoop 结束，需按引用匹配避免
// 误清掉后续 attach 的新连接。
func (h *hubSession) detach(ac *attachedConn) {
	h.mu.Lock()
	if h.conn == ac {
		h.conn = nil
		h.expireAt = time.Now().Add(sessionTTL)
	}
	h.mu.Unlock()
}

// isClosed 返回会话是否已关闭。
func (h *hubSession) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// closeSession 正常结束会话（shell 退出等）：通知附着连接、关闭 pty、并从 hub 移除。
func (h *hubSession) closeSession(reason string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	ac := h.conn
	h.conn = nil
	h.mu.Unlock()

	log.Printf("session %s closing: %s", h.id, reason)
	if ac != nil {
		payload, _ := protocol.CloseMessage(reason).Marshal()
		ac.writeMu.Lock()
		_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
		_ = ac.conn.Close()
		ac.writeMu.Unlock()
	}
	h.sess.Close()
	h.hub.remove(h.id)
}

// pump 读取 pty 输出并分发给当前连接；底层 shell 退出时结束会话。
func (h *hubSession) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := h.sess.Read(buf)
		if n > 0 {
			h.sendOutput(buf[:n])
		}
		if err != nil {
			h.closeSession("shell exited")
			return
		}
	}
}

// readLoop 从附着连接读取客户端消息并写入 pty。连接断开时返回（由调用方 detach）。
func (h *hubSession) readLoop(ac *attachedConn) {
	for {
		_, raw, err := ac.conn.ReadMessage()
		if err != nil {
			return
		}
		msg, err := protocol.Parse(raw)
		if err != nil {
			return
		}
		switch msg.Type {
		case protocol.TypeInput:
			input := msg.ParseInput()
			if input == nil {
				continue
			}
			if _, err := h.sess.Write(input); err != nil {
				return
			}
		case protocol.TypeResize:
			size, ok := msg.ParseResize()
			if !ok {
				continue
			}
			_ = h.sess.Resize(size.Cols, size.Rows)
		default:
		}
	}
}

// sessionHub 管理所有存活会话，支持按 sessionID 恢复同一终端。
type sessionHub struct {
	mu       sync.Mutex
	sessions map[string]*hubSession
}

// NewSessionHub 创建一个会话仓库并启动过期清理巡检 goroutine。
func NewSessionHub() *sessionHub {
	h := &sessionHub{sessions: make(map[string]*hubSession)}
	go h.reaper()
	return h
}

// register 注册一个新会话并启动其 pump goroutine，返回会话（含生成的 ID）。
func (h *sessionHub) register(username string, sess *pty.Session) *hubSession {
	hs := &hubSession{
		hub:      h,
		id:       newSessionID(),
		username: username,
		sess:     sess,
	}
	h.mu.Lock()
	h.sessions[hs.id] = hs
	h.mu.Unlock()
	go hs.pump()
	return hs
}

// get 按 sessionID 取会话；不存在返回 nil。
func (h *sessionHub) get(id string) *hubSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

// remove 从 hub 移除会话（幂等）。
func (h *sessionHub) remove(id string) {
	h.mu.Lock()
	delete(h.sessions, id)
	h.mu.Unlock()
}

// resumable 判断给定 sessionID+用户名是否可恢复一个仍在存活的会话。
func (h *sessionHub) resumable(id, username string) bool {
	hs := h.get(id)
	if hs == nil || hs.username != username {
		return false
	}
	return !hs.isClosed()
}

// expire 由 reaper 调用：原子地从 hub 移除并关闭超时且无连接的会话，
// 避免与 resume attach 竞争。
func (h *sessionHub) expire(hs *hubSession) {
	h.mu.Lock()
	cur, ok := h.sessions[hs.id]
	if !ok || cur != hs {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, hs.id)
	h.mu.Unlock()

	hs.mu.Lock()
	if hs.closed {
		hs.mu.Unlock()
		return
	}
	hs.closed = true
	ac := hs.conn
	hs.conn = nil
	hs.mu.Unlock()

	log.Printf("session %s expired (user %s)", hs.id, hs.username)
	if ac != nil {
		payload, _ := protocol.CloseMessage("session expired").Marshal()
		ac.writeMu.Lock()
		_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
		_ = ac.conn.Close()
		ac.writeMu.Unlock()
	}
	hs.sess.Close()
}

// reaper 周期性清理超时且无连接的会话。
func (h *sessionHub) reaper() {
	for {
		time.Sleep(reaperInterval)
		now := time.Now()
		h.mu.Lock()
		var expired []*hubSession
		for _, s := range h.sessions {
			s.mu.Lock()
			noConn := s.conn == nil
			over := now.After(s.expireAt)
			s.mu.Unlock()
			if noConn && over {
				expired = append(expired, s)
			}
		}
		h.mu.Unlock()
		for _, s := range expired {
			h.expire(s)
		}
	}
}

// newSessionID 生成一个高熵随机会话标识。
func newSessionID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "s" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(buf)
}

// NewResumeHandler 返回检查会话是否可恢复的 HTTP 处理函数。
// GET /api/resume?session=<id>&username=<user> -> 200 {ok:true} 或 404。
func NewResumeHandler(hub *sessionHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		if !hub.resumable(r.URL.Query().Get("session"), r.URL.Query().Get("username")) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
