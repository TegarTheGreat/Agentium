package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tegarthegreat/agentium/internal/config"
)

// Approvals kept for a project ("p" at an approval): stored in agentium's
// own data folder, never in the repository, so a cloned project cannot
// approve anything for itself.

// approvalsPath is per working folder, not per repository: "file
// changes" approved in repo/docs must not cover all of repo.
func approvalsPath(root string) string {
	return filepath.Join(config.ProjectDir(filepath.Clean(root)), "approvals.json")
}

// keepable reports whether an approval may be kept for the project:
// not for writes outside the workspace or into git internals, nor for
// reading files outside it (credentials); those last one session.
func keepable(key string) bool {
	return !strings.HasPrefix(key, "write=") && !strings.HasPrefix(key, "read:")
}

func loadApprovals(root string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(approvalsPath(root))
	if err != nil {
		return out
	}
	var keys []string
	if json.Unmarshal(b, &keys) == nil {
		for _, k := range keys {
			out[k] = true
		}
	}
	return out
}

func saveApprovals(root string, set map[string]bool) error {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, _ := json.MarshalIndent(keys, "", "  ")
	p := approvalsPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// describeApproval turns a scope key back into words.
func describeApproval(key string) string {
	switch {
	case key == "write":
		return "file changes in the workspace"
	case strings.HasPrefix(key, "bash:"):
		return "commands starting with `" + strings.TrimPrefix(key, "bash:") + "`"
	case strings.HasPrefix(key, "bash="):
		return "the command `" + strings.TrimPrefix(key, "bash=") + "`"
	case strings.HasPrefix(key, "network="):
		return "network for `" + strings.TrimPrefix(key, "network=") + "`"
	case strings.HasPrefix(key, "write="):
		return "changes to " + strings.TrimPrefix(key, "write=")
	case strings.HasPrefix(key, "read:"):
		return "reading " + strings.TrimPrefix(key, "read:")
	case strings.HasPrefix(key, "fetch:"):
		return "fetching from " + strings.TrimPrefix(key, "fetch:")
	}
	return key
}

// showPermissions lists what is approved without asking, and lets the
// user take approvals back.
func showPermissions(u *ui, ap *approver) {
	if ap == nil {
		return
	}
	ap.mu.Lock()
	type row struct {
		key   string
		saved bool
	}
	var rows []row
	for k := range ap.saved {
		rows = append(rows, row{k, true})
	}
	for k := range ap.always {
		if !ap.saved[k] {
			rows = append(rows, row{k, false})
		}
	}
	mode := string(ap.gate.GetMode())
	ap.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].saved != rows[j].saved {
			return rows[i].saved
		}
		return rows[i].key < rows[j].key
	})
	fmt.Fprintln(os.Stderr)
	u.note("Mode: " + mode + " · shift+tab or /mode changes it")
	if len(rows) == 0 {
		u.note("Nothing is approved in advance. At an approval, a = always this session, p = always in this project.")
		return
	}
	items := []menuItem{{value: "", label: "Done"}}
	for _, r := range rows {
		where := "this session"
		if r.saved {
			where = "this project"
		}
		items = append(items, menuItem{value: r.key, label: truncate(describeApproval(r.key), 60), hint: where + " · Enter revokes"})
	}
	pick, err := u.choose("Approved without asking", items, "", false)
	if err != nil || pick == "" {
		return
	}
	ap.mu.Lock()
	delete(ap.always, pick)
	wasSaved := ap.saved[pick]
	delete(ap.saved, pick)
	var serr error
	if wasSaved {
		serr = saveApprovals(ap.gate.Root, ap.saved)
	}
	ap.mu.Unlock()
	if serr != nil {
		u.failure(serr.Error())
		return
	}
	u.success("Revoked: " + describeApproval(pick))
}

// cmdStep is one part of a command line as shown for approval, with the
// operator that joins it to the next ("" for the last).
type cmdStep struct{ text, op string }

// commandSteps splits a shell command at its top-level ;, &&, || and |
// (outside quotes, $( ), backticks and braces) so a chain is shown one
// step per line. Anything else, heredocs and multi-line scripts included,
// is shown as written.
func commandSteps(action, what string) []cmdStep {
	if !strings.HasPrefix(action, "bash: ") && !strings.HasPrefix(action, "network: ") || strings.Contains(what, "\n") || strings.Contains(what, "<<") {
		return []cmdStep{{text: what}}
	}
	var steps []cmdStep
	var cur strings.Builder
	var quote rune
	depth := 0
	rs := []rune(what)
	flush := func(op string) {
		if t := strings.TrimSpace(cur.String()); t != "" {
			steps = append(steps, cmdStep{t, op})
		} else if op != "" && len(steps) > 0 {
			steps[len(steps)-1].op += " " + op
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == '\\' && quote == '"' && i+1 < len(rs) {
				cur.WriteRune(r)
				i++
				r = rs[i]
			} else if r == quote {
				quote = 0
			}
		case r == '\\' && i+1 < len(rs):
			cur.WriteRune(r)
			i++
			r = rs[i]
		case r == '\'' || r == '"' || r == '`':
			quote = r
		case r == '(' || r == '{':
			depth++
		case r == ')' || r == '}':
			depth = max(depth-1, 0)
		case depth == 0 && (r == ';' || r == '|' || r == '&'):
			prev := rune(0)
			if i > 0 {
				prev = rs[i-1]
			}
			next := rune(0)
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			switch {
			case r == ';':
				flush(";")
				continue
			case r == '|' && next == '|':
				flush("||")
				i++
				continue
			case r == '&' && next == '&':
				flush("&&")
				i++
				continue
			case r == '|' && prev != '>' && next != '&':
				flush("|")
				continue
			}
		}
		cur.WriteRune(r)
	}
	flush("")
	if len(steps) == 0 {
		return []cmdStep{{text: what}}
	}
	return steps
}
