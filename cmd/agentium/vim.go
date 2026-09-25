package main

import (
	"strings"
	"unicode"
)

// Vim mode for the message box ("vim": true in the config, or /vim):
// Esc leaves insert mode; in normal mode the usual motions, operators
// and edits work, and Enter sends. Keys vim mode does not claim pass on
// to the editor as usual.

// vimKey handles k in vim mode. It returns the key for the editor to
// handle instead ("" when vim consumed it).
func (e *editor) vimKey(k string) string {
	if !e.vimNormal {
		// Esc then a key typed quickly arrives as one Alt+key sequence.
		if len(k) == 2 && k[0] == 0x1b && k[1] != '[' && k[1] != 'O' && len(e.sugg) == 0 && len(e.buf) > 0 {
			e.vimKey("\x1b")
			return e.vimKey(k[1:])
		}
		if k == "\x1b" && len(e.sugg) == 0 && len(e.buf) > 0 {
			e.vimNormal = true
			if e.pos > 0 && e.pos == len(e.buf) {
				e.pos-- // the cursor sits on the last character, as in vim
			}
			return ""
		}
		return k
	}
	before := editState{append([]rune(nil), e.buf...), e.pos}
	changed := func() {
		e.pushUndo(before)
		e.lastEdit = ""
	}
	// Operators wait for their motion: dw, cw, dd, cc, yy, d$ …
	if op := e.vimPending; op != "" {
		e.vimPending = ""
		if op == "r" {
			if r := []rune(k); len(r) == 1 && r[0] >= 0x20 && e.pos < e.lineEnd(e.pos) {
				e.buf[e.pos] = r[0]
				changed()
			}
			return ""
		}
		if op == "g" {
			if k == "g" {
				e.pos = 0
			}
			return ""
		}
		i, j := e.pos, e.pos
		switch k {
		case string(op[0]): // dd, cc, yy: the whole line
			i, j = e.lineStart(e.pos), e.lineEnd(e.pos)
			if op != "c" && j < len(e.buf) {
				j++
			} else if op != "c" && i > 0 {
				i--
			}
		case "w", "e":
			j = e.wordEnd(e.pos, k == "w" && op != "c")
			j = min(j, max(e.lineEnd(e.pos), e.pos+1)) // an operator stays on its line
		case "b":
			i = e.wordBack(e.pos)
		case "$":
			j = e.lineEnd(e.pos)
		case "0", "^":
			i = e.lineStart(e.pos)
		default:
			return "" // not a motion: the operator is dropped
		}
		linewise := k == string(op[0])
		if op == "y" {
			e.killed = e.capture(e.buf[i:j])
			e.killedLine = linewise
			return ""
		}
		e.cut(i, j)
		e.killedLine = linewise && op == "d"
		changed()
		if op == "c" {
			e.vimNormal = false
		}
		e.clampNormal()
		return ""
	}
	switch k {
	case "i":
		e.vimNormal = false
	case "a":
		e.vimNormal = false
		if e.pos < e.lineEnd(e.pos) {
			e.pos++
		}
	case "I":
		e.vimNormal, e.pos = false, e.lineStart(e.pos)
	case "A":
		e.vimNormal, e.pos = false, e.lineEnd(e.pos)
	case "o", "O":
		if k == "o" {
			e.pos = e.lineEnd(e.pos)
		} else {
			e.pos = e.lineStart(e.pos)
		}
		e.insert("\n")
		if k == "O" {
			e.pos--
		}
		changed()
		e.vimNormal = false
	case "h", "\x1b[D":
		if e.pos > e.lineStart(e.pos) {
			e.pos--
		}
	case "l", " ", "\x1b[C":
		if e.pos+1 < e.lineEnd(e.pos) {
			e.pos++
		}
	case "j":
		return "\x1b[B" // a line down, or newer history
	case "k":
		return "\x1b[A"
	case "0":
		e.pos = e.lineStart(e.pos)
	case "^":
		e.pos = e.lineStart(e.pos)
		for e.pos < len(e.buf) && (e.buf[e.pos] == ' ' || e.buf[e.pos] == '\t') {
			e.pos++
		}
	case "$":
		e.pos = max(e.lineEnd(e.pos)-1, e.lineStart(e.pos))
	case "w":
		e.pos = e.wordEnd(e.pos, true)
	case "e":
		p := e.pos + 1
		for p < len(e.buf) && unicode.IsSpace(e.buf[p]) {
			p++
		}
		if p < len(e.buf) {
			e.pos = max(e.wordEnd(p, false)-1, e.pos)
		}
	case "b":
		e.pos = e.wordBack(e.pos)
	case "G":
		e.pos = len(e.buf)
	case "g", "d", "c", "y", "r":
		e.vimPending = k
		return ""
	case "x":
		if e.pos < e.lineEnd(e.pos) {
			e.cut(e.pos, e.pos+1)
			changed()
		}
	case "X":
		if e.pos > e.lineStart(e.pos) {
			e.cut(e.pos-1, e.pos)
			changed()
		}
	case "D":
		e.cut(e.pos, e.lineEnd(e.pos))
		changed()
	case "C":
		e.cut(e.pos, e.lineEnd(e.pos))
		changed()
		e.vimNormal = false
	case "s":
		if e.pos < e.lineEnd(e.pos) {
			e.cut(e.pos, e.pos+1)
			changed()
		}
		e.vimNormal = false
	case "S":
		e.cut(e.lineStart(e.pos), e.lineEnd(e.pos))
		changed()
		e.vimNormal = false
	case "p", "P":
		if len(e.killed.buf) > 0 {
			text := string(e.restore(e.killed))
			if e.killedLine {
				// A whole line goes below (p) or above (P) this one.
				text = strings.TrimSuffix(strings.TrimPrefix(text, "\n"), "\n")
				if k == "p" {
					e.pos = e.lineEnd(e.pos)
					e.insert("\n" + text)
					e.pos -= len([]rune(text))
				} else {
					e.pos = e.lineStart(e.pos)
					e.insert(text + "\n")
					e.pos = e.lineStart(e.pos - 1)
				}
			} else {
				if k == "p" && e.pos < e.lineEnd(e.pos) {
					e.pos++
				}
				e.insert(text)
				e.pos--
			}
			changed()
		}
	case "~":
		if e.pos < e.lineEnd(e.pos) {
			r := e.buf[e.pos]
			if unicode.IsUpper(r) {
				r = unicode.ToLower(r)
			} else {
				r = unicode.ToUpper(r)
			}
			e.buf[e.pos] = r
			e.pos++
			changed()
		}
	case "u":
		return "\x1f" // the editor's undo
	case "\x1b":
		return k // Esc Esc still rewinds, and closes popups
	case "?":
		if len(e.buf) == 0 {
			return k // help
		}
	case "\r", "\x03", "\x04", "\x12", "\x07", "\x0f", "\x0c", "\x1b[Z", "\x1b[A", "\x1b[B":
		return k // send, quit, search, editor, viewer … work as usual
	default:
		if strings.HasPrefix(k, "\x1b") {
			return k
		}
		// Other text is not typed in normal mode.
	}
	e.clampNormal()
	return ""
}

