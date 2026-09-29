package handler

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// testClock 是一个可手动步进的时钟。
type testClock struct {
	t time.Time
}

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *testClock) set(t time.Time)         { c.t = t }

func TestIndexAddGetListOrder(t *testing.T) {
	ix := NewIndex(5*time.Minute, -1, nil)
	base := time.Unix(1_700_000_000, 0)
	_ = ix.Add(SessionInfo{ID: "a", Username: "u", Mode: "bash", CreatedAt: base}, 10, 10)
	_ = ix.Add(SessionInfo{ID: "b", Username: "u", Mode: "codex", CreatedAt: base.Add(time.Minute)}, 10, 10)
	_ = ix.Add(SessionInfo{ID: "c", Username: "other", CreatedAt: base.Add(2 * time.Minute)}, 10, 10)

	if _, ok := ix.Get("a"); !ok {
		t.Fatal("expected a in index")
	}
	list := ix.List("u")
	if len(list) != 2 {
		t.Fatalf("List(u) len = %d, want 2", len(list))
	}
	// createdAt 倒序：b 在前。
	if list[0].ID != "b" || list[1].ID != "a" {
		t.Fatalf("List order = %q,%q, want b,a", list[0].ID, list[1].ID)
	}
	if ix.TotalCount("u") != 2 {
		t.Errorf("TotalCount(u) = %d, want 2", ix.TotalCount("u"))
	}

	ix.Remove("a")
	if _, ok := ix.Get("a"); ok {
		t.Error("expected a removed")
	}
	ix.Remove("a") // 幂等
}

func TestIndexLimits(t *testing.T) {
	ix := NewIndex(5*time.Minute, -1, nil)
	add := func(id string, long bool) error {
		return ix.Add(SessionInfo{ID: id, Username: "u", LongLived: long}, 2, 3)
	}
	if err := add("l1", true); err != nil {
		t.Fatalf("l1: %v", err)
	}
	if err := add("l2", true); err != nil {
		t.Fatalf("l2: %v", err)
	}
	if err := add("l3", true); err != ErrLongLimit {
		t.Fatalf("l3 = %v, want ErrLongLimit", err)
	}
	if err := add("s1", false); err != nil {
		t.Fatalf("s1: %v", err)
	}
	if err := add("s2", false); err != ErrTotalLimit {
		t.Fatalf("s2 = %v, want ErrTotalLimit", err)
	}
	if ix.LongCount("u") != 2 {
		t.Errorf("LongCount = %d, want 2", ix.LongCount("u"))
	}
	if ix.TotalCount("u") != 3 {
		t.Errorf("TotalCount = %d, want 3", ix.TotalCount("u"))
	}

	// maxLong/maxTotal <= 0 表示不限制。
	ix2 := NewIndex(5*time.Minute, -1, nil)
	for i := 0; i < 20; i++ {
		if err := ix2.Add(SessionInfo{ID: newSessionID(), Username: "u", LongLived: true}, 0, 0); err != nil {
			t.Fatalf("unlimited add: %v", err)
		}
	}
}

func TestIndexExtend(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	ix := NewIndex(5*time.Minute, -1, clk.now)

	_ = ix.Add(SessionInfo{ID: "long", Username: "u", LongLived: true, ExpireAt: base.Add(10 * time.Hour)}, 10, 10)
	_ = ix.Add(SessionInfo{ID: "short", Username: "u", LongLived: false, ExpireAt: base.Add(time.Minute)}, 10, 10)
	_ = ix.Add(SessionInfo{ID: "other", Username: "other", LongLived: true, ExpireAt: base.Add(10 * time.Hour)}, 10, 10)

	info, err := ix.Extend("long", "u", 5)
	if err != nil {
		t.Fatalf("Extend long: %v", err)
	}
	if !info.ExpireAt.Equal(base.Add(15 * time.Hour)) {
		t.Errorf("new expire = %v, want %v", info.ExpireAt, base.Add(15*time.Hour))
	}
	// 累计延期。
	info, err = ix.Extend("long", "u", 5)
	if err != nil || !info.ExpireAt.Equal(base.Add(20*time.Hour)) {
		t.Errorf("second extend = %v / %v", info.ExpireAt, err)
	}

	if _, err := ix.Extend("short", "u", 1); err != ErrNotLongLived {
		t.Errorf("extend short = %v, want ErrNotLongLived", err)
	}
	if _, err := ix.Extend("other", "u", 1); err != ErrSessionNotFound {
		t.Errorf("extend foreign = %v, want ErrSessionNotFound", err)
	}
	if _, err := ix.Extend("nope", "u", 1); err != ErrSessionNotFound {
		t.Errorf("extend missing = %v, want ErrSessionNotFound", err)
	}

	// 过期长期不可延期。
	_ = ix.Add(SessionInfo{ID: "expired", Username: "u", LongLived: true, ExpireAt: base.Add(-time.Hour)}, 10, 10)
	if _, err := ix.Extend("expired", "u", 1); err != ErrSessionExpired {
		t.Errorf("extend expired = %v, want ErrSessionExpired", err)
	}
}

