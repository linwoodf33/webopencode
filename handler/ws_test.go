package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"webshell/auth"
)

func TestWsRequiresTicket(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	tickets := auth.NewTicketStore()
	h := NewWs(mgr, tickets, testConfig())

	// 无 ticket → 401。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no ticket = %d, want 401", rec.Code)
	}

	// 错误 ticket → 401。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws?ticket=bogus", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad ticket = %d, want 401", rec.Code)
	}

	// 仅凭 sessionID（无 ticket）→ 401。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws?session=abc&username=alice", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("session-only = %d, want 401", rec.Code)
	}
}

func TestWsTicketReplayRejected(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	tickets := auth.NewTicketStore()
	h := NewWs(mgr, tickets, testConfig())

	// 为一个存在的会话签发 attach ticket。
	addLocalSession(mgr, "s1", "alice", "bash", true, clk.now().Add(time.Hour), false)
	tok, err := tickets.Issue(auth.TicketPurpose{Action: "attach", Username: "alice", Session: "s1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// 第一次：ticket 被消费（随后 Upgrade 在 httptest recorder 上失败，但 ticket 已消费）。
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/ws?ticket="+tok, nil))
	if _, ok := tickets.Consume(tok); ok {
		t.Fatal("ticket should have been consumed on first attempt")
	}

	// 第二次（重放）→ 401。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/ws?ticket="+tok, nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("replay = %d, want 401", rec2.Code)
	}
}
