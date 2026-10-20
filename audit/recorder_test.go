package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestRecorder 在临时目录打开一个 recorder。
func newTestRecorder(t *testing.T, meta SessionMeta, opt Options) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Open(dir, meta, opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r, dir
}

// readCastLines 读取 cast.log 的非空行。
func readCastLines(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "cast.log"))
	if err != nil {
		t.Fatalf("read cast.log: %v", err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines = append(lines, sc.Text())
		}
	}
	return lines
}

// readMetaFile 解析 meta.json。
func readMetaFile(t *testing.T, dir string) SessionMeta {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var m SessionMeta
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	return m
}

func TestRecorderBasic(t *testing.T) {
	started := time.Now()
	meta := SessionMeta{SessionID: "abc", Username: "alice", Mode: "bash", StartedAt: started}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteOutput([]byte("hello\n"))
	r.WriteInput([]byte("ls\n"))
	r.WriteResize(120, 30)
	r.Close("test done")

	lines := readCastLines(t, dir)
	if len(lines) < 4 {
		t.Fatalf("expected header+3 events, got %d: %v", len(lines), lines)
	}
	// header 首行且合法
	var hdr CastHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header not json: %v (%s)", err, lines[0])
	}
	if hdr.Version != 2 {
		t.Fatalf("version=%d", hdr.Version)
	}
	if strings.Contains(lines[0], "idle_time_limit") {
		t.Fatalf("header must not contain idle_time_limit: %s", lines[0])
	}
	if hdr.Width != 120 || hdr.Height != 30 {
		t.Fatalf("header size=%dx%d, want 120x30", hdr.Width, hdr.Height)
	}
	// header 只出现一次（后续行不得是 JSON 对象）
	for i, ln := range lines[1:] {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			t.Fatalf("unexpected header-like line %d: %s", i+1, ln)
		}
	}
	// 每行均为合法 JSON
	for i, ln := range lines {
		var v any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d invalid json: %v", i, err)
		}
	}
	// 事件类型齐全
	var sawO, sawI, sawR bool
	for _, ln := range lines[1:] {
		ev, err := ParseEventLine([]byte(ln))
		if err != nil {
			t.Fatalf("parse event: %v (%s)", err, ln)
		}
		switch ev.Type {
		case "o":
			sawO = true
		case "i":
			sawI = true
		case "r":
			sawR = true
		}
	}
	if !sawO || !sawI || !sawR {
		t.Fatalf("missing events o=%v i=%v r=%v", sawO, sawI, sawR)
	}

	m := readMetaFile(t, dir)
	if m.EndedAt == nil {
		t.Fatal("ended_at missing")
	}
	if m.EndReason != "test done" {
		t.Fatalf("end_reason=%q", m.EndReason)
	}
	if len(m.Timeline) == 0 || m.Timeline[0].Event != "open" {
		t.Fatalf("open event missing: %+v", m.Timeline)
	}
	// 无残留临时文件（原子写干净）
	if matches, _ := filepath.Glob(filepath.Join(dir, ".meta-*.tmp")); len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}

