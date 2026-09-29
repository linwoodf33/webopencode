package handler

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"webshell/auth"
	"webshell/pty"
)

// multipartRequest 构造一个带文件字段的 multipart 请求。
func multipartRequest(t *testing.T, method, target, username string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "hello.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("hello world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = mw.Close()

	r := httptest.NewRequest(method, target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return withUser(r, username)
}

// stubUploadSeams 替换 pty 文件操作接缝，返回恢复函数。
func stubUploadSeams(t *testing.T) {
	t.Helper()
	oldWrite := writeFileAsUser
	oldCheck := checkWritableAsUser
	writeFileAsUser = func(s *pty.Session, filename string, data []byte) (string, error) {
		return "/home/alice/" + filename, nil
	}
	checkWritableAsUser = func(s *pty.Session) (string, error) {
		return "/home/alice", nil
	}
	t.Cleanup(func() { writeFileAsUser = oldWrite; checkWritableAsUser = oldCheck })
}

func TestUploadHandlerAuth(t *testing.T) {
	stubUploadSeams(t)
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	addLocalSession(mgr, "s1", "alice", "bash", true, clk.now().Add(time.Hour), false)

	mgmt := auth.NewMgmtTokenStore(time.Hour)
	h := RequireMgmt(mgmt)(NewUploadHandler(mgr))

	// 无 mgmt → 401。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, multipartRequest(t, http.MethodPost, "/api/upload?session=s1", "alice"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no mgmt = %d, want 401", rec.Code)
	}
}

func TestUploadHandlerOwnership(t *testing.T) {
	stubUploadSeams(t)
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	addLocalSession(mgr, "s1", "alice", "bash", true, clk.now().Add(time.Hour), false)
	addLocalSession(mgr, "s2", "alice", "bash", true, clk.now().Add(time.Hour), true) // 已关闭
	h := NewUploadHandler(mgr)

	// 他人会话 → 404。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, multipartRequest(t, http.MethodPost, "/api/upload?session=s1", "bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign = %d, want 404", rec.Code)
	}
	// 已关闭 → 404。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, multipartRequest(t, http.MethodPost, "/api/upload?session=s2", "alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("closed = %d, want 404", rec.Code)
	}
	// 不存在 → 404。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, multipartRequest(t, http.MethodPost, "/api/upload?session=nope", "alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing = %d, want 404", rec.Code)
	}
	// 正常上传 → 200。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, multipartRequest(t, http.MethodPost, "/api/upload?session=s1", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUploadCheckHandler(t *testing.T) {
	stubUploadSeams(t)
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, _ := newTestManager(clk)
	addLocalSession(mgr, "s1", "alice", "bash", true, clk.now().Add(time.Hour), false)
	h := NewUploadCheckHandler(mgr)

	// 他人 → 404。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodGet, "/api/upload-check?session=s1", "bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign = %d, want 404", rec.Code)
	}
	// 正常 → 200。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(http.MethodGet, "/api/upload-check?session=s1", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload-check = %d body=%s", rec.Code, rec.Body.String())
	}
}
