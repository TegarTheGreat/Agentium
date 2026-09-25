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
	deadline := time.Now().Add(d)
	for {
		tv := syscall.NsecToTimeval(time.Until(deadline).Nanoseconds())
		set := r
		n, err := syscall.Select(fd+1, &set, nil, nil, &tv)
		if err == syscall.EINTR && time.Now().Before(deadline) {
			continue // a signal (e.g. a resize) is not input
		}
		return err == nil && n > 0
	}
}

// vdisable turns a terminal control character off (_POSIX_VDISABLE).
const vdisable = 0
