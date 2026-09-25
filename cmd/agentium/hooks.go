package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// User hooks (hooks in ~/.agentium/config.json only: a repository cannot
// add any). Each is a shell command; exit code 2 means "stop", and what
// the command wrote to stderr says why.

const hookTimeout = 30 * time.Second

// errHookStop is a hook's "no" (exit code 2).
var errHookStop = errors.New("stopped by a hook")

// hookStop is a "no" with the hook's reason.
type hookStop struct{ why string }

func (h hookStop) Error() string        { return h.why }
func (h hookStop) Is(target error) bool { return target == errHookStop }

// runHook runs one hook command in dir with input on stdin.
func runHook(ctx context.Context, command, dir string, input []byte) (stdout string, err error) {
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	cmd := hookCommand(ctx, command)
	// A hook that leaves a child holding its output must not hold the
	// turn: stop waiting shortly after it is ended.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(input)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 2 {
		why := strings.TrimSpace(errb.String())
		if why == "" {
			why = strings.TrimSpace(out.String())
		}
		if why == "" {
			return "", errHookStop
		}
		return "", hookStop{clipHook(why)}
	}
	if err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("hook %q failed: %v %s", command, err, clipHook(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func clipHook(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		s = strings.ToValidUTF8(s[:2000], "") + "…"
	}
	return s
}

// preToolHook builds the agent's PreTool check from hooks.pre_tool, or
// nil. A hook that fails for another reason is reported but does not
// block (a broken hook should not stop all work).
func preToolHook(hooks []config.ToolHook, dir string, warn func(string)) func(context.Context, provider.ToolCall) error {
	type compiled struct {
		re  *regexp.Regexp
		cmd string
	}
	var hs []compiled
	for _, h := range hooks {
		if strings.TrimSpace(h.Command) == "" {
			continue
		}
		var re *regexp.Regexp
		if h.Match != "" {
			var err error
			if re, err = regexp.Compile("^(?:" + h.Match + ")$"); err != nil {
				warn(fmt.Sprintf("pre_tool hook ignored: bad match %q: %v", h.Match, err))
				continue
			}
		}
		hs = append(hs, compiled{re, h.Command})
	}
	if len(hs) == 0 {
		return nil
	}
	return func(ctx context.Context, c provider.ToolCall) error {
		args := c.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		input, _ := json.Marshal(map[string]any{"event": "pre_tool", "tool": c.Name, "args": args, "cwd": dir})
		for _, h := range hs {
			if h.re != nil && !h.re.MatchString(c.Name) {
				continue
			}
			if _, err := runHook(ctx, h.cmd, dir, input); err != nil {
				if errors.Is(err, errHookStop) {
					return err
				}
				warn(err.Error())
			}
		}
		return nil
	}
}

// promptHooks runs hooks.user_prompt for a message and returns the
// context they add; errHookStop means the message must not be sent.
func promptHooks(hooks []string, dir, message string, warn func(string)) (string, error) {
	var added []string
	input, _ := json.Marshal(map[string]any{"event": "user_prompt", "prompt": message, "cwd": dir})
	for _, h := range hooks {
		out, err := runHook(context.Background(), h, dir, input)
		if err != nil {
			if errors.Is(err, errHookStop) {
				return "", err
			}
			warn(err.Error())
		}
		if out != "" {
			added = append(added, clipHook(out))
		}
	}
	return strings.Join(added, "\n"), nil
}

// sessionHooks runs hooks.session_start and returns what they printed.
func sessionHooks(hooks []string, dir string, warn func(string)) string {
	var added []string
	input, _ := json.Marshal(map[string]any{"event": "session_start", "cwd": dir})
	for _, h := range hooks {
		out, err := runHook(context.Background(), h, dir, input)
		if err != nil {
			warn(err.Error())
		}
		if out != "" {
			added = append(added, clipHook(out))
		}
	}
	return strings.Join(added, "\n")
}

// showHooks lists the hooks in the settings (/hooks).
func showHooks(u *ui, h config.Hooks) {
	var sb strings.Builder
	n := 0
	group := func(name, when string, cmds []string) {
		if len(cmds) == 0 {
			return
		}
		sb.WriteString("  " + u.paint(cBold, name) + u.paint(cDim, " · "+when) + "\n")
		for _, c := range cmds {
			sb.WriteString("    $ " + c + "\n")
			n++
		}
	}
	var pre []string
	for _, t := range h.PreTool {
		m := t.Match
		if m == "" {
			m = "every tool"
		}
		pre = append(pre, sanitize(t.Command)+u.paint(cDim, "  ("+sanitize(m)+")"))
	}
	clean := func(cmds []string) []string {
		out := make([]string, len(cmds))
		for i, c := range cmds {
			out[i] = sanitize(c)
		}
		return out
	}
	group("session_start", "once at startup; prints context", clean(h.SessionStart))
	group("user_prompt", "before each message; exit 2 stops it", clean(h.UserPrompt))
	group("pre_tool", "before matching tool calls; exit 2 blocks", pre)
	group("post_edit", "after each edit; {path} is the file", clean(h.PostEdit))
	group("stop", "after each turn", clean(h.Stop))
	if n == 0 {
		u.note("no hooks · add them under \"hooks\" in " + shortPath(filepath.Join(config.Home(), "config.json")))
		return
	}
	fmt.Fprint(os.Stderr, "\n"+sb.String())
	u.note("from " + shortPath(filepath.Join(config.Home(), "config.json")) + " (hooks never come from a repository)")
}
