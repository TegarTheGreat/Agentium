package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Entry is one line of USER.md or MEMORY.md:
//
//   - tests need `make e2e` first (2026-09-23 · Makefile)
//
// The suffix records when the fact was written or last confirmed, and the
// project files it is about. Cited files are checked before the entry is
// shown to the model (GitHub Copilot's memory validates citations the same
// way), so notes about code that is gone do not mislead it.
type Entry struct {
	Text  string
	Date  string   // YYYY-MM-DD, "" for entries written before dates existed
	Cites []string // project-relative paths
}

var entrySuffix = regexp.MustCompile(`\s+\((\d{4}-\d{2}-\d{2})(?: · ([^()]+))?\)$`)

func parseEntry(line string) (Entry, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "- ") {
		return Entry{}, false
	}
	t = strings.TrimSpace(t[2:])
	e := Entry{Text: t}
	if m := entrySuffix.FindStringSubmatchIndex(t); m != nil {
		e.Text = strings.TrimSpace(t[:m[0]])
		e.Date = t[m[2]:m[3]]
		if m[4] >= 0 {
			for _, c := range strings.Split(t[m[4]:m[5]], ",") {
				if c = strings.TrimSpace(c); c != "" {
					e.Cites = append(e.Cites, c)
				}
			}
		}
	}
	return e, e.Text != ""
}

func (e Entry) String() string {
	if e.Date == "" {
		return "- " + e.Text
	}
	if len(e.Cites) == 0 {
		return fmt.Sprintf("- %s (%s)", e.Text, e.Date)
	}
	return fmt.Sprintf("- %s (%s · %s)", e.Text, e.Date, strings.Join(e.Cites, ", "))
}

// readEntries parses the entries of a memory file.
func readEntries(p string) []Entry {
	var out []Entry
	for _, l := range strings.Split(read(p), "\n") {
		if e, ok := parseEntry(l); ok {
			out = append(out, e)
		}
	}
	return out
}

// pathish matches `quoted` names, paths, file.ext names and capitalized
// bare names such as Makefile or Dockerfile; only ones that exist count.
var pathish = regexp.MustCompile("`([^`\\s]+)`|\\b([\\w.-]+/[\\w./-]+|[\\w-]+\\.[A-Za-z0-9]{1,6}|[A-Z][A-Za-z]{3,}file|README|LICENSE)\\b")

// citations finds project files a fact mentions (in backticks or as
// path-like words) that exist under root.
func citations(root, text string) []string {
	if root == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range pathish.FindAllStringSubmatch(text, -1) {
		c := m[1]
		if c == "" {
			c = m[2]
		}
		c = strings.TrimSuffix(strings.TrimPrefix(c, "./"), ".")
		if c == "" || seen[c] || strings.Contains(c, "..") || filepath.IsAbs(c) {
			continue
		}
		seen[c] = true
		if _, err := os.Stat(filepath.Join(root, c)); err == nil {
			out = append(out, c)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}

// similar reports whether two facts say nearly the same thing, so a new
// fact replaces the old one instead of piling up beside it.
func similar(a, b string) bool {
	ta, tb := map[string]bool{}, map[string]bool{}
	for _, t := range tokens(a) {
		ta[t] = true
	}
	for _, t := range tokens(b) {
		tb[t] = true
	}
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	return float64(inter)/float64(union) >= 0.6
}

// staleAfter hides uncited facts nobody has confirmed for this long.
const staleAfter = 120 * 24 * time.Hour

// valid reports whether an entry should be shown, and why not.
func (e Entry) valid(root string, now time.Time) (bool, string) {
	for _, c := range e.Cites {
		if _, err := os.Stat(filepath.Join(root, c)); err != nil {
			return false, "cites " + c + ", which no longer exists"
		}
	}
	if len(e.Cites) == 0 && e.Date != "" {
		if d, err := time.Parse("2006-01-02", e.Date); err == nil && now.Sub(d) > staleAfter {
			return false, "not confirmed since " + e.Date
		}
	}
	return true, ""
}

// age renders how old an entry is when that is worth knowing.
func (e Entry) age(now time.Time) string {
	d, err := time.Parse("2006-01-02", e.Date)
	if err != nil {
		return ""
	}
	switch days := int(now.Sub(d).Hours() / 24); {
	case days >= 60:
		return fmt.Sprintf(" (%d months old)", days/30)
	case days >= 14:
		return fmt.Sprintf(" (%d weeks old)", days/7)
	}
	return ""
}
