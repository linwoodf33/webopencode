//go:build linux

package pty

import (
	"os"

	"golang.org/x/sys/unix"
)

// EchoState 返回 pty 当前的回显与规范模式状态（读取 master 的 termios c_lflag）。
// 语义：echo=true 表示终端正在回显输入；canonical=true 表示规范（行缓冲）模式。
// 判据（供审计脱敏）：ECHO=0 && ICANON=1 命中密码提示；raw 模式 TUI（ICANON=0）不命中。
func (s *Session) EchoState() (echo, canonical bool, err error) {
	// 与 Read 相同模式：仅在锁内取 master/closed 快照，锁外做 ioctl，
	// 避免阻塞 Close 获取锁。
	s.mu.Lock()
	master := s.master
	closed := s.closed
	s.mu.Unlock()

	if closed || master == nil {
		return false, false, os.ErrClosed
	}

	t, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		return false, false, err
	}
	return t.Lflag&unix.ECHO != 0, t.Lflag&unix.ICANON != 0, nil
}