func TestRecorderHeaderDelayed(t *testing.T) {
	started := time.Unix(1700000000, 0)
	meta := SessionMeta{SessionID: "h1", Username: "alice", Mode: "bash", StartedAt: started}
	// ticker 不触发（interval 很大），仅手动调用 flushOnce。
	opt := Options{RecordInput: true, FlushInterval: time.Hour, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	// 原子时钟，避免与 flush goroutine 的数据竞争。
	var clock atomic.Int64
	clock.Store(started.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }

	r.WriteOutput([]byte("x"))
	r.flushOnce()

	r.mu.Lock()
	early := r.headerFlushed || r.headerLine != nil
	r.mu.Unlock()
	if early {
		t.Fatal("header decided too early (before delay)")
	}

	clock.Store(started.Add(2 * time.Second).UnixNano())
	r.flushOnce()

	r.mu.Lock()
	flushed := r.headerFlushed
	r.mu.Unlock()
	if !flushed {
		t.Fatal("header not flushed after delay")
	}

	r.Close("x")
	lines := readCastLines(t, dir)
	var hdr CastHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if hdr.Width != 80 || hdr.Height != 24 {
		t.Fatalf("default size=%dx%d, want 80x24", hdr.Width, hdr.Height)
	}
}

func TestRecorderResizeDefinesHeader(t *testing.T) {
	started := time.Now()
	meta := SessionMeta{SessionID: "h2", Username: "bob", Mode: "bash", StartedAt: started}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteResize(100, 40)
	r.Close("x")

	lines := readCastLines(t, dir)
	var hdr CastHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if hdr.Width != 100 || hdr.Height != 40 {
		t.Fatalf("size=%dx%d, want 100x40", hdr.Width, hdr.Height)
	}
}

func TestRecorderUTF8Reassembly(t *testing.T) {
	meta := SessionMeta{SessionID: "u1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	// "中" = E4 B8 AD，跨两次 Write 传入。
	r.WriteOutput([]byte{0xE4, 0xB8})
	r.WriteOutput([]byte{0xAD})
	r.Close("x")

	b, _ := os.ReadFile(filepath.Join(dir, "cast.log"))
	cast := string(b)
	if !strings.Contains(cast, "\u4e2d") {
		t.Fatalf("reassembled char missing: %q", cast)
	}
	if strings.Contains(cast, "\ufffd") {
		t.Fatalf("unexpected replacement char after reassembly: %q", cast)
	}
}

func TestRecorderInvalidUTF8(t *testing.T) {
	meta := SessionMeta{SessionID: "u2", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteOutput([]byte{0xFF, 'a'})
	r.Close("x")

	lines := readCastLines(t, dir)
	for i, ln := range lines {
		var v any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d invalid json: %v", i, err)
		}
	}
	// 无效字节应被替换为 U+FFFD。断言放在「解析后」的事件 data 上：
	// Go 1.26 的 encoding/json 可能把无效字节写成 JSON 文本转义 `\ufffd`
	// （磁盘上是 6 个 ASCII 字符），而其它版本写真实 ef bf bd 字节；
	// 二者 JSON 解码后都是同一个 U+FFFD rune，故解析后比对才跨版本稳定。
	sawReplacement := false
	for i, ln := range lines[1:] {
		ev, err := ParseEventLine([]byte(ln))
		if err != nil {
			t.Fatalf("line %d parse event: %v (%s)", i+1, err, ln)
		}
		if strings.ContainsRune(ev.Data, '\uFFFD') {
			sawReplacement = true
		}
	}
	if !sawReplacement {
		t.Fatalf("expected decoded replacement char for invalid byte, lines=%v", lines)
	}
}

func TestRecorderWriteInputMasked(t *testing.T) {
	meta := SessionMeta{SessionID: "m1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteInputMasked(5)
	r.WriteInputMasked(1)
	r.Close("x")

	lines := readCastLines(t, dir)
	sawMask := false
	for _, ln := range lines[1:] {
		ev, err := ParseEventLine([]byte(ln))
		if err != nil {
			continue
		}
		if ev.Type == "i" && ev.Data == "*****" {
			sawMask = true
		}
	}
	if !sawMask {
		t.Fatalf("masked i event missing: %v", lines)
	}
	m := readMetaFile(t, dir)
	// masked 按「脱敏事件块」计数：两次调用 → 2。
	if m.MaskedEvents != 2 {
		t.Fatalf("masked_input_events=%d, want 2", m.MaskedEvents)
	}
}

func TestRecorderWriteInputOmit(t *testing.T) {
	meta := SessionMeta{SessionID: "m2", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, PasswordMode: "omit", FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteInputMasked(5)
	r.Close("x")

	lines := readCastLines(t, dir)
	for _, ln := range lines[1:] {
		ev, err := ParseEventLine([]byte(ln))
		if err != nil {
			continue
		}
		if ev.Type == "i" {
			t.Fatalf("omit mode should not record i event: %s", ln)
		}
	}
	m := readMetaFile(t, dir)
	if m.MaskedEvents != 0 {
		t.Fatalf("masked_input_events=%d, want 0", m.MaskedEvents)
	}
}

func TestRecorderRecordInputDisabled(t *testing.T) {
	meta := SessionMeta{SessionID: "r1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: false, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteInput([]byte("secret"))
	r.WriteInputMasked(3)
	r.WriteOutput([]byte("out"))
	r.Close("x")

	lines := readCastLines(t, dir)
	for _, ln := range lines[1:] {
		ev, err := ParseEventLine([]byte(ln))
		if err != nil {
			continue
		}
		if ev.Type == "i" {
			t.Fatalf("record_input=false should not record i event: %s", ln)
		}
	}
}

func TestRecorderQueueDrop(t *testing.T) {
	meta := SessionMeta{SessionID: "d1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, PendingCap: 40, FlushInterval: time.Hour}
	r, dir := newTestRecorder(t, meta, opt)

	for i := 0; i < 100; i++ {
		r.WriteOutput([]byte("0123456789"))
	}
	r.Close("x")

	m := readMetaFile(t, dir)
	if m.Dropped == 0 {
		t.Fatal("expected dropped_events > 0")
	}
}

func TestRecorderCloseIdempotentAndAfterWrite(t *testing.T) {
	meta := SessionMeta{SessionID: "c1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.WriteOutput([]byte("a"))
	r.Close("first")
	r.Close("second") // 幂等，不 panic

	// Close 后写入安全丢弃。
	r.WriteOutput([]byte("after"))
	r.WriteInput([]byte("x"))
	r.WriteInputMasked(2)
	r.WriteResize(1, 1)
	r.Event("late", map[string]any{"k": "v"})

	m := readMetaFile(t, dir)
	if m.EndReason != "first" {
		t.Fatalf("end_reason=%q, want first", m.EndReason)
	}
}

func TestRecorderEventTimeline(t *testing.T) {
	meta := SessionMeta{SessionID: "e1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	r.Event("attach", map[string]any{"ip": "1.2.3.4"})
	r.Event("upload", map[string]any{"file": "a.csv", "dest": "/home/alice/a.csv"})
	r.Close("x")

	m := readMetaFile(t, dir)
	var attach *TimelineEvent
	for i := range m.Timeline {
		if m.Timeline[i].Event == "attach" {
			attach = &m.Timeline[i]
		}
	}
	if attach == nil {
		t.Fatalf("attach event missing: %+v", m.Timeline)
	}
	if attach.Extra["ip"] != "1.2.3.4" {
		t.Fatalf("attach extra ip=%v", attach.Extra["ip"])
	}
}

// castEvents 读取 cast.log 中的事件（跳过 header 行）。
func castEvents(t *testing.T, dir string) []CastEvent {
	t.Helper()
	var evs []CastEvent
	for _, ln := range readCastLines(t, dir) {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			continue // header
		}
		ev, err := ParseEventLine([]byte(ln))
		if err != nil {
			t.Fatalf("parse event: %v (%s)", err, ln)
		}
		evs = append(evs, ev)
	}
	return evs
}

// newEVWRecorder 打开一个 EVW recorder 并注入受控时钟（初始为 base）；返回推进器。
// FlushInterval 设为极长，避免 ticker 干扰；EVW 相关用例手动调用 flushOnce 结算。
func newEVWRecorder(t *testing.T, opt Options) (*Recorder, string, func(time.Duration)) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	meta := SessionMeta{SessionID: "evw", Username: "alice", Mode: "bash", StartedAt: base}
	opt.FlushInterval = time.Hour
	opt.FlushThreshold = 1
	r, dir := newTestRecorder(t, meta, opt)
	var clk atomic.Int64
	clk.Store(base.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clk.Load()) }
	return r, dir, func(d time.Duration) { clk.Add(int64(d)) }
}

// TestRecorderEVWPlaintextOnEcho 校验：deferred 输入在窗口内被 output 回显，随后由
// 「下一个输入/边界事件」确定性关闭窗口 → reached→明文。边界窗口语义下 i 事件由边界
// 事件结算，可能晚于其回显 o，故只需时间戳非降（t 单调）。
func TestRecorderEVWPlaintextOnEcho(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("secret"))
	r.WriteOutput([]byte("secret")) // 累积回显证据（输出块不关窗）
	r.WriteResize(80, 24)           // 边界事件 → 关窗 → reached→明文
	r.Close("x")

	evs := castEvents(t, dir)
	iIdx, oIdx := -1, -1
	for i, ev := range evs {
		if ev.Type == "i" && ev.Data == "secret" {
			iIdx = i
		}
		if ev.Type == "o" && ev.Data == "secret" && oIdx == -1 {
			oIdx = i
		}
	}
	if iIdx < 0 {
		t.Fatalf("plaintext i event missing: %+v", evs)
	}
	if oIdx < 0 {
		t.Fatalf("o event missing: %+v", evs)
	}
	// 断言反转：边界结算使 i 可能晚于 o，但时间戳必须非降。
	if evs[iIdx].T < evs[oIdx].T {
		t.Fatalf("i.t=%v must be >= o.t=%v (boundary settle)", evs[iIdx].T, evs[oIdx].T)
	}
	m := readMetaFile(t, dir)
	if m.MaskedEvents != 0 {
		t.Fatalf("MaskedEvents=%d, want 0", m.MaskedEvents)
	}
}

// TestRecorderEVWPrintbackMasked 核心回归：逐键 `read -s` 输入 "2" 后 Enter（\n 到达
// 即关闭 "2" 窗口，此刻尚未回显 → 掩码）；其后 printback（echo $pw 输出含密码字节）
// 不得翻案。\n 为纯控制块，按明文记。
func TestRecorderEVWPrintbackMasked(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("2"))    // 密码字符
	r.WriteInputDeferred([]byte("\n"))   // Enter：边界 → 关 "2" 窗口（未回显→掩码）
	r.WriteOutput([]byte("GOTDONE:2\n")) // printback 含密码字节 2——不得翻案
	r.Close("x")

	evs := castEvents(t, dir)
	sawMask, sawNL := false, false
	for _, ev := range evs {
		if ev.Type != "i" {
			continue
		}
		switch ev.Data {
		case "2":
			t.Fatalf("printback must not turn masked input into plaintext: %+v", evs)
		case "*":
			sawMask = true
		case "\n":
			sawNL = true
		}
	}
	if !sawMask {
		t.Fatalf("expected masked '2' i event: %+v", evs)
	}
	if !sawNL {
		t.Fatalf("expected plaintext newline i event: %+v", evs)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].T < evs[i-1].T {
			t.Fatalf("cast timestamps must be non-decreasing: %+v", evs)
		}
	}
}

// TestRecorderEVWBatchReadlinePlaintextOnEcho 校验：整块 readline 输入含 \n，回显后由
// 下一个输入块关闭窗口 → reached→明文。
func TestRecorderEVWBatchReadlinePlaintextOnEcho(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("ls -la\n"))
	r.WriteOutput([]byte("ls -la\r\n")) // 回显
	r.WriteInputDeferred([]byte("x"))   // 下一个输入块 = 边界 → 关窗
	r.Close("x")

	saw := false
	for _, ev := range castEvents(t, dir) {
		if ev.Type == "i" && ev.Data == "ls -la\n" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("batch readline with echo should be plaintext: %+v", castEvents(t, dir))
	}
}

// TestRecorderEVWBatchNoEchoMasked 校验：整块 read -s 无回显，边界到达 → 掩码。
func TestRecorderEVWBatchNoEchoMasked(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("s3cret\n")) // 整块，无回显
	r.WriteInputDeferred([]byte("next"))    // 边界 → 关窗（未回显→掩码）
	r.Close("x")

	evs := castEvents(t, dir)
	for _, ev := range evs {
		if ev.Type == "i" && ev.Data == "s3cret\n" {
			t.Fatalf("no-echo batch must be masked: %+v", evs)
		}
	}
	sawMask := false
	for _, ev := range evs {
		if ev.Type == "i" && ev.Data == "*******" {
			sawMask = true
		}
	}
	if !sawMask {
		t.Fatalf("expected masked batch: %+v", evs)
	}
}

