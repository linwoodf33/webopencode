package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// mgmtRecord 记录一个 mgmt_token 的状态。
type mgmtRecord struct {
	username string
	expireAt time.Time
}

// MgmtTokenStore 内存 mgmt_token 存储，线程安全。
//
// mgmt_token 是登录后签发的、可重复使用的用户级管理凭证，绑定 username，
// 用于所有会话级 HTTP 操作（列表 / 新建 / 恢复 / 延期 / 关闭 / 上传）。
// 校验是幂等的（不消费），可多次使用，直到过期或被 Revoke。
type MgmtTokenStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time // 时钟注入；nil 默认 time.Now
	data map[string]*mgmtRecord
}

// NewMgmtTokenStore 创建一个 mgmt_token 存储。ttl <= 0 时使用默认 8 小时。
func NewMgmtTokenStore(ttl time.Duration) *MgmtTokenStore {
	if ttl <= 0 {
		ttl = time.Duration(DefaultMgmtTTLHours) * time.Hour
	}
	return &MgmtTokenStore{
		ttl:  ttl,
		now:  time.Now,
		data: make(map[string]*mgmtRecord),
	}
}

// clock 返回当前时间（尊重注入的时钟，未注入时为 time.Now）。
func (s *MgmtTokenStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// SetNow 注入时钟（供测试使用）。f 为 nil 时忽略。
func (s *MgmtTokenStore) SetNow(f func() time.Time) {
	s.mu.Lock()
	s.now = f
	s.mu.Unlock()
}

// Issue 为指定用户名签发一个 mgmt_token（crypto/rand 32 字节 hex）。
func (s *MgmtTokenStore) Issue(username string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[token] = &mgmtRecord{
		username: username,
		expireAt: s.clock().Add(s.ttl),
	}
	return token, nil
}

// Validate 校验 mgmt_token，返回绑定的用户名。幂等，不消费。
// token 不存在或已过期返回 ok=false（过期的记录会被就地清除）。
func (s *MgmtTokenStore) Validate(raw string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data[raw]
	if !ok {
		return "", false
	}
	if s.clock().After(rec.expireAt) {
		delete(s.data, raw)
		return "", false
	}
	return rec.username, true
}

// Revoke 吊销指定 mgmt_token（/api/logout）。幂等。
func (s *MgmtTokenStore) Revoke(raw string) {
	s.mu.Lock()
	delete(s.data, raw)
	s.mu.Unlock()
}

// Cleanup 删除所有已过期记录，返回删除数量。
func (s *MgmtTokenStore) Cleanup(now time.Time) int {
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

// StartCleanup 周期性清理过期记录，直到 ctx 取消。interval <= 0 时不启动。
func (s *MgmtTokenStore) StartCleanup(ctx context.Context, interval time.Duration) {
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
