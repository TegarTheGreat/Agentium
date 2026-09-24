package main

import (
	"os"
	"sync"
)

// The terminal must never be left in raw mode (no echo, no line editing)
// when Agentium exits unexpectedly: whoever enters raw mode for a while
// registers how to undo it, and exit paths call restoreTerm.

var (
	termMu      sync.Mutex
	termRestore func()
)

func setTermRestore(f func()) {
	termMu.Lock()
	termRestore = f
	termMu.Unlock()
}

// restoreTerm undoes raw mode and shows the cursor again.
func restoreTerm() {
	if fs := activeFS(); fs != nil {
		fs.leave()
	}
	termMu.Lock()
	f := termRestore
	termRestore = nil
	termMu.Unlock()
	if f != nil {
		f()
	}
	if isTTY(os.Stderr) {
		os.Stderr.WriteString("\x1b[?2004l\x1b[?25h")
	}
}

// makeRaw enters raw mode and registers how to leave it, so an exit on a
// signal or crash restores the terminal too.
func makeRaw(f *os.File) (func(), error) {
	restore, err := makeRawOS(f)
	if err != nil {
		return nil, err
	}
	setTermRestore(restore)
	return func() {
		restore()
		setTermRestore(nil)
	}, nil
}
