package audit

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestEncodeParseRoundtrip(t *testing.T) {
	ev := CastEvent{T: 1.5, Type: "o", Data: "a\x1b[0m"}
	b, err := EncodeEventLine(ev, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ParseEventLine(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.T != ev.T || got.Type != ev.Type || got.Data != ev.Data {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, ev)
	}

	// shift 为负导致 <0 时 clamp 到 0。
	b2, err := EncodeEventLine(CastEvent{T: 0.2, Type: "o", Data: "x"}, -1)
	if err != nil {
		t.Fatalf("encode shift: %v", err)
	}
	got2, err := ParseEventLine(b2)
	if err != nil {
		t.Fatalf("parse shift: %v", err)
	}
	if got2.T != 0 {
		t.Fatalf("clamp failed: t=%v", got2.T)
	}
}

const sliceSrc = `{"version":2,"width":80,"height":24,"timestamp":0}
[0.000000,"r","80x24"]
[1.000000,"o","a"]
[2.000000,"r","100x40"]
[3.000000,"o","b"]
[4.000000,"i","x"]
`

func splitNonEmpty(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func TestSliceCastWindow(t *testing.T) {
	var out bytes.Buffer
	if err := SliceCast(strings.NewReader(sliceSrc), 2.5, 4.5, &out); err != nil {
		t.Fatalf("SliceCast: %v", err)
	}
	lines := splitNonEmpty(out.String())
	if len(lines) != 4 {
		t.Fatalf("want header+r+o+i, got %d: %v", len(lines), lines)
	}
	// header 原样
	var h CastHeader
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatalf("header: %v", err)
	}
	// 窗口前最近 r 恢复为 t=0
	ev0, err := ParseEventLine([]byte(lines[1]))
	if err != nil {
		t.Fatalf("parse r: %v", err)
	}
	if ev0.Type != "r" || ev0.T != 0 || ev0.Data != "100x40" {
		t.Fatalf("restored r: %+v", ev0)
	}
	// 平移 -2.5：3.0 -> 0.5
	ev1, _ := ParseEventLine([]byte(lines[2]))
	if ev1.Type != "o" || ev1.Data != "b" || math.Abs(ev1.T-0.5) > 1e-6 {
		t.Fatalf("o event: %+v", ev1)
	}
	// 4.0 -> 1.5
	ev2, _ := ParseEventLine([]byte(lines[3]))
	if ev2.Type != "i" || ev2.Data != "x" || math.Abs(ev2.T-1.5) > 1e-6 {
		t.Fatalf("i event: %+v", ev2)
	}
}

func TestSliceCastEmptyWindow(t *testing.T) {
	// tTo <= tFrom：仅 header。
	var out bytes.Buffer
	if err := SliceCast(strings.NewReader(sliceSrc), 5, 2, &out); err != nil {
		t.Fatalf("SliceCast: %v", err)
	}
	lines := splitNonEmpty(out.String())
	if len(lines) != 1 {
		t.Fatalf("want only header, got %v", lines)
	}
}

func TestSliceCastBoundsExcludeTo(t *testing.T) {
	// [3.0,4.0) 只含 t=3.0 的 "b"（t=4.0 的事件被排除）。
	var out bytes.Buffer
	if err := SliceCast(strings.NewReader(sliceSrc), 3.0, 4.0, &out); err != nil {
		t.Fatalf("SliceCast: %v", err)
	}
	lines := splitNonEmpty(out.String())
	// header + 恢复的 r? 窗口前最近 r 是 t=2.0 "100x40"（<tFrom=3.0），故补一条。
	if len(lines) != 3 {
		t.Fatalf("want header+r+o, got %d: %v", len(lines), lines)
	}
	ev, _ := ParseEventLine([]byte(lines[1]))
	if ev.Type != "r" || ev.T != 0 {
		t.Fatalf("restored r: %+v", ev)
	}
	oev, _ := ParseEventLine([]byte(lines[2]))
	if oev.Type != "o" || oev.Data != "b" || oev.T != 0 {
		t.Fatalf("o event: %+v", oev)
	}
}
