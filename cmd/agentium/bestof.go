package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/sandbox"
	"github.com/tegarthegreat/agentium/internal/skill"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// candidate is one best-of-N attempt.
type candidate struct {
	n        int
	dir      string // worktree root
	work     string // cwd inside the worktree
	stats    agent.Stats
	cost     float64
	runErr   error
	pass     bool
	checkOut string
	patch    []byte
	added    int
	removed  int
}

// git runs git for agentium's own bookkeeping with repository-configured
// programs (fsmonitor, hooks) disabled, so nothing an agent planted in
// .git runs outside the sandbox through us.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// bestOfN runs n independent attempts at prompt in separate git
// worktrees, runs check in each, and applies the passing attempt with
// the smallest diff to the real working tree. Parallel attempts plus a
// verifier is the harness technique with the strongest measured effect
// (+6 to +15 points on SWE-bench-style tasks).
func bestOfN(ctx context.Context, n int, check, prompt, cwd string, res provider.Resolved, a *agent.Agent, u *ui) error {
	if check == "" {
		return errors.New("--best-of needs --check \"<command that exits 0 when the task is done>\"")
	}
	top, err := git(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("--best-of needs a git repository")
	}
	top = strings.TrimSpace(top)
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	rel, _ := filepath.Rel(top, cwd)
	// Capture uncommitted tracked changes too; untracked files are not
	// copied into the attempts.
	base, _ := git(top, "stash", "create")
	base = strings.TrimSpace(base)
	if base == "" {
		h, err := git(top, "rev-parse", "HEAD")
		if err != nil {
			return errors.New("--best-of needs at least one commit")
		}
		base = strings.TrimSpace(h)
	}
	if st, _ := git(top, "status", "--porcelain"); strings.Contains(st, "?? ") {
		u.line("· note: untracked files are not visible to the attempts")
	}

	cands := make([]*candidate, n)
	defer func() {
		for _, c := range cands {
			if c != nil {
				_, _ = git(top, "worktree", "remove", "--force", c.dir)
			}
		}
		_, _ = git(top, "worktree", "prune")
	}()
	for i := range cands {
		dir, err := os.MkdirTemp("", fmt.Sprintf("agentium-try%d-", i+1))
		if err != nil {
			return err
		}
		os.Remove(dir) // git worktree add wants to create it
		if _, err := git(top, "worktree", "add", "--detach", "--quiet", dir, base); err != nil {
			return err
		}
		dir, _ = filepath.EvalSymlinks(dir)
		cands[i] = &candidate{n: i + 1, dir: dir, work: filepath.Join(dir, rel)}
	}

	u.line(fmt.Sprintf("· best of %d: running %d attempts in parallel, then `%s`", n, n, check))
	var wg sync.WaitGroup
	for _, c := range cands {
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			env := &tool.Env{Root: c.work, Gate: &policy.Gate{Mode: policy.Auto, Root: c.work}, Net: policy.NetDeny,
				AllowPrivateNet: a.Env.AllowPrivateNet, PostEdit: a.Env.PostEdit}
			if a.Env.Sandbox != nil {
				env.Sandbox = &sandbox.Config{Write: sandbox.DefaultWrite(c.dir)}
			}
			try := &agent.Agent{
				Client: a.Client, Model: a.Model, System: agent.SystemPrompt(c.work, false, "") + skill.Prompt(skill.Discover(config.Home(), c.work)),
				Tools: tool.All(), Env: env, MaxTurns: a.MaxTurns, MaxTokens: a.MaxTokens,
				ContextTokens: a.ContextTokens, Verify: true, Reasoning: a.Reasoning, FastMode: a.FastMode, Cost: a.Cost,
				PreTool: a.PreTool,
				Events: agent.Events{
					ToolStart: func(tc provider.ToolCall) { u.line(fmt.Sprintf("  [%d] › %s", c.n, summarizeCall(tc))) },
				},
			}
			c.stats, c.runErr = try.Run(ctx, prompt+"\n\nWhen done, this check must pass: "+check)
			env.KillJobs()
			c.cost = try.Spent
			out, err := runCheck(ctx, c.work, check, env.Sandbox)
			c.checkOut, c.pass = out, err == nil
			if _, err := git(c.dir, "add", "-A"); err == nil {
				c.patch = []byte(mustGit(c.dir, "diff", "--cached", "--binary", base))
				for _, l := range strings.Split(mustGit(c.dir, "diff", "--cached", "--numstat", base), "\n") {
					var ad, rm int
					if _, err := fmt.Sscanf(l, "%d\t%d", &ad, &rm); err == nil {
						c.added += ad
						c.removed += rm
					}
				}
			}
		}(c)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	sort.SliceStable(cands, func(i, j int) bool {
		ci, cj := cands[i], cands[j]
		if ci.pass != cj.pass {
			return ci.pass
		}
		if (len(ci.patch) > 0) != (len(cj.patch) > 0) {
			return len(ci.patch) > 0
		}
		return ci.added+ci.removed < cj.added+cj.removed
	})
	var total float64
	for _, c := range cands {
		total += c.cost
		mark := "FAIL"
		if c.pass {
			mark = "pass"
		}
		errNote := ""
		if c.runErr != nil {
			errNote = " · " + firstLine(c.runErr.Error())
		}
		u.line(fmt.Sprintf("  [%d] %s · +%d -%d · %d turns · $%.4f%s", c.n, mark, c.added, c.removed, c.stats.Turns, c.cost, errNote))
	}
	win := cands[0]
	if !win.pass {
		fmt.Fprintf(os.Stderr, "no attempt passed `%s`; nothing applied. Last output of attempt %d:\n%s\n", check, win.n, tail(win.checkOut, 1500))
		return agent.ErrMaxTurns
	}
	if len(win.patch) == 0 {
		fmt.Fprintf(os.Stderr, "attempt %d passes without changes; nothing to apply\n", win.n)
		return nil
	}
	cmd := exec.Command("git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "apply", "--whitespace=nowarn", "-")
	cmd.Dir = top
	cmd.Stdin = bytes.NewReader(win.patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("applying attempt %d failed: %v: %s", win.n, err, out)
	}
	fmt.Fprintf(os.Stdout, "Applied attempt %d (+%d -%d); `%s` passed. Total cost $%.4f.\n", win.n, win.added, win.removed, check, total)
	return nil
}

func mustGit(dir string, args ...string) string {
	out, _ := git(dir, args...)
	return out
}

func runCheck(ctx context.Context, dir, check string, box *sandbox.Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if box != nil {
		c, _, err := sandbox.Command("/bin/sh", check, *box)
		if err != nil {
			return "", err
		}
		cmd = exec.CommandContext(ctx, c.Path, c.Args[1:]...)
		cmd.Env = policy.ScrubEnv(c.Env, nil)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", check)
		cmd.Env = policy.ScrubEnv(os.Environ(), nil)
	}
	// On timeout or Ctrl-C kill the whole process group, and don't wait
	// forever on children that keep the output pipe open.
	setPgid(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
