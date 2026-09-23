//go:build linux || darwin

package main

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

var errEOF = io.EOF

func tcget(fd uintptr) (syscall.Termios, error) {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlGet), uintptr(unsafe.Pointer(&t)))
	if e != 0 {
		return t, e
	}
	return t, nil
}

func tcset(fd uintptr, t *syscall.Termios) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlSet), uintptr(unsafe.Pointer(t)))
	if e != 0 {
		return e
	}
	return nil
}

// makeRaw puts the terminal in raw mode (no echo, no line buffering, no
// signal keys) and returns a function restoring the previous state.
func makeRaw(f *os.File) (func(), error) {
	fd := f.Fd()
	old, err := tcget(fd)
	if err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := tcset(fd, &raw); err != nil {
		return nil, err
	}
	return func() { _ = tcset(fd, &old) }, nil
}

func termWidth(f *os.File) int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

const lineEditing = true
