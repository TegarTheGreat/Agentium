package policy

import (
	"path/filepath"
	"strings"
)

// A command line as the checks see it: quotes and backslashes that only
// disguise words (rm "-rf", \rm) removed, variable assignments and
// wrappers (env, command, nice, sudo, xargs, eval ...) peeled off, paths
// to programs shortened to their names (/bin/rm is rm), and commands
// inside $(...) and backquotes checked as commands of their own.

// normCmd is one simple command after normalizing.
type normCmd struct {
	name    string   // the program, e.g. "rm"
	args    []string // its arguments, quotes removed
	dynamic bool     // an argument (or the name) is a $... or `...` expansion
	dynName bool     // the program itself comes from an expansion or eval
}

func (c normCmd) String() string { return strings.TrimSpace(c.name + " " + strings.Join(c.args, " ")) }

// normalizeCommands splits cmd into its simple commands, normalized, the
// substitutions inside it included.
func normalizeCommands(cmd string) []normCmd {
	var out []normCmd
	for _, part := range splitTop(cmd) {
		out = append(out, normalizePart(part)...)
	}
	return out
}

// splitTop splits at ; & | && || and newlines outside quotes and
// substitutions, and returns the insides of $(...) and `...` as parts of
// their own too.
func splitTop(s string) []string {
	var parts []string
	var cur strings.Builder
	var quote byte
	depth := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			parts = append(parts, t)
		}
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(c)
			i++
			c = s[i]
		case quote == '"' && c == '"':
			quote = 0
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			// Find the matching ")" and check its inside as commands too.
			j, d := i+2, 1
			for ; j < len(s) && d > 0; j++ {
				switch s[j] {
				case '(':
					d++
				case ')':
					d--
				}
			}
			inner := s[i+2 : max(j-1, i+2)]
			parts = append(parts, splitTop(inner)...)
			cur.WriteString(s[i:j])
			i = j - 1
			continue
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j >= 0 {
				parts = append(parts, splitTop(s[i+1:i+1+j])...)
				cur.WriteString(s[i : i+2+j])
				i += j + 1
				continue
			}
		case quote == '"':
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '{':
			depth++
		case c == ')' || c == '}':
			depth = max(depth-1, 0)
		case c == ';' || c == '&' || c == '|' || c == '\n':
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return parts
}

// shellWords splits one simple command into words with quotes and
// backslashes removed; dyn marks words holding an expansion.
func shellWords(s string) (words []string, dyn []bool) {
	var w strings.Builder
	in, d := false, false
	end := func() {
		if in {
			words, dyn = append(words, w.String()), append(dyn, d)
		}
		w.Reset()
		in, d = false, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			w.WriteString(s[i+1 : i+1+j])
			in = true
			i += j + 1
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				} else if s[j] == '$' || s[j] == '`' {
					d = true
				}
			}
			w.WriteString(strings.ReplaceAll(s[i+1:min(j, len(s))], `\`, ""))
			in = true
			i = j
		case c == '\\':
			if i+1 < len(s) {
				w.WriteByte(s[i+1])
				i++
			}
			in = true
		case c == '$' || c == '`':
			d = true
			w.WriteByte(c)
			in = true
		case c == ' ' || c == '\t' || c == '(' || c == ')' || c == '{' || c == '}':
			end()
		default:
			w.WriteByte(c)
			in = true
		}
	}
	end()
	return words, dyn
}

// wrappers run the command given as their arguments; the value says how
// many option arguments to skip.
var wrappers = map[string]bool{"command": true, "builtin": true, "env": true, "nice": true, "nohup": true,
	"time": true, "exec": true, "sudo": true, "doas": true, "xargs": true, "stdbuf": true, "timeout": true,
	"ionice": true, "chroot": true, "setsid": true, "unbuffer": true, "watch": true, "then": true, "do": true,
	"else": true, "if": true, "while": true, "until": true, "!": true}

func normalizePart(part string) []normCmd {
	words, dyn := shellWords(part)
	c := normCmd{}
	i := 0
	for i < len(words) {
		w := words[i]
		switch {
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "-") && isAssignment(w):
			i++ // VAR=value before the command
			continue
		case w == "eval":
			// eval runs its arguments as a new command line.
			c.dynName = true
			rest := normalizeCommands(strings.Join(words[i+1:], " "))
			return append([]normCmd{{name: "eval", args: words[i+1:], dynamic: true, dynName: true}}, rest...)
		case wrappers[filepath.Base(w)]:
			name := filepath.Base(w)
			i++
			// Skip the wrapper's own options (and their values).
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				opt := words[i]
				i++
				if (name == "sudo" || name == "doas") && (opt == "-u" || opt == "-g" || opt == "-C") ||
					name == "nice" && opt == "-n" || name == "xargs" && (opt == "-I" || opt == "-n" || opt == "-P" || opt == "-L") ||
					name == "stdbuf" && len(opt) == 2 || name == "ionice" && (opt == "-c" || opt == "-n") {
					i++
				}
			}
			if name == "timeout" && i < len(words) {
				i++ // the duration
			}
			if name == "env" {
				for i < len(words) && isAssignment(words[i]) {
					i++
				}
			}
			continue
		}
		break
	}
	if i >= len(words) {
		return nil
	}
	c.name = filepath.Base(strings.TrimLeft(words[i], `\`))
	c.dynName = dyn[i]
	c.args = words[i+1:]
	for _, d := range dyn[i:] {
		c.dynamic = c.dynamic || d
	}
	// sh -c "..." / bash -c: the string is a command line of its own.
	out := []normCmd{c}
	if (c.name == "sh" || c.name == "bash" || c.name == "zsh" || c.name == "dash") && len(c.args) >= 2 && c.args[0] == "-c" {
		out = append(out, normalizeCommands(c.args[1])...)
	}
	return out
}

func isAssignment(w string) bool {
	k, _, ok := strings.Cut(w, "=")
	if !ok || k == "" {
		return false
	}
	for i, r := range k {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// destructive programs whose effect cannot be judged when an argument is
// an expansion (rm $F could be rm -rf ~).
var destructive = map[string]bool{"rm": true, "rmdir": true, "dd": true, "shred": true, "mkfs": true,
	"chmod": true, "chown": true, "chgrp": true, "truncate": true, "mv": true, "find": true,
	"kill": true, "pkill": true, "killall": true, "sudo": true, "su": true, "doas": true, "crontab": true}

var gitDestructive = map[string]bool{"push": true, "reset": true, "clean": true, "checkout": true,
	"restore": true, "rebase": true, "filter-branch": true, "branch": true, "stash": true, "gc": true, "rm": true, "update-ref": true}

// hiddenRisk says why a normalized command needs approval although the
// raw text looked harmless, or "".
func hiddenRisk(c normCmd) string {
	switch {
	case c.name == "eval":
		return "runs a command built at run time (eval)"
	case c.dynName:
		return "the program to run comes from a variable or substitution"
	case c.dynamic && (destructive[c.name] || strings.HasPrefix(c.name, "mkfs")):
		return c.name + " with arguments only known when it runs"
	case c.dynamic && c.name == "git" && len(c.args) > 0 && (gitDestructive[c.args[0]] || strings.ContainsAny(c.args[0], "$`")):
		return "git " + c.args[0] + " with arguments only known when it runs"
	}
	return ""
}
