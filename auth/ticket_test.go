package auth

import (
	"context"
	"testing"
	"time"
)

func TestTicketIssueConsume(t *testing.T) {
	s := NewTicketStore()
	p := TicketPurpose{Action: "attach", Username: "alice", Session: "sid1", Mode: "bash"}
	tok, err := s.Issue(p)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty ticket")
	}

	got, ok := s.Consume(tok)
	if !ok {
		t.Fatal("expected first consume to succeed")
	}
	if got.Action != "attach" || got.Username != "alice" || got.Session != "sid1" {
		t.Fatalf("purpose mismatch: %+v", got)
	}

	// 重放（已消费）必须失败。
	if _, ok := s.Consume(tok); ok {
		t.Fatal("expected replay to be rejected")
	}
	// 不存在的 ticket。
	if _, ok := s.Consume("nonexistent"); ok {
		t.Fatal("expected nonexistent ticket rejected")
	}
}

func TestTicketCreatePurposeCopy(t *testing.T) {
	s := NewTicketStore()
	p := TicketPurpose{Action: "create", Username: "bob", HomeDir: "/home/bob", Mode: "opencode", Type: "long", Hours: 10}
	tok, _ := s.Issue(p)
	got, ok := s.Consume(tok)
	if !ok {
		t.Fatal("consume failed")
	}
	if got.Mode != "opencode" || got.Type != "long" || got.Hours != 10 || got.HomeDir != "/home/bob" {
		t.Fatalf("purpose mismatch: %+v", got)
	}
}

func TestTicketExpired(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewTicketStore()
	s.SetNow(clk.now)

	tok, _ := s.Issue(TicketPurpose{Action: "attach", Username: "u"})
	clk.advance(59 * time.Second)
	if _, ok := s.Consume(tok); !ok {
		t.Error("ticket should be valid before 60s")
	}

	tok2, _ := s.Issue(TicketPurpose{Action: "attach", Username: "u"})
	clk.advance(61 * time.Second)
	if _, ok := s.Consume(tok2); ok {
		t.Error("ticket should be expired after 60s")
	}
}

func TestTicketCleanup(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewTicketStore()
	s.SetNow(clk.now)
	s.Issue(TicketPurpose{Action: "attach"})

	if n := s.Cleanup(clk.now()); n != 0 {
		t.Errorf("Cleanup before expiry = %d, want 0", n)
	}
	clk.advance(2 * time.Minute)
	if n := s.Cleanup(clk.now()); n != 1 {
		t.Errorf("Cleanup after expiry = %d, want 1", n)
	}
}

func TestTicketStartCleanupNoop(t *testing.T) {
	s := NewTicketStore()
	s.StartCleanup(context.Background(), 0) // interval<=0：不启动，立即返回
}
