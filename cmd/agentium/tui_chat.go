package main

import (
	"os"
	"strings"
)

// Chat-style rendering for the full-screen UI: the user's messages are
// right-aligned bubbles, Agentium's replies follow its name.

// userMessage renders a submitted message for the transcript, followed by
// the label of the reply to come (commands answer without one).
func (u *ui) userMessage(line string) string {
	width := termWidth(os.Stderr) - 1
	maxW := max(width*3/4, 20)
	var rows []string
	for _, l := range strings.Split(line, "\n") {
		rows = append(rows, wordWrap(sanitize(l), maxW-2)...)
	}
	w := 0
	for _, r := range rows {
		w = max(w, strWidth(r))
	}
	var sb strings.Builder
	indent := max(width-w-2, 0)
	sb.WriteString(strings.Repeat(" ", max(width-3, 0)) + u.paint(cDim, "You") + "\r\n")
	for _, r := range rows {
		pad := w - strWidth(r)
		sb.WriteString(strings.Repeat(" ", indent) + "\x1b[48;5;238m\x1b[38;5;255m " + r + strings.Repeat(" ", pad) + " \x1b[0m\r\n")
	}
	if !strings.HasPrefix(line, "/") && !strings.EqualFold(line, "agentium update") {
		sb.WriteString("\r\n" + u.paint(cCyan, "◆") + " " + u.paint(cBold, "Agentium") + "\r\n")
	}
	return sb.String()
}

// welcome opens a full-screen session.
func (u *ui) welcome(model, mode, box, cwd string) {
	var sb strings.Builder
	sb.WriteString("\n" + u.paint(cCyan, "◆") + " " + u.paint(cBold, "Agentium") + "\n")
	sb.WriteString("  Hi! I work in " + u.paint(cBold, shortPath(cwd)) + " with " + model + ".\n")
	sb.WriteString(u.paint(cDim, "  "+mode+" mode · "+box+" · ask me to build, fix or explain something; /help lists commands.") + "\n")
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
