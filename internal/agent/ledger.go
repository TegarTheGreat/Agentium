package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Ledger is working memory kept by the harness, not the model: what was
// read and changed, which commands ran and how they ended, the latest
// unresolved error, and the todo list. It costs no LLM call, and it is
// re-emitted verbatim after compaction so the summary never has to
// reconstruct file lists or error text (Claude Code, Codex and OpenCode
// all carry such state across compaction).
type Ledger struct {
	mu        sync.Mutex
	read      []string
	edited    []string
	commands  []cmdRecord
	lastError string // verbatim tail of the latest failure not yet fixed
	errCmd    string
	errLine   string   // the telling line of that failure
	fixEdits  []string // files changed since it failed
	lessons   []string // error → fix pairs of this turn
	todos     []Todo

	// Per user turn.
	turnErrors []string
	untrusted  bool // web pages or MCP output entered the context
}

type cmdRecord struct {
	cmd  string
	exit int // -1 when it failed without an exit code
}

// Todo is one item of the model's task list.
type Todo struct {
	Text   string `json:"text"`
	Status string `json:"status"` // pending | in_progress | done
}

const (
	ledgerFiles    = 25
	ledgerCommands = 10
	errorTailLines = 20
	errorTailBytes = 1500
)

func (l *Ledger) startTurn() {
	l.mu.Lock()
	l.turnErrors, l.untrusted, l.lessons = nil, false, nil
	l.mu.Unlock()
}

// Lessons returns this turn's resolved failures: what failed and what
// made it pass.
func (l *Ledger) Lessons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lessons...)
}

var exitLine = regexp.MustCompile(`\[exit (\d+)\]\s*$`)

// record notes one finished tool call.
func (l *Ledger) record(name string, args json.RawMessage, out string, err error) {
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case name == "read" && err == nil:
		l.read = pushUnique(l.read, str("path"), ledgerFiles)
	case name == "edit" && err == nil:
		l.edited = pushUnique(l.edited, str("path"), ledgerFiles)
		if l.errCmd != "" {
			l.fixEdits = pushUnique(l.fixEdits, str("path"), 8)
		}
	case name == "fetch" || strings.HasPrefix(name, "mcp__"):
		l.untrusted = true
	}
	failed := err != nil
	if name == "bash" {
		exit := 0
		if m := exitLine.FindStringSubmatch(out); m != nil {
			fmt.Sscanf(m[1], "%d", &exit)
		} else if err != nil {
			exit = -1
		}
		failed = exit != 0
		cmd := oneLine(str("cmd"), 120)
		l.commands = append(l.commands, cmdRecord{cmd, exit})
		if len(l.commands) > ledgerCommands {
			l.commands = l.commands[len(l.commands)-ledgerCommands:]
		}
		if !failed && cmd == l.errCmd {
			// The failing command passes now: that is a lesson (the brain
			// learns most from prediction errors that get resolved).
			how := "after retrying"
			if len(l.fixEdits) > 0 {
				how = "after changing " + strings.Join(l.fixEdits, ", ")
			}
			l.lessons = append(l.lessons, fmt.Sprintf("`%s` failed (%s); passed %s", cmd, oneLine(l.errLine, 140), how))
			l.lastError, l.errCmd, l.errLine, l.fixEdits = "", "", "", nil
		}
		if failed && cmd != l.errCmd {
			l.errCmd, l.fixEdits = cmd, nil
		}
	}
	if !failed {
		return
	}
	text := out
	if err != nil {
		text = strings.TrimSpace(out + "\n" + err.Error())
	}
	if name != "bash" {
		l.errCmd = ""
	}
	l.lastError = fmt.Sprintf("%s %s:\n%s", name, oneLine(firstArg(name, a), 120), tailLines(text, errorTailLines, errorTailBytes))
	l.errLine = errorLine(text)
	l.turnErrors = append(l.turnErrors, fmt.Sprintf("%s %s → %s", name, oneLine(firstArg(name, a), 80), oneLine(errorLine(text), 160)))
}

func firstArg(name string, a map[string]any) string {
	for _, k := range []string{"cmd", "path", "url", "pattern", "symbol", "refs"} {
		if s, ok := a[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

var errWord = regexp.MustCompile(`(?i)error|fail|panic|exception|traceback|undefined|not found|cannot|denied`)

// errorLine picks the most telling line of a failure.
func errorLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for _, l := range lines {
		if errWord.MatchString(l) {
			return strings.TrimSpace(l)
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return ""
}

// TurnErrors returns the failures of the current user turn (for the
// journal, so later sessions can recall "we hit this before").
func (l *Ledger) TurnErrors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.turnErrors...)
}

// Untrusted reports whether this turn brought web or MCP content into
// the context; memory written in such a turn is held for review.
func (l *Ledger) Untrusted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.untrusted
}

// SetTodos replaces the todo list.
func (l *Ledger) SetTodos(ts []Todo) {
	l.mu.Lock()
	l.todos = ts
	l.mu.Unlock()
}

// Todos returns the todo list.
func (l *Ledger) Todos() []Todo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Todo(nil), l.todos...)
}

// Render is the state block kept verbatim across compaction.
func (l *Ledger) Render() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sb strings.Builder
	if len(l.todos) > 0 {
		sb.WriteString("Todo:\n" + renderTodos(l.todos))
	}
	if len(l.edited) > 0 {
		sb.WriteString("Files changed: " + strings.Join(l.edited, ", ") + "\n")
	}
	if len(l.read) > 0 {
		sb.WriteString("Files read: " + strings.Join(l.read, ", ") + "\n")
	}
	if len(l.commands) > 0 {
		sb.WriteString("Recent commands:\n")
		for _, c := range l.commands {
			st := "ok"
			if c.exit != 0 {
				st = fmt.Sprintf("exit %d", c.exit)
				if c.exit < 0 {
					st = "failed"
				}
			}
			fmt.Fprintf(&sb, "- %s (%s)\n", c.cmd, st)
		}
	}
	if l.lastError != "" {
		sb.WriteString("Latest unresolved error:\n" + l.lastError + "\n")
	}
	if sb.Len() == 0 {
		return ""
	}
	return "<session-state note=\"tracked by the harness; exact\">\n" + sb.String() + "</session-state>"
}

func renderTodos(ts []Todo) string {
	var sb strings.Builder
	for i, t := range ts {
		mark := "[ ]"
		switch t.Status {
		case "in_progress":
			mark = "[~]"
		case "done":
			mark = "[x]"
		}
		fmt.Fprintf(&sb, "%d. %s %s\n", i+1, mark, t.Text)
	}
	return sb.String()
}

func pushUnique(list []string, s string, max int) []string {
	if s == "" {
		return list
	}
	for i, x := range list {
		if x == s {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	list = append(list, s)
	if len(list) > max {
		list = list[len(list)-max:]
	}
	return list
}

func tailLines(s string, n, maxBytes int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	t := strings.Join(lines, "\n")
	if len(t) > maxBytes {
		t = "…" + strings.ToValidUTF8(t[len(t)-maxBytes:], "")
	}
	return t
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}
