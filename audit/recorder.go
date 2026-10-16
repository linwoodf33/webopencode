package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// recorder 常量。
const (
	// defaultCols / defaultRows header 未收到 resize 前的初始尺寸。
	defaultCols, defaultRows = 80, 24
	// headerDelayDefault 首事件外等待真实尺寸的时长；到时仍无 resize 则以当前/80x24 定 header。
	headerDelayDefault = 1 * time.Second
	// headerQueueCap header 未定前事件内存上限（4KB），超了立即定 header。
	headerQueueCap = 4 << 10
	// flushIntervalDefault 异步落盘周期。
	flushIntervalDefault = 500 * time.Millisecond
	// flushThresholdDefault 缓冲超过即落盘的阈值（32KB）。
	flushThresholdDefault = 32 << 10
	// pendingCapDefault 下单事件队列上限（8MB），满则丢弃新事件。
	pendingCapDefault = 8 << 20
	// echoVerifyHangDefault EVW hang 兜底默认时长：pending 的「回显观察窗口」未被
	// 下一个输入块/边界事件确定性关闭时，超过此时长一律保守按掩码结算。
	echoVerifyHangDefault = 2 * time.Second
	// pendingInputCapDefault EVW pending 输入缓冲默认上限（64KB）。
	pendingInputCapDefault = 64 << 10
	// castFileMode cast 文件权限。
	castFileMode = 0o600
	// dirMode 会话目录权限。
	dirMode = 0o700
)

// ClientInfo 记录录制会话的来源信息（§5）。
type ClientInfo struct {
	IP           string `json:"ip,omitempty"`
	ForwardedFor string `json:"forwarded_for,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
}

// TimelineEvent 是 meta.json 时间线中的一条审计事件。
//
// 自定义 MarshalJSON：输出 {"t":..,"event":..} 后把 Extra 的键拍平进同一对象
// （如 {"t":..,"event":"attach","ip":"10.1.2.3"}）；反序列化时未知键回填到 Extra。
type TimelineEvent struct {
	T     int64          `json:"t"`
	Event string         `json:"event"` // open|attach|detach|close|upload|extend|close-req|auth-provided|server restart (abnormal)
	Extra map[string]any `json:"-"`
}

// MarshalJSON 将 T/Event 与 Extra 合并为同一 JSON 对象输出。
func (e TimelineEvent) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(e.Extra)+2)
	m["t"] = e.T
	m["event"] = e.Event
	for k, v := range e.Extra {
		if k == "t" || k == "event" {
			continue // 保护保留键，避免 Extra 覆盖时间/类型
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// UnmarshalJSON 解析 {"t","event",...extra}，未知键回填到 Extra。
func (e *TimelineEvent) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	e.Extra = nil
	for k, v := range raw {
		switch k {
		case "t":
			if err := json.Unmarshal(v, &e.T); err != nil {
				return err
			}
		case "event":
			if err := json.Unmarshal(v, &e.Event); err != nil {
				return err
			}
		default:
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				return err
			}
			if e.Extra == nil {
				e.Extra = make(map[string]any)
			}
			e.Extra[k] = val
		}
	}
	return nil
}

// SessionMeta 是单会话的审计元数据（落盘 meta.json）。
//
// 时间字段保留 time.Time（JSON 自动 RFC3339），SessionItem 中再转为 Unix 秒。
type SessionMeta struct {
	SessionID    string          `json:"session_id"`
	Username     string          `json:"username"`
	Mode         string          `json:"mode"`
	LongLived    bool            `json:"long_lived"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpireAt     time.Time       `json:"expire_at,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	Client       ClientInfo      `json:"client,omitempty"`
	AuthProvided []string        `json:"auth_provided,omitempty"` // 仅变量名清单，绝不含值
	Timeline     []TimelineEvent `json:"timeline,omitempty"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
	EndReason    string          `json:"end_reason,omitempty"`
	MaskedEvents int64           `json:"masked_input_events,omitempty"`
	Dropped      int64           `json:"dropped_events,omitempty"`
	Stopped      bool            `json:"stopped_recording,omitempty"`
}

