package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// codeExt marks files whose changes deserve a build/test/lint run.
var codeExt = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true,
	".rs": true, ".java": true, ".kt": true, ".kts": true, ".scala": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".hpp": true, ".cs": true, ".rb": true, ".php": true, ".swift": true, ".m": true,
	".dart": true, ".lua": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true, ".ml": true,
	".zig": true, ".nim": true, ".sh": true, ".bash": true, ".vue": true, ".svelte": true, ".sql": true,
}

// checkCmd recognizes shell commands that build, test, lint or run code.
var checkCmd = regexp.MustCompile(`(?i)\b(test|tests|build|check|lint|vet|tsc|pytest|unittest|jest|vitest|mocha|cargo|go\s+(run|build|test|vet)|make|cmake|ctest|gradle|gradlew|mvn|npm|pnpm|yarn|bun|deno|ruff|mypy|pyright|eslint|biome|flake8|pylint|rspec|rake|phpunit|dotnet|swift|zig|bazel|nox|tox|node|python3?|ruby|php|javac|gcc|g\+\+|clang|rustc|elixir|mix|shellcheck|bash\s+-n)\b`)

func argString(c provider.ToolCall, key string) string {
	var m map[string]any
	_ = json.Unmarshal(c.Args, &m)
	s, _ := m[key].(string)
	return s
}

// track updates per-run state after a tool batch: whether code changed
// without a check since, and whether the model is looping. Repeats get a
// warning appended to the result; it returns true when the run should stop.
func (a *Agent) track(rs *runState, calls []provider.ToolCall, results []provider.Message) (stop bool) {
	failed, warned := false, false
	defer func() {
		if failed {
			rs.failStreak++
		} else {
			rs.failStreak = 0
		}
		if !stop && (warned || rs.failStreak >= escalateAfter) {
			a.escalate(rs)
		}
	}()
	for i, c := range calls {
		r := &results[i]
		switch c.Name {
		case "edit":
			path := argString(c, "path")
			if !r.IsError && codeExt[strings.ToLower(filepath.Ext(path))] {
				rs.editedCode = true
			}
			// Many edits to one file without a passing check: the model is
			// probably iterating on a wrong idea.
			if !r.IsError && rs.fileEdits != nil {
				rs.fileEdits[path]++
				if rs.fileEdits[path] >= fileEditWarn && !rs.editWarned[path] {
					rs.editWarned[path] = true
					warned = true
					r.Text += fmt.Sprintf("\n[agentium: %d edits to %s without a passing check. Step back: say what you have learned, name two other explanations for the problem, and test the likeliest before editing again]", rs.fileEdits[path], path)
					a.notice(fmt.Sprintf("%d edits to %s without a passing check; asked the model to step back", rs.fileEdits[path], filepath.Base(path)))
				}
			}
		case "bash":
			if !r.IsError && checkCmd.MatchString(argString(c, "cmd")) {
				// A check that ran counts as verification only if it passed.
				rs.editedCode = false
				rs.checkFailed = failedExit.MatchString(r.Text)
				if !rs.checkFailed {
					for k := range rs.fileEdits {
						delete(rs.fileEdits, k)
					}
				}
			}
		}
		// A denial is the user's or the policy's answer, not a sign the
		// model needs to think harder.
		denied := r.IsError && strings.Contains(r.Text, "denied (")
		if !denied && (r.IsError || c.Name == "bash" && failedExit.MatchString(r.Text)) {
			failed = true
		}
		if c.Name == "bash" && strings.Contains(string(c.Args), `"job"`) {
			continue // polling a background job repeats by design
		}
		h := sha256.Sum256([]byte(c.Name + "\x00" + canonical(c.Args) + "\x00" + r.Text))
		sig := hex.EncodeToString(h[:8])
		rs.sigs = append(rs.sigs, sig)
		if len(rs.sigs) > stuckWindow {
			rs.sigs = rs.sigs[len(rs.sigs)-stuckWindow:]
		}
		n := 0
		for _, s := range rs.sigs {
			if s == sig {
				n++
			}
		}
		switch {
		case n >= stuckStop:
			stop = true
		case n >= stuckWarn:
			warned = true
			r.Text += fmt.Sprintf("\n[agentium: this exact call returned the same result %d times; it is not working, change your approach]", n)
			a.notice(fmt.Sprintf("model repeated %s %d times; warned it", c.Name, n))
		}
	}
	return stop
}

// canonical re-encodes JSON so key order and spacing don't matter.
func canonical(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// escalateAfter is how many failing tool batches in a row make the agent
// think harder.
const escalateAfter = 3

// fileEditWarn is how many edits to one file, without a passing check in
// between, trigger a step-back note.
const fileEditWarn = 6

var failedExit = regexp.MustCompile(`\[exit [1-9]\d*\]\s*$`)

// escalate raises reasoning effort one level (at most twice per turn):
// fast, cheap thinking by default, slow deliberate thinking when the
// fast path keeps failing — Kahneman's System 1 and System 2.
func (a *Agent) escalate(rs *runState) {
	if rs.escalations >= 2 {
		return
	}
	next := a.Reasoning.NextEffort()
	if next == "" {
		return
	}
	rs.escalations++
	rs.failStreak = 0
	a.Reasoning.Effort = next
	a.notice("repeated failures: thinking harder (effort " + next + ")")
}
