package audit

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T, opt Options) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), opt)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// writeSession 通过 store 打开并关闭一个会话。
func writeSession(t *testing.T, s *Store, meta SessionMeta, emit func(*Recorder)) {
	t.Helper()
	r, err := s.Open(meta)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if emit != nil {
		emit(r)
	}
	r.Close("test")
}

// mkSession 直接写入一个会话目录（meta.json + cast.log）。
func mkSession(t *testing.T, s *Store, user, sid string, endedAt *time.Time) {
	t.Helper()
	dir := filepath.Join(s.root, user, sid)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := SessionMeta{
		SessionID: sid,
		Username:  user,
		CreatedAt: time.Unix(1700000000, 0),
		StartedAt: time.Unix(1700000000, 0),
		EndedAt:   endedAt,
	}
	if endedAt != nil {
		meta.EndReason = "test"
	}
	if err := writeMetaFile(filepath.Join(dir, "meta.json"), meta); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cast.log"), []byte("{\"version\":2}\n[0.0,\"o\",\"x\"]\n"), castFileMode); err != nil {
		t.Fatalf("write cast: %v", err)
	}
}

func TestStoreOpenLayout(t *testing.T) {
	s := newTestStore(t, Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1})
	meta := SessionMeta{
		SessionID: "sess01", Username: "alice", Mode: "bash",
		CreatedAt: time.Unix(1700000000, 0), StartedAt: time.Unix(1700000000, 0),
	}
	r, err := s.Open(meta)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.WriteOutput([]byte("hi"))
	r.Close("done")

	for _, name := range []string{"meta.json", "cast.log"} {
		p := filepath.Join(s.root, "alice", "sess01", name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
}

func TestStoreList(t *testing.T) {
	s := newTestStore(t, Options{RecordInput: true, FlushInterval: 5 * time.Millisecond})
	writeSession(t, s, SessionMeta{SessionID: "a1", Username: "alice", Mode: "bash", CreatedAt: time.Unix(1700000100, 0), StartedAt: time.Unix(1700000100, 0)}, nil)
	writeSession(t, s, SessionMeta{SessionID: "a2", Username: "alice", Mode: "opencode", CreatedAt: time.Unix(1700000200, 0), StartedAt: time.Unix(1700000200, 0)}, nil)
	writeSession(t, s, SessionMeta{SessionID: "b1", Username: "bob", Mode: "bash", CreatedAt: time.Unix(1700000300, 0), StartedAt: time.Unix(1700000300, 0)}, nil)

	items, total, err := s.List(ListOptions{Username: "alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("alice: total=%d len=%d", total, len(items))
	}
	if items[0].SessionID != "a2" || items[1].SessionID != "a1" {
		t.Fatalf("order wrong: %+v", items)
	}

	_, totalAll, err := s.List(ListOptions{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if totalAll != 3 {
		t.Fatalf("all total=%d, want 3", totalAll)
	}

	// 分页：desc 为 b1,a2,a1；offset=1 limit=1 → a2
	page, tot, err := s.List(ListOptions{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("List page: %v", err)
	}
	if tot != 3 || len(page) != 1 || page[0].SessionID != "a2" {
		t.Fatalf("pagination wrong: tot=%d page=%+v", tot, page)
	}

	// 日期过滤：CreatedAt=1700000000 UTC = 2023-11-14。
	_, n, err := s.List(ListOptions{FromDate: "2023-11-15"})
	if err != nil {
		t.Fatalf("List date: %v", err)
	}
	if n != 0 {
		t.Fatalf("from_date filter expected 0, got %d", n)
	}
	_, n, err = s.List(ListOptions{ToDate: "2023-11-15"})
	if err != nil {
		t.Fatalf("List date: %v", err)
	}
	if n != 3 {
		t.Fatalf("to_date filter expected 3, got %d", n)
	}

	// 越界 offset：items 为空但 total 仍为过滤后总数（前端越界自愈依赖此契约）。
	emptyPage, totBeyond, err := s.List(ListOptions{Offset: 200, Limit: 10})
	if err != nil {
		t.Fatalf("List offset beyond: %v", err)
	}
	if len(emptyPage) != 0 || totBeyond != 3 {
		t.Fatalf("offset beyond: len=%d total=%d, want len=0 total=3", len(emptyPage), totBeyond)
	}

	// limit<=0 兜底为默认 100：返回全部 3 条。
	all, totDefault, err := s.List(ListOptions{Limit: 0})
	if err != nil {
		t.Fatalf("List limit<=0: %v", err)
	}
	if len(all) != 3 || totDefault != 3 {
		t.Fatalf("limit<=0: len=%d total=%d, want len=3 total=3", len(all), totDefault)
	}
}

func TestStoreListActiveMarker(t *testing.T) {
	s := newTestStore(t, Options{RecordInput: true, FlushInterval: 5 * time.Millisecond})
	writeSession(t, s, SessionMeta{SessionID: "a1", Username: "alice", Mode: "bash", CreatedAt: time.Unix(1700000100, 0), StartedAt: time.Unix(1700000100, 0)}, nil)

	s.SetActiveResolver(func() map[string]bool { return map[string]bool{"a1": true} })
	items, _, err := s.List(ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || !items[0].Active {
		t.Fatalf("expected active item: %+v", items)
	}
}

func TestStoreIsActive(t *testing.T) {
	s := newTestStore(t, Options{})

	// resolver 为 nil → 一律 false。
	if s.IsActive("a1") {
		t.Fatal("nil resolver should report inactive")
	}

	s.SetActiveResolver(func() map[string]bool { return map[string]bool{"a1": true} })
	if !s.IsActive("a1") {
		t.Fatal("a1 should be active")
	}
	if s.IsActive("nope") {
		t.Fatal("unknown sid should be inactive")
	}
}

func TestStoreCleanRetention(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s := newTestStore(t, Options{RetentionDays: 1})
	s.now = func() time.Time { return now }

	oldEnd := now.Add(-48 * time.Hour)
	recentEnd := now.Add(-1 * time.Hour)
	mkSession(t, s, "alice", "old1", &oldEnd)
	mkSession(t, s, "alice", "new1", &recentEnd)

	n, err := s.CleanOnce(now)
	if err != nil {
		t.Fatalf("CleanOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted=%d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(s.root, "alice", "old1")); !os.IsNotExist(err) {
		t.Fatal("old1 should be deleted")
	}
	if _, err := os.Stat(filepath.Join(s.root, "alice", "new1")); err != nil {
		t.Fatalf("new1 should remain: %v", err)
	}
}

func TestStoreCleanQuota(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s := newTestStore(t, Options{MaxTotalBytes: 100})
	s.now = func() time.Time { return now }

	older := now.Add(-3 * time.Hour)
	newer := now.Add(-1 * time.Hour)
	writeBigSession(t, s, "alice", "old1", &older, 80)
	writeBigSession(t, s, "alice", "new1", &newer, 80)

	n, err := s.CleanOnce(now)
	if err != nil {
		t.Fatalf("CleanOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted=%d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(s.root, "alice", "old1")); !os.IsNotExist(err) {
		t.Fatal("oldest should be deleted")
	}
	if _, err := os.Stat(filepath.Join(s.root, "alice", "new1")); err != nil {
		t.Fatalf("newest should remain: %v", err)
	}

	// 活跃会话排除：把 old1 标为活跃后，配额下不再有可删候选。
	s.SetActiveResolver(func() map[string]bool { return map[string]bool{"old1": true} })
	n, err = s.CleanOnce(now)
	if err != nil {
		t.Fatalf("CleanOnce active: %v", err)
	}
	if n != 0 {
		t.Fatalf("active session should be excluded, deleted=%d", n)
	}
}

// writeBigSession 写入指定大小的 cast.log。
func writeBigSession(t *testing.T, s *Store, user, sid string, endedAt *time.Time, size int) {
	t.Helper()
	dir := filepath.Join(s.root, user, sid)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := SessionMeta{
		SessionID: sid, Username: user,
		CreatedAt: time.Unix(1700000000, 0), StartedAt: time.Unix(1700000000, 0),
		EndedAt: endedAt, EndReason: "test",
	}
	if err := writeMetaFile(filepath.Join(dir, "meta.json"), meta); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cast.log"), bytes.Repeat([]byte("x"), size), castFileMode); err != nil {
		t.Fatalf("write cast: %v", err)
	}
}

func TestStoreScanAndFinalize(t *testing.T) {
	s := newTestStore(t, Options{})
	dir := filepath.Join(s.root, "alice", "s1")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	started := time.Unix(1700000000, 0)
	if err := writeMetaFile(filepath.Join(dir, "meta.json"), SessionMeta{
		SessionID: "s1", Username: "alice", StartedAt: started, CreatedAt: started,
	}); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cast.log"), []byte("{\"version\":2}\n[2.0,\"o\",\"x\"]\n"), castFileMode); err != nil {
		t.Fatalf("write cast: %v", err)
	}

	if err := s.ScanAndFinalize(); err != nil {
		t.Fatalf("ScanAndFinalize: %v", err)
	}
	m, ok, err := s.ReadMeta("alice", "s1")
	if err != nil || !ok {
		t.Fatalf("ReadMeta: ok=%v err=%v", ok, err)
	}
	if m.EndedAt == nil {
		t.Fatal("ended_at not set")
	}
	if m.EndReason != "server restart (abnormal)" {
		t.Fatalf("end_reason=%q", m.EndReason)
	}
	want := started.Add(2 * time.Second)
	if !m.EndedAt.Equal(want) {
		t.Fatalf("ended_at=%v, want %v", m.EndedAt, want)
	}
	foundClose := false
	for _, e := range m.Timeline {
		if e.Event == "close" {
			foundClose = true
		}
	}
	if !foundClose {
		t.Fatal("close event missing from timeline")
	}

	// 幂等：再次巡检不改变 ended_at。
	first := *m.EndedAt
	if err := s.ScanAndFinalize(); err != nil {
		t.Fatalf("ScanAndFinalize#2: %v", err)
	}
	m2, _, err := s.ReadMeta("alice", "s1")
	if err != nil {
		t.Fatalf("ReadMeta#2: %v", err)
	}
	if !m2.EndedAt.Equal(first) {
		t.Fatalf("not idempotent: %v vs %v", m2.EndedAt, first)
	}
}

func TestStorePathTraversal(t *testing.T) {
	s := newTestStore(t, Options{})

	if _, err := s.Open(SessionMeta{Username: "alice", SessionID: "../evil"}); err == nil {
		t.Fatal("expected error for bad session id")
	}
	if _, err := s.Open(SessionMeta{Username: "..", SessionID: "s1"}); err == nil {
		t.Fatal("expected error for bad username")
	}
	if _, ok, _ := s.ReadMeta("..", "x"); ok {
		t.Fatal("ReadMeta traversal should be rejected")
	}
	if _, err := s.ReadCast("alice", ".."); err == nil {
		t.Fatal("ReadCast traversal should be rejected")
	}
	// 确认 root 外未被创建
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.root), "evil")); err == nil {
		t.Fatal("path traversal created files outside root")
	}
}

func TestStoreDownload(t *testing.T) {
	s := newTestStore(t, Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1})
	writeSession(t, s, SessionMeta{SessionID: "dl1", Username: "alice", Mode: "bash", CreatedAt: time.Unix(1700000000, 0), StartedAt: time.Unix(1700000000, 0)}, func(r *Recorder) {
		r.WriteOutput([]byte("out"))
	})

	var buf bytes.Buffer
	if err := s.Download("alice", "dl1", &buf, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
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
	if !names["meta.json"] || !names["dl1.cast"] {
		t.Fatalf("zip contents wrong: %v", names)
	}
}

// TestStoreDownloadOfflineHTML 验证 offlineHTML 非空时追加 offline-player.html 条目，
// 且内容逐字节等于传入值（三个条目）。
func TestStoreDownloadOfflineHTML(t *testing.T) {
	s := newTestStore(t, Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1})
	writeSession(t, s, SessionMeta{SessionID: "dl2", Username: "alice", Mode: "bash", CreatedAt: time.Unix(1700000000, 0), StartedAt: time.Unix(1700000000, 0)}, func(r *Recorder) {
		r.WriteOutput([]byte("out"))
	})

	html := []byte("<!DOCTYPE html><html><body>offline-marker</body></html>")
	var buf bytes.Buffer
	if err := s.Download("alice", "dl2", &buf, html); err != nil {
		t.Fatalf("Download: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
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
	if len(names) != 3 || !names["meta.json"] || !names["dl2.cast"] || !names["offline-player.html"] {
		t.Fatalf("zip contents wrong: %v", names)
	}
	if !bytes.Equal(offline, html) {
		t.Fatalf("offline html mismatch: got %q want %q", offline, html)
	}
}