// Options 控制 recorder/store 的行为。
type Options struct {
	RecordInput     bool          // record_input；false 时 i 事件整体不记
	PasswordMode    string        // "mask"(默认)|"omit"|"plaintext"
	MaxSessionBytes int64         // 单会话 cast 字节上限；<=0 不限制
	MaxTotalBytes   int64         // 总量配额（store 使用）；<=0 不限
	RetentionDays   int           // 保留天数；0=不自动清理（store 使用）
	FlushInterval   time.Duration // <=0 用 flushIntervalDefault
	FlushThreshold  int           // <=0 用 flushThresholdDefault
	PendingCap      int           // <=0 用 pendingCapDefault

	// EchoVerifyTimeout EVW hang 兜底时长：pending 的回显观察窗口 = [到达, 下一个
	// 输入块/边界事件)。窗口被确定性关闭时按 reached 裁决；若迟迟未被关闭，则超过
	// 此时长按 hang 策略兜底结算。<=0 用 echoVerifyHangDefault（2s）。
	EchoVerifyTimeout time.Duration
	// EchoVerifyHangPolicy hang 兜底裁决策略：""/"mask"（默认）一律掩码；
	// "reached_plain" 对已 reached 的 pending 记明文（未 reached 仍掩码）。
	EchoVerifyHangPolicy string
	// EchoVerifyStrictBatch 严格整块（由 handler 层消费）：echo=on 且整块含换行时
	// 一律掩码，不交给 EVW 验证（快照不可信赖）。recorder 不消费本字段。
	EchoVerifyStrictBatch bool
	// EchoVerifyTUI 是否启用 alt-screen TUI 门：进入 alt-screen 后输入直接按明文录
	//（TUI 自己渲染，不依赖内核回显）。
	EchoVerifyTUI bool
	// PendingInputCap EVW pending 输入缓冲上限（原始字节）；<=0 用 pendingInputCapDefault。
	PendingInputCap int
}

// pendingInput 是一条 EVW 待定输入：先入 pending，其「回显观察窗口」为
// [到达, 下一个输入块/边界事件)。窗口内跨多个 output 块累积观测字节；窗口被
// 确定性关闭时 reached→明文、否则掩码；仅 hang 超时/Close 兜底时保守掩码。
type pendingInput struct {
	b         []byte    // 原始输入字节（结算为明文时原样写 i）
	printable []byte    // 仅可打印字节子集（0x20-0x7E + 合法 UTF-8），用于回显匹配
	t         float64   // 相对 started 的到达时刻
	reached   bool      // 窗口内是否已观测到全部可打印字节（跨 output 块累积）
	seen      [256]bool // 窗口内跨 output 块累计观测到的字节（回显证据）
}

// Recorder 是单会话录制器：写 asciicast v2 cast.log 与审计 meta.json。
//
// 并发契约：Write* 可被 pump/readLoop 并发调用；Close 幂等，Close 后的写入安全
// 丢弃（不 panic、不损坏文件）。所有方法仅内存操作并异步落盘，绝不阻塞调用方。
type Recorder struct {
	dir, castPath, metaPath string
	opt                     Options
	meta                    SessionMeta
	started                 time.Time
	now                     func() time.Time

	mu            sync.Mutex
	closed        bool
	headerFlushed bool
	col, row      uint16
	haveSize      bool
	pending       bytes.Buffer
	bytesSince    int64 // 已生成 cast 字节（含 header）
	stopped       bool
	dropped       int64
	masked        int64

	// headerLine 为已定但尚未写入文件的 header 行；写出后置 nil 且 headerFlushed=true。
	headerLine []byte

	w    *bufio.Writer
	f    *os.File
	wake chan struct{}
	done chan struct{}
	wg   sync.WaitGroup

	utfTail []byte

	// lastCastT 已写出事件的最大时间戳（相对 started 秒），保证 cast 内行序与 t
	// 均非降（并发 pump/readLoop 交错时把新事件的 t 夹到不小于它）。
	lastCastT float64

	// EVW（回显验证窗口）状态；仅 opt.EchoVerifyTimeout>0 相关路径使用。
	pendPend  []pendingInput // 待定输入队列
	pendBytes int            // pendPend 累积原始字节数（<= PendingInputCap）
	setup     bool           // TUI 门是否启用（仅 opt.EchoVerifyTUI）
	setupOpen bool           // 当前是否处于 alt-screen

	// TUI 门解析残余状态（跨 output 块）：escParse 0=普通 1=ESC 2=ESC[ 3=ESC[?数字
	escParse int
	escNum   int
}

