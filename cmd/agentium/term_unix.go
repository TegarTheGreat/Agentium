//go:build linux || darwin

package main

import (
	"io"
	"os"
	"os/signal"
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
func makeRawOS(f *os.File) (func(), error) {
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
	if fs := activeFS(); fs != nil && f == fs.pw {
		return fs.width()
	}
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

// termRows is the terminal height (24 when unknown).
func termRows(f *os.File) int {
	if fs := activeFS(); fs != nil && f == fs.pw {
		return fs.height()
	}
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Row == 0 {
		return 24
	}
	return int(ws.Row)
}

const lineEditing = true

// noEcho stops the terminal from echoing keys and buffering lines, but
// keeps Ctrl-C as a signal (the full-screen UI's resting state).
func noEcho(f *os.File) (func(), error) {
	fd := f.Fd()
	old, err := tcget(fd)
	if err != nil {
		return nil, err
	}
	t := old
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := tcset(fd, &t); err != nil {
		return nil, err
	}
	return func() { _ = tcset(fd, &old) }, nil
}

func notifyResize(ch chan os.Signal) { signal.Notify(ch, syscall.SIGWINCH) }
