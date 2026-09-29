package auth

import (
	"context"
	"testing"
	"time"
)

// fakeClock 返回一个可手动步进的时钟。
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestMgmtTokenIssueValidate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewMgmtTokenStore(8 * time.Hour)
	s.SetNow(clk.now)

	tok, err := s.Issue("alice")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}

	user, ok := s.Validate(tok)
	if !ok || user != "alice" {
		t.Fatalf("Validate = (%q,%v), want (alice,true)", user, ok)
	}
	// 幂等：重复校验仍成功。
	if user, ok := s.Validate(tok); !ok || user != "alice" {
		t.Fatalf("Validate repeat = (%q,%v)", user, ok)
	}

	// 未知 token。
	if _, ok := s.Validate("nope"); ok {
		t.Error("unknown token should fail")
	}
}

func TestMgmtTokenExpiry(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewMgmtTokenStore(1 * time.Hour)
	s.SetNow(clk.now)

	tok, _ := s.Issue("bob")

	clk.advance(59 * time.Minute)
	if _, ok := s.Validate(tok); !ok {
		t.Error("token should still be valid before expiry")
	}

	clk.advance(2 * time.Minute) // 超过 1h
	if _, ok := s.Validate(tok); ok {
		t.Error("token should be expired")
	}
}

func TestMgmtTokenRevoke(t *testing.T) {
	s := NewMgmtTokenStore(time.Hour)
	tok, _ := s.Issue("carol")
	if _, ok := s.Validate(tok); !ok {
		t.Fatal("expected valid before revoke")
	}
	s.Revoke(tok)
	if _, ok := s.Validate(tok); ok {
		t.Error("expected invalid after revoke")
	}
	// 幂等：再次吊销不 panic。
	s.Revoke(tok)
}

func TestMgmtTokenCleanup(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewMgmtTokenStore(time.Hour)
	s.SetNow(clk.now)
	tok, _ := s.Issue("dave")

	if n := s.Cleanup(clk.now()); n != 0 {
		t.Errorf("Cleanup before expiry = %d, want 0", n)
	}
	clk.advance(2 * time.Hour)
	if n := s.Cleanup(clk.now()); n != 1 {
		t.Errorf("Cleanup after expiry = %d, want 1", n)
	}
	// 幂等：已删除后再清理为 0。
	if n := s.Cleanup(clk.now()); n != 0 {
		t.Errorf("Cleanup repeat = %d, want 0", n)
	}
	if _, ok := s.Validate(tok); ok {
		t.Error("cleaned token should be invalid")
	}
}

func TestMgmtTokenStartCleanupContext(t *testing.T) {
	s := NewMgmtTokenStore(time.Hour)
	// interval<=0 不启动：立即返回。
	s.StartCleanup(context.Background(), 0)

	ctx, cancel := context.WithCancel(context.Background())
	s.StartCleanup(ctx, 10*time.Millisecond)
	cancel() // 取消后 goroutine 应退出（此处仅验证不 panic/不泄漏阻塞）
	time.Sleep(30 * time.Millisecond)
}

func TestMgmtTokenDefaultTTL(t *testing.T) {
	// ttl<=0 走默认 8h。
	s := NewMgmtTokenStore(0)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.SetNow(clk.now)
	tok, _ := s.Issue("eve")
	clk.advance(7 * time.Hour)
	if _, ok := s.Validate(tok); !ok {
		t.Error("default ttl should keep token valid at 7h")
	}
	clk.advance(2 * time.Hour) // 9h
	if _, ok := s.Validate(tok); ok {
		t.Error("default ttl should expire after 8h")
	}
}