// Open 打开一个会话目录并创建 recorder（不写 header，延后到首事件/超时/resize）。
//
// dir 为会话目录（已含 username/sessionID 层级）；meta 为会话元数据；opt 为行为选项。
func Open(dir string, meta SessionMeta, opt Options) (*Recorder, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	castPath := filepath.Join(dir, "cast.log")
	f, err := os.OpenFile(castPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, castFileMode)
	if err != nil {
		return nil, err
	}

	if meta.StartedAt.IsZero() {
		meta.StartedAt = time.Now()
	}
	// 保证 open 事件位于 timeline 首条（若调用方没放）。
	if len(meta.Timeline) == 0 || meta.Timeline[0].Event != "open" {
		open := TimelineEvent{T: meta.StartedAt.Unix(), Event: "open"}
		meta.Timeline = append([]TimelineEvent{open}, meta.Timeline...)
	}

	r := &Recorder{
		dir:      dir,
		castPath: castPath,
		metaPath: filepath.Join(dir, "meta.json"),
		opt:      opt,
		meta:     meta,
		started:  meta.StartedAt,
		now:      time.Now,
		w:        bufio.NewWriter(f),
		f:        f,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		setup:    opt.EchoVerifyTUI,
	}

	// 立即原子写一次 meta.json（失败则回滚）。
	if err := writeMetaFile(r.metaPath, meta); err != nil {
		_ = f.Close()
		_ = os.Remove(castPath)
		return nil, err
	}

	r.wg.Add(1)
	go r.flushLoop()
	return r, nil
}

// elapsed 返回相对会话开始的墙钟秒（尊重注入时钟）。
func (r *Recorder) elapsed() float64 {
	return r.now().Sub(r.started).Seconds()
}

// wakeUp 非阻塞唤醒 flush goroutine。
func (r *Recorder) wakeUp() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// WriteOutput 录制 pty 输出（跨块 UTF-8 重组后写 "o" 事件）。
//
// EVW（输入间边界窗口）：输出块**不关闭**观察窗口，只作为回显证据累积到 pending
// 的 seen（跨多个 output 块累积），窗口仅由「下一个输入块/边界事件」确定性关闭。
// 唯一的例外：TUI 门启用且本块「退出 alt-screen」时视为边界事件，确定性关闭窗口。
// 不建议在此结算：否则末尾字符会被 Enter 后 printback 的 output 误判明文。
func (r *Recorder) WriteOutput(b []byte) {
	if len(b) == 0 {
		return
	}
	r.mu.Lock()
	var data []byte
	if len(r.utfTail) > 0 {
		data = append(data, r.utfTail...)
		r.utfTail = nil
	}
	data = append(data, b...)
	complete, tail := splitUTF8Tail(data)
	if len(tail) > 0 {
		r.utfTail = append([]byte(nil), tail...)
	}
	r.mu.Unlock()

	if len(complete) == 0 {
		return
	}

	r.mu.Lock()
	exited := false
	if r.setup {
		exited = r.scanAltScreen(complete)
	}
	// 输出块只累积回显证据，不关闭窗口。
	r.matchPendingLocked(complete)
	if exited {
		// 退出 alt-screen = 边界事件：确定性关闭旧窗口。
		r.settleBoundaryLocked(r.elapsed())
	}
	r.mu.Unlock()

	js, _ := json.Marshal(string(complete))
	r.appendEvent("o", string(js), r.elapsed(), 0)
}

// WriteInput 录制客户端输入（record_input 关闭时整体跳过）。
//
// EVW 边界：输入块本身是边界事件，入口先确定性关闭旧窗口（reached→明文，否则
// 掩码），再写本块明文 i 事件。
func (r *Recorder) WriteInput(b []byte) {
	if !r.opt.RecordInput || len(b) == 0 {
		return
	}
	r.mu.Lock()
	r.settleBoundaryLocked(r.elapsed())
	r.mu.Unlock()

	js, _ := json.Marshal(string(b))
	r.appendEvent("i", string(js), r.elapsed(), 0)
}