// clampNormal keeps the cursor on a character in normal mode.
func (e *editor) clampNormal() {
	if e.vimNormal && e.pos > 0 && e.pos >= e.lineEnd(e.pos) && e.pos > e.lineStart(e.pos) {
		e.pos = e.lineEnd(e.pos) - 1
	}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// wordEnd is where the word at p ends; with skipSpace, where the next
// word starts (vim's w).
func (e *editor) wordEnd(p int, skipSpace bool) int {
	n := len(e.buf)
	if p >= n {
		return n
	}
	if isWordRune(e.buf[p]) {
		for p < n && isWordRune(e.buf[p]) {
			p++
		}
	} else if !unicode.IsSpace(e.buf[p]) {
		for p < n && !isWordRune(e.buf[p]) && !unicode.IsSpace(e.buf[p]) {
			p++
		}
	}
	if skipSpace {
		for p < n && unicode.IsSpace(e.buf[p]) {
			p++
		}
	}
	return p
}

// wordBack is the start of the word before p (vim's b).
func (e *editor) wordBack(p int) int {
	for p > 0 && unicode.IsSpace(e.buf[p-1]) {
		p--
	}
	if p > 0 && isWordRune(e.buf[p-1]) {
		for p > 0 && isWordRune(e.buf[p-1]) {
			p--
		}
	} else {
		for p > 0 && !isWordRune(e.buf[p-1]) && !unicode.IsSpace(e.buf[p-1]) {
			p--
		}
	}
	return p
}
