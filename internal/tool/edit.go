package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/fsx"
	"github.com/tegarthegreat/agentium/internal/lint"
)

var editTool = Tool{
	Def: providerDef("edit",
		"Replace text in a file. old is the file's text without read's line numbers and must match once (whitespace/indent differences are tolerated) unless all=true. Empty old writes the whole file. Several changes to one file: edits=[{old,new,all},...], applied in order, all or none. Notebooks (.ipynb): old/new work inside cells as read shows them, or cell=N with cell_mode replace (default), insert or delete. Edits that introduce a syntax error are rejected.",
		`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"all":{"type":"boolean"},"edits":{"type":"array","items":{"type":"object","required":["old","new"],"properties":{"old":{"type":"string"},"new":{"type":"string"},"all":{"type":"boolean"}}}},"cell":{"type":"integer"},"cell_mode":{"type":"string","enum":["replace","insert","delete"]},"cell_type":{"type":"string","enum":["code","markdown"]}}}`),
	Run: runEdit,
}

func runEdit(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
	var a struct {
		Path     string     `json:"path"`
		Old      string     `json:"old"`
		New      *string    `json:"new"`
		All      bool       `json:"all"`
		Edits    []editPart `json:"edits"`
		Cell     *int       `json:"cell"`
		CellMode string     `json:"cell_mode"`
		CellType string     `json:"cell_type"`
	}
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", errors.New("path is required")
	}
	parts := a.Edits
	switch {
	case len(parts) > 0 && (a.New != nil || a.Old != ""):
		return "", errors.New("give either old/new or edits, not both")
	case len(parts) == 0 && a.New == nil && a.CellMode == "delete":
		parts = []editPart{{Old: a.Old}} // deleting a cell needs no text
	case len(parts) == 0 && a.New == nil:
		return "", errors.New("new is required (or edits)")
	case len(parts) == 0:
		parts = []editPart{{Old: a.Old, New: *a.New, All: a.All}}
	}
	for i := range parts {
		if len(parts) > 1 && parts[i].Old == "" {
			return "", fmt.Errorf("edits[%d]: old is empty (writing a whole file is a single edit)", i)
		}
		// Text copied from read output with its line numbers still on.
		if numbered(parts[i].Old) {
			parts[i].Old = stripNumbers(parts[i].Old)
			if numbered(parts[i].New) {
				parts[i].New = stripNumbers(parts[i].New)
			}
		}
	}
	a.Old = parts[0].Old
	p := real(env.abs(a.Path))
	if env.Gate != nil {
		if ok, why := env.Gate.Write(p); !ok {
			return "", fmt.Errorf("denied (%s); do not get the same effect another way (other commands, or deleting, moving or recreating files): continue with the rest, or ask the user", why)
		}
	}
	unlock := env.lock(p)
	defer unlock()

	seen, stale := env.freshness(p)
	st, statErr := os.Stat(p)
	exists := statErr == nil
	if !exists {
		if lst, err := os.Lstat(p); err == nil && lst.Mode()&os.ModeSymlink != 0 {
			// A link to nothing: writing would replace the link itself.
			target, _ := os.Readlink(p)
			return "", fmt.Errorf("%s is a symlink to %s, which does not exist; edit or create the target instead", filepath.Base(p), target)
		}
	}
	if exists && st.IsDir() {
		return "", errors.New("path is a directory")
	}
	if exists && st.Mode().Perm()&0o222 == 0 {
		// The atomic rename only needs the directory to be writable; a
		// read-only file says it is not meant to change.
		return "", fmt.Errorf("%s is read-only (mode %v); change its permissions first if it really should be edited", a.Path, st.Mode().Perm())
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
	// Edits work on the text as the model read it (UTF-8); the file keeps
	// its own encoding. Binary content is matched byte for byte.
	text, enc, ok := decodeText(before)
	if !ok {
		text, enc = string(before), encUTF8
	}
	var after, how string
	n := 1
	if isNotebook(p) && exists && (a.Cell != nil || a.Old != "") {
		if len(parts) > 1 {
			return "", errors.New("in a notebook, make one change per edit (old/new or cell)")
		}
		data, h, err := editNotebook(before, parts[0].Old, parts[0].New, parts[0].All, a.Cell, a.CellMode, a.CellType)
		if err != nil {
			return "", err
		}
		after, how = string(data), h
	} else if a.Old == "" {
		after = parts[0].New
	} else {
		if !exists {
			return "", fmt.Errorf("%s does not exist; to create it, omit old", a.Path)
		}
		after, n = text, 0
		var hows []string
		for i, part := range parts {
			next, k, h, err := replace(after, part.Old, part.New, part.All)
			if err != nil {
				if len(parts) > 1 {
					return "", fmt.Errorf("edits[%d] (nothing was changed): %w", i, err)
				}
				return "", err
			}
			after, n = next, n+k
			if h != "" {
				hows = append(hows, h)
			}
		}
		how = strings.Join(uniqStrings(hows), "; ")
	}
	if exists && after == text {
		return "", errors.New("no change: new text equals the current content")
	}

	// Reject edits that break a file that parsed before (or a new file
	// that doesn't parse). Already-broken files may still be edited.
	var warn string
	if r := lint.Check(p, []byte(after)); r.Checked && !r.OK {
		prior := lint.Result{OK: true}
		if exists {
			prior = lint.Check(p, []byte(text))
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
	if env.BeforeWrite != nil {
		env.BeforeWrite(p)
	}
	data, err := encodeText(after, enc)
	if err != nil {
		return "", err
	}
	if err := writeIfUnchanged(p, before, exists, data, perm); err != nil {
		return "", err
	}
	env.markSeen(p)
	warn += runPostEdit(env, p)
	if env.LSP != nil {
		// Type errors, bad imports, missing functions: what a syntax
		// check cannot see. Hooks may have reformatted, so re-read.
		if b, err := os.ReadFile(p); err == nil {
			warn += env.LSP.Check(ctx, p, b)
		}
	}

	add, del := diffStat(text, after)
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
			return "", 0, "", fmt.Errorf("old text matches %d times (at lines %s); add surrounding context to pick one, or set all=true", c, matchLines(s, old))
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
	return "", 0, "", errors.New("old text not found." + closestHint(s, old) + " Copy the text exactly as read shows it (without the line numbers).")
}

// matchLines lists the line numbers where old starts in s.
func matchLines(s, old string) string {
	var at []string
	for i, off := 0, 0; i < 10; i++ {
		j := strings.Index(s[off:], old)
		if j < 0 {
			break
		}
		at = append(at, strconv.Itoa(strings.Count(s[:off+j], "\n")+1))
		off += j + len(old)
	}
	return strings.Join(at, ", ")
}

// closestHint finds where old nearly matches and says where it differs,
// so a failed edit can be fixed without reading the whole file again.
func closestHint(s, old string) string {
	fileLines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	oldLines := strings.Split(strings.Trim(strings.ReplaceAll(old, "\r\n", "\n"), "\n"), "\n")
	if len(oldLines) == 0 || len(fileLines) == 0 {
		return ""
	}
	best, bestScore := -1, 0
	for i := 0; i+len(oldLines) <= len(fileLines); i++ {
		score := 0
		for j, ol := range oldLines {
			if strings.TrimSpace(fileLines[i+j]) == strings.TrimSpace(ol) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 || bestScore*2 < len(oldLines) && bestScore < 2 {
		return " Nothing similar is in the file; read it again."
	}
	for j, ol := range oldLines {
		if fl := fileLines[best+j]; strings.TrimSpace(fl) != strings.TrimSpace(ol) {
			return fmt.Sprintf(" The closest match is at lines %d-%d (%d of %d lines equal); line %d differs: the file has %q, you wrote %q.",
				best+1, best+len(oldLines), bestScore, len(oldLines), best+j+1, clipLine(fl), clipLine(ol))
		}
	}
	return fmt.Sprintf(" The closest match is at lines %d-%d.", best+1, best+len(oldLines))
}

func clipLine(l string) string {
	if len(l) > 120 {
		return strings.ToValidUTF8(l[:120], "") + "…"
	}
	return l
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
		l = strings.TrimPrefix(l, "\ufeff") // a byte-order mark is not text
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
	// Only the matched lines change: the rest keeps its bytes, line endings
	// included (a file mixing LF and CRLF stays as it was).
	raw := strings.SplitAfter(s, "\n")
	before := strings.Join(raw[:match], "")
	region := raw[match : match+len(oldLines)]
	after := strings.Join(raw[match+len(oldLines):], "")
	if strings.HasSuffix(region[0], "\r\n") {
		nl = "\r\n"
	} else if strings.HasSuffix(region[0], "\n") {
		nl = "\n"
	}
	end := ""
	if last := region[len(region)-1]; strings.HasSuffix(last, "\r\n") {
		end = "\r\n"
	} else if strings.HasSuffix(last, "\n") {
		end = "\n"
	}
	mid := strings.Join(newLines, nl)
	if len(newLines) > 0 {
		mid += end
	}
	if match == 0 && strings.HasPrefix(region[0], "\ufeff") && !strings.HasPrefix(mid, "\ufeff") {
		mid = "\ufeff" + mid // the file keeps its byte-order mark
	}
	return before + mid + after, true
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

// runPostEdit runs the configured post-edit hooks (formatters, linters).
// A hook that changes the file is fine: the new version counts as seen.
// Failing hooks report their output so the model can react.
func runPostEdit(env *Env, path string) string {
	if len(env.PostEdit) == 0 {
		return ""
	}
	var notes []string
	for _, h := range env.PostEdit {
		cmd := strings.ReplaceAll(h, "{path}", shellQuote(path))
		out, err := runShell(context.Background(), env.Root, cmd, 30*time.Second, env.Sandbox, env.PassEnv)
		if err != nil || strings.Contains(out, "[exit ") {
			notes = append(notes, fmt.Sprintf("hook %q: %s", h, Clip(strings.TrimSpace(out), 1500)))
		}
	}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		env.markSeen(path)
	}
	if len(notes) == 0 {
		return ""
	}
	return "\n" + strings.Join(notes, "\n")
}

// editPart is one replacement of a multi-part edit.
type editPart struct {
	Old string `json:"old"`
	New string `json:"new"`
	All bool   `json:"all"`
}

func uniqStrings(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// writeIfUnchanged writes data to p unless p no longer holds before
// (another process, e.g. a second agentium in the same project, wrote it
// since it was read: its change would be silently lost). A lock shared by
// all processes covers the compare and the write.
func writeIfUnchanged(p string, before []byte, existed bool, data []byte, perm fs.FileMode) error {
	h := sha256.Sum256([]byte(p))
	release := fsx.Lock(filepath.Join(os.TempDir(), "agentium-locks", hex.EncodeToString(h[:12])+".lock"), 5*time.Second)
	defer release()
	cur, err := os.ReadFile(p)
	switch {
	case existed && (err != nil || !bytes.Equal(cur, before)):
		return errors.New("file changed on disk while this edit was being made (another process wrote it); read it again and redo the edit")
	case !existed && err == nil:
		return errors.New("file was created by another process meanwhile; read it, then edit it")
	}
	return writeAtomic(p, data, perm)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeAtomic replaces p so that it holds either the old or the new
// content, never a mix — even if the process is killed or the disk fills
// mid-write: the data goes to a temporary file in the same directory, is
// synced, then renamed over p. It then reads the file back to confirm
// what is on disk is what was meant (a conservation check).
func writeAtomic(p string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(p)
	// Keep hard links, ownership, setuid/setgid bits and very long names
	// intact by writing in place for such files.
	if st, err := os.Stat(p); err == nil && (sharedFile(st) || st.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0) || len(filepath.Base(p)) > 200 {
		return writeInPlace(p, data, perm)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(p)+".agentium-*")
	if err != nil {
		if os.IsPermission(err) {
			return writeInPlace(p, data, perm) // writable file in a read-only directory
		}
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		// e.g. Windows, where a file open in an editor cannot be replaced.
		return writeInPlace(p, data, perm)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // make the rename itself durable where supported
		d.Close()
	}
	got, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("write verification failed for %s: the file on disk differs from what was written", p)
	}
	return nil
}

// writeInPlace is the fallback: write the file directly, then verify.
func writeInPlace(p string, data []byte, perm fs.FileMode) error {
	if err := os.WriteFile(p, data, perm); err != nil {
		return err
	}
	got, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("write verification failed for %s: the file on disk differs from what was written", p)
	}
	return nil
}

var lineNumberPrefix = regexp.MustCompile(`^ *\d+\t`)

// numbered reports whether every line of s starts with read's line number
// prefix.
func numbered(s string) bool {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if s == "" || len(lines) == 0 {
		return false
	}
	for _, l := range lines {
		if !lineNumberPrefix.MatchString(l) {
			return false
		}
	}
	return true
}

func stripNumbers(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = lineNumberPrefix.ReplaceAllString(l, "")
	}
	return strings.Join(lines, "\n")
}
