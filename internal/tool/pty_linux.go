//go:build linux

package tool

import (
	"fmt"
	"os"
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
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, nil, err
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
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
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp session ...; comm may hold spaces.
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) > 3 && f[0] != "Z" && f[3] == strconv.Itoa(sid) {
			pids = append(pids, pid)
		}
	}
	return pids
}
