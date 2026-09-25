package main

import (
	"regexp"
	"strings"
)

// directiveStart matches the start of a memory directive line as the
// model writes it (see memory.Parse); such lines are for agentium, which
// reports what it remembered, not for the reader of the reply.
var directiveStart = regexp.MustCompile(`^\s*(?:[-*]\s*)?@(remember|prefer|decide|forget)\b`)

// directiveFilter passes streamed reply text on, minus memory directive
// lines outside code blocks. A line is held only while it could still
// become one (its start is spaces, a bullet or a prefix of "@remember").
type directiveFilter struct {
	emit    func(string)
	line    strings.Builder // the current line's held text
	passing bool            // the current line is known not to be a directive
	drop    bool            // the current line is a directive
	fence   bool            // inside a ``` block
}

func newDirectiveFilter(emit func(string)) *directiveFilter {
	return &directiveFilter{emit: emit}
}

func (f *directiveFilter) write(d string) {
	var out strings.Builder
	for _, r := range d {
		switch {
		case f.drop:
			if r == '\n' {
				f.drop = false
			}
			continue
		case f.passing:
			out.WriteRune(r)
			if r == '\n' {
				f.passing = false
			}
			continue
		}
		f.line.WriteRune(r)
		held := f.line.String()
		if r == '\n' {
			f.endLine(held, &out)
			continue
		}
		if !couldBeFence(held) && (f.fence || !couldBeDirective(held)) {
			out.WriteString(held)
			f.line.Reset()
			f.passing = true
		} else if directiveStart.MatchString(held) && strings.ContainsAny(held[len(strings.TrimRight(held, " :"))-1:], " :") {
			// "@remember" followed by a space or colon: a directive.
			f.line.Reset()
			f.drop = true
		}
	}
	if out.Len() > 0 {
		f.emit(out.String())
	}
}

// endLine settles a held line at its newline.
func (f *directiveFilter) endLine(held string, out *strings.Builder) {
	f.line.Reset()
	t := strings.TrimSpace(held)
	if strings.HasPrefix(t, "```") {
		f.fence = !f.fence
	}
	if !f.fence && directiveStart.MatchString(held) && len(strings.Fields(t)) > 1 {
		return
	}
	out.WriteString(held)
}

// flush ends a reply: a held partial line is shown unless it is a
// directive.
func (f *directiveFilter) flush() {
	held := f.line.String()
	f.line.Reset()
	f.passing, f.drop, f.fence = false, false, false
	if held == "" {
		return
	}
	t := strings.TrimSpace(held)
	if directiveStart.MatchString(held) && len(strings.Fields(t)) > 1 {
		return
	}
	f.emit(held)
}

// couldBeDirective reports whether s, the start of a line, may still turn
// into a directive.
func couldBeDirective(s string) bool {
	t := strings.TrimLeft(s, " \t")
	if t == "" {
		return true
	}
	if t[0] == '-' || t[0] == '*' {
		t = strings.TrimLeft(t[1:], " \t")
		if t == "" {
			return true
		}
	}
	if t[0] != '@' {
		return false
	}
	word := strings.TrimLeft(t[1:], "")
	for _, k := range []string{"remember", "prefer", "decide", "forget"} {
		if strings.HasPrefix(k, word) || strings.HasPrefix(word, k) {
			return true
		}
	}
	return false
}

// couldBeFence reports whether s, the start of a line, may be a ``` line
// (held to its end, so code blocks are tracked).
func couldBeFence(s string) bool {
	t := strings.TrimLeft(s, " \t")
	return t == "" || strings.HasPrefix("```", t) || strings.HasPrefix(t, "```")
}
