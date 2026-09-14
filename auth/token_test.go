package auth

import (
	"testing"
	"time"
)

func TestTokenStoreIssueConsume(t *testing.T) {
	ts := NewTokenStore(5 * time.Minute)
	tok, err := ts.Issue("alice")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}

	// 首次消费成功，返回绑定用户名。
	user, ok := ts.Consume(tok)
	if !ok {
		t.Fatal("expected first consume to succeed")
	}
	if user != "alice" {
		t.Fatalf("expected alice, got %q", user)
	}

	// 重放（已消费）必须失败。
	if _, ok := ts.Consume(tok); ok {
		t.Fatal("expected replay to be rejected")
	}

	// 不存在的 token 必须失败。
	if _, ok := ts.Consume("nonexistent"); ok {
		t.Fatal("expected nonexistent token to be rejected")
	}
}

func TestTokenStoreExpired(t *testing.T) {
	// 使用极短 TTL，使 token 在签发后立即过期。
	ts := NewTokenStore(-1 * time.Millisecond)
	// 手工构造一个过期记录。
	ts.mu.Lock()
	ts.data["expired"] = &tokenRecord{
		username: "bob",
		expireAt: time.Now().Add(-time.Second),
	}
	ts.mu.Unlock()

	if _, ok := ts.Consume("expired"); ok {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestTokenStoreDifferentUsers(t *testing.T) {
	ts := NewTokenStore(5 * time.Minute)
	t1, _ := ts.Issue("u1")
	t2, _ := ts.Issue("u2")
	u1, ok1 := ts.Consume(t1)
	u2, ok2 := ts.Consume(t2)
	if !ok1 || u1 != "u1" {
		t.Fatalf("u1 consume wrong: %v %q", ok1, u1)
	}
	if !ok2 || u2 != "u2" {
		t.Fatalf("u2 consume wrong: %v %q", ok2, u2)
	}
}
