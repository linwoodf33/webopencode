package handler

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"webshell/protocol"
	"webshell/pty"
)

// pendingMaxBytes 分离期间积压输出的上限（1MB），超出丢弃最早数据。
const pendingMaxBytes = 1 << 20

// attachedConn 封装一个已附着（attach）到会话的 WebSocket 连接。
// 使用独立的 writeMu 与会话锁分开，避免持会话锁做网络写。
type attachedConn struct {
	writeMu sync.Mutex
	conn    *websocket.Conn
}

// localSession 表示本节点持有的一个 pty 会话。
//
// 生命周期：Create 后启动 pump goroutine 独立读取 pty 输出；一个 WebSocket
// 连接可通过 attach/detach 反复附着/分离。会话在 shell 退出、到期或显式关闭时
// 由 SessionManager.Close 终结。expireAt 仅由索引（memoryIndex）作为单一权威
// 维护，localSession 不保存。
type localSession struct {
	id        string
	username  string
	mode      string
	longLived bool
	createdAt time.Time

	proc *pty.Session

	mu      sync.Mutex
	closed  bool
	conn    *attachedConn
	pending []byte
}

// isClosed 返回会话是否已关闭。
func (ls *localSession) isClosed() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.closed
}

// writeTo 向指定连接写一条 output 消息（持 ac.writeMu，不持会话锁）。
func (ls *localSession) writeTo(ac *attachedConn, data []byte) {
	payload, err := protocol.OutputMessage(data).Marshal()
	if err != nil {
		return
	}
	ac.writeMu.Lock()
	_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
	ac.writeMu.Unlock()
}

// sendOutput 把 pty 输出分发给当前附着的连接；若无连接则积压等待重连后冲刷。
func (ls *localSession) sendOutput(data []byte) {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return
	}
	if ls.conn != nil {
		ac := ls.conn
		ls.mu.Unlock()
		ls.writeTo(ac, data)
		return
	}
	ls.pending = append(ls.pending, data...)
	if len(ls.pending) > pendingMaxBytes {
		ls.pending = ls.pending[len(ls.pending)-pendingMaxBytes:]
	}
	ls.mu.Unlock()
}

// pump 读取 pty 输出并分发给当前连接；底层 shell 退出时结束会话。
func (ls *localSession) pump(m *SessionManager) {
	buf := make([]byte, 4096)
	for {
		n, err := ls.proc.Read(buf)
		if n > 0 {
			ls.sendOutput(buf[:n])
		}
		if err != nil {
			m.close(ls.id, "shell exited")
			return
		}
	}
}

// readLoop 从附着连接读取客户端消息并写入 pty。连接断开时返回（由调用方 detach）。
func (ls *localSession) readLoop(ac *attachedConn) {
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
			if _, err := ls.proc.Write(input); err != nil {
				return
			}
		case protocol.TypeResize:
			size, ok := msg.ParseResize()
			if !ok {
				continue
			}
			_ = ls.proc.Resize(size.Cols, size.Rows)
		default:
		}
	}
}

// CreateParams 描述一次会话创建请求，除 pty 之外的参数。
type CreateParams struct {
	ID        string
	Username  string
	Mode      string
	LongLived bool
	Hours     int
	// MaxLong / MaxTotal 为名额上限（<=0 表示不限制），用于索引内二次校验。
	MaxLong  int
	MaxTotal int
}

// SessionManager 持有本节点的 pty 与子进程（不可序列化），并配合 memoryIndex
// 管理元数据/TTL/归属。
//
// 锁层级：indexMu → mgrMu → sMu（见各方法注释）。所有会话终结统一走 Close。
type SessionManager struct {
	index *memoryIndex
	mu    sync.Mutex
	local map[string]*localSession
	now   func() time.Time
	// closeProc 是 pty.Session.Close 的调用接缝：生产为直接 Close；单测可替换为
	// 阻塞桩，用以验证「索引先移除、proc.Close 后执行」的时序（close 步骤 ③/⑤）。
	closeProc func(*pty.Session)
}

