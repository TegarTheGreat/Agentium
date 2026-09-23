package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
// history, bracketed paste (multi-line pastes stay one input) and a
// trailing backslash for explicit newlines. Long input scrolls
// horizontally so redraws stay cheap and exact.
type editor struct {
	in     *os.File
	out    *os.File
	hist   *history
	buf    []rune
	pos    int
	prompt string
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
	for i := 0; i < 8; i++ {
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
	display := make([]rune, len(e.buf))
	for i, r := range e.buf {
		if r == '\n' {
			r = '↵'
		}
		display[i] = r
	}
	avail := width - utf8.RuneCountInString(e.prompt) - 1
	if avail < 10 {
		avail = 10
	}
	start := 0
	if e.pos > avail {
		start = e.pos - avail
	}
	end := start + avail
	if end > len(display) {
		end = len(display)
	}
	var sb strings.Builder
	sb.WriteString("\r" + e.prompt + string(display[start:end]) + "\x1b[K")
	sb.WriteString("\r\x1b[" + itoa(utf8.RuneCountInString(e.prompt)+e.pos-start) + "C")
	if utf8.RuneCountInString(e.prompt)+e.pos-start == 0 {
		sb.WriteString("\r")
	}
	e.out.WriteString(sb.String())
}

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
	e.buf, e.pos = nil, 0
	hi := len(e.hist.items)
	var draft []rune
	width := termWidth(e.out)
	e.render(width)
	pasting := false
	for {
		k, err := e.key()
		if err != nil {
			return "", err
		}
		if pasting {
			switch k {
			case "\x1b[201~":
				pasting = false
			case "\r":
				e.insert("\n")
			default:
				if k[0] >= 0x20 || k == "\n" || k == "\t" {
					e.insert(k)
				}
			}
			e.render(width)
			continue
		}
		switch k {
		case "\x1b[200~":
			pasting = true
		case "\r", "\n":
			if e.pos > 0 && e.pos == len(e.buf) && e.buf[e.pos-1] == '\\' {
				e.buf = e.buf[:e.pos-1]
				e.pos--
				e.insert("\n")
				break
			}
			line := string(e.buf)
			e.out.WriteString("\r\n")
			e.hist.add(line)
			return line, nil
		case "\x03": // Ctrl-C
			if len(e.buf) == 0 {
				e.out.WriteString("\r\n")
				return "", errInterrupt
			}
			e.buf, e.pos = nil, 0
		case "\x04": // Ctrl-D
			if len(e.buf) == 0 {
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
		case "\x01", "\x1b[H", "\x1bOH", "\x1b[1~": // Home
			e.pos = 0
		case "\x05", "\x1b[F", "\x1bOF", "\x1b[4~": // End
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
		case "\x15": // Ctrl-U
			e.buf, e.pos = e.buf[e.pos:], 0
		case "\x0b": // Ctrl-K
			e.buf = e.buf[:e.pos]
		case "\x17": // Ctrl-W
			i := e.pos
			for i > 0 && e.buf[i-1] == ' ' {
				i--
			}
			for i > 0 && e.buf[i-1] != ' ' {
				i--
			}
			e.buf, e.pos = append(e.buf[:i], e.buf[e.pos:]...), i
		case "\x0c": // Ctrl-L
			e.out.WriteString("\x1b[H\x1b[2J")
		case "\x1b[A", "\x10": // Up
			if hi > 0 {
				if hi == len(e.hist.items) {
					draft = append([]rune(nil), e.buf...)
				}
				hi--
				e.buf = []rune(e.hist.items[hi])
				e.pos = len(e.buf)
			}
		case "\x1b[B", "\x0e": // Down
			if hi < len(e.hist.items) {
				hi++
				if hi == len(e.hist.items) {
					e.buf = draft
				} else {
					e.buf = []rune(e.hist.items[hi])
				}
				e.pos = len(e.buf)
			}
		case "\x1b\r": // Alt-Enter: newline
			e.insert("\n")
		default:
			if k != "" && k[0] >= 0x20 && k[0] != 0x7f && k[0] != 0x1b {
				e.insert(k)
			} else if k == "\t" {
				e.insert("  ")
			}
		}
		width = termWidth(e.out)
		e.render(width)
	}
}
