//go:build linux

package pty

import (
	"errors"
	"os"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TestEchoStateClosed 已关闭的会话返回 os.ErrClosed，且 echo/canonical 均为 false。
func TestEchoStateClosed(t *testing.T) {
	s := &Session{closed: true}
	echo, canonical, err := s.EchoState()
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("EchoState(closed) err = %v, want os.ErrClosed", err)
	}
	if echo || canonical {
		t.Errorf("EchoState(closed) = (%v, %v), want (false, false)", echo, canonical)
	}
}

// TestEchoStateNilMaster 未关闭但无 master 的会话亦返回 os.ErrClosed。
func TestEchoStateNilMaster(t *testing.T) {
	s := &Session{}
	echo, canonical, err := s.EchoState()
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("EchoState(nil master) err = %v, want os.ErrClosed", err)
	}
	if echo || canonical {
		t.Errorf("EchoState(nil master) = (%v, %v), want (false, false)", echo, canonical)
	}
}

// TestEchoStateRealPty 用真实 pty 直接设置 termios c_lflag，确定性验证判据：
//   - ECHO+ICANON  → (true, true)
//   - 关 ECHO      → (false, true)   （密码提示：ECHO=0 && ICANON=1）
//   - 关 ICANON    → (false, false)  （raw 模式 TUI：不命中脱敏判据）
//
// 直接写 termios 避免依赖外部 shell，保证稳定、不 flaky。
func TestEchoStateRealPty(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open failed: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	s := &Session{master: master}

	tio, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("IoctlGetTermios failed: %v", err)
	}

	set := func(mut func(*unix.Termios)) {
		mut(tio)
		if err := unix.IoctlSetTermios(int(master.Fd()), unix.TCSETS, tio); err != nil {
			t.Fatalf("IoctlSetTermios failed: %v", err)
		}
	}

	cases := []struct {
		name          string
		setup         func(*unix.Termios)
		wantEcho      bool
		wantCanonical bool
	}{
		{"echo+icanon", func(tt *unix.Termios) { tt.Lflag |= unix.ECHO | unix.ICANON }, true, true},
		{"password (echo off, icanon on)", func(tt *unix.Termios) {
			tt.Lflag &^= unix.ECHO
			tt.Lflag |= unix.ICANON
		}, false, true},
		{"raw tui (no icanon)", func(tt *unix.Termios) {
			tt.Lflag &^= unix.ECHO | unix.ICANON
		}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set(tc.setup)
			echo, canonical, err := s.EchoState()
			if err != nil {
				t.Fatalf("EchoState err = %v, want nil", err)
			}
			if echo != tc.wantEcho || canonical != tc.wantCanonical {
				t.Errorf("EchoState = (%v, %v), want (%v, %v)", echo, canonical, tc.wantEcho, tc.wantCanonical)
			}
		})
	}
}