// WriteInputMasked 录制脱敏输入：PasswordMode mask 时记等长掩码并计数。
//
//   - "omit"：不记任何事件；
//   - 默认 mask：i 事件 data 为 n 个 "*"，masked 计数同步 meta.MaskedEvents；
//   - "plaintext"：本方法仅收到长度 n，无内容可记，安全兜底按 mask 处理。
func (r *Recorder) WriteInputMasked(n int) {
	if n <= 0 || !r.opt.RecordInput {
		return
	}
	if r.opt.PasswordMode == "omit" {
		return
	}
	js, _ := json.Marshal(strings.Repeat("*", n))
	r.mu.Lock()
	// EVW 边界：本输入块先确定性关闭旧窗口。
	r.settleBoundaryLocked(r.elapsed())
	r.masked++
	r.meta.MaskedEvents = r.masked
	r.mu.Unlock()
	r.appendEvent("i", string(js), r.elapsed(), 0)
}

// WriteInputDeferred EVW：raw 分支待定输入先入 pending；其观察窗口 = [本块到达,
// 下一个输入块/边界事件)。窗口被确定性关闭时 reached→明文、否则掩码；迟迟未关闭
// 则由 hang 兜底（默认掩码）。绝不抛错、不阻塞；Close 后调用安全丢弃。
//
//   - !RecordInput：直接 return（不产生 i 事件）；
//   - PasswordMode=="plaintext"：逃生口，直接按明文 WriteInput（EVW 不变更语义）；
//   - 纯控制块（无任何可打印字节，如 \n）：先关闭旧窗口，本块按明文 WriteInput；
//   - TUI 门且处于 alt-screen：先关闭旧窗口，再按明文 WriteInput（TUI 自行渲染）；
//   - 其余：先关闭旧窗口，再入 pending 等待窗口内回显验证。
func (r *Recorder) WriteInputDeferred(b []byte) {
	if len(b) == 0 {
		return
	}
	if !r.opt.RecordInput {
		return
	}
	if r.opt.PasswordMode == "plaintext" {
		r.WriteInput(b)
		return
	}
	printable := extractPrintable(b)
	if len(printable) == 0 {
		// 纯控制块（如 Enter 的 \n）也是输入块边界；WriteInput 入口先关旧窗口。
		r.WriteInput(b)
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if r.setup && r.setupOpen {
		// alt-screen 内：TUI 自行渲染，EVW 不适用，按明文录（入口先关旧窗口）。
		r.mu.Unlock()
		r.WriteInput(b)
		return
	}
	// 下一个输入块到达 = 边界：先确定性关闭旧窗口。
	r.settleBoundaryLocked(r.elapsed())
	cap := r.opt.PendingInputCap
	if cap <= 0 {
		cap = pendingInputCapDefault
	}
	if r.pendBytes+len(b) > cap {
		// 沿用既有丢弃语义：整体丢弃本块并计数。
		r.dropped++
		r.meta.Dropped = r.dropped
		r.mu.Unlock()
		return
	}
	r.pendPend = append(r.pendPend, pendingInput{
		b:         append([]byte(nil), b...),
		printable: append([]byte(nil), printable...),
		t:         r.elapsed(),
	})
	r.pendBytes += len(b)
	r.mu.Unlock()

	// 唤醒 flushLoop：让 hang 兜底结算不必干等下一次 tick。
	r.wakeUp()
}

// matchPendingLocked 以本 output 块作为「回显证据」，跨块累积到每个 pending 的
// seen：当某 pending 的 printable 字节已全部被观测到即置 reached。O(len(block))。
// 必须在持有 r.mu 时调用。
func (r *Recorder) matchPendingLocked(block []byte) {
	if len(r.pendPend) == 0 {
		return
	}
	for i := range r.pendPend {
		p := &r.pendPend[i]
		if p.reached {
			continue
		}
		for _, c := range block {
			p.seen[c] = true
		}
		ok := true
		for _, c := range p.printable {
			if !p.seen[c] {
				ok = false
				break
			}
		}
		if ok {
			p.reached = true
		}
	}
}

// settleBoundaryLocked 边界裁决：窗口被「下一个输入块/边界事件」确定性关闭，
// 依 reached 结算（reached→明文，否则掩码）。必须在持有 r.mu 时调用。
func (r *Recorder) settleBoundaryLocked(now float64) {
	if len(r.pendPend) == 0 {
		return
	}
	for i := range r.pendPend {
		p := &r.pendPend[i]
		r.emitSettled(p, now, p.reached)
		r.pendBytes -= len(p.b)
	}
	// 清理底层引用，避免内存泄漏。
	for i := range r.pendPend {
		r.pendPend[i] = pendingInput{}
	}
	r.pendPend = r.pendPend[:0]
	if r.pendBytes < 0 {
		r.pendBytes = 0
	}
}

// settleHangLocked hang 兜底：仅结算 age>=hang 的 pending。默认一律掩码；
// EchoVerifyHangPolicy=="reached_plain" 时已 reached 的记明文。forceMask 为 true
// （Close 收尾）时无视 age 与策略，一律掩码，杜绝明文残留。
// 必须在持有 r.mu 时调用。
func (r *Recorder) settleHangLocked(now float64, forceMask ...bool) {
	if len(r.pendPend) == 0 {
		return
	}
	hang := r.opt.EchoVerifyTimeout
	if hang <= 0 {
		hang = echoVerifyHangDefault
	}
	window := hang.Seconds()
	force := len(forceMask) > 0 && forceMask[0]

	kept := r.pendPend[:0]
	for _, p := range r.pendPend {
		if !force && now-p.t < window {
			kept = append(kept, p)
			continue
		}
		plain := false
		if !force && r.opt.EchoVerifyHangPolicy == "reached_plain" && p.reached {
			plain = true
		}
		pp := p
		r.emitSettled(&pp, now, plain)
		r.pendBytes -= len(p.b)
	}
	// 清理已移除项的底层引用，避免内存泄漏。
	for i := len(kept); i < len(r.pendPend); i++ {
		r.pendPend[i] = pendingInput{}
	}
	r.pendPend = kept
	if r.pendBytes < 0 {
		r.pendBytes = 0
	}
}

// emitSettled 将一条 pending 结算为 cast i 事件：plain=true 写原文，否则按
// PasswordMode（omit→不写）写等长 '*' 掩码并计数。事件时间取 pending 到达时刻
// p.t，并夹到不小于 lastCastT，保证 cast 内 t 非降。now 预留给后续时序调整。
// 必须在持有 r.mu 时调用。
func (r *Recorder) emitSettled(p *pendingInput, now float64, plain bool) {
	t := p.t
	if t < r.lastCastT {
		t = r.lastCastT
	}
	if plain {
		js, _ := json.Marshal(string(p.b))
		r.appendEventLocked("i", string(js), t, 0)
		return
	}
	if r.opt.PasswordMode == "omit" {
		return
	}
	js, _ := json.Marshal(strings.Repeat("*", len(p.b)))
	r.masked++
	r.meta.MaskedEvents = r.masked
	r.appendEventLocked("i", string(js), t, 0)
}

// scanAltScreen 以极简状态机扫描 output，识别进入/退出 alt-screen 的常见序列
//（ESC [ ? 1049/1047/47 h|l）。支持跨块分片（残余状态存于 r.escParse/r.escNum）。
// 返回本块是否发生了「退出 alt-screen」。必须在持有 r.mu 时调用。
func (r *Recorder) scanAltScreen(block []byte) (exited bool) {
	for _, c := range block {
		switch r.escParse {
		case 0:
			if c == 0x1B {
				r.escParse = 1
			}
		case 1:
			if c == '[' {
				r.escParse = 2
			} else if c != 0x1B {
				r.escParse = 0
			}
		case 2:
			if c == '?' {
				r.escParse = 3
				r.escNum = 0
			} else {
				r.escParse = 0
			}
		case 3:
			switch {
			case c >= '0' && c <= '9':
				r.escNum = r.escNum*10 + int(c-'0')
				if r.escNum > 1_000_000 {
					r.escParse = 0 // 异常长数字：放弃本次解析
				}
			case c == 'h':
				if isAltScreenCode(r.escNum) {
					r.setupOpen = true
				}
				r.escParse = 0
			case c == 'l':
				if isAltScreenCode(r.escNum) {
					if r.setupOpen {
						exited = true
					}
					r.setupOpen = false
				}
				r.escParse = 0
			default:
				r.escParse = 0
			}
		}
	}
	return exited
}

// isAltScreenCode 判断 DEC private 模式号是否为 alt-screen 切换。
func isAltScreenCode(n int) bool {
	return n == 47 || n == 1047 || n == 1049
}

// extractPrintable 返回 b 中可打印字节子集：0x20-0x7E 直接保留；合法 UTF-8 多字节
// 序列（首字节 >=0xC2 且后续续字节 0x80-0xBF 连续）整体保留；其余（<0x20、0x7F
// 及孤立尾字节）剔除。
func extractPrintable(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c >= 0x20 && c <= 0x7E:
			out = append(out, c)
			i++
		case c >= 0xC2 && c <= 0xDF && i+1 < len(b) && b[i+1]&0xC0 == 0x80:
			out = append(out, b[i], b[i+1])
			i += 2
		case c >= 0xE0 && c <= 0xEF && i+2 < len(b) && b[i+1]&0xC0 == 0x80 && b[i+2]&0xC0 == 0x80:
			out = append(out, b[i], b[i+1], b[i+2])
			i += 3
		case c >= 0xF0 && c <= 0xF4 && i+3 < len(b) && b[i+1]&0xC0 == 0x80 && b[i+2]&0xC0 == 0x80 && b[i+3]&0xC0 == 0x80:
			out = append(out, b[i], b[i+1], b[i+2], b[i+3])
			i += 4
		default:
			i++
		}
	}
	return out
}