// TestRecorderEVWHangPolicyDefaultMask 校验：hang 兜底默认一律掩码，即便已 reached。
func TestRecorderEVWHangPolicyDefaultMask(t *testing.T) {
	r, dir, advance := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("pw"))
	r.WriteOutput([]byte("pw")) // reached，但 hang 默认保守掩码
	advance(200 * time.Millisecond)
	r.flushOnce()
	r.Close("x")

	for _, ev := range castEvents(t, dir) {
		if ev.Type == "i" && ev.Data == "pw" {
			t.Fatalf("default hang policy must mask even if reached: %+v", ev)
		}
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 1 {
		t.Fatalf("MaskedEvents=%d, want 1", m.MaskedEvents)
	}
}

// TestRecorderEVWHangPolicyReachedPlain 校验：hang 兜底策略 reached_plain 时，已 reached
// 的 pending 记明文。
func TestRecorderEVWHangPolicyReachedPlain(t *testing.T) {
	r, dir, advance := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond, EchoVerifyHangPolicy: "reached_plain"})

	r.WriteInputDeferred([]byte("pw"))
	r.WriteOutput([]byte("pw")) // reached
	advance(200 * time.Millisecond)
	r.flushOnce() // hang 兜底：reached_plain → 明文
	r.Close("x")

	saw := false
	for _, ev := range castEvents(t, dir) {
		if ev.Type == "i" && ev.Data == "pw" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("reached_plain hang policy should emit plaintext: %+v", castEvents(t, dir))
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 0 {
		t.Fatalf("MaskedEvents=%d, want 0", m.MaskedEvents)
	}
}

