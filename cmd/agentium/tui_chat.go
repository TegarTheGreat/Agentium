package main

import (
	"os"
	"strings"
)

// Chat-style rendering for the full-screen UI: the user's messages are
// right-aligned bubbles, Agentium's replies follow its name.

// userMessage renders a submitted message for the transcript: an ink
// bar down its left side, like a quote of what you said.
func (u *ui) userMessage(line string) string {
	width := termWidth(os.Stderr) - 4
	var sb strings.Builder
	sb.WriteString("\r\n")
	for _, l := range strings.Split(line, "\n") {
		for _, r := range wordWrap(sanitize(l), width) {
			sb.WriteString(u.paint(cInk, "▌") + " " + u.paint(cBold, r) + "\r\n")
		}
	}
	return sb.String()
}

// welcome opens a full-screen session.
func (u *ui) welcome(model, mode, box, cwd string) {
	var sb strings.Builder
	sb.WriteString("\n  " + u.paint(cAccent, "◆") + " " + u.paint(cBold, "Agentium") + " " + u.paint(cGray, version) + "\n")
	sb.WriteString(u.paint(cGray, "    "+shortPath(cwd)+" · "+model+" · "+mode+" mode · "+box) + "\n\n")
	sb.WriteString(u.paint(cDim, "    Try: explain this project · fix the failing test · add a --json flag") + "\n")
	keys := "    / commands · @ mention a file · ! shell · ? shortcuts"
	if termWidth(realTTY()) >= sideMinW {
		keys += " · ctrl+t panel"
	}
	sb.WriteString(u.paint(cDim, keys) + "\n")
	os.Stderr.WriteString(sb.String())
}

// shortPath abbreviates the home directory as ~.
func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// wordWrap breaks s into rows of at most width columns at spaces; a word
// longer than a row is split.
func wordWrap(s string, width int) []string {
	width = max(width, 10)
	var rows []string
	cur, n := "", 0
	for _, word := range strings.Split(s, " ") {
		w := strWidth(word)
		switch {
		case n > 0 && n+1+w <= width:
			cur, n = cur+" "+word, n+1+w
		case n == 0 && w <= width:
			cur, n = word, w
		default:
			if n > 0 {
				rows = append(rows, cur)
			}
			cur, n = "", 0
			for w > width {
				part := truncateCols(word, width)
				rows = append(rows, part)
				word = word[len(part):]
				w = strWidth(word)
			}
			cur, n = word, w
		}
	}
	return append(rows, cur)
}

// truncateCols returns the longest prefix of s at most w columns wide.
func truncateCols(s string, w int) string {
	n := 0
	for i, r := range s {
		if n+runeWidth(r) > w {
			return s[:i]
		}
		n += runeWidth(r)
	}
	return s
}
