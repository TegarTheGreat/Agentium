package main

import (
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The full-screen UI. Agentium's output (stdout and stderr) is redirected
// into a pipe and interpreted by a vterm; a compositor draws the screen:
//
//	header      ◆ Agentium · model · mode · folder
//	office      pixel-art desks (office.go)
//	transcript  the conversation, scrollable (PgUp/PgDn, mouse wheel)
//	input       the prompt, in a box at the bottom
//	status      hints, or how far the view is scrolled
//
// Every feature of the inline UI keeps working unchanged inside the
// transcript pane. On exit the alternate screen is left and the
// conversation is printed to the normal terminal, so it stays in the
// scrollback.

type fullscreen struct {
	mu        sync.Mutex
	vt        *vterm
	tty       *os.File // the real terminal
	pr, pw    *os.File
	rows      int
	cols      int
	prev      []string
	scroll    int // lines scrolled up from the bottom
	office    *office
	header    func() string
	status    func() string
	input     bool // the line editor is active: its line is the input box
	dirty     bool
	truecolor bool
	lastFrame int
	maxEnd    int // longest transcript drawn (the live area shrinks and grows it)
	done      chan struct{}
	readDone  chan struct{}
	stdout    *os.File
	stderr    *os.File
	restore   func()
	closed    bool
}

// fs is the active full-screen UI, if any.
var (
	fsMu sync.Mutex
	fs   *fullscreen
)

func activeFS() *fullscreen {
	fsMu.Lock()
	defer fsMu.Unlock()
	return fs
}

// fullscreenWanted reports whether the full-screen UI should be used.
func fullscreenWanted(classic bool) bool {
	if classic || runtime.GOOS == "windows" || os.Getenv("TERM") == "dumb" || os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch strings.ToLower(os.Getenv("AGENTIUM_UI")) {
	case "classic", "inline":
		return false
	}
	return isTTY(os.Stdin) && isTTY(os.Stdout) && isTTY(os.Stderr) && termRows(os.Stderr) >= 16 && termWidth(os.Stderr) >= 50
}

// enterFullscreen switches to the full-screen UI. The caller defers
// leave().
func enterFullscreen() (*fullscreen, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	f := &fullscreen{tty: os.Stderr, pr: pr, pw: pw, stdout: os.Stdout, stderr: os.Stderr, office: newOffice(),
		truecolor: truecolorTerm(), done: make(chan struct{}), readDone: make(chan struct{}), lastFrame: -1}
	f.rows, f.cols = termRows(f.tty), termWidth(f.tty)
	f.vt = newVterm(f.cols - 4)
	// Keys are not echoed by the terminal (they would land in the middle
	// of the screen); Ctrl-C still interrupts.
	if restore, err := noEcho(os.Stdin); err == nil {
		f.restore = restore
	}
	// Alternate screen, hidden cursor, mouse wheel reporting (SGR).
	f.tty.WriteString("\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[H\x1b[2J")
	os.Stdout, os.Stderr = pw, pw
	fsMu.Lock()
	fs = f
	fsMu.Unlock()

	go func() {
		defer close(f.readDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.vt.Write(buf[:n])
				f.dirty = true
				f.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	go func() {
		t := time.NewTicker(33 * time.Millisecond)
		defer t.Stop()
		defer signal.Stop(winch)
		for {
			select {
			case <-f.done:
				return
			case <-winch:
				f.mu.Lock()
				f.rows, f.cols = termRows(f.tty), termWidth(f.tty)
				f.vt.width = max(f.cols-4, 10)
				f.prev = nil
				f.dirty = true
				f.mu.Unlock()
			case <-t.C:
				f.mu.Lock()
				frame := int(time.Since(f.office.start) / officeFrame)
				if f.dirty || frame != f.lastFrame {
					f.lastFrame = frame
					f.dirty = false
					f.draw()
				}
				f.mu.Unlock()
			}
		}
	}()
	return f, nil
}

// leave restores the normal screen and prints the conversation there.
func (f *fullscreen) leave() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	close(f.done)
	os.Stdout, os.Stderr = f.stdout, f.stderr
	fsMu.Lock()
	fs = nil
	fsMu.Unlock()
	f.pw.Close()
	select {
	case <-f.readDone:
	case <-time.After(time.Second):
	}
	if f.restore != nil {
		f.restore()
	}
	f.tty.WriteString("\x1b[?1000l\x1b[?1006l\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l")
	// The conversation, for the scrollback.
	f.mu.Lock()
	var sb strings.Builder
	end := f.vt.end()
	for end > 0 && strings.TrimSpace(f.vt.plain(end-1)) == "" {
		end--
	}
	for i := 0; i < end; i++ {
		sb.WriteString(f.vt.render(i, f.vt.width) + "\n")
	}
	f.mu.Unlock()
	f.tty.WriteString(sb.String())
}

func (f *fullscreen) width() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vt.width
}

// height is the number of transcript rows.
func (f *fullscreen) height() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, h, _ := f.layout()
	return h
}

// layout: rows used by the office strip, the transcript, and whether the
// office is shown.
func (f *fullscreen) layout() (officeH, transcriptH int, showOffice bool) {
	fixed := 2 // header, status
	if f.input {
		fixed += 3 // the input box: borders and the line
	}
	showOffice = f.rows >= 24 && f.cols >= 60
	if showOffice {
		officeH = officeRows + 1
	}
	return officeH, max(f.rows-fixed-officeH, 3), showOffice
}

func (f *fullscreen) setInput(on bool) {
	f.mu.Lock()
	if f.input != on {
		f.input = on
		f.dirty = true
		if on {
			f.scroll = 0
		}
	}
	f.mu.Unlock()
}

