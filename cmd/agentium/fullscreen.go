package main

import (
	"fmt"
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
//	┌──────────────────────────────────┬───────────────┐
//	│ transcript (scrollable)          │ ops sidebar:  │
//	│                                  │ office, plan, │
//	│ ╭─ ◐ Running go test · 0:04 ───╮ │ staff,        │
//	│ │ ❯ composer                   │ │ changes,      │
//	│ ╰─ enter send · / commands ────╯ │ context       │
//	├──────────────────────────────────┴───────────────┤
//	│ status line: model · mode · ctx meter · cost      │
//	└───────────────────────────────────────────────────┘
//
// Every feature of the inline UI keeps working unchanged inside the
// transcript pane. On exit the alternate screen is left and the
// conversation is printed to the normal terminal, so it stays in the
// scrollback.
//
// Locking: UI code holding ui.mu may call into the fullscreen (which
// takes f.mu); the compositor never takes ui.mu, since a UI writer can be
// blocked on the pipe that the reader drains under f.mu.

const (
	sideW     = 36  // sidebar width, border included
	sideMinW  = 110 // narrower terminals hide the sidebar
	maxQueued = 3
)

type statusInfo struct {
	model, mode, box string
	tokens           int
	cost             float64
	ctxUsed, ctxMax  int
	extra            string // git branch, or the user's status_line output
}

type todoItem struct{ text, status string }

type fileChange struct {
	path     string
	add, del int
}

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
	info      func() statusInfo
	input     bool // the line editor is active: its line is the composer's
	dirty     bool
	truecolor bool
	lastFrame int
	maxEnd    int  // longest transcript drawn (the live area shrinks and grows it)
	hideSide  bool // Ctrl-T

	// The composer while a turn runs.
	busy    bool
	strip   string // what is happening, in the top border
	typing  string
	queued  []string
	todos   []todoItem
	changes []fileChange
	popup   []suggestion
	sel     int

	pager     *pager // Ctrl-O: a full-screen view of tool output
	suspended bool   // an external program owns the terminal
	mouse     bool   // wheel reporting on (native selection then needs Shift)

	done     chan struct{}
	readDone chan struct{}
	drawDone chan struct{}
	dropped  int // vterm lines trimmed, as last seen
	stdout   *os.File
	stderr   *os.File
	restore  func()
	closed   bool
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
func enterFullscreen(mouse bool) (*fullscreen, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	f := &fullscreen{tty: os.Stderr, pr: pr, pw: pw, stdout: os.Stdout, stderr: os.Stderr, office: newOffice(),
		truecolor: truecolorTerm(), done: make(chan struct{}), readDone: make(chan struct{}), drawDone: make(chan struct{}), lastFrame: -1}
	f.rows, f.cols = termRows(f.tty), termWidth(f.tty)
	f.vt = newVterm(f.transcriptWidth())
	// Keys are not echoed by the terminal (they would land in the middle
	// of the screen); Ctrl-C still interrupts.
	if restore, err := noEcho(os.Stdin); err == nil {
		f.restore = restore
	}
	// Alternate screen, hidden cursor, mouse wheel reporting (SGR).
	f.mouse = mouse
	// Bracketed paste stays on: the editor's own mode switches go into
	// the pipe, which the vterm ignores.
	f.tty.WriteString("\x1b[?1049h\x1b[?25l\x1b[?2004h" + f.mouseOn() + "\x1b[H\x1b[2J")
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
		defer close(f.drawDone)
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
				f.vt.width = f.transcriptWidth()
				f.prev = nil
				f.dirty = true
				f.mu.Unlock()
			case <-t.C:
				f.mu.Lock()
				frame := int(time.Since(f.office.start) / officeFrame)
				if f.dirty || (f.sidebar() && frame != f.lastFrame) {
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
	select { // no frame may land on the normal screen
	case <-f.drawDone:
	case <-time.After(time.Second):
	}
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
		line, _ := f.vt.renderW(i, f.vt.width)
		sb.WriteString(line + "\n")
	}
	f.mu.Unlock()
	f.tty.WriteString(sb.String())
}

func (f *fullscreen) mouseOn() string {
	if f.mouse {
		return "\x1b[?1000h\x1b[?1006h"
	}
	return ""
}

// sidebar reports whether the sidebar is shown; the caller holds f.mu.
func (f *fullscreen) sidebar() bool { return !f.hideSide && f.cols >= sideMinW }

// mainWidth is the width of the transcript and composer column.
func (f *fullscreen) mainWidth() int {
	if f.sidebar() {
		return f.cols - sideW
	}
	return f.cols
}

// transcriptWidth is the text width inside the transcript (1 column of
// margin on the left, 2 on the right).
func (f *fullscreen) transcriptWidth() int { return max(f.mainWidth()-3, 10) }

func (f *fullscreen) width() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vt.width
}

