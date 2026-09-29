package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// ticketTTL 一次性 ticket 的固定有效期（60 秒），不进 yaml。
const ticketTTL = 60 * time.Second

// TicketPurpose 描述一个 ticket 承载的、已由服务端校验过的接入意图。
//
// Action:
//   - "create"：新建会话，Username/HomeDir/Mode/Type/Hours 已预解析/校验；
//   - "attach"：附着到已有会话，Session 为会话 ID。
type TicketPurpose struct {
	Action   string // "create" | "attach"
	Username string
	HomeDir  string // create 时预解析
	Session  string // attach
	Mode     string // bash|opencode|codex
	Type     string // ""=short|"long"
	Hours    int    // long create
}

// ticketRecord 记录一个一次性 ticket 的状态。
type ticketRecord struct {
	purpose  TicketPurpose
	expireAt time.Time
}

// TicketStore 内存一次性 ticket 存储，线程安全。
//
// ticket 由 mgmt_token 换取，短时（60s）一次性，仅用于建立一次 WebSocket。
// Consume 为原子一次性语义（防重放）。
type TicketStore struct {
	mu   sync.Mutex
	now  func() time.Time // 时钟注入；nil 默认 time.Now
	data map[string]*ticketRecord
}

// NewTicketStore 创建一个 ticket 存储。
func NewTicketStore() *TicketStore {
	return &TicketStore{
		now:  time.Now,
		data: make(map[string]*ticketRecord),
	}
}

// clock 返回当前时间（尊重注入的时钟）。
func (s *TicketStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// SetNow 注入时钟（供测试使用）。f 为 nil 时忽略。
func (s *TicketStore) SetNow(f func() time.Time) {
	s.mu.Lock()
	s.now = f
	s.mu.Unlock()
}

// Issue 签发一个带校验意图的一次性 ticket。
func (s *TicketStore) Issue(p TicketPurpose) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[token] = &ticketRecord{
		purpose:  p,
		expireAt: s.clock().Add(ticketTTL),
	}
	return token, nil
}

// Consume 原子校验并消费一次性 ticket（防重放），返回其承载的意图副本。
// ticket 不存在/已消费/已过期返回 ok=false。
func (s *TicketStore) Consume(raw string) (*TicketPurpose, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.data[raw]
	if !ok {
		return nil, false
	}
	// 无论成败都移除，保证一次性语义。
	delete(s.data, raw)
	if s.clock().After(rec.expireAt) {
		return nil, false
	}
	p := rec.purpose // 值拷贝，避免调用方持有内部引用
	return &p, true
}

// Cleanup 删除所有已过期 ticket，返回删除数量。
func (s *TicketStore) Cleanup(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for tok, rec := range s.data {
		if now.After(rec.expireAt) {
			delete(s.data, tok)
			n++
		}
	}
	return n
}

// StartCleanup 周期性清理过期 ticket，直到 ctx 取消。interval <= 0 时不启动。
func (s *TicketStore) StartCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Cleanup(s.clock())
			}
		}
	}()
}
