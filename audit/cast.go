package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// CastHeader 是 asciicast v2 的首行 header。
//
// 有意省略 idle_time_limit 字段：不写该字段，避免被 player 解释为「0 秒 idle 上限」。
type CastHeader struct {
	Version   int               `json:"version"`
	Width     uint16            `json:"width"`
	Height    uint16            `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Env       map[string]string `json:"env,omitempty"`
	Title     string            `json:"title,omitempty"`
}

// CastEvent 是 asciicast v2 的一行事件 [T, Type, Data]。
type CastEvent struct {
	T    float64
	Type string // o|i|r|m
	Data string
}

// ParseEventLine 解析一行事件 JSON 数组 [t, type, data]。
func ParseEventLine(line []byte) (CastEvent, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return CastEvent{}, errors.New("audit: empty cast line")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(line, &arr); err != nil {
		return CastEvent{}, err
	}
	if len(arr) < 3 {
		return CastEvent{}, fmt.Errorf("audit: cast event needs 3 fields, got %d", len(arr))
	}
	var ev CastEvent
	if err := json.Unmarshal(arr[0], &ev.T); err != nil {
		return CastEvent{}, fmt.Errorf("audit: cast time: %w", err)
	}
	if err := json.Unmarshal(arr[1], &ev.Type); err != nil {
		return CastEvent{}, fmt.Errorf("audit: cast type: %w", err)
	}
	if err := json.Unmarshal(arr[2], &ev.Data); err != nil {
		return CastEvent{}, fmt.Errorf("audit: cast data: %w", err)
	}
	return ev, nil
}

// EncodeEventLine 编码一行事件，时间以 ev.T+shift 写出（结果 <0 时 clamp 为 0）。
func EncodeEventLine(ev CastEvent, shift float64) ([]byte, error) {
	t := ev.T + shift
	if t < 0 {
		t = 0
	}
	typ, err := json.Marshal(ev.Type)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteByte('[')
	b.WriteString(strconv.FormatFloat(t, 'f', 6, 64))
	b.WriteByte(',')
	b.Write(typ)
	b.WriteByte(',')
	b.Write(data)
	b.WriteString("]\n")
	return b.Bytes(), nil
}

// SliceCast 从 r 读取 asciicast NDJSON，按相对秒窗口 [tFrom,tTo) 切片并平移 -tFrom 后写入 w。
//
//   - 首行 header 原样转发；
//   - 窗口外 t<tFrom 的事件中记住最近一条 "r"；t>=tTo 的事件丢弃；
//   - 窗口内事件平移（t-tFrom），并在首个窗口事件前补一条 t=0 的最近 "r"（若存在）；
//   - tTo<=tFrom 或 tTo<=0：不输出任何窗口事件（仅 header）。
func SliceCast(r io.Reader, tFrom, tTo float64, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	windowOpen := tTo > tFrom && tTo > 0

	first := true
	var lastR *CastEvent
	restored := false

	for sc.Scan() {
		line := sc.Bytes()
		if first {
			first = false
			// header 原样转发（补换行）。
			out := append(append([]byte(nil), line...), '\n')
			if _, err := w.Write(out); err != nil {
				return err
			}
			continue
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		ev, err := ParseEventLine(trimmed)
		if err != nil {
			continue
		}
		if !windowOpen {
			continue
		}
		if ev.T < tFrom {
			if ev.Type == "r" {
				e := ev
				lastR = &e
			}
			continue
		}
		if ev.T >= tTo {
			continue
		}
		if !restored {
			if lastR != nil {
				// 恢复窗口起点的渲染基准尺寸。
				b, err := EncodeEventLine(CastEvent{T: 0, Type: "r", Data: lastR.Data}, 0)
				if err != nil {
					return err
				}
				if _, err := w.Write(b); err != nil {
					return err
				}
			}
			restored = true
		}
		b, err := EncodeEventLine(ev, -tFrom)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return sc.Err()
}