func (f *fullscreen) scrollBy(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, h, _ := f.layout()
	f.scroll = min(max(f.scroll+n, 0), max(f.vt.end()-h, 0))
	f.dirty = true
}

func (f *fullscreen) redraw() {
	f.mu.Lock()
	f.prev = nil
	f.dirty = true
	f.mu.Unlock()
}

// draw composes the screen and writes the rows that changed; the caller
// holds f.mu.
func (f *fullscreen) draw() {
	officeH, h, showOffice := f.layout()
	screen := make([]string, 0, f.rows)
	screen = append(screen, f.headerRow())
	if showOffice {
		screen = append(screen, f.office.render(f.cols, f.truecolor)...)
		screen = append(screen, "")
		for len(screen) < 1+officeH {
			screen = append(screen, "")
		}
	}
	end := f.vt.end()
	inputRow := -1
	if f.input {
		inputRow = f.vt.row
		if inputRow == end-1 {
			end-- // the prompt line is drawn in the input box
		}
	}
	if f.scroll > 0 && end > f.maxEnd {
		// Scrolled back: new output must not move what is being read.
		f.scroll += end - f.maxEnd
	}
	f.maxEnd = max(f.maxEnd, end)
	f.scroll = min(f.scroll, max(end-h, 0))
	// A short conversation starts at the top, like a chat; a long one
	// keeps its newest lines in view.
	first := max(end-h-f.scroll, 0)
	for i := 0; i < h; i++ {
		row := ""
		if first+i < end {
			row = f.vt.render(first+i, f.cols-2)
		}
		screen = append(screen, " "+row)
	}
	if f.input {
		border := "\x1b[38;5;66m"
		inner := max(f.cols-4, 1)
		line := f.vt.render(inputRow, inner)
		pad := max(inner-strWidth(line), 0)
		screen = append(screen, border+"╭"+strings.Repeat("─", f.cols-2)+"╮\x1b[0m")
		screen = append(screen, border+"│\x1b[0m "+line+strings.Repeat(" ", pad)+" "+border+"│\x1b[0m")
		screen = append(screen, border+"╰"+strings.Repeat("─", f.cols-2)+"╯\x1b[0m")
	}
	screen = append(screen, f.statusRow())

	var out strings.Builder
	out.WriteString("\x1b[?25l")
	for i, row := range screen {
		if i < len(f.prev) && f.prev[i] == row {
			continue
		}
		out.WriteString("\x1b[" + strconv.Itoa(i+1) + ";1H\x1b[0m" + row + "\x1b[0m\x1b[K")
	}
	for i := len(screen); i < len(f.prev); i++ {
		out.WriteString("\x1b[" + strconv.Itoa(i+1) + ";1H\x1b[2K")
	}
	f.prev = screen
	if f.input && f.scroll == 0 {
		// The cursor in the input box.
		out.WriteString("\x1b[" + strconv.Itoa(len(screen)-2) + ";" + strconv.Itoa(min(f.vt.col, f.cols-4)+3) + "H\x1b[?25h")
	}
	f.tty.WriteString(out.String())
}

func (f *fullscreen) headerRow() string {
	left := " ◆ Agentium " + "\x1b[2m" + version + "\x1b[22m"
	right := " "
	if f.header != nil {
		right = f.header() + " "
	}
	pad := f.cols - strWidth(left) - strWidth(right)
	if pad < 1 {
		right = truncate(right, max(f.cols-strWidth(left)-2, 0))
		pad = max(f.cols-strWidth(left)-strWidth(right), 0)
	}
	return "\x1b[48;5;236m\x1b[38;5;252m" + left + strings.Repeat(" ", pad) + right + "\x1b[0m"
}

func (f *fullscreen) statusRow() string {
	var left string
	if f.scroll > 0 {
		left = " \x1b[33m↓ " + strconv.Itoa(f.scroll) + " more line" + plural(f.scroll) + "\x1b[0m\x1b[2m · PgDn or wheel to go back\x1b[0m"
	} else if f.input {
		left = " \x1b[2mEnter send · \\ + Enter new line · PgUp/wheel scroll · /help\x1b[0m"
	} else {
		left = " \x1b[2mtype to queue a message · Ctrl-C interrupt · PgUp/wheel scroll\x1b[0m"
	}
	right := ""
	if f.status != nil {
		right = f.status()
	}
	if strWidth(left)+strWidth(right)+1 > f.cols {
		left = truncate(left, max(f.cols-strWidth(right)-2, 0))
	}
	pad := max(f.cols-strWidth(left)-strWidth(right)-1, 0)
	return left + strings.Repeat(" ", pad) + "\x1b[2m" + right + "\x1b[0m"
}

// scrollKey handles a scrolling key (PgUp/PgDn, mouse wheel) and reports
// whether k was one; other mouse events are swallowed too.
func scrollKey(k string) bool {
	f := activeFS()
	if f == nil {
		return false
	}
	switch {
	case k == "\x1b[5~":
		f.scrollBy(f.height() - 2)
	case k == "\x1b[6~":
		f.scrollBy(-(f.height() - 2))
	case strings.HasPrefix(k, "\x1b[<"):
		// SGR mouse: button 64 = wheel up, 65 = wheel down.
		btn, _, _ := strings.Cut(strings.TrimPrefix(k, "\x1b[<"), ";")
		switch btn {
		case "64":
			f.scrollBy(3)
		case "65":
			f.scrollBy(-3)
		}
	default:
		return false
	}
	return true
}
