package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"webshell/auth"
)

// testConfig 返回一份默认配置（用于 handler 级测试）。
func testConfig() *auth.Config {
	return &auth.Config{
		SessionTTLHours:    24,
		MaxSessionTTLHours: 168,
		MaxLongSessions:    3,
		MaxTotalSessions:   10,
		MgmtTTLHours:       8,
		SessionTTLd:        24 * time.Hour,
		MaxAttachTTL:       168 * time.Hour,
		MgmtTTLd:           8 * time.Hour,
		MaxLongN:           3,
		MaxTotalN:          10,
	}
}

// newTestManager 返回一个 reaper 已禁用、使用假时钟的 manager/index。
func newTestManager(clk *testClock) (*SessionManager, *memoryIndex) {
	ix := NewIndex(5*time.Minute, -1, clk.now)
	mgr := NewManager(ix, clk.now)
	return mgr, ix
}

// addLocalSession 手工登记一个会话（无需真实 pty），用于 handler 级测试。
func addLocalSession(mgr *SessionManager, id, username, mode string, longLived bool, expireAt time.Time, closed bool) *localSession {
	now := mgr.clock()
	_ = mgr.index.Add(SessionInfo{
		ID: id, Username: username, Mode: mode, LongLived: longLived,
		CreatedAt: now, ExpireAt: expireAt,
	}, 0, 0)
	ls := &localSession{
		id: id, username: username, mode: mode, longLived: longLived,
		createdAt: now, closed: closed,
	}
	mgr.mu.Lock()
	mgr.local[id] = ls
	mgr.mu.Unlock()
	return ls
}

// requestAs 构造一个已注入 ctx 用户名的请求。
func requestAs(method, target, username string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return withUser(r, username)
}

// withUser 直接给请求注入 ctx 用户名。
func withUser(r *http.Request, username string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxUserKey, username))
}

// bodyRequest 给请求设置 JSON body。
func bodyRequest(r *http.Request, body string) *http.Request {
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}