// NewManager 创建一个会话管理器，并与索引互相接线（索引 reaper 到期回调 Close）。
func NewManager(index *memoryIndex, now func() time.Time) *SessionManager {
	if now == nil {
		now = time.Now
	}
	m := &SessionManager{
		index: index,
		local: make(map[string]*localSession),
		now:   now,
		closeProc: func(s *pty.Session) {
			s.Close()
		},
	}
	if index != nil {
		index.setCloseFunc(func(id string) { m.close(id, "session expired") })
	}
	return m
}

// SetNow 注入时钟（供测试使用）。f 为 nil 时忽略；同时同步到索引。
func (m *SessionManager) SetNow(f func() time.Time) {
	if f == nil {
		return
	}
	m.mu.Lock()
	m.now = f
	m.mu.Unlock()
	if m.index != nil {
		m.index.SetNow(f)
	}
}

// clock 返回当前时间（尊重注入的时钟）。
func (m *SessionManager) clock() time.Time {
	m.mu.Lock()
	f := m.now
	m.mu.Unlock()
	if f == nil {
		return time.Now()
	}
	return f()
}

// GetLocal 按 ID 取本地会话（不存在返回 nil）。mgrMu 快进快出。
func (m *SessionManager) GetLocal(id string) *localSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.local[id]
}

// Create 登记一个已创建好的 pty 会话（pty 由调用方在无锁状态下先建）。
//
// 时序：indexMu 内 Add（原子二次校验名额，超额不写入）→ mgrMu 登记 local →
// 起 pump。indexMu 与 mgrMu 不重叠，保证并发创建恰好不超额。
// 名额不足时关闭刚建好的 pty 并返回 ErrLongLimit / ErrTotalLimit。
func (m *SessionManager) Create(p CreateParams, proc *pty.Session) (*localSession, error) {
	now := m.clock()
	var expireAt time.Time
	if p.LongLived {
		expireAt = now.Add(time.Duration(p.Hours) * time.Hour)
	} else {
		// 短期初始给一个保活窗口；首次 detach 时再刷新。
		expireAt = now.Add(defaultShortTTL)
	}

	info := SessionInfo{
		ID:        p.ID,
		Username:  p.Username,
		Mode:      p.Mode,
		LongLived: p.LongLived,
		CreatedAt: now,
		ExpireAt:  expireAt,
		Attached:  false,
	}
	if err := m.index.Add(info, p.MaxLong, p.MaxTotal); err != nil {
		proc.Close()
		return nil, err
	}

	ls := &localSession{
		id:        p.ID,
		username:  p.Username,
		mode:      p.Mode,
		longLived: p.LongLived,
		createdAt: now,
		proc:      proc,
	}
	m.mu.Lock()
	m.local[p.ID] = ls
	m.mu.Unlock()

	go ls.pump(m)
	return ls, nil
}

// Attach 将连接附着到会话：sMu 内 swap conn、取 pending → 释放 → 锁外踢旧连接
// → 冲刷 pending → 发 SessionMessage。索引仅更新 attached 状态（不改 TTL）。
//
// re-attach 后的前台应用重绘由前端负责：前端在 attach 完成后做一次 resize 抖动
// （rows-1 → 延时 → 真实尺寸），复用 resize→SIGWINCH 通路，兼容 bash/opencode 等 TUI。
// R4 合规：绝不持 sMu/indexMu/mgrMu 做 pty ioctl。
// 返回 false 表示会话已关闭（已向新连接发送 close 并关闭）。
func (m *SessionManager) Attach(ls *localSession, ac *attachedConn) bool {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		payload, _ := protocol.CloseMessage("session not available").Marshal()
		ac.writeMu.Lock()
		_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
		_ = ac.conn.Close()
		ac.writeMu.Unlock()
		return false
	}
	old := ls.conn
	ls.conn = ac
	pending := ls.pending
	ls.pending = nil
	ls.mu.Unlock()

	m.index.setAttached(ls.id, true)

	// 若已存在旧连接（如刷新时新旧短暂重叠），关闭旧连接，避免重复写。
	if old != nil && old != ac {
		payload, _ := protocol.CloseMessage("replaced by new connection").Marshal()
		old.writeMu.Lock()
		_ = old.conn.WriteMessage(websocket.TextMessage, payload)
		_ = old.conn.Close()
		old.writeMu.Unlock()
	}

	// 冲刷分离期间积压的输出：整段一次性写入。
	//
	// 与 pump 的 sendOutput 复用同一 writeTo（同一把 ac.writeMu），单次调用即
	// 单条 output 消息、单次写，避免先前 4096 分块写与 pump 并发写同一 conn 时
	// 块间交错导致的终端字节流乱序 / ANSI 撕裂。pending 上限 pendingMaxBytes
	// （1MB），单次大 Buffer 写在 writeMu 下原子完成；即便短暂阻塞也仅影响本连接。
	// 注意：本函数在释放 sMu 后执行（R4：不得持锁做网络写）。
	if len(pending) > 0 {
		ls.writeTo(ac, pending)
	}

	// 通知客户端本次会话标识。
	payload, _ := protocol.SessionMessage(ls.id).Marshal()
	ac.writeMu.Lock()
	_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
	ac.writeMu.Unlock()
	return true
}

