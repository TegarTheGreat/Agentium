//go:build darwin

package tool

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// openPTY returns a pseudo-terminal pair (no cgo: /dev/ptmx + ioctls).
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := ioctl(m.Fd(), syscall.TIOCPTYGRANT, 0); err != nil {
		m.Close()
		return nil, nil, err
	}
	if err := ioctl(m.Fd(), syscall.TIOCPTYUNLK, 0); err != nil {
		m.Close()
		return nil, nil, err
	}
	var name [128]byte
	if err := ioctl(m.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		m.Close()
		return nil, nil, err
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	s, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	setWinsize(m, 40, 120)
	return m, s, nil
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

func setWinsize(f *os.File, rows, cols uint16) {
	ws := [4]uint16{rows, cols, 0, 0}
	_ = ioctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// sessionPIDs lists the live processes in session sid.
func sessionPIDs(sid int) []int {
	out, _ := exec.Command("/usr/bin/pgrep", "-s", strconv.Itoa(sid), ".").Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
