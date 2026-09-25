package main

import (
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// mdStream renders streamed Markdown for a terminal as it arrives:
// headings and **bold** in bold, `code` in cyan, bullets as •, quotes
// dimmed, fenced code passed through untouched. Only a line's first few
// characters are held back (to recognise block markers), so text still
// streams. Used only when stdout is a terminal; pipes get raw Markdown.
type mdStream struct {
	w         io.Writer
	head      strings.Builder // held start of the current line
	lineStart bool
	fence     bool   // inside a fenced code block
	fenceMark string // the fence that opened it (``` or ````, ~~~)
	bold      bool
	code      bool
	heading   bool
	quote     bool
	stars     int  // '*' held until the next rune shows what they are
	before    rune // the rune before the held stars
	prev      rune // the last rune of the current line
	styled    bool // an SGR is active
}

func newMD(w io.Writer) *mdStream { return &mdStream{w: w, lineStart: true} }

const (
	sgrReset = "\033[0m"
	sgrBold  = "\033[1m"
	sgrDim   = "\033[2m"
)

// sgrCyan marks inline code; applyTheme sets it.
var sgrCyan = "\033[36m"

func (m *mdStream) Write(d string) {
	var out strings.Builder
	for _, r := range d {
		if r == '\r' {
			continue // CRLF text renders like LF text
		}
		if m.lineStart {
			m.head.WriteRune(r)
			if r == '\n' || m.decidable() {
				m.block(&out)
				m.syncWrap(&out)
			}
			continue
		}
		m.inline(&out, r)
	}
	io.WriteString(m.w, out.String())
}

// syncWrap stops word-wrapping inside code blocks: text so far is
// written with the old setting before it changes.
func (m *mdStream) syncWrap(out *strings.Builder) {
	if ww, ok := m.w.(*wrapWriter); ok && ww.raw != m.fence {
		io.WriteString(m.w, out.String())
		out.Reset()
		ww.raw = m.fence
	}
}

// Pending reports whether part of a line is held back.
func (m *mdStream) Pending() bool { return m.head.Len() > 0 || m.stars > 0 || !m.lineStart }

// Flush writes anything held back and ends the line.
func (m *mdStream) Flush() {
	var out strings.Builder
	if m.head.Len() > 0 {
		m.block(&out)
	}
	if !m.lineStart {
		m.inline(&out, '\n')
	}
	io.WriteString(m.w, out.String())
	if ww, ok := m.w.(*wrapWriter); ok {
		ww.Flush()
	}
}

// End finishes a reply: whatever is held is written and block state (an
// unclosed code fence from a cut-off reply) does not leak into the next.
func (m *mdStream) End() {
	if m.Pending() {
		m.Flush()
	}
	if m.styled {
		io.WriteString(m.w, sgrReset)
	}
	if ww, ok := m.w.(*wrapWriter); ok {
		ww.Flush()
		ww.raw = false
	}
	*m = mdStream{w: m.w, lineStart: true}
}

// decidable reports whether the held line start is enough to tell which
// block it begins.
func (m *mdStream) decidable() bool {
	h := m.head.String()
	t := strings.TrimLeft(h, " \t")
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "`") || strings.HasPrefix(t, "~~~") {
		return false // maybe a fence: wait for the whole line
	}
	if m.fence {
		return true
	}
	if !strings.ContainsRune("#-*+>_", rune(t[0])) {
		return true
	}
	if len(t) >= 2 && strings.ContainsRune("-*+", rune(t[0])) && t[1] == ' ' && !strings.HasSuffix(t, "\n") {
		// Maybe a rule ("* * *", "- - -"): wait for the line to end.
		if strings.Trim(t, string(t[0])+" \t") == "" {
			return false
		}
		// Maybe a task: wait to see "[ ] " or "[x] " after the marker.
		if rest := t[2:]; len(rest) < 4 && (strings.HasPrefix("[ ] ", rest) || strings.HasPrefix("[x] ", strings.ToLower(rest))) {
			return false
		}
	}
	return strings.ContainsAny(t, " \t") || utf8.RuneCountInString(t) >= 8
}

