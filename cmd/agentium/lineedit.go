package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tegarthegreat/agentium/internal/config"
)

// errInterrupt is returned by the editor on Ctrl-C with an empty line.
var errInterrupt = errors.New("interrupt")

// history is the persistent input history (~/.agentium/history, one JSON
// string per line so multi-line inputs survive).
type history struct {
	items []string
	path  string
}

const historyMax = 1000

func loadHistory() *history {
	h := &history{path: filepath.Join(config.Home(), "history")}
	b, err := os.ReadFile(h.path)
	if err != nil {
		return h
	}
	for _, l := range strings.Split(string(b), "\n") {
		var s string
		if json.Unmarshal([]byte(l), &s) == nil && s != "" {
			h.items = append(h.items, s)
		}
	}
	if len(h.items) > historyMax {
		h.items = h.items[len(h.items)-historyMax:]
	}
	return h
}

func (h *history) add(s string) {
	if strings.TrimSpace(s) == "" || (len(h.items) > 0 && h.items[len(h.items)-1] == s) {
		return
	}
	h.items = append(h.items, s)
	_ = os.MkdirAll(filepath.Dir(h.path), 0o700)
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(s)
	f.Write(append(b, '\n'))
}

// editor is a small line editor: cursor movement, word/line kills,
// history and its search (Ctrl-R), bracketed paste (a large paste
// becomes a chip), newlines (Ctrl-J, Alt-Enter, Shift-Enter, a trailing
// backslash), and a suggestion popup for /commands and @files. Long
// input scrolls horizontally so redraws stay cheap and exact.
type editor struct {
	in     *os.File
	out    *os.File
	hist   *history
	buf    []rune
	pos    int
	prompt string
	draft  []rune // text to start with (typed during the last turn)
	// echo, if set, renders a submitted input for the transcript.
	echo func(line string) string
	// placeholder is shown dimmed while the input is empty.
	placeholder string
	// hook sees each key first and reports whether it handled it
	// (mode switching, shortcuts); it may change the prompt.
	hook func(e *editor, k string) bool
	// complete returns suggestions for the text before the cursor, and
	// where the completed word starts.
	complete func(before []rune) (items []suggestion, start int)
	// autoSubmit, set by hook, submits the buffer at once.
	autoSubmit bool

	pastes     []string // pasted blocks shown as chips
	sugg       []suggestion
	suggStart  int
	sel        int
	dismissed  string // the text the popup was closed for
	drawnBelow int    // popup rows drawn under the line (inline UI)
	searching  bool
	query      []rune
	found      int // history index of the search match, -1 for none

	// ghost, if set, returns a suggested message, shown dimmed while the
	// input is empty; tab takes it. It may change while the editor waits.
	ghost func() string

	vim        bool   // vim mode ("vim": true, /vim)
	vimNormal  bool   // in vim's normal mode (else insert)
	vimPending string // an operator waiting for its motion (d, c, y, g, r)
	killedLine bool   // the cut buffer holds whole lines (dd, yy)

	killed   clip        // the last text cut with ctrl+k/u/w (ctrl+y puts it back)
	stash    clip        // a draft put aside with ctrl+s
	undo     []editState // ctrl+_ steps back through these
	lastEdit string      // "type" while plain typing continues (one undo step)
}

// clip is text taken out of the input, with the pastes its chips stand
// for (pastes are numbered per message, so they travel with the text).
type clip struct {
	buf    []rune
	pastes []string
}

func (e *editor) capture(rs []rune) clip {
	return clip{append([]rune(nil), rs...), append([]string(nil), e.pastes...)}
}

// restore returns c's text with its chips renumbered into this input.
func (e *editor) restore(c clip) []rune {
	off := len(e.pastes)
	e.pastes = append(e.pastes, c.pastes...)
	out := make([]rune, len(c.buf))
	for i, r := range c.buf {
		if isChip(r) {
			r += rune(off)
		}
		out[i] = r
	}
	return out
}

// editState is the text and cursor, for undo.
type editState struct {
	buf []rune
	pos int
}

