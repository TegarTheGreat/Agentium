// Command agentium is a fast, minimal coding agent.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/checkpoint"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/session"
	"github.com/tegarthegreat/agentium/internal/tool"
)

var version = "0.1.0"

const usage = `agentium — fast, minimal coding agent

Usage:
  agentium                      interactive session
  agentium "fix the tests"      one-shot task (also: -p "...", or pipe stdin)
  agentium login <provider>     store an API key (~/.agentium/auth.json, 0600)
  agentium logout <provider>
  agentium providers            list providers and credential status
  agentium undo                 revert the file changes of the last turn here
  agentium bench [-m model]     measure startup/RAM/prompt; with -m also run live tasks
  agentium version

Flags:
  -m provider/model   model to use (env AGENTIUM_MODEL, config "model")
  -p prompt           one-shot prompt
  -c                  continue the latest session in this directory
  --mode ask|auto|yolo  approvals: every action | risky only (default) | never
  --yolo              same as --mode yolo
  -q                  quiet: no tool lines or stats
  --max-turns N       stop after N model turns (default 100)

In a session: /undo  /clear  /model <ref>  /mode <m>  /usage  /exit
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("agentium", version)
			return
		case "help", "--help", "-h":
			fmt.Print(usage)
			return
		case "login":
			exit(cmdLogin(os.Args[2:]))
			return
		case "logout":
			exit(cmdLogout(os.Args[2:]))
			return
		case "providers":
			exit(cmdProviders())
			return
		case "undo":
			exit(cmdUndo())
			return
		case "bench":
			exit(cmdBench(os.Args[2:]))
			return
		}
	}
	exit(run(os.Args[1:]))
}

func exit(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "agentium:", err)
	if errors.Is(err, context.Canceled) {
		os.Exit(130)
	}
	os.Exit(1)
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// ui renders agent events tersely.
type ui struct {
	mu      sync.Mutex
	quiet   bool
	color   bool
	midLine bool // stdout has text without a trailing newline
}

func (u *ui) dim(s string) string {
	if u.color {
		return "\033[2m" + s + "\033[0m"
	}
	return s
}

func (u *ui) text(d string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	os.Stdout.WriteString(d)
	if d != "" {
		u.midLine = !strings.HasSuffix(d, "\n")
	}
}

func (u *ui) endLine() {
	if u.midLine {
		os.Stdout.WriteString("\n")
		u.midLine = false
	}
}

func (u *ui) line(s string) {
	if u.quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endLine()
	fmt.Fprintln(os.Stderr, u.dim(s))
}

func summarizeCall(c provider.ToolCall) string {
	var m map[string]any
	_ = jsonUnmarshal(c.Args, &m)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	s := pick("cmd", "path", "pattern", "url", "glob")
	if c.Name == "search" && m["glob"] != nil && m["pattern"] != nil {
		s = fmt.Sprintf("%v in %v", m["pattern"], m["glob"])
	}
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return c.Name + " " + s
}

func fmtK(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

func statsLine(st agent.Stats) string {
	u := st.Usage
	s := fmt.Sprintf("· %d turn%s · %d tool%s · in %s", st.Turns, plural(st.Turns), st.ToolCalls, plural(st.ToolCalls), fmtK(u.Input+u.CacheRead+u.CacheWrite))
	if u.CacheRead > 0 {
		s += fmt.Sprintf(" (cached %s)", fmtK(u.CacheRead))
	}
	return s + fmt.Sprintf(" · out %s · %.1fs", fmtK(u.Output), st.Elapsed.Seconds())
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// approver asks on the terminal; parallel tools are serialized.
type approver struct {
	mu     sync.Mutex
	in     *bufio.Reader
	ui     *ui
	gate   *policy.Gate
	enable bool
}

func (a *approver) ask(action, reason string) bool {
	if !a.enable {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gate.GetMode() == policy.Yolo { // "always" chosen meanwhile
		return true
	}
	a.ui.mu.Lock()
	a.ui.endLine()
	fmt.Fprintf(os.Stderr, "⚠ %s  (%s)\n  allow? [y]es / [N]o / [a]lways: ", action, reason)
	a.ui.mu.Unlock()
	line, _ := a.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "a", "always":
		a.gate.SetMode(policy.Yolo)
		return true
	}
	return false
}

func run(args []string) error {
	fs := flag.NewFlagSet("agentium", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	modelRef := fs.String("m", "", "")
	prompt := fs.String("p", "", "")
	cont := fs.Bool("c", false, "")
	mode := fs.String("mode", "", "")
	yolo := fs.Bool("yolo", false, "")
	quiet := fs.Bool("q", false, "")
	maxTurns := fs.Int("max-turns", 0, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *prompt == "" && fs.NArg() > 0 {
		*prompt = strings.Join(fs.Args(), " ")
	}
	stdinTTY := isTTY(os.Stdin)
	if *prompt == "" && !stdinTTY {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		*prompt = strings.TrimSpace(string(b))
		if *prompt == "" {
			return errors.New("empty prompt on stdin")
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	ref := firstNonEmpty(*modelRef, os.Getenv("AGENTIUM_MODEL"), cfg.Model)
	res, err := provider.Resolve(ref, cfg, auth)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}

	m := policy.ParseMode(firstNonEmpty(*mode, cfg.Mode))
	if *yolo {
		m = policy.Yolo
	}
	u := &ui{quiet: *quiet, color: isTTY(os.Stderr) && os.Getenv("NO_COLOR") == ""}
	in := bufio.NewReader(os.Stdin)
	gate := &policy.Gate{Mode: m, Root: cwd}
	ap := &approver{in: in, ui: u, gate: gate, enable: stdinTTY}
	gate.Approve = ap.ask

	sess := session.New(cwd, res.Provider+"/"+res.Model)
	a := &agent.Agent{
		Client: res.Client, Model: res.Model, System: agent.SystemPrompt(cwd),
		Tools: tool.All(), Env: &tool.Env{Root: cwd, Gate: gate, AllowPrivateNet: cfg.FetchPrivate},
		MaxTurns: firstPositive(*maxTurns, cfg.MaxTurns), MaxTokens: cfg.MaxTokens,
		ContextChars: 400_000,
	}
	if *cont {
		if prev, err := session.Latest(cwd); err == nil && prev != nil {
			sess = prev
			a.Messages = prev.Messages
			a.Note, sess.Note = prev.Note, ""
		}
	}
	store := openCheckpoints(cfg, cwd)
	var curPrompt string
	if store != nil {
		a.Env.BeforeMutate = func() {
			id, err := store.Snapshot(context.Background(), "before: "+firstLine(curPrompt))
			if err != nil {
				u.line("  checkpoint failed: " + firstLine(err.Error()))
				return
			}
			sess.AddCheckpoint(id, curPrompt)
		}
	}
	a.Events = agent.Events{
		Text:      u.text,
		ToolStart: func(c provider.ToolCall) { u.line("› " + summarizeCall(c)) },
		ToolDone: func(c provider.ToolCall, out string, err error, d time.Duration) {
			if err != nil {
				u.line(fmt.Sprintf("  ✗ %s: %s", c.Name, firstLine(err.Error())))
			}
		},
		Retry: func(err error, wait time.Duration) {
			u.line(fmt.Sprintf("  retrying in %s: %s", wait, firstLine(err.Error())))
		},
	}

	var active atomic.Pointer[context.CancelFunc]
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
			if c := active.Load(); c != nil {
				(*c)()
				continue
			}
			u.mu.Lock()
			u.endLine()
			u.mu.Unlock()
			os.Exit(130)
		}
	}()

	turn := func(input string) error {
		curPrompt = input
		ctx, cancel := context.WithCancel(context.Background())
		active.Store(&cancel)
		st, err := a.Run(ctx, input)
		active.Store(nil)
		cancel()
		sess.Messages = a.Messages
		_ = sess.Save()
		u.mu.Lock()
		u.endLine()
		u.mu.Unlock()
		if !*quiet {
			fmt.Fprintln(os.Stderr, u.dim(statsLine(st)))
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return err
	}

	if *prompt != "" {
		return turn(*prompt)
	}

	fmt.Fprintln(os.Stderr, u.dim(fmt.Sprintf("agentium %s · %s/%s · %s mode · /exit to quit", version, res.Provider, res.Model, gate.GetMode())))
	for {
		fmt.Fprint(os.Stderr, "\n› ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(os.Stderr)
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if done := slash(line, a, gate, cfg, auth, sess, store); done {
				return nil
			}
			continue
		}
		if err := turn(line); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, u.dim("· interrupted"))
				continue
			}
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

func slash(line string, a *agent.Agent, gate *policy.Gate, cfg config.Config, auth config.Auth, sess *session.Session, store *checkpoint.Store) (exit bool) {
	f := strings.Fields(line)
	switch f[0] {
	case "/exit", "/quit", "/q":
		return true
	case "/clear", "/new":
		a.Reset()
		*sess = *session.New(sess.Cwd, sess.Model)
		fmt.Fprintln(os.Stderr, "· new conversation")
	case "/model":
		if len(f) < 2 {
			fmt.Fprintln(os.Stderr, "· model:", sess.Model)
			return false
		}
		res, err := provider.Resolve(f[1], cfg, auth)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return false
		}
		a.Client, a.Model = res.Client, res.Model
		sess.Model = res.Provider + "/" + res.Model
		fmt.Fprintln(os.Stderr, "· model:", sess.Model)
	case "/mode":
		if len(f) > 1 {
			gate.SetMode(policy.ParseMode(f[1]))
		}
		fmt.Fprintln(os.Stderr, "· mode:", gate.GetMode())
	case "/undo":
		note, err := undoLast(store, sess)
		if err != nil {
			fmt.Fprintln(os.Stderr, "·", err)
			return false
		}
		a.Note = note
		_ = sess.Save()
	case "/usage":
		u := a.Usage
		fmt.Fprintf(os.Stderr, "· %d turn%s · in %s (cached %s) · out %s\n", a.Turns, plural(a.Turns), fmtK(u.Input+u.CacheRead+u.CacheWrite), fmtK(u.CacheRead), fmtK(u.Output))
	default:
		fmt.Fprintln(os.Stderr, "· commands: /undo /clear /model <ref> /mode <ask|auto|yolo> /usage /exit")
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstPositive(ns ...int) int {
	for _, n := range ns {
		if n > 0 {
			return n
		}
	}
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
