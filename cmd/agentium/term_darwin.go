//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"
)

const (
	ioctlGet = syscall.TIOCGETA
	ioctlSet = syscall.TIOCSETA
)

// inputReady reports whether f has input within d (a lone Esc key is
// told apart from the start of an escape sequence this way).
func inputReady(f *os.File, d time.Duration) bool {
	var r syscall.FdSet
	fd := int(f.Fd())
	r.Bits[fd/32] |= 1 << (uint(fd) % 32)
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	if err := syscall.Select(fd+1, &r, nil, nil, &tv); err != nil {
		return true
	}
	return r.Bits[fd/32]&(1<<(uint(fd)%32)) != 0
}