// height is the number of transcript rows.
func (f *fullscreen) height() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transcriptRows()
}

func (f *fullscreen) composerRows() int {
	n := 3 // borders and the input line
	if f.busy {
		n += min(len(f.queued), maxQueued)
	}
	return n
}

func (f *fullscreen) transcriptRows() int {
	return max(f.rows-1-f.composerRows(), 3)
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

// setBusy shows a running turn in the composer: what is happening, the
// message being typed and the queue.
func (f *fullscreen) setBusy(busy bool, strip, typing string, queued []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy == busy && f.strip == strip && f.typing == typing && strings.Join(f.queued, "\x00") == strings.Join(queued, "\x00") {
		return
	}
	f.busy, f.strip, f.typing = busy, strip, typing
	f.queued = append(f.queued[:0], queued...)
	f.dirty = true
}

// setPopup shows the editor's suggestions above the composer.
func (f *fullscreen) setPopup(items []suggestion, sel int) {
	f.mu.Lock()
	f.popup, f.sel = append([]suggestion(nil), items...), sel
	f.dirty = true
	f.mu.Unlock()
}

func (f *fullscreen) setTodos(items []todoItem) {
	f.mu.Lock()
	f.todos = items
	f.dirty = true
	f.mu.Unlock()
}

// addChange records an edit for the CHANGES section.
func (f *fullscreen) addChange(path string, add, del int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty = true
	for i := range f.changes {
		if f.changes[i].path == path {
			f.changes[i].add += add
			f.changes[i].del += del
			return
		}
	}
	f.changes = append(f.changes, fileChange{path, add, del})
}

func (f *fullscreen) toggleSidebar() {
	f.mu.Lock()
	f.hideSide = !f.hideSide
	f.vt.width = f.transcriptWidth()
	f.prev = nil
	f.dirty = true
	f.mu.Unlock()
}

func (f *fullscreen) scrollBy(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scroll = min(max(f.scroll+n, 0), max(f.vt.end()-f.transcriptRows(), 0))
	f.dirty = true
}

func (f *fullscreen) redraw() {
	f.mu.Lock()
	f.prev = nil
	f.dirty = true
	f.mu.Unlock()
}

func sgr(code string) string { return "\x1b[" + code + "m" }

// padTo pads a styled string to w columns (it must not be wider).
func padTo(s string, w int) string {
	return s + strings.Repeat(" ", max(w-strWidth(s), 0))
}

// suspend hands the terminal to another program (an editor).
func (f *fullscreen) suspend() {
	f.mu.Lock()
	f.suspended = true
	f.tty.WriteString("\x1b[?1000l\x1b[?1006l\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l")
	f.mu.Unlock()
}

func (f *fullscreen) resume() {
	f.mu.Lock()
	f.tty.WriteString("\x1b[?1049h\x1b[?25l\x1b[?2004h" + f.mouseOn() + "\x1b[H\x1b[2J")
	f.suspended, f.prev, f.dirty = false, nil, true
	f.mu.Unlock()
}

// draw composes the screen and writes the rows that changed; the caller
// holds f.mu.
func (f *fullscreen) draw() {
	if f.suspended || f.closed {
		return
	}
	if d := f.vt.dropped - f.dropped; d > 0 { // the scrollback was trimmed
		f.maxEnd = max(f.maxEnd-d, 0)
		f.dropped = f.vt.dropped
	}
	mainW := f.mainWidth()
	h := f.transcriptRows()
	var side []string
	if f.sidebar() {
		side = f.sidebarRows(f.rows - 1)
	}
	left := make([]string, 0, f.rows)

	end := f.vt.end()
	inputRow := -1
	if f.input {
		inputRow = f.vt.row
		if inputRow == end-1 {
			end-- // the prompt line is drawn in the composer
		}
	}
	if f.scroll > 0 && end > f.maxEnd {
		// Scrolled back: new output must not move what is being read.
		f.scroll += end - f.maxEnd
	}
	f.maxEnd = max(f.maxEnd, end)
	f.scroll = min(f.scroll, max(end-h, 0))
	// A short conversation starts at the top; a long one keeps its newest
	// lines in view.
	first := max(end-h-f.scroll, 0)
	for i := 0; i < h; i++ {
		row, w := "", 0
		if first+i < end {
			row, w = f.vt.renderW(first+i, mainW-3)
		}
		_ = w
		left = append(left, " "+row)
	}
	if len(f.popup) > 0 && f.input {
		// The suggestions float over the bottom of the transcript.
		rows := popupRows(f.popup, f.sel, mainW-6)
		boxW := 0
		for _, r := range rows {
			boxW = max(boxW, strWidth(r))
		}
		boxW = min(boxW+2, mainW-4)
		border := sgr(cGray)
		box := []string{border + "╭" + strings.Repeat("─", boxW) + "╮\x1b[0m"}
		for _, r := range rows {
			box = append(box, border+"│\x1b[0m"+padTo(r, boxW)+border+"│\x1b[0m")
		}
		for i, b := range box {
			at := len(left) - len(box) + i
			if at >= 0 {
				left[at] = " " + padTo(b, mainW-1)
			}
		}
	}
	if f.pager != nil {
		left = f.pager.rows(mainW, len(left))
	}
	left = append(left, f.composer(mainW, inputRow)...)

	screen := make([]string, 0, f.rows)
	for i, l := range left {
		if side != nil {
			s := ""
			if i < len(side) {
				s = side[i]
			}
			// Move to the panel's column rather than padding with spaces:
			// text copied from the conversation has no trailing blanks.
			l += "\x1b[0m\x1b[K\x1b[" + strconv.Itoa(mainW+1) + "G" + sgr(cGray) + "│" + "\x1b[0m" + s
		}
		screen = append(screen, l)
	}
	screen = append(screen, f.statusRow())

	var out strings.Builder
	// Synchronized output (DEC 2026): the terminal shows the frame whole,
	// never half drawn. Terminals without it ignore the codes.
	out.WriteString("\x1b[?2026h\x1b[?25l")
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
		// The cursor in the composer.
		row := h + 2
		out.WriteString("\x1b[" + strconv.Itoa(row) + ";" + strconv.Itoa(min(f.vt.col, mainW-5)+3) + "H\x1b[?25h")
	}
	out.WriteString("\x1b[?2026l")
	f.tty.WriteString(out.String())
}