func TestReapOnceClockInjection(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &testClock{t: base}
	ix := NewIndex(5*time.Minute, -1, clk.now)

	// 记录被回收的 id，并模拟 manager 从索引移除。
	var mu sync.Mutex
	var reaped []string
	ix.setCloseFunc(func(id string) {
		mu.Lock()
		reaped = append(reaped, id)
		mu.Unlock()
		ix.Remove(id)
	})

	// 长期：无论是否 attached，到点即回收。
	_ = ix.Add(SessionInfo{ID: "long", Username: "u", LongLived: true, ExpireAt: base.Add(1 * time.Minute)}, 10, 10)
	// 短期 attached：不回收。
	_ = ix.Add(SessionInfo{ID: "short-attached", Username: "u", LongLived: false, ExpireAt: base.Add(1 * time.Minute), Attached: true}, 10, 10)
	// 短期 idle：到点回收。
	_ = ix.Add(SessionInfo{ID: "short-idle", Username: "u", LongLived: false, ExpireAt: base.Add(1 * time.Minute)}, 10, 10)

	// 时钟未步进：无回收。
	if n := ix.ReapOnce(); n != 0 {
		t.Fatalf("ReapOnce at base = %d, want 0", n)
	}

	clk.advance(2 * time.Minute)
	if n := ix.ReapOnce(); n != 2 {
		t.Fatalf("ReapOnce after step = %d, want 2", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reaped) != 2 {
		t.Fatalf("reaped = %v, want 2", reaped)
	}
	if _, ok := ix.Get("short-attached"); !ok {
		t.Error("attached short should not be reaped")
	}
	if _, ok := ix.Get("long"); ok {
		t.Error("long should be reaped")
	}
	if _, ok := ix.Get("short-idle"); ok {
		t.Error("idle short should be reaped")
	}
}

func TestNewIndexDefaults(t *testing.T) {
	// shortTTL<=0 → 5min；reaperInterval=0 → 30s 并启动。
	ix := NewIndex(0, 0, nil)
	if ix.shortTTL != defaultShortTTL {
		t.Errorf("shortTTL = %v, want %v", ix.shortTTL, defaultShortTTL)
	}
	if ix.reaperInterval != defaultReaperInterval {
		t.Errorf("reaperInterval = %v, want %v", ix.reaperInterval, defaultReaperInterval)
	}
}

func TestNewIndexNoReaperNoGoroutineLeak(t *testing.T) {
	// reaperInterval<0：不启动 reaper，不泄漏 goroutine。
	runtime.GC()
	before := runtime.NumGoroutine()
	ix := NewIndex(0, -1, nil)
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after != before {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
	// 基本可用。
	if err := ix.Add(SessionInfo{ID: "x", Username: "u"}, 10, 10); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestIndexLimitRelease(t *testing.T) {
	ix := NewIndex(5*time.Minute, -1, nil)
	_ = ix.Add(SessionInfo{ID: "l1", Username: "u", LongLived: true}, 1, 10)
	if err := ix.Add(SessionInfo{ID: "l2", Username: "u", LongLived: true}, 1, 10); err != ErrLongLimit {
		t.Fatalf("second long = %v, want ErrLongLimit", err)
	}
	// 关闭后名额立即释放。
	ix.Remove("l1")
	if ix.LongCount("u") != 0 {
		t.Fatalf("LongCount after remove = %d, want 0", ix.LongCount("u"))
	}
	if err := ix.Add(SessionInfo{ID: "l3", Username: "u", LongLived: true}, 1, 10); err != nil {
		t.Fatalf("create after release = %v, want nil", err)
	}
}

func TestConcurrentAddLimit(t *testing.T) {
	ix := NewIndex(5*time.Minute, -1, nil)
	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ix.Add(SessionInfo{ID: newSessionID(), Username: "u", LongLived: true}, 3, 10); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 3 {
		t.Fatalf("concurrent long create succeeded %d, want exactly 3", ok)
	}
	if ix.LongCount("u") != 3 {
		t.Errorf("LongCount = %d, want 3", ix.LongCount("u"))
	}
}
