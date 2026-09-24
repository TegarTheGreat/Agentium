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
	deadline := time.Now().Add(d)
	for {
		tv := syscall.NsecToTimeval(time.Until(deadline).Nanoseconds())
		set := r
		err := syscall.Select(fd+1, &set, nil, nil, &tv)
		if err == syscall.EINTR && time.Now().Before(deadline) {
			continue // a signal (e.g. a resize) is not input
		}
		return err == nil && set.Bits[fd/32]&(1<<(uint(fd)%32)) != 0
	}
}
