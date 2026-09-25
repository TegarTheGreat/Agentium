package main

import (
	"io"
	"strings"
	"unicode/utf8"
)

// wrapWriter word-wraps text for a terminal: a word that would cross the
// right edge moves to the next line instead of being split, and wrapped
// lines of a list item or quote keep its indent. ANSI escape sequences
// take no width. With raw set (code blocks) text passes through as is.
type wrapWriter struct {
	w     io.Writer
	width func() int
	raw   bool

	col     int    // visible column of the cursor
	word    []byte // pending word, escapes included
	wordW   int
	spaces  int // pending spaces before the word
	hang    int // indent for wrapped lines of this line
	started bool
	head    []byte // the line's first word, to detect bullets
	esc     bool

	// Margin: every line starts with indent spaces, or once with lead (a
	// marker as wide as indent) instead.
	indent   int
	lead     string
	margined bool
}

// setMargin indents the lines that follow; lead, if set, replaces the
// indent of the next line.
func (ww *wrapWriter) setMargin(indent int, lead string) {
	ww.indent, ww.lead = indent, lead
}

func newWrap(w io.Writer, width func() int) *wrapWriter {
	return &wrapWriter{w: w, width: width}
}

func (ww *wrapWriter) Write(p []byte) (int, error) {
	var out []byte
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRune(p[i:])
		b := p[i : i+size]
		i += size
		if !ww.margined && r != '\n' && ww.col == 0 && (ww.indent > 0 || ww.lead != "") {
			ww.margined = true
			if ww.lead != "" {
				out = append(out, ww.lead...)
				ww.lead = ""
			} else {
				out = append(out, strings.Repeat(" ", ww.indent)...)
			}
			ww.col = ww.indent
		}
		switch {
		case ww.esc:
			ww.word = append(ww.word, b...)
			if r >= 0x40 && r <= 0x7e && r != '[' {
				ww.esc = false
			}
		case r == 0x1b:
			ww.esc = true
			ww.word = append(ww.word, b...)
		case r == '\n':
			out = ww.flushWord(out)
			out = append(out, '\n')
			ww.col, ww.spaces, ww.hang, ww.started, ww.head, ww.margined = 0, 0, 0, false, nil, false
		case ww.raw:
			out = ww.flushWord(out)
			out = append(out, b...)
			ww.col += runeWidth(r)
		case r == ' ':
			out = ww.flushWord(out)
			if !ww.started {
				out = append(out, ' ') // leading indent is kept as is
				ww.col++
				ww.hang = ww.col
			} else {
				ww.spaces++
			}
		default:
			ww.word = append(ww.word, b...)
			ww.wordW += runeWidth(r)
		}
	}
	_, err := ww.w.Write(out)
	return len(p), err
}

// flushWord writes the pending word, wrapping before it when needed.
func (ww *wrapWriter) flushWord(out []byte) []byte {
	if len(ww.word) == 0 {
		return out
	}
	width := ww.width()
	if ww.started && width > 20 && ww.col+ww.spaces+ww.wordW > width {
		out = append(out, '\n')
		hang := max(ww.hang, ww.indent)
		out = append(out, strings.Repeat(" ", hang)...)
		ww.col = hang
	} else {
		out = append(out, strings.Repeat(" ", ww.spaces)...)
		ww.col += ww.spaces
	}
	out = append(out, ww.word...)
	ww.col += ww.wordW
	if !ww.started {
		// A bullet, quote bar or list number: wrapped lines align after it.
		plain := ansiRE.ReplaceAllString(string(ww.word), "")
		if plain == "•" || plain == "☐" || plain == "☑" || plain == "│" || isListNumber(plain) {
			ww.hang = ww.col + 1
		}
		ww.started = true
	}
	ww.word, ww.wordW, ww.spaces = ww.word[:0], 0, 0
	return out
}

func isListNumber(s string) bool {
	if len(s) < 2 || (s[len(s)-1] != '.' && s[len(s)-1] != ')') {
		return false
	}
	for _, c := range s[:len(s)-1] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Flush writes any pending word.
func (ww *wrapWriter) Flush() {
	if out := ww.flushWord(nil); len(out) > 0 {
		ww.w.Write(out)
	}
}