// TestRecorderEVWCrossBlockAccumulation 校验：回显证据跨多个 output 块累积至 reached。
func TestRecorderEVWCrossBlockAccumulation(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("pwd"))
	r.WriteOutput([]byte("p"))  // 部分回显
	r.WriteOutput([]byte("wd")) // 补齐 → reached
	r.WriteResize(80, 24)       // 边界 → 明文
	r.Close("x")

	saw := false
	for _, ev := range castEvents(t, dir) {
		if ev.Type == "i" && ev.Data == "pwd" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("cross-block accumulation should reach and record plaintext: %+v", castEvents(t, dir))
	}
}

// TestRecorderEVWCastMonotonic 乱序回归：逐键 + Enter，穿插回显与 printback，cast 内
// 行序与时间戳 t 均非降。
func TestRecorderEVWCastMonotonic(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("a"))
	r.WriteOutput([]byte("a"))
	r.WriteInputDeferred([]byte("b"))
	r.WriteOutput([]byte("b"))
	r.WriteInputDeferred([]byte("\n"))
	r.WriteOutput([]byte("ab\r\n"))
	r.Close("x")

	evs := castEvents(t, dir)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].T < evs[i-1].T {
			t.Fatalf("cast timestamps must be non-decreasing at %d: %v < %v (%+v)", i, evs[i].T, evs[i-1].T, evs)
		}
	}
}

