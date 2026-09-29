package handler

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"webshell/pty"
)

// newWSServerConn 建立一个真实 WebSocket 连接，并返回服务端一侧的 *websocket.Conn
// （供 SessionManager.Attach 写入）。对端连接与服务由 t.Cleanup 自动关闭。
func newWSServerConn(t *testing.T) *websocket.Conn {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
		// 阻塞读取直到连接关闭，防止 handler 提前返回导致连接被回收。
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case c := <-connCh:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ws server conn")
		return nil
	}
}

// TestAttachNilProc 校验 proc 为 nil 时 Attach 不 panic（nil-proc 守卫生效），
// 并正常附着返回 true。
func TestAttachNilProc(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	mgr, ix := newTestManager(clk)
	// addLocalSession 不设置 proc，故 proc == nil。
	ls := addLocalSession(mgr, "s1", "alice", "bash", false, clk.now().Add(time.Hour), false)

	ac := &attachedConn{conn: newWSServerConn(t)}
	if !mgr.Attach(ls, ac) {
		t.Fatal("Attach(proc=nil) should succeed")
	}
	if info, ok := ix.Get("s1"); !ok || !info.Attached {
		t.Errorf("session should be marked attached, got %+v ok=%v", info, ok)
	}
	mgr.Detach(ls, ac)
}

// mustPty 在测试中创建一个真实 pty（bash）。不可用时跳过。
func mustPty(t *testing.T) *pty.Session {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	s, err := pty.NewSession(t.TempDir())
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	return s
}

func TestManagerCreateClose(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, ix := newTestManager(clk)

	proc := mustPty(t)
	ls, err := mgr.Create(CreateParams{
		ID: "s1", Username: "alice", Mode: "bash",
		LongLived: true, Hours: 2, MaxLong: 3, MaxTotal: 10,
	}, proc)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if mgr.GetLocal("s1") != ls {
		t.Error("GetLocal should return created session")
	}
	info, ok := ix.Get("s1")
	if !ok {
		t.Fatal("index should contain s1")
	}
	if !info.LongLived || !info.ExpireAt.Equal(base.Add(2*time.Hour)) {
		t.Errorf("info = %+v, want long expire +2h", info)
	}

	if !mgr.Close("s1") {
		t.Fatal("Close should return true first time")
	}
	if !ls.isClosed() {
		t.Error("local session should be closed")
	}
	if _, ok := ix.Get("s1"); ok {
		t.Error("index should not contain s1 after close")
	}
	if mgr.GetLocal("s1") != nil {
		t.Error("local map should not contain s1 after close")
	}
	// 幂等。
	if mgr.Close("s1") {
		t.Error("second Close should return false")
	}
}

func TestManagerCreateLimits(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)

	p1 := mustPty(t)
	_, err := mgr.Create(CreateParams{ID: "l1", Username: "u", LongLived: true, Hours: 1, MaxLong: 1, MaxTotal: 10}, p1)
	if err != nil {
		t.Fatalf("first long create: %v", err)
	}

	p2 := mustPty(t)
	if _, err := mgr.Create(CreateParams{ID: "l2", Username: "u", LongLived: true, Hours: 1, MaxLong: 1, MaxTotal: 10}, p2); err != ErrLongLimit {
		t.Fatalf("second long create = %v, want ErrLongLimit", err)
	}
	// 超额时刚建好的 pty 应被关闭。
	if !p2.IsClosed() {
		t.Error("rejected pty should be closed")
	}
	if mgr.GetLocal("l2") != nil {
		t.Error("rejected session should not be registered")
	}
}

func TestManagerCloseConcurrent(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, ix := newTestManager(clk)

	proc := mustPty(t)
	if _, err := mgr.Create(CreateParams{ID: "s1", Username: "u", LongLived: true, Hours: 1, MaxLong: 3, MaxTotal: 10}, proc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 并发 Close：恰好一个返回 true（幂等，无双重关闭/死锁）。
	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mgr.Close("s1") {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if okCount != 1 {
		t.Errorf("Close returned true %d times, want 1", okCount)
	}
	if _, ok := ix.Get("s1"); ok {
		t.Error("index should not contain s1")
	}
	if !proc.IsClosed() {
		t.Error("pty should be closed")
	}
}

func TestManagerReaperClosesExpired(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, ix := newTestManager(clk)

	proc := mustPty(t)
	// 短期：expireAt = base + 5min（Create 内设定），无连接。
	if _, err := mgr.Create(CreateParams{ID: "short", Username: "u", MaxLong: 3, MaxTotal: 10}, proc); err != nil {
		t.Fatalf("Create short: %v", err)
	}

	if n := ix.ReapOnce(); n != 0 {
		t.Fatalf("ReapOnce before expiry = %d, want 0", n)
	}
	clk.advance(6 * time.Minute)
	if n := ix.ReapOnce(); n != 1 {
		t.Fatalf("ReapOnce after expiry = %d, want 1", n)
	}
	if mgr.GetLocal("short") != nil {
		t.Error("expired short session should be closed/removed")
	}
	if !proc.IsClosed() {
		t.Error("pty should be closed after reap")
	}
}
