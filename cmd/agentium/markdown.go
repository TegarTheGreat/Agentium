package main

import (
	"io"
	"strings"
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
	fence     bool // inside ``` code block
	bold      bool
	code      bool
	heading   bool
	quote     bool
	star      bool // a single '*' held to see if it starts "**"
	styled    bool // an SGR is active
}

func newMD(w io.Writer) *mdStream { return &mdStream{w: w, lineStart: true} }

const (
	sgrReset = "\033[0m"
	sgrBold  = "\033[1m"
	sgrDim   = "\033[2m"
	sgrCyan  = "\033[36m"
)

func (m *mdStream) Write(d string) {
	var out strings.Builder
	for _, r := range d {
		if m.lineStart {
			m.head.WriteRune(r)
			if r == '\n' || m.decidable() {
				m.block(&out)
			}
			continue
		}
		m.inline(&out, r)
	}
	io.WriteString(m.w, out.String())
}

// Pending reports whether part of a line is held back.
func (m *mdStream) Pending() bool { return m.head.Len() > 0 || m.star || !m.lineStart }

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
}

// decidable reports whether the held line start is enough to tell which
// block it begins.
func (m *mdStream) decidable() bool {
	h := m.head.String()
	t := strings.TrimLeft(h, " \t")
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "`") {
		return false // maybe a fence: wait for the whole line
	}
	if m.fence {
		return true
	}
	if !strings.ContainsRune("#-*+>_", rune(t[0])) {
		return true
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

	if strings.HasPrefix(body, "```") {
		m.fence = !m.fence
		out.WriteString(indent + sgrDim + body + sgrReset)
		m.endLine(out, full)
		return
	}
	if m.fence {
		out.WriteString(h)
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
		out.WriteString(indent + "• ")
		rest = t[2:]
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
	if r != '*' && m.star {
		m.star = false
		out.WriteByte('*')
	}
	switch {
	case r == '\n':
		m.bold, m.code, m.heading, m.quote = false, false, false, false
		m.style(out)
		out.WriteByte('\n')
		m.lineStart = true
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
		if m.star {
			m.star = false
			m.bold = !m.bold
			m.style(out)
		} else {
			m.star = true
		}
	default:
		out.WriteRune(r)
	}
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
