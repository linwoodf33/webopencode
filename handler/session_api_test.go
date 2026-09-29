package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"webshell/auth"
)

// stubHome 临时替换 lookupHome，返回 /home/<user>。
func stubHome(t *testing.T) {
	t.Helper()
	old := lookupHome
	lookupHome = func(username string) (string, error) { return "/home/" + username, nil }
	t.Cleanup(func() { lookupHome = old })
}

func TestRequireMgmt(t *testing.T) {
	mgmt := auth.NewMgmtTokenStore(time.Hour)
	tok, _ := mgmt.Issue("alice")

	reached := ""
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = ctxUsername(r)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	h := RequireMgmt(mgmt)(next)

	// 无 Authorization → 401。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", rec.Code)
	}

	// 错误 token → 401。
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	r.Header.Set("Authorization", "Bearer bogus")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", rec.Code)
	}

	// 正确 token → 200，且注入用户名。
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || reached != "alice" {
		t.Errorf("good token = %d user=%q, want 200 alice", rec.Code, reached)
	}
}

func TestSessionsList(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)
	_ = mgr.index.Add(SessionInfo{ID: "a", Username: "alice", Mode: "bash", CreatedAt: base, ExpireAt: base.Add(time.Hour)}, 0, 0)
	_ = mgr.index.Add(SessionInfo{ID: "b", Username: "alice", Mode: "codex", CreatedAt: base.Add(time.Minute), ExpireAt: base.Add(time.Hour), Attached: true}, 0, 0)
	_ = mgr.index.Add(SessionInfo{ID: "c", Username: "bob", CreatedAt: base, ExpireAt: base.Add(time.Hour)}, 0, 0)

	rec := httptest.NewRecorder()
	NewSessionsHandler(mgr).ServeHTTP(rec, requestAs(http.MethodGet, "/api/sessions", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []sessionInfoDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "b" || got[1].ID != "a" {
		t.Errorf("order = %q,%q, want b,a", got[0].ID, got[1].ID)
	}
	if !got[0].Active {
		t.Error("b should be active")
	}
	if got[0].CreatedAt != base.Add(time.Minute).Unix() {
		t.Errorf("created_at = %d", got[0].CreatedAt)
	}
}

func TestTicketCreate(t *testing.T) {
	stubHome(t)
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	cfg := testConfig()
	tickets := auth.NewTicketStore()
	h := NewTicketHandler(mgr, tickets, cfg)

	// 长期 + 指定 hours。
	body := `{"action":"create","mode":"bash","type":"long","hours":5}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), body))
	if rec.Code != http.StatusOK {
		t.Fatalf("create long status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ticketResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Ticket == "" || resp.ExpiresIn != 60 {
		t.Fatalf("bad ticket response: %+v", resp)
	}
	p, ok := tickets.Consume(resp.Ticket)
	if !ok || p.Action != "create" || p.Username != "alice" || p.HomeDir != "/home/alice" || p.Mode != "bash" || p.Type != "long" || p.Hours != 5 {
		t.Fatalf("purpose = %+v ok=%v", p, ok)
	}

	// 长期缺省 hours → cfg.SessionTTLHours。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"create","mode":"bash","type":"long"}`))
	var resp2 ticketResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp2)
	p2, _ := tickets.Consume(resp2.Ticket)
	if p2 == nil || p2.Hours != 24 {
		t.Fatalf("default hours = %+v, want 24", p2)
	}

	// 缺省 type → short（向后兼容）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"create"}`))
	var resp3 ticketResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp3)
	p3, _ := tickets.Consume(resp3.Ticket)
	if p3 == nil || p3.Type != "short" || p3.Mode != "bash" || p3.Hours != 0 {
		t.Fatalf("default type = %+v, want short/bash/0", p3)
	}

	// hours 越界 / 非整数。
	for _, bad := range []string{`{"action":"create","type":"long","hours":0.5}`, `{"action":"create","type":"long","hours":200}`, `{"action":"create","type":"long","hours":2.5}`} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), bad))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad hours %s = %d, want 400", bad, rec.Code)
		}
	}

	// 非法 mode。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"create","mode":"zsh"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad mode = %d, want 400", rec.Code)
	}

	// 长期名额满。
	for i := 0; i < 3; i++ {
		_ = mgr.index.Add(SessionInfo{ID: newSessionID(), Username: "alice", LongLived: true, ExpireAt: clk.now().Add(time.Hour)}, 0, 0)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"create","type":"long","hours":1}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "长期会话数量已达上限") {
		t.Errorf("long limit = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTicketCreateTotalLimit(t *testing.T) {
	stubHome(t)
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	cfg := testConfig()
	tickets := auth.NewTicketStore()
	h := NewTicketHandler(mgr, tickets, cfg)

	for i := 0; i < cfg.MaxTotalN; i++ {
		_ = mgr.index.Add(SessionInfo{ID: newSessionID(), Username: "alice", ExpireAt: clk.now().Add(time.Hour)}, 0, 0)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"create"}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "会话总数已达上限") {
		t.Errorf("total limit = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTicketAttach(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)
	tickets := auth.NewTicketStore()
	h := NewTicketHandler(mgr, tickets, testConfig())

	addLocalSession(mgr, "s1", "alice", "codex", false, base.Add(time.Hour), false)

	// 正常 attach。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"attach","session":"s1"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ticketResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	p, ok := tickets.Consume(resp.Ticket)
	if !ok || p.Action != "attach" || p.Session != "s1" || p.Mode != "codex" {
		t.Fatalf("purpose = %+v ok=%v", p, ok)
	}

	// 他人会话 → 404（防枚举）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "bob"), `{"action":"attach","session":"s1"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign = %d, want 404", rec.Code)
	}

	// 不存在 → 404。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"attach","session":"nope"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing = %d, want 404", rec.Code)
	}

	// 长期已过期 → 404。
	addLocalSession(mgr, "s2", "alice", "bash", true, base.Add(-time.Hour), false)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bodyRequest(requestAs(http.MethodPost, "/api/session/ticket", "alice"), `{"action":"attach","session":"s2"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expired = %d, want 404", rec.Code)
	}
}

func TestExtendHandler(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)
	cfg := testConfig()
	h := NewExtendHandler(mgr, cfg)

	addLocalSession(mgr, "long", "alice", "bash", true, base.Add(10*time.Hour), false)
	addLocalSession(mgr, "short", "alice", "bash", false, base.Add(10*time.Minute), false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodPost, "/api/session/extend?session=long&hours=5", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("extend = %d body=%s", rec.Code, rec.Body.String())
	}
	info, _ := mgr.index.Get("long")
	if !info.ExpireAt.Equal(base.Add(15 * time.Hour)) {
		t.Errorf("expire = %v, want %v", info.ExpireAt, base.Add(15*time.Hour))
	}

	// 非长期 → 400。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodPost, "/api/session/extend?session=short&hours=5", "alice"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("extend short = %d, want 400", rec.Code)
	}
	// 他人 → 404。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodPost, "/api/session/extend?session=long&hours=5", "bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("extend foreign = %d, want 404", rec.Code)
	}
	// hours 越界 → 400。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodPost, "/api/session/extend?session=long&hours=999", "alice"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("extend bad hours = %d, want 400", rec.Code)
	}
}

func TestCloseHandler(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)
	h := NewCloseHandler(mgr)

	addLocalSession(mgr, "s1", "alice", "bash", true, base.Add(time.Hour), false)

	// 他人 → 404。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodPost, "/api/session/close?session=s1", "bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("close foreign = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodPost, "/api/session/close?session=s1", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("close = %d", rec.Code)
	}
	if _, ok := mgr.index.Get("s1"); ok {
		t.Error("session should be removed after close")
	}
	if mgr.GetLocal("s1") != nil {
		t.Error("local session should be removed after close")
	}
}

func TestLogoutHandler(t *testing.T) {
	mgmt := auth.NewMgmtTokenStore(time.Hour)
	tok, _ := mgmt.Issue("alice")
	h := NewLogoutHandler(mgmt)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}
	if _, ok := mgmt.Validate(tok); ok {
		t.Error("token should be revoked")
	}
}

func TestResumeHandler(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)
	h := NewResumeHandler(mgr)

	addLocalSession(mgr, "s1", "alice", "opencode", true, base.Add(time.Hour), false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodGet, "/api/resume?session=s1", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["mode"] != "opencode" || got["long_lived"] != true {
		t.Errorf("resume body = %v", got)
	}

	// 他人 → 404。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodGet, "/api/resume?session=s1", "bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("resume foreign = %d, want 404", rec.Code)
	}
	// 不存在 → 404。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodGet, "/api/resume?session=nope", "alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("resume missing = %d, want 404", rec.Code)
	}
}
