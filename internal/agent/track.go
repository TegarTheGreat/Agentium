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
	for i, c := range calls {
		r := &results[i]
		switch c.Name {
		case "edit":
			if !r.IsError && codeExt[strings.ToLower(filepath.Ext(argString(c, "path")))] {
				rs.editedCode = true
			}
		case "bash":
			if !r.IsError && checkCmd.MatchString(argString(c, "cmd")) {
				rs.editedCode = false
			}
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