// TestRecorderEVWMaskOnTimeout 校验：窗口内未回显 → 超时结算为掩码并计数。
func TestRecorderEVWMaskOnTimeout(t *testing.T) {
	r, dir, advance := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("pw"))
	advance(200 * time.Millisecond)
	r.flushOnce() // 超时结算
	r.Close("x")

	evs := castEvents(t, dir)
	sawMask, sawPlain := false, false
	for _, ev := range evs {
		if ev.Type != "i" {
			continue
		}
		if ev.Data == "**" {
			sawMask = true
		}
		if ev.Data == "pw" {
			sawPlain = true
		}
	}
	if !sawMask {
		t.Fatalf("masked i event missing: %+v", evs)
	}
	if sawPlain {
		t.Fatalf("plaintext must not be recorded on timeout: %+v", evs)
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 1 {
		t.Fatalf("MaskedEvents=%d, want 1", m.MaskedEvents)
	}
}

// TestRecorderEVWPrintbackAfterTimeout 校验：超时已掩码后，即便后续 output 出现相同
// 字节（脚本 echo $pw 的 printback），也不回看、不改判为明文。
func TestRecorderEVWPrintbackAfterTimeout(t *testing.T) {
	r, dir, advance := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("pw"))
	advance(200 * time.Millisecond)
	r.flushOnce()             // 掩码结算
	r.WriteOutput([]byte("pw")) // 迟到的 printback
	r.Close("x")

	evs := castEvents(t, dir)
	for _, ev := range evs {
		if ev.Type == "i" && ev.Data == "pw" {
			t.Fatalf("late printback must not turn masked input into plaintext: %+v", evs)
		}
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 1 {
		t.Fatalf("MaskedEvents=%d, want 1", m.MaskedEvents)
	}
}

