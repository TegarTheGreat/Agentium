//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"
)

var errEOF = io.EOF

func makeRaw(*os.File) (func(), error) { return nil, errors.New("line editing not supported") }

func termWidth(*os.File) int { return 80 }

const lineEditing = false

// Windows consoles show ANSI colors only with virtual terminal
// processing on (Windows 10+); without it they print escape codes.
func init() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	get, set := k32.NewProc("GetConsoleMode"), k32.NewProc("SetConsoleMode")
	const enableVT = 0x0004
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		var mode uint32
		if r, _, _ := get.Call(f.Fd(), uintptr(unsafe.Pointer(&mode))); r == 0 {
			continue
		}
		set.Call(f.Fd(), uintptr(mode|enableVT))
	}
}
