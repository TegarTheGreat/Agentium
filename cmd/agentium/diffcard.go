package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Edits are shown as small diff cards under their step: removed and
// added lines on muted red and green washes, with line numbers, a few
// lines of context, and long changes folded.

const cardRows = 12

type diffLine struct {
	op   byte // ' ', '-', '+'
	text string
}

// lineDiff is a longest-common-subsequence diff of two short texts.
func lineDiff(a, b []string) []diffLine {
	if len(a)*len(b) > 250_000 { // too big to align: show it as replaced
		var out []diffLine
		for _, l := range a {
			out = append(out, diffLine{'-', l})
		}
		for _, l := range b {
			out = append(out, diffLine{'+', l})
		}
		return out
	}
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffLine{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffLine{'-', a[i]})
			i++
		default:
			out = append(out, diffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{'+', b[j]})
	}
	return out
}

// diffCounts returns the numbers of added and removed lines.
func diffCounts(lines []diffLine) (add, del int) {
	for _, l := range lines {
		switch l.op {
		case '+':
			add++
		case '-':
			del++
		}
	}
	return add, del
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// editDiff returns the diff of an edit call and the line number where the
// new text starts in the file (1 when unknown).
func editDiff(root string, args []byte) (path string, lines []diffLine, start int) {
	return editDiffAt(root, args, false)
}

// editDiffAt is editDiff; with preview (before the edit runs) a whole-file
// write is compared with the file as it is now, so the preview shows what
// would be deleted too.
func editDiffAt(root string, args []byte, preview bool) (path string, lines []diffLine, start int) {
	if parts := editParts(args); len(parts) > 0 {
		// A multi-part edit: its parts' diffs one after another.
		for i, pa := range parts {
			p, l, st := editDiffAt(root, pa, preview)
			if i == 0 {
				path, start = p, st
			}
			lines = append(lines, l...)
		}
		return path, lines, start
	}
	var a struct{ Path, Old, New string }
	if jsonUnmarshal(args, &a) != nil || a.Path == "" {
		return "", nil, 0
	}
	p := a.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	old := a.Old
	if preview && old == "" {
		if b, err := os.ReadFile(p); err == nil {
			old = string(b)
		}
	}
	lines = lineDiff(splitLines(old), splitLines(a.New))
	start = 1
	if a.Old != "" {
		if b, err := os.ReadFile(p); err == nil {
			// After the edit the new text is in the file; before it (an
			// approval preview), the old text is.
			i := -1
			if a.New != "" {
				i = strings.Index(string(b), a.New)
			}
			if i < 0 {
				i = strings.Index(string(b), a.Old)
			}
			if i >= 0 {
				start = strings.Count(string(b[:i]), "\n") + 1
			}
		}
	}
	return a.Path, lines, start
}

// diffCard renders an edit's diff for the transcript, indented under its
// step.
func (u *ui) diffCard(c []byte, width int) []string {
	return u.diffCardAt(c, width, false)
}

func (u *ui) diffCardAt(c []byte, width int, preview bool) []string {
	if parts := editParts(c); len(parts) > 0 {
		var out []string
		for _, pa := range parts {
			out = append(out, u.diffCardAt(pa, width, preview)...)
		}
		return out
	}
	_, lines, start := editDiffAt(u.cwd, c, preview)
	if len(lines) == 0 {
		return nil
	}
	// Only changed lines and two lines of context around them.
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if l.op != ' ' {
			for k := max(i-2, 0); k <= min(i+2, len(lines)-1); k++ {
				keep[k] = true
			}
		}
	}
	oldN, newN := start, start
	numW := len(fmt.Sprint(start + len(lines)))
	inner := max(width-6-numW-3, 10)
	var out []string
	shown, skipped := 0, false
	for i, l := range lines {
		num := newN
		switch l.op {
		case ' ':
			oldN++
			newN++
		case '-':
			num = oldN
			oldN++
		case '+':
			newN++
		}
		if !keep[i] {
			skipped = true
			continue
		}
		if skipped && shown > 0 {
			out = append(out, "    "+u.paint(cDim, strings.Repeat(" ", numW)+"  ⋮"))
		}
		skipped = false
		if shown == cardRows {
			rest := 0
			for _, k := range lines[i:] {
				if k.op != ' ' {
					rest++
				}
			}
			if rest > 0 {
				out = append(out, "    "+u.paint(cDim, fmt.Sprintf("%*s  … %d more changed line%s", numW, "", rest, plural(rest))))
			}
			break
		}
		shown++
		text := truncate(strings.ReplaceAll(sanitize(l.text), "\t", "    "), inner)
		pad := strings.Repeat(" ", max(inner-strWidth(text), 0))
		n := u.paint(cDim, fmt.Sprintf("%*d", numW, num))
		switch l.op {
		case '-':
			out = append(out, "    "+n+" "+u.bgLine(bgDel, cRed, "-", text+pad))
		case '+':
			out = append(out, "    "+n+" "+u.bgLine(bgAdd, cGreen, "+", text+pad))
		default:
			out = append(out, "    "+n+"   "+u.paint(cDim, text))
		}
	}
	return out
}

// bgLine paints a diff line: the sign in color, all on a washed background.
func (u *ui) bgLine(bg, fg, sign, text string) string {
	if !u.color {
		return sign + " " + text
	}
	return "\033[" + bg + "m\033[" + fg + "m" + sign + "\033[39m " + text + "\033[0m"
}

// outputTail is the end of a command's output, shown under its step.
func (u *ui) outputTail(out string, width int) []string {
	out = strings.TrimRight(exitRE.ReplaceAllString(out, ""), "\n ")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	all := strings.Split(ansiRE.ReplaceAllString(out, ""), "\n")
	var lines []string
	for _, l := range all {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	const tail = 3
	var rows []string
	if len(lines) > tail {
		rows = append(rows, u.paint(cGray, fmt.Sprintf("… %d earlier line%s · ctrl+o shows all", len(lines)-tail, plural(len(lines)-tail))))
		lines = lines[len(lines)-tail:]
	}
	for _, l := range lines {
		rows = append(rows, u.paint(cDim, truncate(sanitize(strings.TrimRight(l, "\r")), width-8)))
	}
	rows = u.gutter(rows)
	return rows
}

// editParts splits an edit call with edits=[...] into single-edit
// arguments ({path, old, new} each); nil for an ordinary edit.
func editParts(args []byte) [][]byte {
	var a struct {
		Path  string
		Edits []struct{ Old, New string }
	}
	if jsonUnmarshal(args, &a) != nil || len(a.Edits) == 0 {
		return nil
	}
	var out [][]byte
	for _, e := range a.Edits {
		b, err := json.Marshal(map[string]string{"path": a.Path, "old": e.Old, "new": e.New})
		if err == nil {
			out = append(out, b)
		}
	}
	return out
}
