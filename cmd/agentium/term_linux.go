//go:build linux

package main

import (
	"os"
	"syscall"
	"time"
)

const (
	ioctlGet = syscall.TCGETS
	ioctlSet = syscall.TCSETS
)

// inputReady reports whether f has input within d (a lone Esc key is
// told apart from the start of an escape sequence this way).
func inputReady(f *os.File, d time.Duration) bool {
	var r syscall.FdSet
	fd := int(f.Fd())
	r.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	n, err := syscall.Select(fd+1, &r, nil, nil, &tv)
	return err != nil || n > 0
}