// WriteResize 录制窗口尺寸变更（"r" 事件），并在 header 未定时以真实尺寸定 header。
func (r *Recorder) WriteResize(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	r.mu.Lock()
	// EVW 边界：resize 事件确定性关闭旧窗口（reached→明文，否则掩码；i 先于 r）。
	r.settleBoundaryLocked(r.elapsed())
	r.col, r.row = cols, rows
	r.haveSize = true
	if r.headerLine == nil && !r.headerFlushed {
		r.decideHeaderLocked()
	}
	r.mu.Unlock()

	data, _ := json.Marshal(strconv.Itoa(int(cols)) + "x" + strconv.Itoa(int(rows)))
	r.appendEvent("r", string(data), r.elapsed(), 0)
}

// Event 向 timeline 追加一条审计事件并原子重写 meta.json（失败仅记日志，不阻塞）。
func (r *Recorder) Event(name string, fields map[string]any) {
	if name == "" {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	// EVW 边界：审计事件确定性关闭旧窗口，令 MaskedEvents 计数为最新。
	r.settleBoundaryLocked(r.elapsed())
	r.meta.Timeline = append(r.meta.Timeline, TimelineEvent{
		T:     r.now().Unix(),
		Event: name,
		Extra: fields,
	})
	meta := r.metaSnapshotLocked()
	r.mu.Unlock()

	if err := writeMetaFile(r.metaPath, meta); err != nil {
		log.Printf("audit: write meta %s: %v", r.metaPath, err)
	}
}

// Close 终结录制（幂等）：补 close 事件与 ended_at，flush 全部尾部数据并原子写 meta.json。
//
// Close 后任何 Write* 安全丢弃。实现先将 closed 置位并停止 flush goroutine，再由
// 本协程独占写出 header/pending，保证不含并发写。
func (r *Recorder) Close(reason string) {
	// 先把跨块 UTF-8 尾部残留作为 output 落盘，避免丢尾字符。
	r.flushUTF8Tail()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	// EVW：收尾前强制结算所有未结算 pending（一律按掩码），杜绝明文残留。
	r.settleHangLocked(r.elapsed(), true)
	r.closed = true

	now := r.now()
	if r.meta.EndedAt == nil {
		t := now
		r.meta.EndedAt = &t
	}
	if r.meta.EndReason == "" {
		r.meta.EndReason = reason
	}
	r.meta.Timeline = append(r.meta.Timeline, TimelineEvent{
		T:     now.Unix(),
		Event: "close",
		Extra: map[string]any{"reason": reason},
	})
	r.meta.MaskedEvents = r.masked
	r.meta.Dropped = r.dropped
	if r.stopped {
		r.meta.Stopped = true
	}
	if r.headerLine == nil && !r.headerFlushed {
		r.decideHeaderLocked()
	}
	r.mu.Unlock()

	// 停止 flush goroutine，随后独占写出。
	close(r.done)
	r.wg.Wait()

	r.mu.Lock()
	header := r.headerLine
	r.headerLine = nil
	var body []byte
	if r.pending.Len() > 0 {
		body = append(body, r.pending.Bytes()...)
		r.pending.Reset()
	}
	meta := r.metaSnapshotLocked()
	w, f := r.w, r.f
	r.mu.Unlock()

	if w != nil {
		if len(header) > 0 {
			_, _ = w.Write(header)
		}
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
		_ = w.Flush()
	}
	if f != nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := writeMetaFile(r.metaPath, meta); err != nil {
		log.Printf("audit: finalize meta %s: %v", r.metaPath, err)
	}
}

// flushUTF8Tail 将跨块 UTF-8 重组缓冲中的残留字节作为一条 output 事件写出。
func (r *Recorder) flushUTF8Tail() {
	r.mu.Lock()
	tail := r.utfTail
	r.utfTail = nil
	if len(tail) > 0 {
		// EVW：作为 output 块累积回显证据（不关闭窗口；随后 Close 会 force 掩码兜底）。
		r.matchPendingLocked(tail)
	}
	r.mu.Unlock()
	if len(tail) == 0 {
		return
	}
	js, _ := json.Marshal(string(tail))
	r.appendEvent("o", string(js), r.elapsed(), 0)
}

// appendEvent 是所有事件的唯一入队路径：容量/上限/丢弃判定 + header 时机 + wake。
//
// size 为事件行字节数（<=0 时按实际格式化结果计）。时间戳单调（见 appendLineLocked）
// 要求时间戳在持锁内定案，故事件行格式化在锁内完成。
func (r *Recorder) appendEvent(eventType, dataJSON string, t float64, size int) {
	r.mu.Lock()
	queued := r.appendLineLocked(eventType, dataJSON, t, size)
	r.mu.Unlock()
	if queued {
		r.wakeUp()
	}
}

// appendEventLocked 是 appendEvent 的持锁版本，供 settle 等已持锁路径复用。
// 必须在持有 r.mu 时调用；返回是否已入队。
func (r *Recorder) appendEventLocked(eventType, dataJSON string, t float64, size int) bool {
	return r.appendLineLocked(eventType, dataJSON, t, size)
}

// appendLineLocked 是唯一入队路径的持锁核心：时间戳单调夹取 + 格式化 + 容量/上限/
// 丢弃判定 + header 时机。必须在持有 r.mu 时调用；返回是否已入队（需唤醒 flushLoop）。
func (r *Recorder) appendLineLocked(eventType, dataJSON string, t float64, size int) bool {
	if r.closed {
		return false
	}
	if eventType == "o" && r.stopped {
		return false
	}
	if !r.opt.RecordInput && eventType == "i" {
		return false
	}
	// 时间戳单调：写出任何事件行前，把 t 夹到不小于已写出事件的最大时间戳。
	// 由副作用导致 i 事件时间戳可能晚于其回显 o，但绝不回退，保证 cast 行序与 t 非降。
	if t < r.lastCastT {
		t = r.lastCastT
	}
	line := r.formatEventLine(t, eventType, dataJSON)
	if size <= 0 {
		size = len(line)
	}
	// 单会话字节上限：仅 output 触发停止（保留前段，避免撑爆磁盘）。
	if r.opt.MaxSessionBytes > 0 && r.bytesSince+int64(size) > r.opt.MaxSessionBytes {
		if eventType == "o" {
			r.stopped = true
			r.meta.Stopped = true
			return false
		}
	}
	cap := r.opt.PendingCap
	if cap <= 0 {
		cap = pendingCapDefault
	}
	if r.pending.Len()+size > cap {
		r.dropped++
		r.meta.Dropped = r.dropped
		return false
	}
	// header 未定且事件积压达到上限：立即定 header（真实/默认尺寸）。
	if r.headerLine == nil && !r.headerFlushed && r.pending.Len()+size >= headerQueueCap {
		r.decideHeaderLocked()
	}
	r.pending.Write(line)
	r.bytesSince += int64(size)
	r.lastCastT = t
	return true
}

// formatEventLine 生成 asciicast 事件行 [<t>,"<typ>","<data>"]\n。
func (r *Recorder) formatEventLine(t float64, typ, data string) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	b.WriteString(strconv.FormatFloat(t, 'f', 6, 64))
	b.WriteString(`,"`)
	b.WriteString(typ)
	b.WriteString(`",`)
	b.WriteString(data) // data 已是合法 JSON 字符串（含引号）
	b.WriteString("]\n")
	return b.Bytes()
}

