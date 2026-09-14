package auth

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// tokenRecord 记录一个一次性 token 的状态。
type tokenRecord struct {
	username string
	expireAt time.Time
	consumed bool
}

// TokenStore 内存一次性 token 存储，线程安全。
//
// 使用 sync.Mutex 保护 map；token 用 crypto/rand 生成足够长的随机 hex。
// 消费是原子的（防重放）：每次 Consume 都将 token 标记/移除。
type TokenStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[string]*tokenRecord
}

// NewTokenStore 创建一个 token 存储。ttl <= 0 时使用默认 5 分钟。
func NewTokenStore(ttl time.Duration) *TokenStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &TokenStore{
		ttl:  ttl,
		data: make(map[string]*tokenRecord),
	}
}

// Issue 为指定用户名签发一个一次性 token，返回 token 字符串。
func (s *TokenStore) Issue(username string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[token] = &tokenRecord{
		username: username,
		expireAt: time.Now().Add(s.ttl),
	}
	return token, nil
}

// Consume 校验并消费一次性 token，返回绑定的用户名。
// token 不存在/过期/已消费时返回 ok=false。消费是原子的，可防重放。
func (s *TokenStore) Consume(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.data[token]
	if !ok {
		return "", false
	}
	// 无论成败都移除，保证一次性语义。
	delete(s.data, token)
	if rec.consumed {
		return "", false
	}
	if time.Now().After(rec.expireAt) {
		return "", false
	}
	rec.consumed = true
	return rec.username, true
}