// composer draws the input box: the editor's line when it is active, the
// type-ahead while a turn runs.
func (f *fullscreen) composer(w, inputRow int) []string {
	border := sgr(cGray)
	if f.input {
		border = sgr(cInk)
	} else if f.busy {
		border = sgr(cAccent)
	}
	inner := max(w-4, 1)
	top := border + "╭" + strings.Repeat("─", w-2) + "╮\x1b[0m"
	if f.busy && f.strip != "" {
		strip := truncate(f.strip, w-8)
		top = border + "╭─ \x1b[0m" + strip + " " + border + strings.Repeat("─", max(w-5-strWidth(strip), 0)) + "╮\x1b[0m"
	}
	var rows []string
	rows = append(rows, top)
	if f.busy {
		for i, q := range f.queued {
			if i == maxQueued {
				break
			}
			q = truncate(strings.ReplaceAll(q, "\n", "↵"), inner-12)
			rows = append(rows, border+"│\x1b[0m "+padTo(sgr(cDim)+"↳ "+q+"\x1b[0m", inner)+" "+border+"│\x1b[0m")
		}
	}
	var line string
	switch {
	case f.input:
		line, _ = f.vt.renderW(inputRow, inner)
	case f.busy && f.typing != "":
		t := strings.ReplaceAll(f.typing, "\n", "↵")
		for strWidth(t) > inner-4 {
			_, t = firstRune(t)
		}
		line = sgr(cInk) + "❯\x1b[0m " + t + sgr(cDim) + "▏\x1b[0m"
	case f.busy:
		line = sgr(cGray) + "❯ type to steer: enter sends at the next step · tab queues for after\x1b[0m"
	}
	rows = append(rows, border+"│\x1b[0m "+padTo(line, inner)+" "+border+"│\x1b[0m")
	hint := "enter send · ctrl+j new line · / commands · pgup scroll"
	if f.busy {
		hint = "enter steer · tab queue · ↑ take back · esc stop"
	}
	if f.cols >= sideMinW {
		hint += " · ctrl+t panel"
	}
	hint = truncate(hint, w-8)
	rows = append(rows, border+"╰─ \x1b[0m"+sgr(cGray)+hint+"\x1b[0m "+border+strings.Repeat("─", max(w-5-strWidth(hint), 0))+"╯\x1b[0m")
	return rows
}

