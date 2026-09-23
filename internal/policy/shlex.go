package policy

import "strings"

// word is one shell word after quote removal. unsafe marks unquoted (or
// double-quoted) expansions — $var, $(…), `…`, {a,b} — whose result the
// checks cannot see.
type word struct {
	text   string
	unsafe bool
}

// splitShell splits cmdline into simple commands (at | ; & && || and
// newlines) of words, the way a POSIX shell would for the purpose of
// vetting arguments. ok is false for anything it will not vouch for:
// unbalanced quotes, redirections other than to /dev/null or between
// descriptors (2>&1), process substitution, here-docs.
func splitShell(s string) (cmds [][]word, ok bool) {
	var cur []word
	var w strings.Builder
	inWord, unsafe := false, false
	endWord := func() {
		if inWord {
			cur = append(cur, word{w.String(), unsafe})
		}
		w.Reset()
		inWord, unsafe = false, false
	}
	endCmd := func() {
		endWord()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
		}
		cur = nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			w.WriteString(s[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' {
					j++
				} else if s[j] == '$' || s[j] == '`' {
					unsafe = true
				}
			}
			if j >= len(s) {
				return nil, false
			}
			w.WriteString(strings.ReplaceAll(s[i+1:j], `\`, ""))
			inWord = true
			i = j
		case c == '\\':
			if i+1 < len(s) {
				w.WriteByte(s[i+1])
				i++
				inWord = true
			}
		case c == '$' || c == '`' || c == '{' || c == '}' || c == '(' || c == ')':
			unsafe = true
			w.WriteByte(c)
			inWord = true
		case c == '>' || c == '<':
			// Allowed: [n]>/dev/null, [n]>&m, 2>&1. Nothing else.
			fd := w.String()
			if inWord && strings.Trim(fd, "0123456789") != "" {
				return nil, false // e.g. foo>bar
			}
			w.Reset()
			inWord = false
			if c == '<' {
				return nil, false
			}
			rest := s[i+1:]
			if strings.HasPrefix(rest, ">") { // >>
				rest = rest[1:]
				i++
			}
			if strings.HasPrefix(rest, "&") {
				n := 1
				for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
					n++
				}
				if n == 1 || n < len(rest) && !strings.ContainsRune(" \t\n;|&", rune(rest[n])) {
					return nil, false // >&file writes a file
				}
				i += n
				continue
			}
			t := strings.TrimLeft(rest, " \t")
			if !strings.HasPrefix(t, "/dev/null") || len(t) > 9 && !strings.ContainsRune(" \t\n;|&", rune(t[9])) {
				return nil, false
			}
			i += len(rest) - len(t) + 9
		case c == '|' || c == ';' || c == '&' || c == '\n':
			endCmd()
		case c == ' ' || c == '\t':
			endWord()
		default:
			w.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return cmds, true
}