// decideHeaderLocked 决定 header 行（幂等）。必须在持有 r.mu 时调用。
//
// header 使用当前已知尺寸（haveSize）或 80x24；含 header 字节计入 bytesSince。
func (r *Recorder) decideHeaderLocked() {
	if r.headerLine != nil || r.headerFlushed {
		return
	}
	w, h := uint16(defaultCols), uint16(defaultRows)
	if r.haveSize {
		w, h = r.col, r.row
	}
	host, _ := os.Hostname()
	title := r.meta.Username + "@" + host + ": " + r.meta.Mode
	hdr := CastHeader{
		Version:   2,
		Width:     w,
		Height:    h,
		Timestamp: r.started.Unix(),
		Env:       map[string]string{"SHELL": "", "TERM": ""},
		Title:     title,
	}
	b, err := json.Marshal(hdr)
	if err != nil {
		b = []byte(`{"version":2}`)
	}
	r.headerLine = append(b, '\n')
	r.bytesSince += int64(len(r.headerLine))
}

// flushOnce 执行一次异步落盘：超时定 header → 取出 header/pending → 锁外写出。
func (r *Recorder) flushOnce() {
	r.mu.Lock()
	// EVW：周期性（或 wake 时）对超过 hang 兜底时长的 pending 保守结算，避免长期滞留。
	r.settleHangLocked(r.elapsed())
	if r.headerLine == nil && !r.headerFlushed {
		if r.now().Sub(r.started) < headerDelayDefault {
			// header 尚未决定：暂不落盘任何事件，确保 header 始终是首行。
			r.mu.Unlock()
			return
		}
		r.decideHeaderLocked()
	}
	var header []byte
	if r.headerLine != nil {
		header = r.headerLine
		r.headerLine = nil
		r.headerFlushed = true
	}
	var body []byte
	if r.pending.Len() > 0 {
		body = append(body, r.pending.Bytes()...)
		r.pending.Reset()
	}
	w := r.w
	r.mu.Unlock()

	if w == nil {
		return
	}
	if len(header) > 0 {
		_, _ = w.Write(header)
	}
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
	threshold := r.opt.FlushThreshold
	if threshold <= 0 {
		threshold = flushThresholdDefault
	}
	if w.Buffered() >= threshold {
		_ = w.Flush()
	}
}

