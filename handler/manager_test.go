package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"webshell/audit"
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
	if !mgr.Attach(ls, ac, audit.ClientInfo{IP: "10.0.0.1"}) {
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

// TestManagerCloseRemovesIndexBeforeProcClose 回归：close 的索引移除必须先于
// proc.Close（其含 cmd.Wait，可能长时间阻塞）。用阻塞桩替代 proc.Close，断言在
// 桩尚未返回时 index.Get 已 !ok——即状态一致性窗口不随 proc.Close 耗时增长。
func TestManagerCloseRemovesIndexBeforeProcClose(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, ix := newTestManager(clk)

	ls := addLocalSession(mgr, "s1", "alice", "bash", false, base.Add(time.Hour), false)
	// 非 nil 的 proc（零值即可）；其 Close 由桩接管，不会真正触碰 pty。
	ls.proc = &pty.Session{}

	entered := make(chan struct{})
	release := make(chan struct{})
	orig := mgr.closeProc
	mgr.closeProc = func(p *pty.Session) {
		if p != ls.proc {
			orig(p)
			return
		}
		close(entered)
		<-release
	}
	t.Cleanup(func() { mgr.closeProc = orig })

	done := make(chan struct{})
	go func() {
		mgr.Close("s1")
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("closeProc was not invoked")
	}

	// proc.Close 仍在阻塞，但索引必须已经移除。
	if _, ok := ix.Get("s1"); ok {
		t.Error("index must not contain s1 while proc.Close is still in progress")
	}
	// 本地会话已标记 closed（GetLocal 不再返回可 attach 的会话）。
	if l := mgr.GetLocal("s1"); l == nil || !l.isClosed() {
		t.Error("local session should be marked closed during proc.Close")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after proc.Close released")
	}
	if mgr.GetLocal("s1") != nil {
		t.Error("local map should not contain s1 after Close returns")
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

// TestManagerCreateAuditFailClosed 校验 recFactory 失败且 fail_closed=true 时拒绝
// 建会话（pty 被关闭、不登记）。
func TestManagerCreateAuditFailClosed(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)

	proc := mustPty(t)
	mgr.SetAudit(func(audit.SessionMeta) (*audit.Recorder, error) {
		return nil, errors.New("disk full")
	}, true, true, "mask", "echo_off_canonical", false)

	_, err := mgr.Create(CreateParams{ID: "s1", Username: "u", MaxLong: 3, MaxTotal: 10}, proc)
	if err == nil {
		t.Fatal("fail_closed: Create should fail when recorder open fails")
	}
	if !proc.IsClosed() {
		t.Error("proc should be closed on fail-closed rejection")
	}
	if mgr.GetLocal("s1") != nil {
		t.Error("rejected session should not be registered")
	}
}

// TestManagerCreateAuditFailOpen 校验 recFactory 失败且 fail_closed=false 时会话
// 照常建立（rec=nil，fail-open）。
func TestManagerCreateAuditFailOpen(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)

	proc := mustPty(t)
	mgr.SetAudit(func(audit.SessionMeta) (*audit.Recorder, error) {
		return nil, errors.New("disk full")
	}, false, true, "mask", "echo_off_canonical", false)

	ls, err := mgr.Create(CreateParams{ID: "s1", Username: "u", MaxLong: 3, MaxTotal: 10}, proc)
	if err != nil {
		t.Fatalf("fail-open: Create should succeed, got %v", err)
	}
	if ls.rec != nil {
		t.Error("rec should be nil on fail-open")
	}
	mgr.Close("s1")
}

// newAuditRecorder 在临时目录打开一个真实 recorder（供输入分派测试观察落盘）。
func newAuditRecorder(t *testing.T, opt audit.Options) (*audit.Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := audit.Open(dir, audit.SessionMeta{
		SessionID: "t", Username: "u", Mode: "bash", StartedAt: time.Now(),
	}, opt)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	return r, dir
}

// castInputEvents 读取 cast.log 中所有 i 事件的 data。
func castInputEvents(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "cast.log"))
	if err != nil {
		t.Fatalf("read cast.log: %v", err)
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "{") {
			continue
		}
		ev, err := audit.ParseEventLine([]byte(ln))
		if err != nil {
			continue
		}
		if ev.Type == "i" {
			out = append(out, ev.Data)
		}
	}
	return out
}