// TestRecorderEVWControlBlockPlaintext 校验：纯控制块（无可打印字节）直接明文录。
func TestRecorderEVWControlBlockPlaintext(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte{0x7f}) // DEL，无可打印字节
	r.Close("x")

	evs := castEvents(t, dir)
	saw := false
	for _, ev := range evs {
		if ev.Type == "i" && ev.Data == string([]byte{0x7f}) {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("control block should be recorded plaintext: %+v", evs)
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 0 {
		t.Fatalf("MaskedEvents=%d, want 0", m.MaskedEvents)
	}
}

// TestRecorderEVWPlaintextMode 校验：plaintext 模式下 EVW 不变更语义，直接明文。
func TestRecorderEVWPlaintextMode(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, PasswordMode: "plaintext", EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("pw"))
	r.Close("x")

	evs := castEvents(t, dir)
	saw := false
	for _, ev := range evs {
		if ev.Type == "i" && ev.Data == "pw" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("plaintext mode should record plaintext: %+v", evs)
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 0 {
		t.Fatalf("MaskedEvents=%d, want 0", m.MaskedEvents)
	}
}

// TestRecorderEVWOmitMode 校验：omit 模式下 EVW 超时结算不写事件、不计数（与
// WriteInputMasked 的 omit 语义一致）。
func TestRecorderEVWOmitMode(t *testing.T) {
	r, dir, advance := newEVWRecorder(t, Options{RecordInput: true, PasswordMode: "omit", EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("pw"))
	advance(200 * time.Millisecond)
	r.flushOnce()
	r.Close("x")

	for _, ev := range castEvents(t, dir) {
		if ev.Type == "i" {
			t.Fatalf("omit mode must not write i event: %+v", ev)
		}
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 0 {
		t.Fatalf("MaskedEvents=%d, want 0", m.MaskedEvents)
	}
}

// TestRecorderEVWPendingCapDrop 校验：pending 超上限整体丢弃并计数，不 panic。
func TestRecorderEVWPendingCapDrop(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, PendingInputCap: 4, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteInputDeferred([]byte("abcde")) // len 5 > cap 4 → 丢弃
	r.Close("x")

	if m := readMetaFile(t, dir); m.Dropped == 0 {
		t.Fatal("expected dropped_events > 0")
	}
}

// TestRecorderEVWCloseMasksUnsettled 校验：Close 收尾时未结算 pending 一律按掩码落盘，
// 杜绝明文残留。
func TestRecorderEVWCloseMasksUnsettled(t *testing.T) {
	r, dir, _ := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTimeout: time.Hour})

	r.WriteInputDeferred([]byte("pw")) // 不推进时钟、不回显，直接 Close
	r.Close("x")

	for _, ev := range castEvents(t, dir) {
		if ev.Type == "i" && ev.Data == "pw" {
			t.Fatalf("Close must mask unsettled pending, got plaintext: %+v", ev)
		}
	}
	if m := readMetaFile(t, dir); m.MaskedEvents != 1 {
		t.Fatalf("MaskedEvents=%d, want 1", m.MaskedEvents)
	}
}

// TestRecorderEVWTUIGate 校验：alt-screen 内 deferred 输入直接明文；退出后恢复 EVW。
func TestRecorderEVWTUIGate(t *testing.T) {
	r, dir, advance := newEVWRecorder(t, Options{RecordInput: true, EchoVerifyTUI: true, EchoVerifyTimeout: 100 * time.Millisecond})

	r.WriteOutput([]byte("\x1b[?1049h")) // 进入 alt-screen
	r.WriteInputDeferred([]byte("pw"))   // TUI 内 → 明文
	r.WriteOutput([]byte("\x1b[?1049l")) // 退出 alt-screen
	r.WriteInputDeferred([]byte("noecho"))
	advance(200 * time.Millisecond)
	r.flushOnce() // 超时 → 掩码
	r.Close("x")

	evs := castEvents(t, dir)
	sawPlain, sawMask := false, false
	for _, ev := range evs {
		if ev.Type != "i" {
			continue
		}
		if ev.Data == "pw" {
			sawPlain = true
		}
		if ev.Data == "******" {
			sawMask = true
		}
		if ev.Data == "noecho" {
			t.Fatalf("after exiting alt-screen EVW must apply: %+v", evs)
		}
	}
	if !sawPlain {
		t.Fatalf("input inside alt-screen should be plaintext: %+v", evs)
	}
	if !sawMask {
		t.Fatalf("input after alt-screen exit should be masked on timeout: %+v", evs)
	}
}

// TestExtractPrintable 校验可打印字节子集提取规则。
func TestExtractPrintable(t *testing.T) {
	in := []byte{0x41, 0x0a, 0xe4, 0xb8, 0xad, 0x7f, 0x80}
	want := []byte{0x41, 0xe4, 0xb8, 0xad}
	got := extractPrintable(in)
	if !bytes.Equal(got, want) {
		t.Fatalf("extractPrintable=% x, want % x", got, want)
	}
	if len(extractPrintable([]byte{0x00, 0x0d, 0x7f})) != 0 {
		t.Fatal("pure control block should yield empty printable")
	}
}

func TestRecorderMaxSessionBytes(t *testing.T) {
	meta := SessionMeta{SessionID: "x1", Username: "alice", Mode: "bash", StartedAt: time.Now()}
	opt := Options{RecordInput: true, MaxSessionBytes: 80, FlushInterval: 5 * time.Millisecond, FlushThreshold: 1}
	r, dir := newTestRecorder(t, meta, opt)

	for i := 0; i < 50; i++ {
		r.WriteOutput([]byte("abcdefghij"))
	}
	r.Close("x")

	m := readMetaFile(t, dir)
	if !m.Stopped {
		t.Fatal("expected stopped_recording=true")
	}
	fi, err := os.Stat(filepath.Join(dir, "cast.log"))
	if err != nil {
		t.Fatalf("stat cast: %v", err)
	}
	if fi.Size() > 2000 {
		t.Fatalf("cast grew unbounded: %d bytes", fi.Size())
	}
}

// TestRecorderConcurrentFlushNoRace 回归：多写协程与多 flushOnce 协程并发，
// 验证写盘互斥（wmu）下 cast.log 结构完好——header 首行唯一、事件无重无丢、
// 逐行可解析、时间戳非降、事件内容与写入一致。配合 `go test -race` 检出并发写盘竞争。
func TestRecorderConcurrentFlushNoRace(t *testing.T) {
	started := time.Now()
	meta := SessionMeta{SessionID: "race", Username: "alice", Mode: "bash", StartedAt: started}
	// ticker 设极长以排除其干扰；FlushThreshold=1 令每次 flushOnce 都落盘。
	opt := Options{RecordInput: true, FlushThreshold: 1, FlushInterval: time.Hour}
	r, dir := newTestRecorder(t, meta, opt)

	// 受控时钟：固定为 started+2s，确保 flushOnce 立即越过 headerDelayDefault 定
	// header 并真正写出，从而与并发写盘路径充分交错。（在启动协程前注入，协程只读。）
	r.now = func() time.Time { return started.Add(2 * time.Second) }

	const (
		writers    = 6
		flushers   = 6
		perWriter  = 50
		flushIters = 300
	)

	// 每个写协程独占自己的期望内容，避免共享切片写竞争。
	expected := make([][]string, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				s := fmt.Sprintf("w%d-%d", id, j)
				if j%2 == 0 {
					r.WriteOutput([]byte(s))
				} else {
					r.WriteInput([]byte(s))
				}
				expected[id] = append(expected[id], s)
			}
		}(i)
	}

	// 并发反复 flushOnce，制造与写盘路径的交错。
	var fwg sync.WaitGroup
	for i := 0; i < flushers; i++ {
		fwg.Add(1)
		go func() {
			defer fwg.Done()
			for k := 0; k < flushIters; k++ {
				r.flushOnce()
			}
		}()
	}

	wg.Wait()
	fwg.Wait()
	r.Close("done")

	lines := readCastLines(t, dir)
	if len(lines) == 0 {
		t.Fatal("no cast lines")
	}

	// 1) header 恰一行且为首行：首行是 JSON 对象，其余行不以 '{' 开头。
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "{") {
		t.Fatalf("first line must be header json: %s", lines[0])
	}
	headerCount := 0
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			headerCount++
		}
	}
	if headerCount != 1 {
		t.Fatalf("want exactly 1 header line, got %d", headerCount)
	}

	// 3) 逐行 JSON 可解析（含 header）。
	for i, ln := range lines {
		var v any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d invalid json: %v (%s)", i, err, ln)
		}
	}

	evs := castEvents(t, dir)

	// 2) 事件行数 == 写入事件总数（无重复/无丢失）。
	total := 0
	for _, exp := range expected {
		total += len(exp)
	}
	if len(evs) != total {
		t.Fatalf("event count=%d, want %d", len(evs), total)
	}

	// 期望内容 → 事件类型（内容唯一，可用作 map 键）。
	want := make(map[string]string, total)
	for _, exp := range expected {
		for j, s := range exp {
			if j%2 == 0 {
				want[s] = "o"
			} else {
				want[s] = "i"
			}
		}
	}

	// 4) t 单调非降；5) 事件内容与写入内容、类型一致。
	got := make(map[string]string, len(evs))
	var prevT float64
	for i, ev := range evs {
		if i > 0 && ev.T < prevT {
			t.Fatalf("cast timestamps must be non-decreasing at %d: %v < %v", i, ev.T, prevT)
		}
		prevT = ev.T
		if w, ok := want[ev.Data]; !ok || w != ev.Type {
			t.Fatalf("unexpected event at %d: type=%s data=%q", i, ev.Type, ev.Data)
		}
		got[ev.Data] = ev.Type
	}
	if len(got) != len(want) {
		t.Fatalf("unique events=%d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("event %q: got type %q, want %q", k, got[k], v)
		}
	}
}
