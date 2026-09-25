package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// A custom command can carry live context, as in Claude Code: !`git diff`
// in its file is replaced by the command's output when it is used.

var bangCmd = regexp.MustCompile("!`([^`\n]+)`")

const bangMaxOut = 16 << 10

// bangCommands lists the !`command` spans in a command file, in order.
func bangCommands(body string) []string {
	var out []string
	for _, m := range bangCmd.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// runBang runs one command in dir and returns what it printed, capped;
// a failure is said after the output.
func runBang(command, dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := hookCommand(ctx, command)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	b, err := cmd.CombinedOutput()
	_ = killGroup(cmd) // children it left behind
	out := strings.TrimRight(strings.ToValidUTF8(string(b), "�"), "\r\n")
	if len(out) > bangMaxOut {
		out = strings.ToValidUTF8(out[:bangMaxOut], "") + "\n… (cut)"
	}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		out += "\n(" + command + ": stopped after 30s)"
	case errors.As(err, &ee):
		out += "\n(" + command + ": exit status " + itoa(ee.ExitCode()) + ")"
	case err != nil:
		out += "\n(" + command + ": " + err.Error() + ")"
	}
	return out
}

// commandShell runs a command file's !`commands` for expandCommandShell.
// Yours run; a repository's run only when you say so, and never without
// asking (one-shot runs leave them as written).
func commandShell(u *ui, c userCmd, cmds []string, dir string, interactive bool) []string {
	name := sanitize(c.name)
	if !c.personal {
		if !interactive {
			u.note("/" + name + " is the repository's: its !`commands` were not run")
			return nil
		}
		for _, cmd := range cmds {
			if hasFormatChars(cmd) {
				u.note("/" + name + " has hidden characters in a command; not run")
				return nil
			}
		}
		u.note("/" + name + " (from this repository) wants to run:")
		for _, cmd := range cmds {
			u.note("  $ " + sanitize(cmd)) // in full: nothing hides past a cut
		}
		pick, err := u.choose("Run them and include their output?", []menuItem{
			{value: "run", label: "Run them"},
			{value: "no", label: "Don't run; send the command as written"},
		}, "", false)
		if err != nil || pick != "run" {
			return nil
		}
	}
	outs := make([]string, len(cmds))
	start := time.Now()
	for i, cmd := range cmds {
		if time.Since(start) > 2*time.Minute {
			outs[i] = "(" + cmd + ": not run, the command's time ran out)"
			continue
		}
		u.note("$ " + sanitize(oneLine(cmd, 200)))
		outs[i] = runBang(cmd, dir)
	}
	return outs
}

// hasFormatChars reports invisible format characters (bidi overrides and
// the like) that could make a command read differently from what runs.
func hasFormatChars(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// commandModel is the custom command line runs, when it names a model.
func commandModel(cwd, line string) (userCmd, bool) {
	if !strings.HasPrefix(line, "/") {
		return userCmd{}, false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	for _, c := range userCommands(cwd) {
		if c.name == strings.ToLower(name) && c.model != "" {
			return c, true
		}
	}
	return userCmd{}, false
}

// allowCommandModel asks before a repository's command moves the turn to
// another model (it may cost more, or send the conversation to another
// provider); yours are trusted.
func allowCommandModel(u *ui, c userCmd) bool {
	if c.personal {
		return true
	}
	pick, err := u.choose("/"+sanitize(c.name)+" (from this repository) wants to run on "+c.model, []menuItem{
		{value: "yes", label: "Use " + c.model + " for this turn"},
		{value: "no", label: "Use the session's model"},
	}, "", false)
	return err == nil && pick == "yes"
}

// useModelOnce points the agent at ref for one turn; the returned func
// puts the session's model back.
func useModelOnce(ref string, cfg config.Config, res *provider.Resolved, a *agent.Agent, fb *provider.Fallback, maxCost float64) (func(), error) {
	if fb != nil {
		return nil, errors.New("not with a fallback chain")
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return nil, err
	}
	nr, err := provider.Resolve(ref, cfg, auth)
	if err != nil {
		return nil, err
	}
	if maxCost > 0 && !nr.Known {
		return nil, errors.New("no known price, and --max-cost could not count it")
	}
	// The conversation keeps the session model's context budget (a
	// smaller window must not compact it for good); it has to fit.
	if window := firstPositive(nr.Info.Context, provider.ContextWindow(nr.Model)); window > 0 {
		if used, _ := a.ContextUsed(); used > window*3/4 {
			return nil, fmt.Errorf("the conversation (%s tokens) is too long for its %s window", fmtK(used), fmtK(window))
		}
	}
	old := *res
	oc, om, ov, or, oout := a.Client, a.Model, a.Env.Vision, a.Reasoning, a.MaxOutput
	oldName, _ := curModel.Load().(string)
	*res = nr
	a.Client, a.Model, a.Env.Vision = nr.Client, nr.Model, nr.Vision()
	a.Reasoning = nr.Reasoning(or.Effort)
	a.MaxOutput = nr.Info.Output
	curModel.Store(nr.Provider + "/" + nr.Model)
	modelSwapped.Store(true)
	return func() {
		*res = old
		a.Client, a.Model, a.Env.Vision, a.Reasoning, a.MaxOutput = oc, om, ov, or, oout
		curModel.Store(oldName)
		modelSwapped.Store(false)
	}, nil
}

// modelSwapped is set while a command's model runs a turn (no next-
// message guess then: it would run and be priced on the wrong model).
var modelSwapped atomic.Bool
