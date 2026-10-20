package handler

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"webshell/audit"
	"webshell/auth"
)

// auditTestCfg 返回一份可用的审计配置：auditor 为审计员，普通用户可见本人。
func auditTestCfg() *auth.Config {
	return &auth.Config{
		AuditUserSelf: true,
		AuditAdmins:   map[string]bool{"auditor": true},
	}
}

// newAuditAPIWithRoot 在临时目录建审计 store（可选注入活跃集合）并返回 API 与根目录。
func newAuditAPIWithRoot(t *testing.T, active map[string]bool) (*AuditAPI, string) {
	t.Helper()
	root := t.TempDir()
	st, err := audit.NewStore(root, audit.Options{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if active != nil {
		st.SetActiveResolver(func() map[string]bool { return active })
	}
	return NewAuditAPI(nil, st, auditTestCfg()), root
}

// writeAuditSession 直接写入一个会话目录（meta.json + cast.log）。
func writeAuditSession(t *testing.T, root, user, sid, cast string, ended bool) {
	t.Helper()
	dir := filepath.Join(root, user, sid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := audit.SessionMeta{
		SessionID: sid, Username: user, Mode: "bash",
		CreatedAt: time.Unix(1700000000, 0), StartedAt: time.Unix(1700000000, 0),
	}
	if ended {
		e := time.Unix(1700003600, 0)
		meta.EndedAt = &e
		meta.EndReason = "test"
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cast.log"), []byte(cast), 0o600); err != nil {
		t.Fatalf("write cast: %v", err)
	}
}

const auditCastBody = "{\"version\":2}\n[0.0,\"o\",\"x\"]\n"

// active 会话 cast → 409。
func TestAuditCastActiveConflict(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, map[string]bool{"s1": true})
	writeAuditSession(t, root, "alice", "s1", auditCastBody, false)

	h := NewAuditCastHandler(api)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/cast?session=s1", "auditor"))
	if w.Code != http.StatusConflict {
		t.Fatalf("active cast = %d, want 409", w.Code)
	}
}

// active 会话 download → 409。
func TestAuditDownloadActiveConflict(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, map[string]bool{"s1": true})
	writeAuditSession(t, root, "alice", "s1", auditCastBody, false)

	h := NewAuditDownloadHandler(api, nil)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/download?session=s1", "auditor"))
	if w.Code != http.StatusConflict {
		t.Fatalf("active download = %d, want 409", w.Code)
	}
}

// inactive 会话 cast → 200 且 body == cast 原文。
func TestAuditCastInactiveOK(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, nil)
	writeAuditSession(t, root, "alice", "s1", auditCastBody, true)

	h := NewAuditCastHandler(api)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/cast?session=s1", "auditor"))
	if w.Code != http.StatusOK {
		t.Fatalf("inactive cast = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != auditCastBody {
		t.Fatalf("cast body = %q, want %q", got, auditCastBody)
	}
}

// inactive 会话 download → 200 且 zip 可解析含两个条目。
func TestAuditDownloadInactiveOK(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, nil)
	writeAuditSession(t, root, "alice", "s1", auditCastBody, true)

	h := NewAuditDownloadHandler(api, nil)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/download?session=s1", "auditor"))
	if w.Code != http.StatusOK {
		t.Fatalf("inactive download = %d, want 200", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("zip entry open: %v", err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("zip entry read: %v", err)
		}
		_ = rc.Close()
	}
	if len(names) != 2 || !names["meta.json"] || !names["s1.cast"] {
		t.Fatalf("zip contents wrong: %v", names)
	}
}

// 普通用户（非审计员，user_can_view_self=true）查本人 active 会话 → 409。
func TestAuditCastSelfActiveConflict(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, map[string]bool{"s1": true})
	writeAuditSession(t, root, "alice", "s1", auditCastBody, false)

	h := NewAuditCastHandler(api)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/cast?session=s1", "alice"))
	if w.Code != http.StatusConflict {
		t.Fatalf("self active cast = %d, want 409", w.Code)
	}
}

// 非审计员查他人 inactive 会话 → 404（防枚举）。
func TestAuditCastForeignNotFound(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, nil)
	writeAuditSession(t, root, "bob", "s2", auditCastBody, true)

	h := NewAuditCastHandler(api)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/cast?session=s2", "alice"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign cast = %d, want 404", w.Code)
	}
}

// inactive 会话 download（带离线模板）→ 200，zip 三条目，offline-player.html 内联
// js/css 特征与元信息。
func TestAuditDownloadInactiveWithTemplate(t *testing.T) {
	api, root := newAuditAPIWithRoot(t, nil)
	writeAuditSession(t, root, "alice", "s1", auditCastBody, true)

	tmpl := audit.OfflinePlayerTemplate([]byte("js-marker"), []byte("css-marker"))
	h := NewAuditDownloadHandler(api, tmpl)
	w := httptest.NewRecorder()
	h(w, requestAs(http.MethodGet, "/api/audit/download?session=s1", "auditor"))
	if w.Code != http.StatusOK {
		t.Fatalf("inactive download = %d, want 200", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	names := map[string]bool{}
	var offline []byte
	for _, f := range zr.File {
		names[f.Name] = true
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("zip entry open: %v", err)
		}
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("zip entry read: %v", err)
		}
		_ = rc.Close()
		if f.Name == "offline-player.html" {
			offline = b
		}
	}
	if len(names) != 3 || !names["meta.json"] || !names["s1.cast"] || !names["offline-player.html"] {
		t.Fatalf("zip contents wrong: %v", names)
	}
	for _, marker := range []string{"js-marker", "css-marker", `id="player"`, "f-username"} {
		if !bytes.Contains(offline, []byte(marker)) {
			t.Fatalf("offline html missing marker %q", marker)
		}
	}
}
