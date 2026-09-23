// Command agentium is a fast, minimal coding agent.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/tegarthegreat/agentium/internal/memory"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/sandbox"
	"github.com/tegarthegreat/agentium/internal/session"
	"github.com/tegarthegreat/agentium/internal/tool"
)

var version = "0.6.0"

const usage = `agentium — fast, minimal coding agent

Usage:
  agentium                      interactive session
  agentium "fix the tests"      one-shot task (also: -p "...", or pipe stdin)
  agentium login <provider>     store an API key (OS keychain, else ~/.agentium/auth.json 0600)
  agentium login --oauth openrouter   log in through the browser
  agentium logout <provider>
  agentium providers            list providers and credential status
  agentium models [provider]    list models with context size and price (models.dev)
  agentium undo                 revert the file changes of the last turn here
  agentium tidy [--yes]         consolidate long-term memory (shows a diff first)
  agentium bench [-m model]     measure startup/RAM/prompt; with -m also run live tasks
  agentium version

Flags:
  -m provider/model   model to use (env AGENTIUM_MODEL, config "model")
  -p prompt           one-shot prompt
  -c                  continue the latest session in this directory
  --mode ask|auto|yolo  approvals: every action | risky only (default) | never
  --yolo              same as --mode yolo
  --no-sandbox        run shell commands unconfined
  --effort LEVEL      reasoning effort: low|medium|high|xhigh|max (model default if unset)
  --fast              provider fast mode where available (Claude Opus: up to 2.5x output speed)
  -q                  quiet: no tool lines or stats
  --json              one-shot mode emitting JSON Lines events on stdout (for CI and scripts)
  --max-cost USD      stop once the session has cost this much (needs a known price)
  --best-of N --check CMD   run N attempts in parallel git worktrees, apply the passing one with the smallest diff
  --max-turns N       stop after N model turns (default 100)

In a session: /undo  /sessions  /resume <n>  /clear  /model <ref>  /mode <m>  /usage  /exit
Keys: ↑/↓ history · Ctrl-A/E/U/K/W · paste keeps newlines · end a line with \ for a newline
`

func main() {
	sandbox.MaybeRunHelper()
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
		case "tidy":
			exit(cmdTidy(os.Args[2:]))
			return
		case "models":
			exit(cmdModels(os.Args[2:]))
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
	var js *jsonOut
	if errors.As(err, &js) {
		os.Exit(js.code)
	}
	fmt.Fprintln(os.Stderr, "agentium:", err)
	os.Exit(exitCode(err))
}

// exitCode: 0 ok, 1 error, 2 stopped by a limit or safeguard, 130 interrupted.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return 130
	case errors.Is(err, agent.ErrMaxTurns), errors.Is(err, agent.ErrStuck), errors.Is(err, agent.ErrTruncated),
		errors.Is(err, agent.ErrRefused), errors.Is(err, agent.ErrBudget):
		return 2
	}
	return 1
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// jsonOut carries an exit code for --json runs, whose result was already
// reported on stdout.
type jsonOut struct{ code int }

func (j *jsonOut) Error() string { return fmt.Sprintf("exit %d", j.code) }

type jsonWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newJSONWriter(w io.Writer) *jsonWriter { return &jsonWriter{enc: json.NewEncoder(w)} }