// Detach 移除指定连接（仅当它仍是当前附着连接时生效）。若确实分离，则索引
// attached=false；短期会话顺带刷新 expireAt=now+shortTTL，长期不动 expireAt。
func (m *SessionManager) Detach(ls *localSession, ac *attachedConn) {
	ls.mu.Lock()
	cleared := false
	if ls.conn == ac {
		ls.conn = nil
		cleared = true
	}
	ls.mu.Unlock()
	if !cleared {
		return
	}
	m.index.setAttached(ls.id, false)
	if !ls.longLived {
		m.index.Update(ls.id, m.clock().Add(defaultShortTTL))
	}
}

// Close 终结一个会话（幂等）。六步且同序：
//  1. mgrMu 取 ls（nil 即 return）；
//  2. sMu 内判 closed（已 closed 则 return）并置 closed=true、摘 conn，立即释放 sMu；
//  3. 移除索引 index.Remove（纯内存、幂等，单独取/放 indexMu；此时 sMu 已释放，
//     符合 indexMu → mgrMu → sMu 锁层级）；
//  4. 锁外通知旧 conn（close 消息 + Close）；
//  5. 锁外 proc.Close()（保持同步、不异步化）；
//  6. mgrMu delete(local, id)。
//
// 步骤 ③ 必须排在 ④/⑤ 之前：④ 的网络写与 ⑤ 的 proc.Close（含 cmd.Wait，可能
// 长时间阻塞，如 opencode TUI 回收）期间，若索引尚未移除，GET /api/sessions 仍
// 会列出该会话且 active=true（见 sessionInfoDTO.Active=info.Attached），而 attach
// ticket 仍签发、WS attach 却因 ls.closed 必然报 "session not found or expired"，
// 形成状态一致性窗口。提前移除索引即消除该窗口。
//
// 绝不持锁做网络写或 pty.Close。
func (m *SessionManager) Close(id string) bool { return m.close(id, "session closed") }

func (m *SessionManager) close(id, reason string) bool {
	// ①
	m.mu.Lock()
	ls := m.local[id]
	m.mu.Unlock()
	if ls == nil {
		return false
	}

	// ②
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return false
	}
	ls.closed = true
	ac := ls.conn
	ls.conn = nil
	ls.mu.Unlock()

	log.Printf("session %s closing: %s", id, reason)

	// ③ 提前移除索引（纯内存、幂等；仅 indexMu，sMu 已释放）。
	m.index.Remove(id)

	// ④ 锁外通知旧 conn。
	if ac != nil {
		payload, _ := protocol.CloseMessage(reason).Marshal()
		ac.writeMu.Lock()
		_ = ac.conn.WriteMessage(websocket.TextMessage, payload)
		_ = ac.conn.Close()
		ac.writeMu.Unlock()
	}

	// ⑤ 锁外 proc.Close()（若 proc != nil；同步执行、不异步化）。
	if ls.proc != nil && m.closeProc != nil {
		m.closeProc(ls.proc)
	}

	// ⑥
	m.mu.Lock()
	delete(m.local, id)
	m.mu.Unlock()
	return true
}

// newSessionID 生成一个高熵随机会话标识。
//
// crypto/rand 失败时退化为「时间字符串哈希」仅作兜底，无安全影响：sessionID
// 不是接入凭证（接入一律凭 ticket），仅用于标识会话/命名 cgroup，故即使可预测
// 也不构成安全缺口；正常路径始终为 32 字节密码学随机。
func newSessionID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "s" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(buf)
}