// flushLoop 周期/唤醒式异步落盘，直到 done 关闭。
func (r *Recorder) flushLoop() {
	defer r.wg.Done()
	interval := r.opt.FlushInterval
	if interval <= 0 {
		interval = flushIntervalDefault
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.flushOnce()
		case <-r.wake:
			r.flushOnce()
		}
	}
}

// metaSnapshotLocked 在锁内生成 meta 的深拷贝（Timeline/Extra/AuthProvided），
// 以便锁外序列化时不受并发追加影响。必须在持有 r.mu 时调用。
func (r *Recorder) metaSnapshotLocked() SessionMeta {
	m := r.meta
	if len(r.meta.Timeline) > 0 {
		tl := make([]TimelineEvent, len(r.meta.Timeline))
		copy(tl, r.meta.Timeline)
		for i := range tl {
			if src := r.meta.Timeline[i].Extra; src != nil {
				ex := make(map[string]any, len(src))
				for k, v := range src {
					ex[k] = v
				}
				tl[i].Extra = ex
			}
		}
		m.Timeline = tl
	}
	if len(r.meta.AuthProvided) > 0 {
		m.AuthProvided = append([]string(nil), r.meta.AuthProvided...)
	}
	return m
}

// splitUTF8Tail 从字节块末尾识别「不完整的多字节序列」并切分为 (完整段, 残留尾)。
//
// lead 范围：0xC2–0xDF 需 1 个续字节、0xE0–0xEF 需 2、0xF0–0xF4 需 3；
// lead 为 0xC0/0xC1/0x80–0xBF/>=0xF5 视为损坏，不跨块保留（交给 json.Marshal
// 替换为 U+FFFD）。当连续 trailing 续字节数 < lead 所需且 >0（或 lead 后尚无
// 续字节）时保留尾段；否则视为完整。
func splitUTF8Tail(b []byte) (complete, tail []byte) {
	n := len(b)
	i := n
	for i > 0 && b[i-1]&0xC0 == 0x80 {
		i--
	}
	trailing := n - i
	if i == 0 {
		// 全是续字节，无 lead：损坏，不保留。
		return b, nil
	}
	lead := b[i-1]
	var need int
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead >= 0xE0 && lead <= 0xEF:
		need = 2
	case lead >= 0xF0 && lead <= 0xF4:
		need = 3
	default:
		// ASCII 或损坏 lead：不超过块边界保留。
		return b, nil
	}
	if trailing < need {
		return b[:i-1], b[i-1:]
	}
	return b, nil
}