func (j *jsonWriter) emit(v any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.enc.Encode(v)
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
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
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
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
	noSandbox := fs.Bool("no-sandbox", false, "")
	effort := fs.String("effort", "", "")
	asJSON := fs.Bool("json", false, "")
	maxCost := fs.Float64("max-cost", 0, "")
	bestOf := fs.Int("best-of", 0, "")
	check := fs.String("check", "", "")
	fast := fs.Bool("fast", false, "")
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
	if *asJSON && *prompt == "" {
		return errors.New("--json needs a prompt (argument, -p, or stdin)")
	}
	if *asJSON {
		*quiet = true
	}
	u := &ui{quiet: *quiet, color: isTTY(os.Stderr) && os.Getenv("NO_COLOR") == ""}
	in := bufio.NewReader(os.Stdin)
	gate := &policy.Gate{Mode: m, Root: cwd}
	ap := &approver{in: in, ui: u, gate: gate, enable: stdinTTY && !*asJSON}
	gate.Approve = ap.ask

	sess := session.New(cwd, res.Provider+"/"+res.Model)
	go session.Prune(200, 90*24*time.Hour)
	mem := openMemory(cfg, cwd)
	snapshot := ""
	if mem != nil {
		snapshot = mem.store.Snapshot()
	}
	var client provider.Client = res.Client
	var fb *provider.Fallback
	if len(cfg.Fallback) > 0 {
		chain := []provider.Resolved{res}
		for _, ref := range cfg.Fallback {
			if r, err := provider.Resolve(ref, cfg, auth); err == nil {
				chain = append(chain, r)
			} else if !*quiet {
				fmt.Fprintln(os.Stderr, u.dim("· fallback "+ref+" ignored: "+firstLine(err.Error())))
			}
		}
		if len(chain) > 1 {
			fb = &provider.Fallback{Chain: chain, OnSwap: func(from, to provider.Resolved, err error) {
				u.line(fmt.Sprintf("· %s/%s unavailable (%s); switching to %s/%s", from.Provider, from.Model, firstLine(err.Error()), to.Provider, to.Model))
			}}
			client = fb
		}
	}
	tools := tool.All()
	if len(cfg.MCP) > 0 {
		clients := startMCP(cfg.MCP, cwd, func(msg string) {
			if !*quiet {
				fmt.Fprintln(os.Stderr, u.dim("· "+msg))
			}
		})
		defer func() {
			for _, c := range clients {
				c.Close()
			}
		}()
		tools = append(tools, tool.MCPTools(clients)...)
	}
	system := agent.SystemPrompt(cwd, mem != nil, snapshot)
	a := &agent.Agent{
		Client: client, Model: res.Model, System: system,
		Reasoning: res.Reasoning(firstNonEmpty(*effort, cfg.Effort)), FastMode: *fast || cfg.Fast,
		MaxCost: *maxCost,
		Tools:   tools, Env: &tool.Env{Root: cwd, Gate: gate, AllowPrivateNet: cfg.FetchPrivate},
		MaxTurns: firstPositive(*maxTurns, cfg.MaxTurns), MaxTokens: cfg.MaxTokens,
		ContextTokens: firstPositive(cfg.ContextTokens, res.Info.Context, provider.ContextWindow(res.Model)),
		Verify:        cfg.Verify == nil || *cfg.Verify,
	}
	if cfg.FastModel != "" {
		if fr, err := provider.Resolve(cfg.FastModel, cfg, auth); err == nil {
			a.Fast, a.FastModel = fr.Client, fr.Model
		} else if !*quiet {
			fmt.Fprintln(os.Stderr, u.dim("· fast_model ignored: "+firstLine(err.Error())))
		}
	}
	sess.SystemHash = hashString(system)
	if *cont {
		if prev, err := session.Latest(cwd); err == nil && prev != nil {
			sess = prev
			a.Messages = prev.Messages
			a.Note, sess.Note = prev.Note, ""
			if prev.SystemHash != hashString(system) {
				// Memory or instructions changed since: signed thinking
				// blocks from the old prompt would no longer verify.
				for i := range a.Messages {
					a.Messages[i].Raw = nil
				}
			}
			sess.SystemHash = hashString(system)
		}
	}
	boxStatus := setupSandbox(a.Env, cfg, cwd, *noSandbox)
	if !boxStatus.Available && !*quiet && !*noSandbox && sandboxWanted(cfg) {
		fmt.Fprintln(os.Stderr, u.dim("· "+boxStatus.Detail))
	}
	if mem != nil {
		mem.skip = sess.ID
		go mem.buildIndex()
		a.OnRemember = func(fact string) {
			for _, r := range mem.store.Apply([]memory.Directive{{Kind: "remember", Text: fact}}) {
				u.line("· " + r)
			}
		}
	}
	var replies, edited []string
	var stopHooks []string
	if cfg.Hooks != nil {
		a.Env.PostEdit = cfg.Hooks.PostEdit
		stopHooks = cfg.Hooks.Stop
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
	a.Cost = func(us provider.Usage) float64 {
		info, known := res.Info, res.Known
		if fb != nil {
			act := fb.Active()
			info, known = act.Info, act.Known
		}
		if !known {
			return 0
		}
		return info.Price(us.Input, us.Output, us.CacheRead, us.CacheWrite)
	}
	if *maxCost > 0 && !res.Known && !*quiet {
		fmt.Fprintln(os.Stderr, u.dim("· --max-cost: no price known for this model; the limit cannot be enforced"))
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
		Notice: func(msg string) { u.line("· " + msg) },
		TurnFinish: func(r provider.Response) {
			if r.Text != "" {
				replies = append(replies, r.Text)
			}
			for _, c := range r.ToolCalls {
				if c.Name == "edit" {
					var m map[string]any
					if jsonUnmarshal(c.Args, &m) == nil {
						if p, ok := m["path"].(string); ok {
							edited = appendUnique(edited, p)
						}
					}
				}
			}
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
		replies, edited = nil, nil
		send := input
		if mem != nil {
			if block, n := mem.recall(input); n > 0 {
				send = block + "\n\n" + input
				u.line(fmt.Sprintf("· recalled %d item%s from memory", n, plural(n)))
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		active.Store(&cancel)
		st, err := a.Run(ctx, send)
		if mem != nil {
			mem.afterTurn(input, replies, edited, u.line)
		}
		runStopHooks(stopHooks, cwd)
		active.Store(nil)
		cancel()
		sess.Messages = a.Messages
		_ = sess.Save()
		u.mu.Lock()
		u.endLine()
		u.mu.Unlock()
		if !*quiet {
			line := statsLine(st)
			info, known := res.Info, res.Known
			if fb != nil {
				act := fb.Active()
				info, known = act.Info, act.Known
			}
			if known {
				if c := info.Price(st.Usage.Input, st.Usage.Output, st.Usage.CacheRead, st.Usage.CacheWrite); c > 0 {
					line += fmt.Sprintf(" · $%.4f", c)
				}
			}
			fmt.Fprintln(os.Stderr, u.dim(line))
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return err
	}

	if *bestOf > 1 {
		if *prompt == "" {
			return errors.New("--best-of needs a prompt")
		}
		ctx, cancel := context.WithCancel(context.Background())
		active.Store(&cancel)
		defer cancel()
		if store != nil {
			if id, err := store.Snapshot(ctx, "before best-of"); err == nil {
				sess.AddCheckpoint(id, *prompt)
				_ = sess.Save()
			}
		}
		return bestOfN(ctx, *bestOf, *check, *prompt, cwd, res, a, u)
	}
	if *prompt != "" && *asJSON {
		jw := newJSONWriter(os.Stdout)
		jw.emit(map[string]any{"type": "session", "id": sess.ID, "model": res.Provider + "/" + res.Model, "sandbox": a.Env.Sandbox != nil, "mode": string(gate.GetMode())})
		a.Events.Text = func(d string) { jw.emit(map[string]any{"type": "text", "text": d}) }
		a.Events.ToolStart = func(c provider.ToolCall) {
			jw.emit(map[string]any{"type": "tool_call", "id": c.ID, "name": c.Name, "args": c.Args})
		}
		a.Events.ToolDone = func(c provider.ToolCall, out string, err error, d time.Duration) {
			ev := map[string]any{"type": "tool_result", "id": c.ID, "name": c.Name, "ok": err == nil, "ms": d.Milliseconds(), "output": clipText(out, 2000)}
			if err != nil {
				ev["error"] = err.Error()
			}
			jw.emit(ev)
		}
		a.Events.Notice = func(msg string) { jw.emit(map[string]any{"type": "notice", "message": msg}) }
		a.Events.Retry = func(err error, wait time.Duration) {
			jw.emit(map[string]any{"type": "retry", "error": err.Error(), "wait_ms": wait.Milliseconds()})
		}
		t0 := time.Now()
		err := turn(*prompt)
		final := ""
		if len(replies) > 0 {
			final = replies[len(replies)-1]
		}
		result := map[string]any{"type": "result", "ok": err == nil, "text": final, "turns": a.Turns,
			"usage": a.Usage, "cost_usd": a.Spent, "elapsed_ms": time.Since(t0).Milliseconds(), "files_changed": edited}
		if err != nil {
			result["error"] = err.Error()
		}
		jw.emit(result)
		if err != nil {
			return &jsonOut{code: exitCode(err)}
		}
		return nil
	}
	if *prompt != "" {
		return turn(*prompt)
	}

	box := "sandbox off"
	if a.Env.Sandbox != nil {
		box = "sandboxed"
	}
	fmt.Fprintln(os.Stderr, u.dim(fmt.Sprintf("agentium %s · %s/%s · %s mode · %s · /exit to quit", version, res.Provider, res.Model, gate.GetMode(), box)))
	var ed *editor
	if lineEditing && isTTY(os.Stderr) {
		ed = &editor{in: os.Stdin, out: os.Stderr, hist: loadHistory(), prompt: "› "}
	}
	for {
		var line string
		var err error
		if ed != nil {
			fmt.Fprint(os.Stderr, "\n")
			line, err = ed.readLine()
			if errors.Is(err, errInterrupt) || errors.Is(err, errEOF) {
				return nil
			}
			if err != nil { // no raw mode available: fall back to plain input
				ed = nil
				continue
			}
		} else {
			fmt.Fprint(os.Stderr, "\n› ")
			line, err = in.ReadString('\n')
			if err != nil && line == "" {
				fmt.Fprintln(os.Stderr)
				return nil
			}
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
	case "/sessions":
		list, _ := session.ForCwd(sess.Cwd, 10)
		if len(list) == 0 {
			fmt.Fprintln(os.Stderr, "· no saved sessions here")
		}
		for i, ss := range list {
			first := ""
			for _, m := range ss.Messages {
				if m.Role == provider.RoleUser && m.Text != "" {
					first = m.Text
					if j := strings.Index(first, "</recall>"); j >= 0 {
						first = strings.TrimSpace(first[j+9:])
					}
					break
				}
			}
			mark := " "
			if ss.ID == sess.ID {
				mark = "*"
			}
			fmt.Fprintf(os.Stderr, "%s%2d. %s · %d msgs · %s\n", mark, i+1, ss.Updated.Format("2006-01-02 15:04"), len(ss.Messages), oneLine(first, 60))
		}
		fmt.Fprintln(os.Stderr, "· /resume <n> to continue one")
	case "/resume":
		list, _ := session.ForCwd(sess.Cwd, 10)
		n := 1
		if len(f) > 1 {
			fmt.Sscanf(f[1], "%d", &n)
		}
		if n < 1 || n > len(list) {
			fmt.Fprintln(os.Stderr, "· no such session (see /sessions)")
			return false
		}
		chosen := list[n-1]
		hash := sess.SystemHash
		*sess = *chosen
		a.Messages = chosen.Messages
		if chosen.SystemHash != hash {
			for i := range a.Messages {
				a.Messages[i].Raw = nil
			}
		}
		sess.SystemHash = hash
		fmt.Fprintf(os.Stderr, "· resumed session from %s (%d messages)\n", chosen.Updated.Format("2006-01-02 15:04"), len(chosen.Messages))
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
		fmt.Fprintln(os.Stderr, "· commands: /undo /sessions /resume <n> /clear /model <ref> /mode <ask|auto|yolo> /usage /exit")
	}
	return false
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
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
