package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tegarthegreat/agentium/internal/lint"
)

var editTool = Tool{
	Def: providerDef("edit",
		"Replace text in a file. old must match once (whitespace/indent differences are tolerated) unless all=true. Empty old writes the whole file. Edits that introduce a syntax error are rejected.",
		`{"type":"object","required":["path","new"],"properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"all":{"type":"boolean"}}}`),
	Run: runEdit,
}

func runEdit(_ context.Context, env *Env, raw json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
		All  bool   `json:"all"`
	}
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", errors.New("path is required")
	}
	p := env.abs(a.Path)
	if env.Gate != nil {
		if ok, why := env.Gate.Write(p); !ok {
			return "", fmt.Errorf("denied (%s); choose another approach or ask the user", why)
		}
	}
	unlock := env.lock(p)
	defer unlock()

	seen, stale := env.freshness(p)
	st, statErr := os.Stat(p)
	exists := statErr == nil
	if exists && st.IsDir() {
		return "", errors.New("path is a directory")
	}
	if exists && stale {
		return "", errors.New("file changed on disk since you read it; read it again before editing")
	}
	if a.Old == "" && exists && !seen {
		return "", errors.New("file exists: read it first, or pass old to change part of it")
	}

	var before []byte
	if exists {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		before = b
	}
	var after, how string
	n := 1
	if a.Old == "" {
		after = a.New
	} else {
		if !exists {
			return "", fmt.Errorf("%s does not exist; to create it, omit old", a.Path)
		}
		var err error
		after, n, how, err = replace(string(before), a.Old, a.New, a.All)
		if err != nil {
			return "", err
		}
	}
	if exists && after == string(before) {
		return "", errors.New("no change: new text equals the current content")
	}

	// Reject edits that break a file that parsed before (or a new file
	// that doesn't parse). Already-broken files may still be edited.
	var warn string
	if r := lint.Check(p, []byte(after)); r.Checked && !r.OK {
		prior := lint.Result{OK: true}
		if exists {
			prior = lint.Check(p, before)
		}
		if prior.OK {
			return "", fmt.Errorf("edit rejected, it would introduce a syntax error (file unchanged):\n%s", r.Msg)
		}
		warn = "\nwarning: file still has syntax errors:\n" + r.Msg
	}

	env.mutate()
	perm := fs.FileMode(0o644)
	if exists {
		perm = st.Mode().Perm()
	} else if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(after), perm); err != nil {
		return "", err
	}
	env.markSeen(p)

	add, del := diffStat(string(before), after)
	switch {
	case !exists:
		return fmt.Sprintf("created %s (%d lines)%s", a.Path, lineCount(after), warn), nil
	case a.Old == "":
		return fmt.Sprintf("rewrote %s (+%d -%d)%s", a.Path, add, del, warn), nil
	}
	msg := fmt.Sprintf("edited %s (+%d -%d", a.Path, add, del)
	if n > 1 {
		msg += fmt.Sprintf(", %d places", n)
	}
	if how != "" {
		msg += ", matched " + how
	}
	return msg + ")" + warn, nil
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// replace applies old→new to s. Exact matches are preferred; otherwise it
// tolerates CRLF line endings, trailing whitespace, and a consistent
// indentation shift, but only when the match is unique.
func replace(s, old, new string, all bool) (out string, n int, how string, err error) {
	if c := strings.Count(s, old); c > 0 {
		if c > 1 && !all {
			return "", 0, "", fmt.Errorf("old text matches %d times; add surrounding context or set all=true", c)
		}
		if all {
			return strings.ReplaceAll(s, old, new), c, "", nil
		}
		return strings.Replace(s, old, new, 1), 1, "", nil
	}
	crlf := strings.Contains(s, "\r\n")
	if crlf && !strings.Contains(old, "\r\n") {
		o, nw := toCRLF(old), toCRLF(new)
		if c := strings.Count(s, o); c == 1 || (c > 1 && all) {
			return strings.ReplaceAll(s, o, nw), c, "with CRLF line endings", nil
		}
	}
	if all {
		return "", 0, "", errors.New("old text not found (all=true needs an exact match); read the file again and copy it exactly")
	}
	for _, mode := range []string{"trailing whitespace", "indentation"} {
		if res, ok := fuzzyReplace(s, old, new, mode); ok {
			return res, 1, "ignoring " + mode, nil
		}
	}
	return "", 0, "", errors.New("old text not found; read the file again and copy it exactly")
}

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// fuzzyReplace matches old line-by-line after normalizing each line.
func fuzzyReplace(s, old, new, mode string) (string, bool) {
	nl := "\n"
	if strings.Contains(s, "\r\n") {
		nl = "\r\n"
	}
	fileLines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	oldLines := strings.Split(strings.Trim(strings.ReplaceAll(old, "\r\n", "\n"), "\n"), "\n")
	if len(oldLines) == 0 || strings.TrimSpace(strings.Join(oldLines, "")) == "" {
		return "", false
	}
	norm := func(l string) string {
		if mode == "indentation" {
			return strings.TrimSpace(l)
		}
		return strings.TrimRight(l, " \t")
	}
	match := -1
	for i := 0; i+len(oldLines) <= len(fileLines); i++ {
		ok := true
		for j, ol := range oldLines {
			if norm(fileLines[i+j]) != norm(ol) {
				ok = false
				break
			}
		}
		if ok {
			if match >= 0 {
				return "", false // ambiguous
			}
			match = i
		}
	}
	if match < 0 {
		return "", false
	}
	newText := strings.Trim(strings.ReplaceAll(new, "\r\n", "\n"), "\n")
	newLines := strings.Split(newText, "\n")
	if newText == "" {
		newLines = nil
	}
	if mode == "indentation" {
		region := fileLines[match : match+len(oldLines)]
		fileInd := leadingWS(firstNonBlank(region))
		oldInd := leadingWS(firstNonBlank(oldLines))
		fileTabs := strings.Contains(fileInd, "\t")
		for _, l := range region {
			if strings.HasPrefix(l, "\t") {
				fileTabs = true
			}
		}
		modelUnit, fileUnit := indentUnit(oldLines), indentUnit(region)
		for i, l := range newLines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			ws := leadingWS(l)
			rel := strings.TrimPrefix(ws, oldInd)
			if len(rel) == len(ws) && oldInd != "" {
				rel = "" // shallower than the old block: align to it
			}
			switch {
			case fileTabs && !strings.Contains(rel, "\t"):
				rel = strings.Repeat("\t", len(rel)/modelUnit) + strings.Repeat(" ", len(rel)%modelUnit)
			case !fileTabs && strings.Contains(rel, "\t"):
				rel = strings.ReplaceAll(rel, "\t", strings.Repeat(" ", fileUnit))
			}
			newLines[i] = fileInd + rel + strings.TrimLeft(l, " \t")
		}
	}
	out := append(append(append([]string{}, fileLines[:match]...), newLines...), fileLines[match+len(oldLines):]...)
	return strings.Join(out, nl), true
}

// indentUnit guesses the indentation step (in columns) used by lines.
func indentUnit(lines []string) int {
	unit := 0
	prev := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		w := len(strings.ReplaceAll(leadingWS(l), "\t", "    "))
		if prev >= 0 {
			d := w - prev
			if d < 0 {
				d = -d
			}
			if d > 0 && (unit == 0 || d < unit) {
				unit = d
			}
		}
		prev = w
	}
	if unit == 0 {
		return 4
	}
	return unit
}

func firstNonBlank(lines []string) string {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// diffStat counts added and removed lines between a and b by trimming
// the common prefix and suffix (cheap and exact for single-region edits).
func diffStat(a, b string) (add, del int) {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	if a == "" {
		al = nil
	}
	p := 0
	for p < len(al) && p < len(bl) && al[p] == bl[p] {
		p++
	}
	q := 0
	for q < len(al)-p && q < len(bl)-p && al[len(al)-1-q] == bl[len(bl)-1-q] {
		q++
	}
	return len(bl) - p - q, len(al) - p - q
}