// block handles the held line start once its kind is known.
func (m *mdStream) block(out *strings.Builder) {
	h := m.head.String()
	m.head.Reset()
	m.lineStart = false
	t := strings.TrimLeft(h, " \t")
	indent := h[:len(h)-len(t)]
	body := strings.TrimRight(t, "\n")
	full := strings.HasSuffix(h, "\n")

	if mark := fenceMark(body); mark != "" && (!m.fence || strings.HasPrefix(mark, m.fenceMark) && strings.TrimSpace(body) == mark) {
		// A fence closes only with the same character, at least as long,
		// and nothing after it (so ```` can contain ```).
		// Code blocks are drawn as a card: a titled top edge, a bar down
		// the left, a bottom edge.
		if m.fence {
			m.fence, m.fenceMark = false, ""
			out.WriteString(indent + sgrDim + "└─" + sgrReset)
		} else {
			m.fence, m.fenceMark = true, mark
			lang := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), mark))
			out.WriteString(indent + sgrDim + strings.TrimSpace("┌─ "+lang) + sgrReset)
		}
		m.endLine(out, full)
		return
	}
	if m.fence {
		out.WriteString(sgrDim + "│" + sgrReset + " " + h)
		m.lineStart = full
		return
	}
	if full && isRule(body) {
		out.WriteString(indent + sgrDim + strings.Repeat("─", 40) + sgrReset + "\n")
		m.lineStart = true
		return
	}
	rest := t
	switch {
	case headingLevel(t) > 0:
		m.heading = true
		rest = strings.TrimLeft(t[headingLevel(t):], " ")
		out.WriteString(indent)
		m.style(out)
	case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ "):
		rest = t[2:]
		switch {
		case strings.HasPrefix(rest, "[ ] "):
			out.WriteString(indent + "☐ ")
			rest = rest[4:]
		case strings.HasPrefix(rest, "[x] ") || strings.HasPrefix(rest, "[X] "):
			out.WriteString(indent + "\033[32m☑\033[39m ")
			rest = rest[4:]
		default:
			out.WriteString(indent + "• ")
		}
	case strings.HasPrefix(t, "> ") || t == ">\n":
		m.quote = true
		rest = strings.TrimPrefix(strings.TrimPrefix(t, ">"), " ")
		out.WriteString(indent)
		m.style(out)
		out.WriteString("│ ")
	default:
		out.WriteString(indent)
	}
	for _, r := range rest {
		m.inline(out, r)
	}
}

func (m *mdStream) endLine(out *strings.Builder, full bool) {
	if full {
		out.WriteByte('\n')
		m.lineStart = true
	}
}

func (m *mdStream) inline(out *strings.Builder, r rune) {
	if m.fence {
		out.WriteRune(r)
		if r == '\n' {
			m.lineStart = true
		}
		return
	}
	if r != '*' && m.stars > 0 {
		m.resolveStars(out, r)
	}
	switch {
	case r == '\n':
		m.bold, m.code, m.heading, m.quote = false, false, false, false
		m.style(out)
		out.WriteByte('\n')
		m.lineStart = true
		m.prev = 0
		return
	case m.code:
		if r == '`' {
			m.code = false
			m.style(out)
		} else {
			out.WriteRune(r)
		}
	case r == '`':
		m.code = true
		m.style(out)
	case r == '*':
		if m.stars == 0 {
			m.before = m.prev
		}
		m.stars++
	default:
		out.WriteRune(r)
	}
	m.prev = r
}

// resolveStars decides what held '*'s were once the next rune is known.
// "**" opens bold only at a word start (after a space or opening
// bracket, before a non-space) and closes only after a non-space;
// anything else — a ** b, src/**/*.go — stays literal.
func (m *mdStream) resolveStars(out *strings.Builder, next rune) {
	n := m.stars
	m.stars = 0
	if n == 2 || n == 3 { // ***x*** renders bold (italics are not styled)
		opens := !m.bold && (m.before == 0 || strings.ContainsRune(" \t([{\"'", m.before)) &&
			next != '\n' && !unicode.IsSpace(next)
		closes := m.bold && m.before != 0 && !unicode.IsSpace(m.before)
		if opens || closes {
			m.bold = !m.bold
			m.style(out)
			return
		}
	}
	out.WriteString(strings.Repeat("*", n))
}

// style emits the SGR sequence for the current inline state.
func (m *mdStream) style(out *strings.Builder) {
	var s string
	if m.bold || m.heading {
		s += sgrBold
	}
	if m.code {
		s += sgrCyan
	}
	if m.quote {
		s += sgrDim
	}
	if s == "" && !m.styled {
		return
	}
	out.WriteString(sgrReset + s)
	m.styled = s != ""
}

// fenceMark returns the fence at the start of a line (``` / ~~~ and
// longer), or "".
func fenceMark(line string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(line) && line[n] == c {
			n++
		}
		if n >= 3 {
			return line[:n]
		}
	}
	return ""
}

func headingLevel(t string) int {
	n := 0
	for n < len(t) && n < 6 && t[n] == '#' {
		n++
	}
	if n > 0 && n < len(t) && t[n] == ' ' {
		return n
	}
	return 0
}

func isRule(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 3 {
		return false
	}
	for _, c := range []string{"-", "*", "_"} {
		if strings.Trim(s, c) == "" {
			return true
		}
	}
	return false
}
