//go:build !linux

package pty

import "errors"

// EchoState 依赖 Linux termios，非 Linux 平台返回错误。
func (s *Session) EchoState() (bool, bool, error) {
	return false, false, errors.New("EchoState requires linux")
}