// suggestion is one entry of the completion popup.
type suggestion struct {
	insert, label, hint string
	run                 bool // Enter runs it at once (a command without arguments)
}

// Pasted blocks live in the buffer as one private-use rune each.
const chipBase = 0xF0000

func isChip(r rune) bool { return r >= chipBase && r < chipBase+0x10000 }

func (e *editor) chipText(r rune) string {
	i := int(r - chipBase)
	if i < 0 || i >= len(e.pastes) {
		return "[paste]"
	}
	n := strings.Count(strings.TrimRight(e.pastes[i], "\n"), "\n") + 1
	return fmt.Sprintf("[Pasted text #%d +%d lines]", i+1, n)
}

// text is the input with chips expanded.
func (e *editor) text() string {
	var sb strings.Builder
	for _, r := range e.buf {
		if isChip(r) {
			if i := int(r - chipBase); i < len(e.pastes) {
				sb.WriteString(e.pastes[i])
			}
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// shown is the input as displayed: chips as labels, newlines as ↵.
func (e *editor) shown() string {
	var sb strings.Builder
	for _, r := range e.buf {
		switch {
		case isChip(r):
			sb.WriteString(e.chipText(r))
		case r == '\n':
			sb.WriteString("\n  ")
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// key reads one input event (a rune or an escape sequence).
func (e *editor) key() (string, error) {
	var b [1]byte
	if _, err := e.in.Read(b[:]); err != nil {
		return "", err
	}
	if b[0] != 0x1b {
		if b[0] < 0x80 {
			return string(b[:]), nil
		}
		// Multi-byte UTF-8 rune.
		buf := []byte{b[0]}
		for !utf8.FullRune(buf) && len(buf) < 4 {
			if _, err := e.in.Read(b[:]); err != nil {
				return "", err
			}
			buf = append(buf, b[0])
		}
		return string(buf), nil
	}
	seq := []byte{0x1b}
	if !inputReady(e.in, 40*time.Millisecond) {
		return "\x1b", nil // Esc on its own
	}
	for i := 0; i < 32; i++ { // SGR mouse reports run long
		if _, err := e.in.Read(b[:]); err != nil {
			return string(seq), nil
		}
		seq = append(seq, b[0])
		if len(seq) == 2 && b[0] != '[' && b[0] != 'O' {
			break // Alt+key
		}
		if len(seq) > 2 && (b[0] >= 0x40 && b[0] <= 0x7e) {
			break
		}
	}
	return string(seq), nil
}

func (e *editor) render(width int) {
	// Each buffer rune is drawn as a segment: chips as their label.
	segs := make([]string, len(e.buf))
	for i, r := range e.buf {
		switch {
		case isChip(r):
			segs[i] = "\x1b[7m" + e.chipText(r) + "\x1b[27m"
		case r == '\n':
			segs[i] = "↵"
		case r == '\t':
			segs[i] = " " // one column, so cursor math stays exact
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			segs[i] = "·"
		default:
			segs[i] = string(r)
		}
	}
	prompt := e.prompt
	if e.vim && e.vimNormal {
		prompt = "\x1b[2m[N]\x1b[0m " + prompt // vim's normal mode
	}
	if e.searching {
		q := string(e.query)
		prompt = "\x1b[2m(history search)\x1b[0m " + q + " \x1b[2m›\x1b[0m "
		segs = nil
		if e.found >= 0 {
			for _, r := range []rune(strings.ReplaceAll(e.hist.items[e.found], "\n", "↵")) {
				segs = append(segs, string(r))
			}
		}
	}
	promptW := strWidth(prompt)
	avail := max(width-promptW-1, 10)
	var sb strings.Builder
	if ph := e.placeholder; !e.searching && len(e.buf) == 0 && (ph != "" || len(e.stash.buf) > 0 || e.ghostText() != "") {
		if len(e.stash.buf) > 0 {
			ph = "draft put aside · ctrl+s brings it back"
		} else if g := e.ghostText(); g != "" {
			ph = g + "   ⇥ tab"
		}
		sb.WriteString("\r" + prompt + "\x1b[2m" + truncate(ph, avail) + "\x1b[0m\x1b[K\r")
		if promptW > 0 {
			sb.WriteString("\x1b[" + itoa(promptW) + "C")
		}
		e.out.WriteString(sb.String())
		e.drawPopup(width, promptW)
		return
	}
	pos := e.pos
	if e.searching {
		pos = len(segs)
	}
	// Scroll by terminal columns, not runes: CJK and emoji take two.
	start, used := pos, 0
	for start > 0 && used+strWidth(segs[start-1]) <= avail {
		start--
		used += strWidth(segs[start])
	}
	end, cols := start, 0
	for end < len(segs) && cols+strWidth(segs[end]) <= avail {
		cols += strWidth(segs[end])
		end++
	}
	cursor := promptW
	for _, sg := range segs[start:pos] {
		cursor += strWidth(sg)
	}
	sb.WriteString("\r" + prompt + strings.Join(segs[start:end], "") + "\x1b[K")
	sb.WriteString("\r")
	if cursor > 0 {
		sb.WriteString("\x1b[" + itoa(cursor) + "C")
	}
	e.out.WriteString(sb.String())
	e.drawPopup(width, cursor)
}

// suggest recomputes the popup for the text before the cursor.
func (e *editor) suggest() {
	e.sugg, e.sel = nil, 0
	if e.complete == nil || e.searching {
		return
	}
	before := string(e.buf[:e.pos])
	if before == e.dismissed {
		return
	}
	e.dismissed = ""
	e.sugg, e.suggStart = e.complete(e.buf[:e.pos])
}

// accept puts the selected suggestion into the input.
func (e *editor) accept() suggestion {
	s := e.sugg[e.sel]
	e.suggStart = min(e.suggStart, e.pos)
	rest := append([]rune(nil), e.buf[e.pos:]...)
	e.buf = append(append(e.buf[:e.suggStart], []rune(s.insert)...), rest...)
	e.pos = e.suggStart + len([]rune(s.insert))
	e.sugg = nil
	e.dismissed = string(e.buf[:e.pos])
	return s
}

// drawPopup shows the suggestions: in the full-screen composer, or under
// the input line, returning the cursor to column col of the input line.
// (Relative moves, not save/restore: drawing rows at the bottom of the
// screen scrolls it.)
func (e *editor) drawPopup(width, col int) {
	if f := activeFS(); f != nil {
		f.setPopup(e.sugg, e.sel)
		return
	}
	rows := popupRows(e.sugg, e.sel, width)
	if len(rows) == 0 && e.drawnBelow == 0 {
		return
	}
	var sb strings.Builder
	for _, r := range rows {
		sb.WriteString("\r\n\x1b[2K" + r)
	}
	for i := len(rows); i < e.drawnBelow; i++ {
		sb.WriteString("\r\n\x1b[2K")
	}
	if n := max(len(rows), e.drawnBelow); n > 0 {
		sb.WriteString("\x1b[" + itoa(n) + "A")
	}
	sb.WriteString("\r")
	if col > 0 {
		sb.WriteString("\x1b[" + itoa(col) + "C")
	}
	e.drawnBelow = len(rows)
	e.out.WriteString(sb.String())
}

// popupRows renders up to 8 suggestions around the selected one.
func popupRows(items []suggestion, sel, width int) []string {
	const show = 8
	first := 0
	if sel >= show {
		first = sel - show + 1
	}
	labelW := 0
	for _, it := range items {
		labelW = max(labelW, strWidth(it.label))
	}
	labelW = min(labelW, width/2)
	var rows []string
	for i := first; i < len(items) && i < first+show; i++ {
		it := items[i]
		label := truncate(sanitize(it.label), labelW)
		it.hint = sanitize(it.hint)
		row := "  " + label + strings.Repeat(" ", labelW-strWidth(label)) + "  " + "\x1b[2m" + truncate(it.hint, width-labelW-6) + "\x1b[0m"
		if i == sel {
			row = "\x1b[" + cAccent + "m❯ \x1b[0m\x1b[1m" + label + "\x1b[0m" + strings.Repeat(" ", labelW-strWidth(label)) + "  " + "\x1b[2m" + truncate(it.hint, width-labelW-6) + "\x1b[0m"
		}
		rows = append(rows, row)
	}
	if len(items) > first+show {
		rows = append(rows, fmt.Sprintf("  \x1b[2m… %d more\x1b[0m", len(items)-first-show))
	}
	return rows
}

// strWidth is the display width of s; ANSI escape sequences count zero.
func strWidth(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r >= 0x40 && r <= 0x7e && r != '[' {
				esc = false
			}
		case r == 0x1b:
			esc = true
		default:
			n += runeWidth(r)
		}
	}
	return n
}

// runeWidth is the number of terminal columns r occupies: 0 for
// combining marks, 2 for East Asian wide/fullwidth characters and most
// emoji, else 1.
func runeWidth(r rune) int {
	switch {
	case r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200B || r == 0x200D || (r >= 0xFE00 && r <= 0xFE0F):
		return 0
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF, r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1F64F,
		r >= 0x1F900 && r <= 0x1F9FF, r >= 0x1F680 && r <= 0x1F6FF, r >= 0x20000 && r <= 0x3FFFD,
		r >= 0x1F000 && r <= 0x1F2FF, r >= 0x1FA70 && r <= 0x1FAFF:
		return 2
	}
	if emojiWide[r] {
		return 2
	}
	return 1
}

// emojiWide lists symbols below U+1F000 that terminals draw two columns
// wide (Unicode East_Asian_Width=W: ⌚ ☕ ⚡ ✅ ❌ ⭐ …).
var emojiWide = func() map[rune]bool {
	m := map[rune]bool{}
	for _, rg := range [][2]rune{{0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE},
		{0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1}, {0x26AA, 0x26AB},
		{0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3},
		{0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728},
		{0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0},
		{0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}} {
		for r := rg[0]; r <= rg[1]; r++ {
			m[r] = true
		}
	}
	return m
}()

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (e *editor) insert(s string) {
	rs := []rune(s)
	e.buf = append(e.buf[:e.pos], append(rs, e.buf[e.pos:]...)...)
	e.pos += len(rs)
}

// readLine edits one input. It returns io.EOF on Ctrl-D with an empty
// line and errInterrupt on Ctrl-C with an empty line.
func (e *editor) readLine() (string, error) {
	restore, err := makeRaw(e.in)
	if err != nil {
		return "", err
	}
	defer restore()
	e.out.WriteString("\x1b[?2004h") // bracketed paste on
	defer e.out.WriteString("\x1b[?2004l")
	if f := activeFS(); f != nil {
		f.setInput(true)
		defer f.setInput(false)
		defer f.setPopup(nil, 0)
	}
	e.buf, e.pos = e.draft, len(e.draft)
	e.draft, e.pastes, e.sugg, e.dismissed, e.searching = nil, nil, nil, "", false
	e.undo, e.lastEdit = nil, ""
	e.vimNormal, e.vimPending = false, "" // each message starts in insert mode
	hi := len(e.hist.items)
	var draft []rune
	width := termWidth(e.out)
	e.render(width)
	pasting, lastCR := false, false
	var paste strings.Builder
	for {
		if e.ghost != nil && !pasting {
			// Redraw when a suggestion arrives while waiting for a key.
			shown := e.ghost()
			for !inputReady(e.in, 250*time.Millisecond) {
				if g := e.ghost(); g != shown {
					shown = g
					if len(e.buf) == 0 && !e.searching {
						e.render(termWidth(e.out))
					}
				}
			}
		}
		k, err := e.key()
		if err != nil {
			return "", err
		}
		width = termWidth(e.out) // the panel or the window may have changed
		if pasting {
			wasCR := lastCR
			lastCR = k == "\r"
			switch k {
			case "\x1b[201~":
				pasting = false
				e.insertPaste(paste.String())
				paste.Reset()
				e.suggest()
			case "\r":
				paste.WriteByte('\n')
			case "\n":
				if !(wasCR && strings.HasSuffix(paste.String(), "\n")) { // CRLF: one newline
					paste.WriteByte('\n')
				}
			default:
				if k[0] >= 0x20 || k == "\t" {
					paste.WriteString(k)
				}
			}
			if !pasting {
				e.render(width)
			}
			continue
		}
		if scrollKey(k) {
			continue
		}
		if e.searching {
			e.searchKey(k)
			e.render(width)
			continue
		}
		if e.vim {
			if k = e.vimKey(k); k == "" {
				e.suggest()
				e.render(width)
				continue
			}
		}
		if e.hook != nil && e.hook(e, k) {
			e.pos = min(e.pos, len(e.buf))
			if !e.autoSubmit {
				e.suggest() // the hook may have replaced the text
				width = termWidth(e.out)
				e.render(width)
				continue
			}
			e.autoSubmit, e.sugg, k = false, nil, "\r"
		}
		if len(e.sugg) > 0 {
			switch k {
			case "\x1b[A", "\x10":
				e.sel = (e.sel + len(e.sugg) - 1) % len(e.sugg)
				e.render(width)
				continue
			case "\x1b[B", "\x0e":
				e.sel = (e.sel + 1) % len(e.sugg)
				e.render(width)
				continue
			case "\t":
				e.accept()
				e.suggest()
				e.render(width)
				continue
			case "\r":
				if s := e.accept(); !s.run {
					e.suggest()
					e.render(width)
					continue
				}
				// A command without arguments runs at once.
			case "\x1b":
				e.dismissed, e.sugg = string(e.buf[:e.pos]), nil
				e.render(width)
				continue
			}
		}
		before := editState{append([]rune(nil), e.buf...), e.pos}
		switch k {
		case "\x1b[200~":
			e.pushUndo(before)
			e.lastEdit = ""
			pasting = true
			continue
		case "\r":
			if e.pos > 0 && e.pos == len(e.buf) && e.buf[e.pos-1] == '\\' {
				e.buf = e.buf[:e.pos-1]
				e.pos--
				e.insert("\n")
				break
			}
			line := e.text()
			e.sugg = nil
			e.drawPopup(width, 0)
			if e.echo != nil {
				e.out.WriteString("\r\x1b[K" + e.echo(line))
			} else {
				// Replace the scrolled one-row view with the whole input.
				e.out.WriteString("\r\x1b[K" + e.prompt + strings.ReplaceAll(e.shown(), "\n", "\r\n") + "\r\n")
			}
			e.hist.add(line)
			return line, nil
		case "\n", "\x1b\r", "\x1b[13;2u", "\x1b[27;2;13~": // Ctrl-J, Alt-Enter, Shift-Enter
			e.insert("\n")
		case "\x03": // Ctrl-C
			if len(e.buf) == 0 {
				e.sugg = nil
				e.drawPopup(width, 0)
				e.out.WriteString("\r\n")
				return "", errInterrupt
			}
			e.buf, e.pos = nil, 0
		case "\x04": // Ctrl-D
			if len(e.buf) == 0 {
				e.sugg = nil
				e.drawPopup(width, 0)
				e.out.WriteString("\r\n")
				return "", errEOF
			}
			if e.pos < len(e.buf) {
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
			}
		case "\x7f", "\x08": // Backspace
			if e.pos > 0 {
				e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
				e.pos--
			}
		case "\x1b[3~": // Delete
			if e.pos < len(e.buf) {
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
			}
		case "\x01", "\x1b[H", "\x1bOH", "\x1b[1~": // Home: start of the line
			e.pos = e.lineStart(e.pos)
		case "\x05", "\x1b[F", "\x1bOF", "\x1b[4~": // End: end of the line
			e.pos = e.lineEnd(e.pos)
		case "\x1b[1;5H", "\x1b<": // ctrl+home, alt+<: start of the text
			e.pos = 0
		case "\x1b[1;5F", "\x1b>": // ctrl+end, alt+>: end of the text
			e.pos = len(e.buf)
		case "\x1b[D", "\x02":
			if e.pos > 0 {
				e.pos--
			}
		case "\x1b[C", "\x06":
			if e.pos < len(e.buf) {
				e.pos++
			}
		case "\x1b[1;5D", "\x1bb": // word left
			for e.pos > 0 && e.buf[e.pos-1] == ' ' {
				e.pos--
			}
			for e.pos > 0 && e.buf[e.pos-1] != ' ' {
				e.pos--
			}
		case "\x1b[1;5C", "\x1bf": // word right
			for e.pos < len(e.buf) && e.buf[e.pos] == ' ' {
				e.pos++
			}
			for e.pos < len(e.buf) && e.buf[e.pos] != ' ' {
				e.pos++
			}
		case "\x15": // Ctrl-U: cut to the start of the line
			i := e.lineStart(e.pos)
			if i == e.pos && i > 0 {
				i-- // at a line's start: join it to the previous one
			}
			e.cut(i, e.pos)
		case "\x0b": // Ctrl-K: cut to the end of the line
			j := e.lineEnd(e.pos)
			if j == e.pos && j < len(e.buf) {
				j++
			}
			e.cut(e.pos, j)
		case "\x17", "\x1b\x7f", "\x1b\x08": // Ctrl-W, Alt-Backspace: cut the word before
			i := e.pos
			for i > 0 && (e.buf[i-1] == ' ' || e.buf[i-1] == '\n') {
				i--
			}
			for i > 0 && e.buf[i-1] != ' ' && e.buf[i-1] != '\n' {
				i--
			}
			e.cut(i, e.pos)
		case "\x1bd": // Alt-D: cut the word after
			j := e.pos
			for j < len(e.buf) && (e.buf[j] == ' ' || e.buf[j] == '\n') {
				j++
			}
			for j < len(e.buf) && e.buf[j] != ' ' && e.buf[j] != '\n' {
				j++
			}
			e.cut(e.pos, j)
		case "\x19": // Ctrl-Y: put back what was cut
			if len(e.killed.buf) > 0 {
				e.insert(string(e.restore(e.killed)))
			}
		case "\x1f", "\x1b[45;5u": // Ctrl-_ (ctrl+/ on most terminals): undo
			if n := len(e.undo); n > 0 {
				st := e.undo[n-1]
				e.undo = e.undo[:n-1]
				e.buf, e.pos = st.buf, min(st.pos, len(st.buf))
			}
			e.lastEdit = ""
		case "\x13": // Ctrl-S: put the draft aside, or bring it back
			// With both a draft and one put aside, they swap.
			old := e.stash
			e.stash, e.buf, e.pos = clip{}, nil, 0
			if len(before.buf) > 0 {
				e.stash = e.capture(before.buf)
			}
			if len(old.buf) > 0 {
				e.buf = e.restore(old)
				e.pos = len(e.buf)
			}
		case "\x0c": // Ctrl-L
			if f := activeFS(); f != nil {
				f.redraw()
			} else {
				e.out.WriteString("\x1b[H\x1b[2J")
			}
		case "\x12": // Ctrl-R
			e.searching, e.query, e.found = true, nil, -1
			e.sugg = nil
		case "\x1b[A", "\x10": // Up: the line above, else older history
			if ls := e.lineStart(e.pos); ls > 0 {
				col := e.pos - ls
				prev := e.lineStart(ls - 1)
				e.pos = min(prev+col, ls-1)
			} else if hi > 0 {
				if hi == len(e.hist.items) {
					draft = append([]rune(nil), e.buf...)
				}
				hi--
				e.buf = []rune(e.hist.items[hi])
				e.pos = len(e.buf)
				e.dismissed = string(e.buf)
			}
		case "\x1b[B", "\x0e": // Down: the line below, else newer history
			if le := e.lineEnd(e.pos); le < len(e.buf) {
				col := e.pos - e.lineStart(e.pos)
				e.pos = min(le+1+col, e.lineEnd(le+1))
			} else if hi < len(e.hist.items) {
				hi++
				if hi == len(e.hist.items) {
					e.buf = draft
				} else {
					e.buf = []rune(e.hist.items[hi])
				}
				e.pos = len(e.buf)
				e.dismissed = string(e.buf)
			}
		default:
			if k != "" && k[0] >= 0x20 && k[0] != 0x7f && k[0] != 0x1b {
				e.insert(k)
			} else if k == "\t" {
				if g := e.ghostText(); len(e.buf) == 0 && g != "" {
					e.insert(g) // take the suggestion
				} else {
					e.insert("  ")
				}
			}
		}
		if string(before.buf) == string(e.buf) && before.pos != e.pos {
			e.lastEdit = "" // typing after moving the cursor is a new undo step
		}
		if k != "\x1f" && k != "\x1b[45;5u" && string(before.buf) != string(e.buf) {
			// Plain typing is one undo step until a space or another edit.
			kind := "edit"
			if len([]rune(k)) == 1 && k != " " && k != "\n" && k[0] >= 0x20 {
				kind = "type"
			}
			if kind != "type" || e.lastEdit != "type" {
				e.pushUndo(before)
			}
			e.lastEdit = kind
		}
		if e.vim {
			e.clampNormal() // history, search or undo may leave the cursor past the end
		}
		e.suggest()
		width = termWidth(e.out)
		e.render(width)
	}
}

func (e *editor) ghostText() string {
	if e.ghost == nil {
		return ""
	}
	return e.ghost()
}

func (e *editor) pushUndo(st editState) {
	if len(e.undo) >= 200 {
		e.undo = e.undo[1:]
	}
	e.undo = append(e.undo, st)
}

// cut removes buf[i:j] into the kill buffer.
func (e *editor) cut(i, j int) {
	if i >= j {
		return
	}
	e.killed, e.killedLine = e.capture(e.buf[i:j]), false
	e.buf = append(e.buf[:i], e.buf[j:]...)
	e.pos = i
}

// lineStart and lineEnd bound the input line holding position p.
func (e *editor) lineStart(p int) int {
	for p > 0 && e.buf[p-1] != '\n' {
		p--
	}
	return p
}

func (e *editor) lineEnd(p int) int {
	for p < len(e.buf) && e.buf[p] != '\n' {
		p++
	}
	return p
}

// insertPaste inserts pasted text; a large paste becomes a chip.
func (e *editor) insertPaste(text string) {
	if len(text) > 800 || strings.Count(strings.TrimRight(text, "\n"), "\n") >= 3 {
		e.pastes = append(e.pastes, text)
		e.insert(string(rune(chipBase + len(e.pastes) - 1)))
		return
	}
	e.insert(text)
}

// searchKey handles a key during a history search.
func (e *editor) searchKey(k string) {
	find := func(from int) {
		q := strings.ToLower(string(e.query))
		for i := from; i >= 0; i-- {
			if strings.Contains(strings.ToLower(e.hist.items[i]), q) {
				e.found = i
				return
			}
		}
	}
	switch k {
	case "\x12": // Ctrl-R again: an older match
		if e.found > 0 {
			find(e.found - 1)
		}
	case "\r", "\t", "\x1b[C", "\x1b[D":
		if e.found >= 0 {
			e.buf = []rune(e.hist.items[e.found])
			e.pos = len(e.buf)
			e.dismissed = string(e.buf)
		}
		e.searching = false
	case "\x1b", "\x03", "\x07":
		e.searching = false
	case "\x7f", "\x08":
		if len(e.query) > 0 {
			e.query = e.query[:len(e.query)-1]
			e.found = -1
			find(len(e.hist.items) - 1)
		}
	default:
		if k != "" && k[0] >= 0x20 && k[0] != 0x7f && k[0] != 0x1b {
			e.query = append(e.query, []rune(k)...)
			e.found = -1
			find(len(e.hist.items) - 1)
		}
	}
}
