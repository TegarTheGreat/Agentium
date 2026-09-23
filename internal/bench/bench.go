// Package bench measures what Agentium promises: fast startup, small
// footprint, small prompt, few tokens and turns per task.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/sandbox"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// Offline holds metrics that need no model.
type Offline struct {
	BinaryBytes    int64         `json:"binary_bytes"`
	StartupMedian  time.Duration `json:"startup_median_ns"`
	StartupP90     time.Duration `json:"startup_p90_ns"`
	MaxRSSBytes    int64         `json:"max_rss_bytes"`
	PromptChars    int           `json:"prompt_chars"`
	ToolChars      int           `json:"tool_schema_chars"`
	OverheadTokens int           `json:"overhead_tokens_est"`
}

// MeasureOffline runs the binary `runs` times with args (e.g. "version").
func MeasureOffline(runs int) (Offline, error) {
	var o Offline
	exe, err := os.Executable()
	if err != nil {
		return o, err
	}
	if st, err := os.Stat(exe); err == nil {
		o.BinaryBytes = st.Size()
	}
	times := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		cmd := exec.Command(exe, "version")
		t0 := time.Now()
		if err := cmd.Run(); err != nil {
			return o, fmt.Errorf("run %s version: %w", exe, err)
		}
		times = append(times, time.Since(t0))
		if rss := maxRSS(cmd.ProcessState); rss > o.MaxRSSBytes {
			o.MaxRSSBytes = rss
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	o.StartupMedian = times[len(times)/2]
	o.StartupP90 = times[len(times)*9/10]

	dir, err := os.MkdirTemp("", "agentium-bench-")
	if err != nil {
		return o, err
	}
	defer os.RemoveAll(dir)
	o.PromptChars = len(agent.SystemPrompt(dir))
	defs, _ := json.Marshal(tool.Defs(tool.All()))
	o.ToolChars = len(defs)
	// ~4 characters per token is the usual estimate for English + JSON.
	o.OverheadTokens = (o.PromptChars + o.ToolChars + 3) / 4
	return o, nil
}

// Task is one live benchmark task.
type Task struct {
	Name   string
	Setup  map[string]string // relative path -> content
	Prompt string
	Check  func(dir, reply string) error
}

// Result of one live task.
type Result struct {
	Name      string         `json:"name"`
	Pass      bool           `json:"pass"`
	Error     string         `json:"error,omitempty"`
	Turns     int            `json:"turns"`
	ToolCalls int            `json:"tool_calls"`
	Usage     provider.Usage `json:"usage"`
	Elapsed   time.Duration  `json:"elapsed_ns"`
}

func fileEquals(name, want string) func(string, string) error {
	return func(dir, _ string) error {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(b)) != want {
			return fmt.Errorf("%s = %q, want %q", name, strings.TrimSpace(string(b)), want)
		}
		return nil
	}
}

func replyHas(want string) func(string, string) error {
	return func(_, reply string) error {
		if !strings.Contains(reply, want) {
			return fmt.Errorf("reply %q does not contain %q", strings.TrimSpace(reply), want)
		}
		return nil
	}
}

func csv(n int) string {
	var sb strings.Builder
	sb.WriteString("id,value\n")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&sb, "%d,%d\n", i, i*7%13)
	}
	return sb.String()
}

// Tasks is the default live suite: small, deterministic, checkable.
var Tasks = []Task{
	{
		Name:   "write",
		Prompt: "Create a file named hello.txt containing exactly: hello agentium",
		Check:  fileEquals("hello.txt", "hello agentium"),
	},
	{
		Name:   "edit",
		Setup:  map[string]string{"config.json": "{\n  \"name\": \"demo\",\n  \"port\": 8080,\n  \"debug\": true\n}\n"},
		Prompt: "In config.json change the port to 9090. Change nothing else.",
		Check:  fileEquals("config.json", "{\n  \"name\": \"demo\",\n  \"port\": 9090,\n  \"debug\": true\n}"),
	},
	{
		Name:   "recall-file",
		Setup:  map[string]string{"notes/deep/secret.txt": "the access code is ZEBRA-4471\n"},
		Prompt: "Somewhere under notes/ there is an access code. Reply with only the code.",
		Check:  replyHas("ZEBRA-4471"),
	},
	{
		Name:   "shell",
		Setup:  map[string]string{"data.csv": csv(137)},
		Prompt: "How many data rows (excluding the header) does data.csv have? Reply with only the number.",
		Check:  replyHas("136"),
	},
	{
		Name: "fix-test",
		Setup: map[string]string{
			"calc.sh": "#!/bin/sh\n# add two numbers\necho $(( $1 - $2 ))\n",
			"test.sh": "#!/bin/sh\n[ \"$(sh calc.sh 2 3)\" = \"5\" ] && [ \"$(sh calc.sh 10 -4)\" = \"6\" ] && echo PASS || { echo FAIL; exit 1; }\n",
		},
		Prompt: "sh test.sh fails. Fix calc.sh so it passes.",
		Check: func(dir, _ string) error {
			cmd := exec.Command("sh", "test.sh")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("test.sh: %s", strings.TrimSpace(string(out)))
			}
			return nil
		},
	},
}

// RunLive runs tasks against a model, each in a fresh temp workspace.
func RunLive(ctx context.Context, client provider.Client, model string, tasks []Task, progress func(Result)) ([]Result, error) {
	var results []Result
	for _, t := range tasks {
		dir, err := os.MkdirTemp("", "agentium-task-")
		if err != nil {
			return results, err
		}
		dir, _ = filepath.EvalSymlinks(dir)
		for p, c := range t.Setup {
			fp := filepath.Join(dir, p)
			_ = os.MkdirAll(filepath.Dir(fp), 0o755)
			if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
				return results, err
			}
		}
		var reply strings.Builder
		env := &tool.Env{Root: dir, Gate: &policy.Gate{Mode: policy.Auto, Root: dir}, Net: policy.NetDeny}
		if sandbox.Probe().Available {
			env.Sandbox = &sandbox.Config{Write: sandbox.DefaultWrite(dir)}
		}
		a := &agent.Agent{
			Client: client, Model: model, System: agent.SystemPrompt(dir),
			Tools: tool.All(), Env: env,
			MaxTurns: 20, ContextChars: 200_000,
			Events: agent.Events{TurnFinish: func(r provider.Response) {
				reply.Reset()
				reply.WriteString(r.Text)
			}},
		}
		tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		st, err := a.Run(tctx, t.Prompt)
		cancel()
		r := Result{Name: t.Name, Turns: st.Turns, ToolCalls: st.ToolCalls, Usage: st.Usage, Elapsed: st.Elapsed}
		if err != nil {
			r.Error = err.Error()
		} else if cerr := t.Check(dir, reply.String()); cerr != nil {
			r.Error = cerr.Error()
		} else {
			r.Pass = true
		}
		os.RemoveAll(dir)
		results = append(results, r)
		if progress != nil {
			progress(r)
		}
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
	}
	return results, nil
}