// newDispatchSession 构造一个仅用于输入分派测试的 localSession（proc 为 nil，
// 通过注入 procState 提供 ECHO/ICANON 状态）。
func newDispatchSession(rec *audit.Recorder, passwordMode, maskWhen string, echo, canon bool, err error) *localSession {
	ls := &localSession{
		id: "s1", username: "u", mode: "bash",
		rec: rec, recordInput: true, passwordMode: passwordMode, maskWhen: maskWhen,
	}
	ls.procState = func() (bool, bool, error) { return echo, canon, err }
	return ls
}

// TestRecordInputDispatch 校验 readLoop 输入埋点的三态分派（经 recordInputBlock）。
func TestRecordInputDispatch(t *testing.T) {
	opt := audit.Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond, FlushInterval: time.Hour, FlushThreshold: 1}

	t.Run("echo_on_plaintext", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", true, true, nil)
		ls.recordInputBlock([]byte("secret"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "secret" {
			t.Fatalf("echo=on should record plaintext, got %v", got)
		}
	})

	// echo=on 但整块含换行（readline 整块读取）：快照不可信赖，交 EVW 走边界窗口；
	// 回显到达后由边界事件确定关闭窗口 → 明文。
	t.Run("echo_on_batch_evw_echo_plaintext", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", true, true, nil)
		ls.recordInputBlock([]byte("secret\n")) // 含 \n → EVW deferred
		rec.WriteOutput([]byte("secret\r\n"))   // 累积回显证据（不关窗）
		rec.WriteResize(80, 24)                 // 边界事件 → 确定性关窗 → 明文
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "secret\n" {
			t.Fatalf("echo=on batch with echo should be plaintext via EVW, got %v", got)
		}
	})

	// echo=on 整块含换行 + strictBatch：一律掩码（严格整块防线）。
	t.Run("echo_on_strict_batch_masked", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", true, true, nil)
		ls.strictBatch = true
		ls.recordInputBlock([]byte("secret\n"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "*******" {
			t.Fatalf("strict batch echo=on with newline should be masked, got %v", got)
		}
	})

	t.Run("echo_off_canonical_masked", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", false, true, nil)
		ls.recordInputBlock([]byte("secret"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "******" {
			t.Fatalf("ECHO=off ICANON=on should be masked, got %v", got)
		}
	})

	t.Run("raw_echo_off_masked", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_off", false, false, nil)
		ls.recordInputBlock([]byte("secret"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "******" {
			t.Fatalf("raw + echo_off should be masked, got %v", got)
		}
	})

	t.Run("raw_echo_verify_deferred_echo_plaintext", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", false, false, nil)
		ls.recordInputBlock([]byte("secret"))
		rec.WriteOutput([]byte("secret")) // 累积回显证据（输出块不关窗）
		rec.WriteResize(80, 24)           // 边界事件 → 确定性关窗 → 明文
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "secret" {
			t.Fatalf("raw + echo_verify with echo should be plaintext, got %v", got)
		}
	})

	t.Run("raw_echo_verify_no_echo_masked", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", false, false, nil)
		ls.recordInputBlock([]byte("secret")) // 无回显，Close 时按掩码结算
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "******" {
			t.Fatalf("raw + echo_verify without echo should be masked, got %v", got)
		}
	})

	t.Run("raw_echo_off_canonical_plaintext", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_off_canonical", false, false, nil)
		ls.recordInputBlock([]byte("secret"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "secret" {
			t.Fatalf("raw + echo_off_canonical (legacy) should be plaintext, got %v", got)
		}
	})

	t.Run("plaintext_mode", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "plaintext", "echo_verify", false, false, nil)
		ls.recordInputBlock([]byte("secret"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 1 || got[0] != "secret" {
			t.Fatalf("plaintext mode should record plaintext, got %v", got)
		}
	})

	t.Run("echo_state_error_no_record", func(t *testing.T) {
		rec, dir := newAuditRecorder(t, opt)
		ls := newDispatchSession(rec, "mask", "echo_verify", false, false, errors.New("ioctl failed"))
		ls.recordInputBlock([]byte("secret"))
		rec.Close("x")
		if got := castInputEvents(t, dir); len(got) != 0 {
			t.Fatalf("echoState error should record nothing, got %v", got)
		}
	})
}

// TestActiveSessionIDs 校验存活会话 ID 快照。
func TestActiveSessionIDs(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	mgr, _ := newTestManager(clk)
	addLocalSession(mgr, "a", "u", "bash", false, base.Add(time.Hour), false)
	addLocalSession(mgr, "b", "u", "bash", false, base.Add(time.Hour), false)

	ids := mgr.ActiveSessionIDs()
	if len(ids) != 2 {
		t.Fatalf("ActiveSessionIDs len = %d, want 2", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("ActiveSessionIDs = %v, want a,b", ids)
	}
}
