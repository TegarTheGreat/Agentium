package main

import (
	"os"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// recap shows where a resumed conversation left off: your last message
// and the start of the reply to it.
func recap(u *ui, msgs []provider.Message) {
	turns := userTurns(msgs)
	if len(turns) == 0 {
		return
	}
	last := turns[len(turns)-1]
	reply := ""
	for _, m := range msgs[last.index+1:] {
		if m.Role == provider.RoleAssistant && strings.TrimSpace(m.Text) != "" {
			reply = m.Text // the last one: the answer, not the "let me look"
		}
	}
	width := termWidth(os.Stderr) - 6
	var sb strings.Builder
	sb.WriteString(u.paint(cDim, "  Where you left off:") + "\n")
	for _, l := range recapLines(last.words, 3, width) {
		sb.WriteString("  " + u.paint(cInk, "▌") + " " + l + "\n")
	}
	if reply != "" {
		for i, l := range recapLines(stripANSI(reply), 6, width) {
			mark := "  "
			if i == 0 {
				mark = u.paint(cAccent, "◆") + " "
			}
			sb.WriteString("  " + mark + u.paint(cGray, l) + "\n")
		}
	}
	os.Stderr.WriteString(sb.String())
}

// recapLines is the first n non-blank lines of s, cleaned and cut to
// width, with "…" when there was more.
func recapLines(s string, n, width int) []string {
	var out []string
	more := false
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(sanitize(l))
		if l == "" || strings.HasPrefix(l, "```") {
			continue
		}
		if len(out) == n {
			more = true
			break
		}
		out = append(out, truncate(l, max(width, 20)))
	}
	if more && len(out) > 0 {
		out[len(out)-1] += " …"
	}
	return out
}