func firstRune(s string) (string, string) {
	for i := range s {
		if i > 0 {
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// sectionTitle is a sidebar heading with an optional right-hand note.
func sectionTitle(title, note string) string {
	w := sideW - 3
	t := "\x1b[1m" + sgr(cGray) + title + "\x1b[0m"
	if note != "" {
		t += strings.Repeat(" ", max(w-strWidth(title)-strWidth(note), 1)) + note
	}
	return t
}

// sidebarRows draws the ops sidebar, h rows tall.
func (f *fullscreen) sidebarRows(h int) []string {
	w := sideW - 3 // "│ " … " "
	var rows []string
	add := func(s string) { rows = append(rows, " "+padTo(s, w)+" ") }
	add("")
	lead := f.office.leadState()
	add(sectionTitle("OFFICE", sgr(cAccent)+actWord(lead.act)+"\x1b[0m"))
	for _, r := range f.office.render(w, f.truecolor) {
		add(r)
	}

	if len(f.todos) > 0 {
		done := 0
		for _, t := range f.todos {
			if t.status == "done" {
				done++
			}
		}
		add("")
		add(sectionTitle("PLAN", sgr(cDim)+fmt.Sprintf("%d/%d ", done, len(f.todos))+"\x1b[0m"+meter(done, len(f.todos), 8, cGreen)))
		for _, t := range f.todos {
			switch t.status {
			case "done":
				add(sgr(cGreen) + "✓ " + "\x1b[0m" + sgr(cDim) + truncate(t.text, w-2) + "\x1b[0m")
			case "in_progress":
				add(sgr(cAccent) + "▸ " + "\x1b[0m\x1b[1m" + truncate(t.text, w-2) + "\x1b[0m")
			default:
				add(sgr(cGray) + "· " + "\x1b[0m" + truncate(t.text, w-2))
			}
		}
	}

	if len(f.changes) > 0 {
		add, del := 0, 0
		for _, c := range f.changes {
			add += c.add
			del += c.del
		}
		rows = append(rows, " "+padTo("", w)+" ")
		rows = append(rows, " "+padTo(sectionTitle("CHANGES", sgr(cGreen)+fmt.Sprintf("+%d", add)+"\x1b[0m "+sgr(cRed)+fmt.Sprintf("−%d", del)+"\x1b[0m"), w)+" ")
		for _, c := range f.changes {
			counts := sgr(cGreen) + fmt.Sprintf("+%d", c.add) + "\x1b[0m " + sgr(cRed) + fmt.Sprintf("−%d", c.del) + "\x1b[0m"
			name := truncate(c.path, w-strWidth(counts)-1)
			rows = append(rows, " "+padTo(name+strings.Repeat(" ", max(w-strWidth(name)-strWidth(counts), 1))+counts, w)+" ")
		}
	}

	// CONTEXT sits at the bottom.
	var foot []string
	if f.info != nil {
		in := f.info()
		if in.ctxMax > 0 {
			pct := min(in.ctxUsed*100/in.ctxMax, 100)
			color := cGreen
			switch {
			case pct >= 90:
				color = cRed
			case pct >= 70:
				color = cYellow
			}
			foot = append(foot, " "+padTo(sectionTitle("CONTEXT", sgr(cDim)+fmt.Sprintf("%s / %s", fmtK(in.ctxUsed), fmtK(in.ctxMax))+"\x1b[0m"), w)+" ")
			foot = append(foot, " "+padTo(meter(pct, 100, w-5, color)+fmt.Sprintf(" %3d%%", pct), w)+" ")
		}
	}
	for len(rows)+len(foot) < h {
		rows = append(rows, strings.Repeat(" ", sideW-1))
	}
	if len(rows)+len(foot) > h {
		rows = rows[:max(h-len(foot), 0)]
	}
	return append(rows, foot...)
}

// meter is a bar of n cells filled in proportion to v/total.
func meter(v, total, n int, color string) string {
	if total <= 0 || n <= 0 {
		return ""
	}
	full := min(v*n/total, n)
	if v > 0 && full == 0 {
		full = 1
	}
	return sgr(color) + strings.Repeat("▰", full) + "\x1b[0m" + sgr(cGray) + strings.Repeat("▱", n-full) + "\x1b[0m"
}

func (f *fullscreen) statusRow() string {
	var in statusInfo
	if f.info != nil {
		in = f.info()
	}
	var left string
	if f.scroll > 0 {
		left = " " + sgr(cYellow) + "↓ " + strconv.Itoa(f.scroll) + " more line" + plural(f.scroll) + "\x1b[0m" + sgr(cGray) + " · pgdn or wheel to return\x1b[0m"
	} else {
		modeColor := cGreen
		switch in.mode {
		case "plan":
			modeColor = cMagenta
		case "ask":
			modeColor = cYellow
		case "yolo":
			modeColor = cRed
		}
		left = " " + sgr(cAccent) + "◆ agentium\x1b[0m" + sgr(cGray) + " · \x1b[0m" + in.model + sgr(cGray) + " · \x1b[0m" +
			sgr(modeColor) + in.mode + "\x1b[0m" + sgr(cGray) + " · " + in.box + "\x1b[0m"
		if in.extra != "" {
			left += sgr(cGray) + " · \x1b[0m" + in.extra
		}
	}
	var right []string
	if in.ctxMax > 0 && !f.sidebar() {
		pct := min(in.ctxUsed*100/in.ctxMax, 100)
		right = append(right, "ctx "+meter(pct, 100, 6, cGreen)+sgr(cGray)+fmt.Sprintf(" %d%%", pct)+"\x1b[0m")
	}
	if in.tokens > 0 {
		right = append(right, sgr(cGray)+fmtK(in.tokens)+" tok\x1b[0m")
	}
	if in.cost > 0 {
		right = append(right, sgr(cGray)+fmt.Sprintf("$%.4f", in.cost)+"\x1b[0m")
	}
	r := strings.Join(right, sgr(cGray)+" · \x1b[0m") + " "
	if strWidth(left)+strWidth(r)+1 > f.cols {
		left = truncate(left, max(f.cols-strWidth(r)-2, 0))
	}
	return left + strings.Repeat(" ", max(f.cols-strWidth(left)-strWidth(r), 0)) + r
}

// scrollKey handles a scrolling key (PgUp/PgDn, mouse wheel) and reports
// whether k was one; other mouse events are swallowed too. Ctrl-T
// toggles the sidebar.
func scrollKey(k string) bool {
	f := activeFS()
	if f == nil {
		return false
	}
	f.mu.Lock()
	if f.pager != nil {
		if f.pager.key(k) {
			f.pager = nil
		}
		edit := ""
		if f.pager != nil {
			edit, f.pager.edit = f.pager.edit, ""
		}
		f.dirty = true
		f.mu.Unlock()
		if edit != "" {
			_, _ = externalEdit(edit) // to read, search or copy from
		}
		return true
	}
	f.mu.Unlock()
	switch {
	case k == "\x1b[5~":
		f.scrollBy(f.height() - 2)
	case k == "\x1b[6~":
		f.scrollBy(-(f.height() - 2))
	case k == "\x14": // Ctrl-T
		f.toggleSidebar()
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
