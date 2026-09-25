//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var errEOF = io.EOF

var (
	k32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode   = k32.NewProc("GetConsoleMode")
	procSetConsoleMode   = k32.NewProc("SetConsoleMode")
	procGetScreenBufInfo = k32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
	enableVTInput        = 0x0200
	enableVTProcessing   = 0x0004 // output mode
	lineEditing          = true
	errNoConsole         = "not a console"
)

func consoleMode(f *os.File) (uint32, bool) {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return mode, r != 0
}

// makeRaw switches the console to raw VT input: no echo, no line
// buffering, Ctrl-C as a key, arrow keys as escape sequences — what the
// line editor reads on Unix terminals too.
func makeRawOS(f *os.File) (func(), error) {
	old, ok := consoleMode(f)
	if !ok {
		return nil, errors.New(errNoConsole)
	}
	raw := old&^(enableEchoInput|enableLineInput|enableProcessedInput) | enableVTInput
	if r, _, _ := procSetConsoleMode.Call(f.Fd(), uintptr(raw)); r == 0 {
		return nil, errors.New("console does not support VT input")
	}
	return func() { procSetConsoleMode.Call(f.Fd(), uintptr(old)) }, nil
}

func termWidth(f *os.File) int {
	var info struct {
		Size, Cursor             [2]int16
		Attrs                    uint16
		Left, Top, Right, Bottom int16
		MaxSize                  [2]int16
	}
	if r, _, _ := procGetScreenBufInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 || info.Right <= info.Left {
		return 80
	}
	return int(info.Right-info.Left) + 1
}

// Windows consoles show ANSI colors only with virtual terminal
// processing on (Windows 10+); without it they print escape codes.
func init() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if mode, ok := consoleMode(f); ok {
			procSetConsoleMode.Call(f.Fd(), uintptr(mode|enableVTProcessing))
		}
	}
}

var procWaitForSingleObject = k32.NewProc("WaitForSingleObject")

// inputReady reports whether console input arrives within d (so a lone
// Esc is not merged with the next key).
func inputReady(f *os.File, d time.Duration) bool {
	r, _, _ := procWaitForSingleObject.Call(f.Fd(), uintptr(d.Milliseconds()))
	return r == 0 // WAIT_OBJECT_0; WAIT_TIMEOUT is 0x102
}

// termRows is the console window height.
func termRows(f *os.File) int {
	var info struct {
		Size, Cursor             [2]int16
		Attrs                    uint16
		Left, Top, Right, Bottom int16
		MaxSize                  [2]int16
	}
	if r, _, _ := procGetScreenBufInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 || info.Bottom <= info.Top {
		return 24
	}
	return int(info.Bottom-info.Top) + 1
}

func noEcho(*os.File) (func(), error) { return nil, errors.New("not supported") }

func notifyResize(chan os.Signal) {}

func withCookedTerm(_ *os.File, fn func() error) error { return fn() }

// suspendSelf is not available on Windows.
func suspendSelf() bool { return false }
