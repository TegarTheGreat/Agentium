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
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/checkpoint"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/lsp"
	"github.com/tegarthegreat/agentium/internal/memory"
	"github.com/tegarthegreat/agentium/internal/models"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/sandbox"
	"github.com/tegarthegreat/agentium/internal/session"
	"github.com/tegarthegreat/agentium/internal/skill"
	"github.com/tegarthegreat/agentium/internal/tool"
)

var version = "0.26.1"

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
  agentium skills [list|show|add|remove]   manage SKILL.md skills (add: dir, git URL or owner/repo, pinned + reviewed)
  agentium mcp [list|login|logout <name>]   remote MCP servers and their OAuth login
  agentium acp [-m model]       serve the Agent Client Protocol on stdio (Zed, JetBrains)
  agentium bench [-m model]     measure startup/RAM/prompt; with -m also run live tasks
  agentium update               update to the latest release
  agentium version

Flags:
  -m provider/model   model to use (env AGENTIUM_MODEL, config "model")
  -p prompt           one-shot prompt
  -c                  continue the latest session in this directory
  -r, --resume        choose a saved conversation to continue
  --mode ask|auto|yolo|plan  approvals: every action | risky only (default) | never | read-only plan
  --yolo              same as --mode yolo
  --plan              plan mode: read-only investigation that ends in a plan (same as --mode plan)
  --no-sandbox        run shell commands unconfined
  --effort LEVEL      reasoning effort: low|medium|high|xhigh|max (model default if unset)
  --classic           inline terminal UI instead of the full-screen one (config: "ui": "classic")
  --fast              provider fast mode where available (Claude Opus: up to 2.5x output speed)
  -q                  quiet: no tool lines or stats
  --json              one-shot mode emitting JSON Lines events on stdout (for CI and scripts)
  --max-cost USD      stop once the session has cost this much (needs a known price)
  --session ID        continue that conversation (the id --json reports; or a title)
  --add-dir PATH      another working directory the agent may change (repeatable; config "dirs")
  --allow RULE        run this without asking, e.g. "bash(go test*)", "edit(src/**)" (repeatable)
  --deny RULE         never allow this, in any mode, e.g. "bash(rm -rf*)" (repeatable; config "permissions")
  --worktree NAME     work in a separate git worktree (branch agentium/NAME) so sessions don't collide
  --best-of N --check CMD   run N attempts in parallel git worktrees, apply the passing one with the smallest diff
  --max-turns N       stop after N model steps (default: none in a session, 100 for -p)

In a session: /help lists commands and keys (/model /mode /undo /rewind /diff /context /compact /memory /btw …),
  @file mentions a file, !cmd runs a shell command, Esc stops a turn, Enter during a turn steers it.
`

func main() {
	sandbox.MaybeRunHelper()
	defer func() {
		if r := recover(); r != nil {
			restoreTerm()
			if p := writeCrashLog(r); p != "" {
				fmt.Fprintf(os.Stderr, "agentium crashed; details are in %s\n(/bug opens a report: please attach that file)\n", p)
			}
			panic(r)
		}
	}()
	if len(os.Args) > 2 {
		if help, ok := subcommandHelp[os.Args[1]]; ok && wantsHelp(os.Args[2:]) {
			fmt.Println("usage: agentium " + help)
			return
		}
	}
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
			if len(os.Args) > 2 {
				exit(errors.New("undo takes no arguments; it reverts the last turn in this directory"))
				return
			}
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
		case "skills":
			exit(cmdSkills(os.Args[2:]))
			return
		case "acp":
			exit(cmdACP(os.Args[2:]))
			return
		case "mcp":
			exit(cmdMCP(os.Args[2:]))
			return
		case "update", "upgrade":
			exit(cmdUpdate(os.Args[2:]))
			return
		}
	}
	exit(run(os.Args[1:]))
}

func exit(err error) {
	restoreTerm()
	if err == nil {
		return
	}
	var js *jsonOut
	if errors.As(err, &js) {
		os.Exit(js.code)
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "agentium: interrupted")
	} else {
		fmt.Fprintln(os.Stderr, "agentium:", err)
	}
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

// curModel is the session's model, for the status line.
var curModel atomic.Value

func isTTY(f *os.File) bool {
	if fs := activeFS(); fs != nil && f == fs.pw {
		return true
	}
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

// subcommandHelp is the usage line of each subcommand, for -h/--help
// (which must never run the command: undo --help used to undo).
var subcommandHelp = map[string]string{
	"login":     "login [--oauth] <provider>    store credentials for a provider",
	"logout":    "logout <provider>             remove a provider's stored credentials",
	"providers": "providers                     list providers and credential status",
	"models":    "models [provider] [--refresh] list models with context size and price",
	"undo":      "undo                          revert the last turn's changes in this directory",
	"tidy":      "tidy [--yes]                  review and consolidate long-term memory",
	"skills":    "skills [list|show|add|remove] manage skills",
	"mcp":       "mcp [list|login|logout <name>] manage remote MCP servers and their login",
	"acp":       "acp [-m model]                serve the Agent Client Protocol on stdio",
	"bench":     "bench [-m model]              measure performance; with -m, run live tasks",
	"update":    "update [version] [--rollback] update to the latest (or given) release, or go back",
	"upgrade":   "upgrade [version] [--rollback] same as update",
}

// checkEffort validates a reasoning level ("" and "default" mean the
// model's own).
func checkEffort(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "default":
		return "", nil
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return v, nil
	}
	return "", fmt.Errorf("unknown effort %q (low, medium, high, xhigh, max or default)", s)
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" || a == "help" && len(args) == 1 {
			return true
		}
	}
	return false
}

// jsonExitLine finds a bash result's exit status.
var jsonExitLine = regexp.MustCompile(`\[exit (\d+)\]\s*$`)

// onTerminate, when set, runs before exiting on SIGTERM/SIGHUP;
// termSignal is the signal's name once one arrived.
var onTerminate, termSignal atomic.Value

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// ui renders agent events tersely.
type ui struct {
	detach  func() bool // Ctrl-B during a turn: the running command to the background
	mu      sync.Mutex
	quiet   bool
	color   bool
	midLine bool      // stdout has text without a trailing newline
	md      *mdStream // renders Markdown when stdout is a terminal
	wrap    *wrapWriter
	// replyStart: the next reply text opens a new block (◆ marker).
	replyStart bool
	cwd        string

	// Live status area (terminal only): what is running right now.
	live     bool
	thinking bool
	thinkT   time.Time
	tools    []*liveTool
	drawn    int // lines of the live area on screen
	frame    int
	paused   bool // an approval prompt owns the terminal
	// afterTool: the last line printed was a tool line, so a reply
	// starts after a blank line.
	afterTool bool
	held      []string // lines printed while an approval prompt waited
	// approvalWait is the total time spent waiting on approval prompts.
	approvalWait time.Duration
	lastKey      string // the last tool line, for collapsing repeats
	lastPerm     string // the last line printed with permanent (sub-agent repeats)
	subCount     int    // how many times that sub-agent step repeated
	lastCount    int
	lastDur      time.Duration
	outputs      []stepOutput // recent step output, for Ctrl-O
	steer        []string     // typed during a turn, for its next step
	canSteer     bool
	turnStart    time.Time

	// Type-ahead while a turn runs (see typeahead.go).
	keys   chan string
	typing []rune
	queued []string
	cancel func()
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
	u.clearLive()
	u.thinking = false
	u.lastKey, u.lastPerm = "", ""
	if u.afterTool && d != "" {
		os.Stderr.WriteString("\n")
		u.afterTool = false
		u.replyStart = true
	}
	if u.replyStart && u.live && u.wrap != nil && strings.TrimSpace(d) != "" {
		u.wrap.setMargin(2, u.paint(cAccent, "◆")+" ")
		u.replyStart = false
	}
	if u.md != nil {
		u.md.Write(d)
		u.midLine = u.md.Pending()
		return
	}
	os.Stdout.WriteString(d)
	if d != "" {
		u.midLine = !strings.HasSuffix(d, "\n")
	}
}

func (u *ui) endLine() {
	if u.md != nil {
		if u.md.Pending() {
			u.md.Flush() // a tool line mid-reply: keep block state (e.g. a fence)
		}
		u.midLine = false
		return
	}
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
	if u.paused {
		u.held = append(u.held, u.dim(s))
		return
	}
	u.clearLive()
	u.endLine()
	u.lastKey, u.lastPerm = "", ""
	if u.live {
		fmt.Fprintln(os.Stderr, "  "+u.noteLine(s))
	} else {
		fmt.Fprintln(os.Stderr, u.dim(s))
	}
	u.drawLive()
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
	s := pick("cmd", "path", "pattern", "url", "glob", "symbol", "refs", "memory", "search", "query", "title", "prompt")
	if j, ok := m["job"].(float64); ok {
		s = fmt.Sprintf("job %d", int(j))
		if pick("stdin") != "" {
			s += " ← " + pick("stdin")
		} else if m["kill"] == true {
			s += " (stop)"
		}
	}
	if m["background"] == true {
		s += " &"
	}
	if c.Name == "search" && m["glob"] != nil && m["pattern"] != nil {
		s = fmt.Sprintf("%v in %v", m["pattern"], m["glob"])
	}
	if strings.TrimSpace(s) == "" && len(c.Args) > 2 {
		// Unexpected argument shape: show it rather than a bare name.
		s = string(c.Args)
	}
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 100 {
		s = strings.ToValidUTF8(s[:100], "") + "…" // not in the middle of a character
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
	always map[string]bool // scopes approved with "always"
	saved  map[string]bool // scopes approved for this project, kept on disk
	notes  []string        // said while approving ("c"), for the next step
	// feedback is what the user said when declining, by action.
	feedback map[string]string
}

// takeNotes returns what was said while approving, once.
func (a *approver) takeNotes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.notes
	a.notes = nil
	return n
}

// takeFeedback returns the user's reason for refusing action, once.
func (a *approver) takeFeedback(action string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	fb := a.feedback[action]
	delete(a.feedback, action)
	return fb
}

func (a *approver) ask(action, reason string) bool {
	if !a.enable {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key, scope := alwaysScope(action, reason, a.gate.Root)
	if a.gate.GetMode() == policy.Yolo || a.always[key] || a.saved[key] { // "always" chosen earlier
		return true
	}
	remember := func() {
		if a.always == nil {
			a.always = map[string]bool{}
		}
		a.always[key] = true
	}
	keep := func() {
		remember()
		if a.saved == nil {
			a.saved = map[string]bool{}
		}
		a.saved[key] = true
		if err := saveApprovals(a.gate.Root, a.saved); err != nil {
			fmt.Fprintln(os.Stderr, a.ui.dim("· could not save the approval: "+err.Error()))
		}
	}
	if a.ui.live && lineEditing {
		if k, err := a.ui.approve(action, reason, scope, keepable(key)); err == nil {
			switch {
			case k == "a" || k == "p" && !keepable(key):
				remember()
			case k == "p":
				keep()
			}
			if note, ok := strings.CutPrefix(k, "c:"); ok {
				// Approved with a note: it reaches the agent that asked,
				// at its next step.
				if note = strings.TrimSpace(note); note != "" {
					a.notes = append(a.notes, note)
				}
				return true
			}
			if fb, ok := strings.CutPrefix(k, "t:"); ok {
				if a.feedback == nil {
					a.feedback = map[string]string{}
				}
				a.feedback[action] = fb
			}
			return k == "y" || k == "a" || k == "p"
		}
	}
	a.ui.mu.Lock()
	a.ui.endLine()
	keepHint := ""
	if keepable(key) {
		keepHint = " / [p] always, in this project"
	}
	fmt.Fprintf(os.Stderr, "⚠ %s  (%s)\n  allow? [y]es / [N]o / [a]lways %s%s: ", action, reason, scope, keepHint)
	a.ui.mu.Unlock()
	line, _ := a.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "a", "always":
		remember()
		return true
	case "p":
		if keepable(key) {
			keep()
		} else {
			remember()
		}
		return true
	}
	return false
}

// alwaysScope is what "always" approves for the rest of the session: the
// same program for simple commands, file changes, the same host for
// fetches. A compound command, or one run through a wrapper that could
// run anything (sudo, sh -c, xargs, env …), is approved only verbatim, so
// approving "cd x && go test" never approves "cd x && rm -rf ~".
func alwaysScope(action, reason, root string) (key, label string) {
	kind, rest, _ := strings.Cut(action, ": ")
	switch kind {
	case "bash", "network":
		cmd := strings.TrimSpace(rest)
		if root != "" {
			cmd = strings.TrimPrefix(cmd, "cd "+root+" && ")
		}
		prog := ""
		if f := strings.Fields(cmd); len(f) > 0 {
			prog = f[0]
		}
		// Broad only for an ordinary command in ask mode: a bare program
		// name (not ./go or /tmp/x/go, a binary the agent may have
		// written), no VAR= prefix (LD_PRELOAD=…, GIT_SSH_COMMAND=…), no
		// shell syntax, no wrapper. A command flagged for a reason of its
		// own (force push, rm -rf …) is approved verbatim, so approving
		// "git status" never approves "git push --force".
		if reason != "ask mode" || strings.ContainsAny(cmd, ";&|`$<>\n()'\"\\") || wrapperCommands[prog] ||
			prog == "" || strings.ContainsAny(prog, "/=") {
			return kind + "=" + cmd, "for this exact command"
		}
		if kind == "network" {
			// Network access is approved per exact command: one program
			// (curl, npm) can reach any host.
			return kind + "=" + cmd, "for this exact command"
		}
		return kind + ":" + prog, "for `" + prog + "`"
	case "write":
		if reason != "" && reason != "ask mode" {
			// Outside the workspace, git internals …: only this file.
			return "write=" + rest, "for this file"
		}
		return "write", "for file changes"
	case "read":
		return "read:" + rest, "for this file"
	case "fetch":
		if strings.HasPrefix(rest, "search: ") {
			return "fetch:search", "for web searches"
		}
		host := rest
		if u, err := url.Parse(rest); err == nil && u.Host != "" {
			host = u.Host
		}
		return "fetch:" + host, "for " + host
	}
	return action, "for this"
}

// wrapperCommands run another command given as arguments.
var wrapperCommands = map[string]bool{"cd": true, "sudo": true, "doas": true, "su": true, "env": true, "sh": true,
	"bash": true, "zsh": true, "dash": true, "fish": true, "xargs": true, "npx": true, "bunx": true, "pnpx": true,
	"timeout": true, "nice": true, "nohup": true, "exec": true, "eval": true, "command": true, "time": true,
	"watch": true, "find": true, "ssh": true, "docker": true, "kubectl": true, "python": true, "python3": true,
	"node": true, "perl": true, "ruby": true, "busybox": true, "setsid": true, "stdbuf": true, "chroot": true}

func run(args []string) error {
	defer sandbox.CleanupTemp()
	fs := flag.NewFlagSet("agentium", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	modelRef := fs.String("m", "", "")
	prompt := fs.String("p", "", "")
	cont := fs.Bool("c", false, "")
	resumePick := fs.Bool("resume", false, "")
	sessionArg := fs.String("session", "", "")
	fs.BoolVar(resumePick, "r", false, "")
	mode := fs.String("mode", "", "")
	yolo := fs.Bool("yolo", false, "")
	plan := fs.Bool("plan", false, "")
	quiet := fs.Bool("q", false, "")
	maxTurns := fs.Int("max-turns", 0, "")
	noSandbox := fs.Bool("no-sandbox", false, "")
	effort := fs.String("effort", "", "")
	asJSON := fs.Bool("json", false, "")
	maxCost := fs.Float64("max-cost", 0, "")
	bestOf := fs.Int("best-of", 0, "")
	check := fs.String("check", "", "")
	fast := fs.Bool("fast", false, "")
	classic := fs.Bool("classic", false, "")
	worktree := fs.String("worktree", "", "")
	var addDirs dirList
	fs.Var(&addDirs, "add-dir", "")
	var allowRules, denyRules dirList
	fs.Var(&allowRules, "allow", "")
	fs.Var(&denyRules, "deny", "")
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
	reduceMotion = cfg.ReduceMotion || os.Getenv("AGENTIUM_REDUCE_MOTION") != ""
	auth, err := config.LoadAuth()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	exportSearchKeys(auth)
	ref := firstNonEmpty(*modelRef, os.Getenv("AGENTIUM_MODEL"), cfg.Model)
	res, err := provider.Resolve(ref, cfg, auth)
	if err != nil && stdinTTY && isTTY(os.Stderr) && !*asJSON && lineEditing && !dumbTerm() {
		// First run (or a missing key): set up a provider right here.
		su := &ui{color: os.Getenv("NO_COLOR") == ""}
		fmt.Fprintln(os.Stderr, "\n"+su.paint(cCyan, "◆")+" "+su.paint(cBold, "Welcome to Agentium"))
		if !strings.HasPrefix(err.Error(), "no model configured") {
			su.note(firstLine(err.Error()))
		}
		su.note("Let's connect a model. Your key is stored in the OS keychain or ~/.agentium/auth.json (0600).")
		pid := ""
		if *modelRef != "" {
			pid, _, _ = strings.Cut(*modelRef, "/")
			if _, ok := provider.Specs(cfg)[pid]; !ok {
				pid = ""
			}
		}
		newRef, serr := su.setup(pid)
		if serr != nil {
			if errors.Is(serr, errCanceled) {
				return errors.New("setup canceled; run agentium again or use `agentium login <provider>`")
			}
			return serr
		}
		su.success("Using " + newRef + " (saved as your default)")
		if auth, err = config.LoadAuth(); err != nil {
			return err
		}
		if *modelRef != "" && strings.Contains(*modelRef, "/") {
			newRef = *modelRef
		}
		res, err = provider.Resolve(newRef, cfg, auth)
	}
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
	startDir := cwd // relative --add-dir paths mean the folder agentium started in
	if *worktree != "" {
		dir, done, err := enterWorktree(cwd, *worktree)
		if err != nil {
			return err
		}
		if err := os.Chdir(dir); err != nil {
			return err
		}
		cwd = dir
		fmt.Fprintln(os.Stderr, "· working in worktree "+dir)
		defer func() {
			if msg := done(); msg != "" {
				fmt.Fprintln(os.Stderr, "· "+msg)
			}
		}()
	}

	if _, err := checkEffort(firstNonEmpty(*effort, cfg.Effort)); err != nil {
		return fmt.Errorf("--effort: %w", err)
	}
	m, err := policy.CheckMode(firstNonEmpty(*mode, cfg.Mode))
	if err != nil {
		if *mode == "" {
			return fmt.Errorf("config: %w", err)
		}
		return fmt.Errorf("--mode: %w", err)
	}
	if *yolo {
		m = policy.Yolo
	}
	if *plan {
		m = policy.Plan
	}
	if *asJSON && *prompt == "" {
		return errors.New("--json needs a prompt (argument, -p, or stdin)")
	}
	if *asJSON {
		*quiet = true
	}
	if *prompt == "" && !*asJSON && !*quiet && isTTY(os.Stderr) && os.Getenv("NO_COLOR") == "" {
		applyTheme(firstNonEmpty(os.Getenv("AGENTIUM_THEME"), cfg.Theme))
	}
	var screen *fullscreen
	if *prompt == "" && !*asJSON && !*quiet && *bestOf <= 1 && fullscreenWanted(*classic || strings.EqualFold(cfg.UI, "classic")) {
		if screen, err = enterFullscreen(cfg.Mouse == nil || *cfg.Mouse); err == nil {
			defer screen.leave()
		}
	}
	// Until the session's own handler is installed, a signal must still
	// give the terminal back.
	early := make(chan os.Signal, 1)
	signal.Notify(early, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if _, ok := <-early; ok {
			restoreTerm()
			os.Exit(130)
		}
	}()
	u := &ui{quiet: *quiet, color: isTTY(os.Stderr) && os.Getenv("NO_COLOR") == "" && !dumbTerm()}
	u.live = isTTY(os.Stderr) && !*quiet && !dumbTerm()
	if !*asJSON && isTTY(os.Stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("AGENTIUM_RAW") == "" {
		u.wrap = newWrap(os.Stdout, func() int { return termWidth(os.Stdout) - 1 })
		u.md = newMD(u.wrap)
	}
	in := bufio.NewReader(os.Stdin)
	gate := &policy.Gate{Mode: m, Root: cwd}
	gate.SetRules(cfg.Permissions)
	gate.SetRules(policy.Rules{Allow: allowRules, Deny: denyRules})
	broadRoot := tooBroad(cwd)
	if broadRoot {
		// The home folder (or /) as the project would make the sandbox and
		// the workspace boundary cover everything, and the undo snapshot
		// would copy the whole disk.
		if *mode == "" || policy.ParseMode(*mode) == policy.Auto {
			gate.Mode = policy.Ask
		}
		gate.Protected = append(gate.Protected, sensitivePaths()...)
		if !*quiet {
			fmt.Fprintln(os.Stderr, u.paint(cYellow, "· "+shortPath(cwd)+" holds your home folder or settings: every change asks here, and undo is off. Start agentium in a project folder to work normally."))
		}
	}
	extraDirs, err := resolveDirs(addDirs, startDir)
	if err != nil {
		return err
	}
	// "dirs" in the config apply to every project: absolute paths only,
	// and one that is missing here is skipped, not fatal.
	for _, d := range cfg.Dirs {
		if !filepath.IsAbs(d) && d != "~" && !strings.HasPrefix(d, "~/") {
			fmt.Fprintln(os.Stderr, u.dim("· \"dirs\": "+d+" ignored (use an absolute or ~/ path)"))
			continue
		}
		if r, err := resolveDirs([]string{d}, startDir); err == nil {
			extraDirs = append(extraDirs, r...)
		} else if !*quiet {
			fmt.Fprintln(os.Stderr, u.dim("· "+err.Error()))
		}
	}
	gate.Protected = gitProtected(cwd)
	for _, d := range extraDirs {
		gate.AddDir(d)
		gate.Protected = append(gate.Protected, gitProtected(d)...)
	}
	ap := &approver{in: in, ui: u, gate: gate, enable: stdinTTY && !*asJSON, saved: loadApprovals(cwd)}
	if ap.enable {
		gate.Approve, gate.Feedback = ap.ask, ap.takeFeedback
	}

	sess := session.New(cwd, res.Provider+"/"+res.Model)
	curModel.Store(sess.Model)
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
	if ap.enable && u.live && lineEditing && *prompt == "" {
		tools = append(tools, tool.AskTool) // someone is there to answer
	}
	var mcps *mcpState
	if len(cfg.MCP) > 0 {
		mcps = startMCP(cfg.MCP, cwd, func(msg string) {
			if !*quiet {
				fmt.Fprintln(os.Stderr, u.dim("· "+msg))
			}
		})
		defer func() {
			for _, c := range mcps.clients {
				c.Close()
			}
		}()
		tools = append(tools, tool.MCPTools(mcps.clients)...)
	}
	skills := skill.Discover(config.Home(), cwd)
	if !*quiet {
		for _, sh := range skill.Shadowed(config.Home(), cwd) {
			fmt.Fprintln(os.Stderr, u.dim("· "+sh))
		}
	}
	var skillBox atomic.Value // for the composer, which may run during a turn
	skillBox.Store(skills)
	sysHead, sysTail := agent.SystemPrompt(cwd, mem != nil, snapshot)+selfPrompt(cwd)+
		"\n- This session started on the model "+res.Provider+"/"+res.Model+" (the user may switch with /model; the status bar shows the current one).", dirsPrompt(extraDirs)
	baseSystem := sysHead + skill.Prompt(skills) + sysTail
	system := baseSystem + stylePrompt(cfg.Style)
	a := &agent.Agent{
		Client: client, Model: res.Model, System: system,
		Reasoning: res.Reasoning(firstNonEmpty(*effort, cfg.Effort)), FastMode: *fast || cfg.Fast,
		MaxCost: *maxCost,
		Tools:   tools, Env: &tool.Env{Root: cwd, Gate: gate, AllowPrivateNet: cfg.FetchPrivate, Vision: res.Vision(), CodeCache: codeCache(cwd)},
		MaxTurns: firstPositive(*maxTurns, cfg.MaxTurns), MaxTokens: cfg.MaxTokens, MaxOutput: res.Info.Output,
		ContextTokens: firstPositive(cfg.ContextTokens, res.Info.Context, provider.ContextWindow(res.Model)),
		Verify:        cfg.Verify == nil || *cfg.Verify,
	}
	u.detach = a.Env.DetachForeground
	// Output that cannot be taken back (a pipe, a file) gets each reply
	// once, not again after a retried call.
	a.BufferText = !isTTY(os.Stdout) && !*asJSON
	agentFiles := loadAgents(cwd)
	for _, f := range agentFiles {
		def := f.def
		if f.model != "" {
			c := cfg
			c.SubagentModel = f.model
			m, known, err := subModel(c, auth, firstNonEmpty(*effort, cfg.Effort))
			switch {
			case err != nil:
				fmt.Fprintln(os.Stderr, u.dim("· agent "+def.Name+": model ignored: "+firstLine(err.Error())))
			case *maxCost > 0 && !known:
				fmt.Fprintln(os.Stderr, u.dim("· agent "+def.Name+": model "+f.model+" has no known price; --max-cost could not count it, so it uses the main model"))
			default:
				def.Model = m
			}
		}
		a.Agents = append(a.Agents, def)
	}
	a.Tools = append(a.Tools, a.TodoTool(), a.TaskTool())
	if ap.enable && u.live && lineEditing && *prompt == "" {
		a.Env.Ask = func(q string, opts []string) (string, error) {
			ap.mu.Lock() // one question on screen at a time, approvals included
			defer ap.mu.Unlock()
			return u.askUser(q, opts)
		}
	}
	a.Notes = ap.takeNotes
	defer a.Env.KillJobs() // background servers do not outlive the session
	if cfg.LSP == nil || *cfg.LSP {
		a.Env.LSP = lsp.NewManager(cwd, policy.ScrubEnv(os.Environ(), nil))
		defer a.Env.LSP.Close()
	}
	if mem != nil {
		a.Env.Recall = mem.search
		if stale := mem.store.Stale(); len(stale) > 0 && !*quiet {
			fmt.Fprintln(os.Stderr, u.dim(fmt.Sprintf("· memory: %d stale note(s) hidden (cited files gone or unconfirmed for months); `agentium tidy` reviews them", len(stale))))
		}
	}
	if cfg.SubagentModel != "" {
		if sub, known, err := subModel(cfg, auth, firstNonEmpty(*effort, cfg.Effort)); err == nil {
			a.Sub = sub
			if *maxCost > 0 && !known && !*quiet {
				fmt.Fprintln(os.Stderr, u.dim("· --max-cost: no price known for subagent_model; its spending is not counted"))
			}
		} else if !*quiet {
			fmt.Fprintln(os.Stderr, u.dim("· subagent_model ignored: "+firstLine(err.Error())))
		}
	}
	if cfg.OracleModel != "" {
		if o, err := oracleModel(cfg, auth); err == nil {
			a.Oracle = o
			a.Tools = append(a.Tools, a.OracleTool())
		} else if !*quiet {
			fmt.Fprintln(os.Stderr, u.dim("· oracle_model ignored: "+firstLine(err.Error())))
		}
	}
	if cfg.FastModel != "" {
		if fr, err := provider.Resolve(cfg.FastModel, cfg, auth); err == nil {
			a.Fast, a.FastModel = fr.Client, fr.Model
		} else if !*quiet {
			fmt.Fprintln(os.Stderr, u.dim("· fast_model ignored: "+firstLine(err.Error())))
		}
	}
	sess.SystemHash = hashString(system)
	if *cont || *sessionArg != "" {
		var prev *session.Session
		var err error
		if *sessionArg != "" {
			// A given conversation (the id --json reports), for scripts and CI.
			list, _ := session.ForCwd(cwd, 500)
			for _, ss := range list {
				if ss.ID == *sessionArg {
					prev = ss
				}
			}
			if prev == nil {
				prev = findSession(list, *sessionArg)
			}
			if prev == nil {
				return fmt.Errorf("no saved conversation %q in this folder (/sessions inside agentium lists them)", *sessionArg)
			}
		} else {
			prev, err = session.Latest(cwd)
		}
		if err == nil && prev != nil {
			sess = prev
			a.Messages = provider.CloseToolCalls(prev.Messages)
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
		for _, c := range session.TakeCorrupt() {
			fmt.Fprintln(os.Stderr, u.paint(cYellow, "· a saved conversation could not be read and was skipped: "+sanitize(oneLine(c, 160))))
		}
	}
	// One process writes a conversation at a time: one already open in
	// another agentium continues here in a copy (the two would overwrite
	// each other's turns).
	if release, ok := sess.Own(); ok {
		defer release()
	} else {
		fork := *sess
		fork.ID = session.New(sess.Cwd, sess.Model).ID
		fork.Messages = append([]provider.Message(nil), sess.Messages...)
		fork.Checkpoints = append([]session.Checkpoint(nil), sess.Checkpoints...)
		*sess = fork
		if release, ok := sess.Own(); ok {
			defer release()
		}
		if !*quiet {
			fmt.Fprintln(os.Stderr, u.paint(cYellow, "· that conversation is open in another agentium: continuing in a copy"))
		}
	}
	boxStatus := setupSandbox(a.Env, cfg, cwd, *noSandbox)
	gate.Unconfined = a.Env.Sandbox == nil // then auto mode asks before commands that change things
	if a.Env.Sandbox != nil {
		a.Env.Sandbox.Write = append(a.Env.Sandbox.Write, extraDirs...)
	}
	if !*quiet && !*noSandbox && sandboxWanted(cfg) && (!boxStatus.Available || !boxStatus.Network) {
		// Say plainly what is not enforced (e.g. network on kernels < 6.7).
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
	var replies []string
	var stopHooks, userPromptHooks []string
	startContext := ""
	if cfg.Hooks != nil {
		a.Env.PostEdit = cfg.Hooks.PostEdit
		stopHooks = cfg.Hooks.Stop
		userPromptHooks = cfg.Hooks.UserPrompt
		hookWarn := func(s string) { u.line("· " + firstLine(s)) }
		a.PreTool = preToolHook(cfg.Hooks.PreTool, cwd, hookWarn)
		if len(cfg.Hooks.SessionStart) > 0 {
			startContext = sessionHooks(cfg.Hooks.SessionStart, cwd, hookWarn)
		}
	}
	store := openCheckpoints(cfg, cwd)
	if broadRoot {
		store = nil
	}
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
		a.Env.BeforeWrite = func(path string) {
			if n := len(sess.Checkpoints); n > 0 {
				store.KeepOriginal(sess.Checkpoints[n-1].ID, path)
			}
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
	// Memory directives in a reply are for agentium (it reports what it
	// remembered), not shown as part of the answer.
	directives := newDirectiveFilter(u.text)
	a.Events = agent.Events{
		Text:         directives.write,
		ToolStart:    func(c provider.ToolCall) { u.line("› " + summarizeCall(c)) },
		SubToolStart: func(c provider.ToolCall) { u.line("  ↳ " + summarizeCall(c)) },
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
			directives.flush()
			if u.md != nil {
				u.mu.Lock()
				u.md.End() // a reply is over: its unclosed fence must not leak
				u.mu.Unlock()
			}
			if r.Text != "" {
				replies = append(replies, r.Text)
			}
			if t := strings.TrimSpace(firstNonEmpty(r.Thought, r.Reasoning)); t != "" {
				u.keepThought(t) // ctrl+o shows it
			}
		},
	}

	if u.live {
		u.cwd = cwd
		a.Events.ToolStart = u.toolStart
		a.Events.ToolDone = u.toolDone
		a.Events.ToolOutput = u.toolOutput
		a.Events.Notice = func(msg string) { u.line("· " + msg); u.think() }
		a.Events.SubToolStart, a.Events.SubAgentTool = nil, u.subAgentTool
		u.canSteer = true
		a.Steer = u.takeSteer
		if len(userPromptHooks) > 0 {
			// Messages sent mid-turn pass the user_prompt hooks too.
			a.Steer = func() []string {
				var out []string
				for _, m := range u.takeSteer() {
					added, err := promptHooks(userPromptHooks, cwd, m, func(s string) { u.line("· " + firstLine(s)) })
					if err != nil {
						u.line("· not sent: " + err.Error())
						continue
					}
					if added != "" {
						m = "<hook-context>\n" + added + "\n</hook-context>\n\n" + m
					}
					out = append(out, m)
				}
				return out
			}
		}
		u.startTicker()
	}

	var active atomic.Pointer[context.CancelFunc]
	var turnDone atomic.Pointer[chan struct{}] // closed when the running turn returns
	var lastStepSave time.Time
	a.OnStep = func() {
		// Save as the turn goes (at most every few seconds), so a crash
		// or a closed terminal keeps the finished steps.
		if time.Since(lastStepSave) < 3*time.Second {
			return
		}
		lastStepSave = time.Now()
		sess.Messages = a.Messages
		saveSession(sess)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	signal.Stop(early)
	close(early)
	go func() {
		for s := range sig {
			if s != os.Interrupt {
				name := "SIGTERM"
				if s == syscall.SIGHUP {
					name = "SIGHUP"
				}
				termSignal.Store(name)
				// Terminated or the terminal closed: stop the turn (and the
				// command it runs), keep the conversation, leave the
				// terminal usable.
				if c := active.Load(); c != nil {
					(*c)()
					if d := turnDone.Load(); d != nil {
						select {
						case <-*d:
						case <-time.After(3 * time.Second):
						}
					}
				}
				sess.Messages = provider.CloseToolCalls(a.Messages)
				saveSession(sess)
				if f, ok := onTerminate.Load().(func(os.Signal)); ok {
					f(s)
				}
				restoreTerm()
				a.Env.KillJobs()
				code := 128 + 15
				if s == syscall.SIGHUP {
					code = 128 + 1
				}
				os.Exit(code)
			}
			if c := active.Load(); c != nil {
				(*c)()
				continue
			}
			u.mu.Lock()
			u.endLine()
			u.mu.Unlock()
			restoreTerm()
			a.Env.KillJobs()
			os.Exit(130)
		}
	}()

	interactive := false // type-ahead only where queued input gets sent
	sug := &suggester{}
	suggestOn := cfg.Suggest != nil && *cfg.Suggest ||
		cfg.Suggest == nil && (cfg.FastModel != "" || res.Known && res.Info.Price(1_000_000, 0, 0, 0) < 1)
	// Session totals for the full-screen status bar.
	var totalTok atomic.Int64
	var totalCost atomic.Uint64 // float64 bits
	// oneShotModel applies a custom command's model: to agentium -p /cmd
	// (yours only: a repository's cannot ask here).
	oneShotModel := func() func() {
		c, ok := commandModel(cwd, *prompt)
		if !ok {
			return func() {}
		}
		if !c.personal {
			u.note("/" + sanitize(c.name) + " is the repository's: its model is not used here")
			return func() {}
		}
		r, err := useModelOnce(c.model, cfg, &res, a, fb, *maxCost)
		if err != nil {
			u.note("model " + sanitize(c.model) + " not used: " + firstLine(err.Error()))
			return func() {}
		}
		return r
	}
	turn := func(input string) error {
		curPrompt = input
		replies = nil
		send := input
		if imgs, notes := mentionedImages(input, cwd, a.Env.Vision, gate.Inside); len(imgs)+len(notes) > 0 {
			a.Attach = imgs
			for _, n := range notes {
				u.line("· " + n)
			}
		}
		if msg, ok := expandCommandShell(cwd, input, func(c userCmd, cmds []string) []string {
			outs, _ := commandShell(u, c, cmds, cwd, false)
			return outs
		}); ok && !interactive && !skillCall(skills, input) {
			if msg == "" {
				return fmt.Errorf("%s is an empty command file", strings.Fields(input)[0])
			}
			send = msg // agentium -p /review, and the like
		}
		if msg, ok, err := skill.Invoke(skills, input); ok {
			if err != nil {
				a.Attach = nil // don't carry this turn's images into the next
				return err
			}
			send = msg
		}
		if mem != nil {
			if block, n := mem.recall(input); n > 0 {
				send = block + "\n\n" + send
				u.line(fmt.Sprintf("· recalled %d item%s from memory", n, plural(n)))
			}
		}
		if strings.HasPrefix(input, "[/watch]") {
			// Comments from files: their @words are not the user's mentions.
		} else if block, names := mentionedFiles(input, cwd, func(p string) bool {
			// Workspace files only (a pasted "@~/.bash_history" is not
			// sent), and credential files still ask.
			ok, _ := gate.Read(p)
			return ok && gate.Inside(p)
		}); block != "" {
			send = block + "\n" + send
			u.line("· included " + strings.Join(names, ", "))
		}
		if len(userPromptHooks) > 0 {
			added, err := promptHooks(userPromptHooks, cwd, input, func(s string) { u.line("· " + firstLine(s)) })
			if err != nil {
				u.line("· not sent: " + err.Error())
				a.Attach = nil
				return nil
			}
			if added != "" {
				send = "<hook-context>\n" + added + "\n</hook-context>\n\n" + send
			}
		}
		if startContext != "" {
			send = "<session-start>\n" + startContext + "\n</session-start>\n\n" + send
			startContext = ""
		}
		ctx, cancel := context.WithCancel(context.Background())
		active.Store(&cancel)
		done := make(chan struct{})
		turnDone.Store(&done)
		defer close(done)
		u.beginTurn()
		stopTyping := func() {}
		if interactive {
			stopTyping = u.startTyping(cancel)
		}
		checkpoints := len(sess.Checkpoints)
		sug.clear()
		a.Typed = input // kept with the message (/rewind puts it back)
		spentBefore := a.Spent
		st, err := a.Run(ctx, send)
		if errors.Is(err, agent.ErrMaxTurns) && interactive && ctx.Err() == nil {
			// Out of steps mid-task: say where things stand, so "continue"
			// picks up from a clear point.
			steps := a.MaxTurns
			if steps <= 0 {
				steps = 100
			}
			u.line(u.paint(cYellow, fmt.Sprintf("%d steps used for this message; asking for a status report", steps)))
			_ = a.WrapUp(ctx, steps)
		}
		// What this turn cost, sub-agents on their own model included.
		cost := a.Spent - spentBefore
		totalTok.Store(int64(a.Usage.Input + a.Usage.CacheRead + a.Usage.CacheWrite + a.Usage.Output))
		totalCost.Store(math.Float64bits(a.Spent))
		stopTyping()
		u.endTurn()
		if store != nil && len(sess.Checkpoints) > checkpoints {
			// Remember where this turn's changes end, so undo reverts only
			// them and keeps later edits by the user.
			if after, serr := store.Snapshot(context.Background(), "after: "+firstLine(input)); serr == nil {
				sess.EndCheckpoint(after)
			}
		}
		if mem != nil {
			mem.afterTurn(input, replies, a.Ledger.TurnEdited(), a.Ledger.TurnErrors(), a.Ledger.Lessons(), a.Ledger.Untrusted(), u.line)
		}
		runStopHooks(stopHooks, cwd)
		active.Store(nil)
		cancel()
		sess.Messages = a.Messages
		saveSession(sess)
		u.mu.Lock()
		u.endLine()
		u.mu.Unlock()
		if err == nil && interactive && suggestOn && u.live && len(replies) > 0 {
			if !modelSwapped.Load() {
				sug.guess(a, input, replies[len(replies)-1])
			}
		}
		if !*quiet && u.live {
			fmt.Fprintln(os.Stderr, u.turnSummary(st, err, cost))
		} else if !*quiet {
			line := statsLine(st)
			if cost > 0 {
				line += fmt.Sprintf(" · $%.4f", cost)
			}
			fmt.Fprintln(os.Stderr, u.dim(line))
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return err
	}

	if *bestOf > 1 && len(extraDirs) > 0 {
		fmt.Fprintln(os.Stderr, u.dim("· --best-of works in the main folder only; added directories are not part of the attempts"))
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
				saveSession(sess)
			}
		}
		return bestOfN(ctx, *bestOf, *check, *prompt, cwd, res, a, u)
	}
	if *prompt != "" && *asJSON {
		defer oneShotModel()() // before the session event: it names the model
		jw := newJSONWriter(os.Stdout)
		jw.emit(map[string]any{"type": "session", "id": sess.ID, "model": res.Provider + "/" + res.Model, "sandbox": a.Env.Sandbox != nil, "mode": string(gate.GetMode())})
		// Text streamed by an attempt that then fails is sent again by the
		// retry: the retry event says to drop it (discard_text).
		var partial atomic.Bool
		a.Events.Text = func(d string) {
			partial.Store(true)
			jw.emit(map[string]any{"type": "text", "text": d})
		}
		a.Events.TurnFinish = func(prev func(provider.Response)) func(provider.Response) {
			return func(r provider.Response) {
				partial.Store(false)
				if prev != nil {
					prev(r)
				}
			}
		}(a.Events.TurnFinish)
		a.Events.ToolStart = func(c provider.ToolCall) {
			jw.emit(map[string]any{"type": "tool_call", "id": c.ID, "name": c.Name, "args": c.Args})
		}
		a.Events.SubToolStart = func(c provider.ToolCall) {
			jw.emit(map[string]any{"type": "tool_call", "id": c.ID, "name": c.Name, "args": c.Args, "subagent": true})
		}
		a.Events.ToolDone = func(c provider.ToolCall, out string, err error, d time.Duration) {
			ev := map[string]any{"type": "tool_result", "id": c.ID, "name": c.Name, "ok": err == nil, "ms": d.Milliseconds(), "output": clipText(out, 2000)}
			if m := jsonExitLine.FindStringSubmatch(out); m != nil && c.Name == "bash" {
				code, _ := strconv.Atoi(m[1])
				ev["exit"], ev["ok"] = code, err == nil && code == 0
			}
			if err != nil {
				ev["error"] = err.Error()
			}
			jw.emit(ev)
		}
		a.Events.Notice = func(msg string) { jw.emit(map[string]any{"type": "notice", "message": msg}) }
		a.Events.Retry = func(err error, wait time.Duration) {
			jw.emit(map[string]any{"type": "retry", "error": err.Error(), "wait_ms": wait.Milliseconds(), "discard_text": partial.Swap(false)})
		}
		t0 := time.Now()
		cpBefore := len(sess.Checkpoints) // the run's changes are those since its first checkpoint
		var once sync.Once
		emitResult := func(err error, final string) {
			once.Do(func() {
				if sig, ok := termSignal.Load().(string); ok {
					err = errors.New("terminated by " + sig)
				}
				us, spent := a.Totals()
				result := map[string]any{"type": "result", "ok": err == nil, "text": final, "turns": a.Turns,
					"usage": us, "cost_usd": spent, "elapsed_ms": time.Since(t0).Milliseconds(), "files_changed": filesChanged(cwd, a.Ledger.TurnEdited(), store, sess, cpBefore)}
				if err != nil {
					result["error"] = err.Error()
				}
				// A script resuming this run with --session must know when
				// there is nothing to resume.
				if e := saveError(); e != "" {
					result["save_error"] = e
				}
				jw.emit(result)
			})
		}
		// Killed (a CI timeout, SIGTERM): still report what was spent.
		onTerminate.Store(func(os.Signal) { emitResult(nil, "") })
		err := turn(*prompt)
		final := ""
		if len(replies) > 0 {
			final = replies[len(replies)-1]
		}
		emitResult(err, final)
		if err != nil {
			return &jsonOut{code: exitCode(err)}
		}
		return nil
	}
	if *prompt != "" {
		defer oneShotModel()()
		return turn(*prompt)
	}

	box := "sandbox off"
	if a.Env.Sandbox != nil {
		box = "sandboxed"
	}
	if screen != nil {
		a.Events.TurnFinish = func(prev func(provider.Response)) func(provider.Response) {
			return func(r provider.Response) {
				if prev != nil {
					prev(r)
				}
				// Sub-agents add their usage only between these calls.
				totalTok.Store(int64(a.Usage.Input + a.Usage.CacheRead + a.Usage.CacheWrite + a.Usage.Output))
				totalCost.Store(math.Float64bits(a.Spent))
			}
		}(a.Events.TurnFinish)
		extra := &statusExtra{dir: cwd, command: cfg.StatusLine, session: func() map[string]any {
			used, limit := a.ContextUsed()
			model, _ := curModel.Load().(string)
			return map[string]any{"model": model, "mode": string(gate.GetMode()), "cwd": cwd,
				"cost_usd": math.Float64frombits(totalCost.Load()), "tokens": totalTok.Load(),
				"context_used": used, "context_max": limit}
		}}
		extra.refresh()
		extra.every(15 * time.Second)
		a.Events.TurnFinish = func(prev func(provider.Response)) func(provider.Response) {
			return func(r provider.Response) {
				if prev != nil {
					prev(r)
				}
				extra.refresh()
			}
		}(a.Events.TurnFinish)
		screen.mu.Lock()
		screen.info = func() statusInfo {
			used, limit := a.ContextUsed()
			model, _ := curModel.Load().(string)
			if i := strings.LastIndex(model, "/"); i >= 0 {
				model = model[i+1:]
			}
			return statusInfo{model: model, mode: string(gate.GetMode()), box: box,
				tokens: int(totalTok.Load()), cost: math.Float64frombits(totalCost.Load()), ctxUsed: used, ctxMax: limit,
				extra: extra.get()}
		}
		screen.mu.Unlock()
		u.welcome(res.Provider+"/"+res.Model, string(gate.GetMode()), box, cwd)
		if prev := justUpdated(); prev != "" {
			u.note(u.paint(cGreen, "Updated from "+prev+" to "+version) + u.paint(cDim, " · /release-notes shows what changed"))
		}
		if v := updateNotice(); v != "" {
			u.note(u.paint(cYellow, "agentium "+v+" is available") + u.paint(cDim, " · type /update"))
		}
	} else if u.live {
		u.banner(res.Provider+"/"+res.Model, string(gate.GetMode()), box, cwd)
		if prev := justUpdated(); prev != "" {
			u.note(u.paint(cGreen, "Updated from "+prev+" to "+version) + u.paint(cDim, " · /release-notes shows what changed"))
		}
		if v := updateNotice(); v != "" {
			u.note(u.paint(cYellow, "agentium "+v+" is available") + u.paint(cDim, " · run `agentium update`"))
		}
	} else {
		fmt.Fprintln(os.Stderr, u.dim(fmt.Sprintf("agentium %s · %s/%s · %s mode · %s · /exit to quit", version, res.Provider, res.Model, gate.GetMode(), box)))
	}
	if *cont && len(a.Messages) > 0 && (screen != nil || u.live) {
		recap(u, a.Messages)
	}
	interactive = true
	if a.MaxTurns <= 0 {
		// An interactive session has no step limit, as in Claude Code: the
		// task runs until it is done, Esc stops it, and the stuck detector
		// and --max-cost still end a loop. "max_turns" sets one if wanted.
		a.MaxTurns = -1
	}
	if *resumePick {
		u.mu.Lock()
		u.queued = append([]string{"/resume"}, u.queued...) // open the picker first
		u.mu.Unlock()
	}
	defer func() {
		// Background servers end with the session; say which.
		if jobs := a.Env.RunningJobs(); len(jobs) > 0 && !*quiet {
			u.note("stopping background jobs: " + strings.Join(jobs, " · "))
		}
	}()
	var ed *editor
	var watch *watcher // /watch
	defer func() {
		if watch != nil {
			watch.close()
		}
	}()
	if lineEditing && isTTY(os.Stderr) && !dumbTerm() { // a dumb terminal gets plain line input
		ed = &editor{in: os.Stdin, out: os.Stderr, hist: loadHistory(), prompt: "› "}
		if u.live {
			ed.echo = u.userMessage
			ed.placeholder = "Message Agentium…  / commands · @ files"
			if suggestOn {
				ed.ghost = sug.get
			}
		}
		ed.vim = cfg.Vim
		shortcuts, badKeys := userKeys(cfg.Keys)
		for _, b := range badKeys {
			fmt.Fprintln(os.Stderr, u.dim("· "+b))
		}
		ed.complete = (&completer{root: cwd, skillList: func() []skill.Skill { return skillBox.Load().([]skill.Skill) }, cmds: userCommands(cwd), cmdsAt: time.Now()}).complete
		var lastEsc time.Time
		ed.hook = func(e *editor, k string) bool {
			submit := func(cmd string) bool {
				e.buf, e.pos, e.autoSubmit = []rune(cmd), len([]rune(cmd)), true
				return true
			}
			if act, ok := shortcuts[k]; ok && len(e.buf) == 0 {
				return submit(act) // the user's own shortcut
			}
			switch {
			case k == "\x1b[Z": // Shift-Tab: the next approval mode
				next := map[policy.Mode]policy.Mode{policy.Ask: policy.Auto, policy.Auto: policy.Plan, policy.Plan: policy.Ask, policy.Yolo: policy.Ask}
				gate.SetMode(next[gate.GetMode()])
				e.prompt = u.prompt(gate.GetMode() == policy.Plan)
				if activeFS() == nil {
					e.out.WriteString("\r\x1b[K" + u.paint(cDim, "  mode: "+string(gate.GetMode())+" (shift+tab to change)") + "\r\n")
				}
				return true
			case k == "\x1b" && len(e.buf) == 0: // Esc Esc: rewind
				if time.Since(lastEsc) < 800*time.Millisecond {
					lastEsc = time.Time{}
					return submit("/rewind")
				}
				lastEsc = time.Now()
				return true
			case k == "?" && len(e.buf) == 0:
				return submit("/help")
			case k == "\x1bp" || k == "\x1bt": // Alt-P, Alt-T: model, effort
				if len(e.buf) > 0 {
					if len(e.stash.buf) > 0 {
						return true // a draft is already put aside: don't lose this one
					}
					e.stash = e.capture(e.buf) // ctrl+s brings it back
				}
				if k == "\x1bp" {
					return submit("/model")
				}
				return submit("/effort")
			case k == "\x16": // Ctrl-V: an image from the clipboard
				if m, err := pasteImage(); err == nil {
					e.insert(m)
				} else if activeFS() == nil {
					e.out.WriteString("\r\x1b[K" + u.paint(cDim, "  "+err.Error()) + "\r\n")
				} else {
					u.note(err.Error())
				}
				return true
			case k == "\x1a": // Ctrl-Z: suspend to the shell; fg comes back
				f := activeFS()
				if f != nil {
					f.suspend()
				} else {
					e.out.WriteString("\r\n\x1b[J") // below the line, a popup may show
				}
				ok := suspendSelf()
				if f != nil {
					f.resume()
				}
				if !ok {
					u.note("Ctrl-Z suspends on macOS and Linux")
				}
				return true
			case k == "\x0f": // Ctrl-O: the full output of recent steps
				u.openViewer()
				return true
			case k == "\x07": // Ctrl-G: write the message in an editor
				text, err := externalEdit(e.text())
				if err != nil {
					e.out.WriteString("\r\x1b[K" + u.paint(cDim, "  "+err.Error()) + "\r\n")
				}
				e.buf, e.pastes = []rune(text), nil
				e.pos = len(e.buf)
				return true
			}
			return false
		}
	}
	// afterPlan is the mode /go switches to.
	afterPlan := policy.Auto
	if m := gate.GetMode(); m != policy.Plan {
		afterPlan = m
	}
	var lastInterrupt time.Time
	for {
		var line string
		var err error
		ps := "› "
		if gate.GetMode() == policy.Plan {
			ps = "plan› "
		}
		if u.live {
			ps = u.prompt(gate.GetMode() == policy.Plan)
		}
		next, queued, draft := "", false, ""
		if ed == nil || len(ed.draft) == 0 {
			// A /handoff brief or a rewound message waits in the box: it
			// goes first, queued messages after it.
			next, queued, draft = u.takeQueued()
		}
		if queued {
			// A message typed while the last turn ran.
			line = next
			if u.live {
				os.Stderr.WriteString(u.userMessage(line))
			} else {
				fmt.Fprintln(os.Stderr, "\n"+ps+line)
			}
			if ed != nil {
				ed.hist.add(line)
			}
		} else if ed != nil {
			ed.prompt = ps
			if len(ed.draft) == 0 { // a /handoff brief may be waiting
				ed.draft = []rune(draft)
			}
			fmt.Fprint(os.Stderr, "\n")
			line, err = ed.readLine()
			if errors.Is(err, errInterrupt) {
				// One stray Ctrl-C (meant for a command that just ended)
				// must not end the session: a second one within 2 s does.
				if time.Since(lastInterrupt) < 2*time.Second {
					return nil
				}
				lastInterrupt = time.Now()
				u.note("press Ctrl-C again to exit (or /exit)")
				continue
			}
			if errors.Is(err, errEOF) {
				return nil
			}
			if err != nil { // no raw mode available: fall back to plain input
				ed = nil
				continue
			}
		} else {
			fmt.Fprint(os.Stderr, "\n"+ps)
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
		if strings.HasPrefix(line, "!") {
			// Shell mode: run it yourself; the agent hears about it next.
			shellCommand(u, a, cwd, line[1:])
			continue
		}
		if strings.EqualFold(line, "agentium update") {
			// The shell command, typed here: do it rather than ask the model.
			line = "/update"
		}
		switch f := strings.Fields(line); f[0] {
		case "/plan":
			if m := gate.GetMode(); m != policy.Plan {
				afterPlan = m
			}
			gate.SetMode(policy.Plan)
			fmt.Fprintln(os.Stderr, u.dim("· plan mode: read-only; the agent investigates and proposes a plan. /go to carry it out"))
			if len(f) == 1 {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "/plan"))
		case "/go":
			if gate.GetMode() != policy.Plan {
				fmt.Fprintln(os.Stderr, u.dim("· not in plan mode"))
				continue
			}
			gate.SetMode(afterPlan)
			fmt.Fprintln(os.Stderr, u.dim(fmt.Sprintf("· %s mode: carrying out the plan", afterPlan)))
			extra := strings.TrimSpace(strings.TrimPrefix(line, "/go"))
			line = "Carry out the plan above, then verify it."
			if extra != "" {
				line += "\n\n" + extra
			}
		}
		// Skills added, changed or removed since the last message count
		// now, without a restart.
		if fresh := skill.Discover(config.Home(), cwd); skillKey(fresh) != skillKey(skills) {
			added, removed := skillDiff(skills, fresh)
			index := skill.Prompt(skills) != skill.Prompt(fresh)
			skills = fresh
			skillBox.Store(fresh)
			if len(added)+len(removed) > 0 {
				u.note(skillChangeNote(added, removed))
			}
			if index { // (the prompt's list is capped: it may not change)
				baseSystem = sysHead + skill.Prompt(skills) + sysTail
				a.System = baseSystem + stylePrompt(cfg.Style)
				for i := range a.Messages {
					a.Messages[i].Raw = nil // signed thinking belongs to the old system prompt
				}
				sess.SystemHash = hashString(a.System)
			}
		}
		cmdModel, cancelled := "", false
		if !skillCall(skills, line) {
			if c, ok := commandModel(cwd, line); ok && allowCommandModel(u, c) {
				cmdModel = c.model
			}
		}
		if skillCall(skills, line) {
			// a skill: sent as it is, below
		} else if msg, ok := expandCommandShell(cwd, line, func(c userCmd, cmds []string) []string {
			outs, cancel := commandShell(u, c, cmds, cwd, true)
			cancelled = cancelled || cancel
			return outs
		}); cancelled {
			continue
		} else if ok {
			if msg == "" {
				u.note(strings.Fields(line)[0] + " is an empty command file; nothing sent")
				continue
			}
			line = msg // /init, /review or a custom command: a message
		}
		if strings.HasPrefix(line, "/") && !skillCall(skills, line) {
			if line == "/skills" {
				printSkills(skills)
				continue
			}
			if line == "/rewind" && ed != nil && len(a.Messages) > 0 {
				rewind(u, a, sess, store, ed)
				continue
			}
			if (line == "/handoff" || strings.HasPrefix(line, "/handoff ")) && ed != nil {
				if len(a.Messages) == 0 {
					u.note("nothing to hand off yet")
					continue
				}
				goal := strings.TrimSpace(strings.TrimPrefix(line, "/handoff"))
				u.note("writing the hand-off…")
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				brief, err := a.Handoff(ctx, goal)
				cancel()
				if err != nil {
					u.failure("hand-off failed: " + firstLine(err.Error()))
					continue
				}
				old := sess.Label()
				saveSession(sess)
				a.Reset()
				a.Note = "" // notes about the old conversation (undone turns …)
				if dirs := gate.Dirs(); len(dirs) > 0 {
					a.Note = "Working directories besides this one: " + strings.Join(dirs, ", ")
				}
				*sess = *session.New(sess.Cwd, sess.Model)
				ed.draft = []rune(brief)
				fmt.Fprintln(os.Stderr)
				for _, l := range strings.Split(brief, "\n") {
					for _, w := range wordWrap(sanitize(l), termWidth(os.Stderr)-14) {
						fmt.Fprintln(os.Stderr, u.paint(cDim, "    │ ")+w)
					}
				}
				u.success("New conversation · the hand-off is in the message box: edit it (ctrl+g opens an editor), then enter" +
					u.paint(cDim, " · the old one: /resume "+oneLine(old, 30)))
				continue
			}
			if line == "/watch" && ed != nil {
				if watch != nil {
					watch.close()
					watch, ed.inject = nil, nil
					u.success("Stopped watching for AI comments")
				} else {
					comp := &completer{root: cwd}
					watch = newWatcher(cwd, func() []string { f, _ := comp.projectFiles(); return f[:min(len(f), 20000)] }) // it stats every file each 2 s
					ed.inject = watch.take
					u.success("Watching the project: end a comment with AI! to ask for a change, AI? to ask a question" +
						u.paint(cDim, " · /watch again stops"))
				}
				continue
			}
			if line == "/style" || strings.HasPrefix(line, "/style ") {
				name := strings.TrimSpace(strings.TrimPrefix(line, "/style"))
				if name == "" {
					var items []menuItem
					for _, st := range outputStyles {
						items = append(items, menuItem{value: st.name, hint: st.hint})
					}
					cur := firstNonEmpty(cfg.Style, "default")
					pick, err := u.choose("Output style", items, cur, false)
					if err != nil {
						continue
					}
					name = pick
				}
				known := false
				for _, st := range outputStyles {
					known = known || strings.EqualFold(st.name, name)
				}
				if !known {
					u.failure("no style " + name + " (default, explanatory, learning, terse)")
					continue
				}
				cfg.Style = strings.ToLower(name)
				a.System = baseSystem + stylePrompt(cfg.Style)
				for i := range a.Messages {
					a.Messages[i].Raw = nil // signed thinking belongs to the old system prompt
				}
				sess.SystemHash = hashString(a.System)
				_ = config.Set("style", cfg.Style)
				u.success("Style: " + cfg.Style + u.paint(cDim, " (saved)"))
				continue
			}
			if line == "/release-notes" || strings.HasPrefix(line, "/release-notes ") {
				showReleaseNotes(u, strings.TrimPrefix(line, "/release-notes"))
				continue
			}
			if line == "/bug" || strings.HasPrefix(line, "/bug ") {
				model, _ := curModel.Load().(string)
				what := strings.TrimSpace(strings.TrimPrefix(line, "/bug"))
				body := fmt.Sprintf("**What happened**\n%s\n\n**Expected**\n\n**Setup**\nagentium %s · %s/%s · %s · %s\n",
					what, version, runtime.GOOS, runtime.GOARCH, firstNonEmpty(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM")), model)
				link := "https://github.com/TegarTheGreat/Agentium/issues/new?title=" + url.QueryEscape(oneLine(firstNonEmpty(what, "Bug: "), 80)) + "&body=" + url.QueryEscape(body)
				u.success("Report it here (the form is filled in; nothing is sent until you submit it):")
				fmt.Fprintln(os.Stderr, "  "+fileLinkURL(link, link))
				continue
			}
			if line == "/agents" {
				showAgents(u, agentFiles)
				continue
			}
			if line == "/vim" && ed != nil {
				ed.vim = !ed.vim
				_ = config.Set("vim", ed.vim)
				if ed.vim {
					u.success("Vim keys on · esc normal mode, i insert · /vim turns them off")
				} else {
					u.success("Vim keys off (saved)")
				}
				continue
			}
			if done := slash(line, &slashEnv{a: a, gate: gate, cfg: cfg, sess: sess, store: store, res: &res, u: u, box: box, mem: mem, mcp: mcps, ap: ap}); done {
				return nil
			}
			continue
		}
		restore := func() {}
		if cmdModel != "" {
			// A command's own model ("model:" in its front matter) runs this
			// one turn; the session's comes back after.
			r, err := useModelOnce(cmdModel, cfg, &res, a, fb, *maxCost)
			if err != nil {
				u.note("model " + sanitize(cmdModel) + " not used: " + firstLine(err.Error()))
			} else {
				restore = r
				u.note("this turn on " + res.Provider + "/" + res.Model)
			}
		}
		err = turn(line)
		restore()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, u.dim("· interrupted"))
				continue
			}
			if errors.Is(err, agent.ErrMaxTurns) {
				// Not a failure: the work can go on.
				u.note(u.paint(cYellow, "Paused at the step limit") + u.paint(cDim, " · press Enter to continue (\"max_turns\" in the config raises it)"))
				if ed != nil {
					ed.draft = []rune("continue")
				}
				continue
			}
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

// slashEnv is what slash commands may read and change.
type slashEnv struct {
	a     *agent.Agent
	gate  *policy.Gate
	cfg   config.Config
	sess  *session.Session
	store *checkpoint.Store
	res   *provider.Resolved
	u     *ui
	box   string
	mem   *memCtl
	mcp   *mcpState
	ap    *approver
}

// switchModel points the agent at ref and saves it as the default.
func (e *slashEnv) switchModel(ref string) error {
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	res, err := provider.Resolve(ref, e.cfg, auth)
	if err != nil {
		return err
	}
	a := e.a
	a.Client, a.Model = res.Client, res.Model
	a.Env.Vision = res.Vision()
	// The new model's reasoning capabilities, same requested effort.
	a.Reasoning = res.Reasoning(a.Reasoning.Effort)
	// The new model's limits (a guess from its name when unknown), so
	// compaction triggers at the right size.
	a.ContextTokens = firstPositive(e.cfg.ContextTokens, res.Info.Context, provider.ContextWindow(res.Model))
	a.MaxOutput = res.Info.Output
	*e.res = res
	e.sess.Model = res.Provider + "/" + res.Model
	curModel.Store(e.sess.Model)
	registryID := res.Provider
	if alias, ok := models.Aliases[registryID]; ok {
		registryID = alias
	}
	if !res.Known && len(models.List(config.Home(), registryID)) > 0 {
		// The provider's models are listed and this is not one of them:
		// likely a typo, and a saved default would break every session.
		e.u.success("Model: " + e.sess.Model + e.u.paint(cDim, " (for this session only: it is not in "+res.Provider+"'s model list, see agentium models "+res.Provider+")"))
		return nil
	}
	_ = config.Set("model", e.sess.Model)
	e.u.success("Model: " + e.sess.Model + e.u.paint(cDim, " (saved as default)"))
	return nil
}

func slash(line string, e *slashEnv) (exit bool) {
	a, gate, cfg, sess, store, u := e.a, e.gate, e.cfg, e.sess, e.store, e.u
	f := strings.Fields(line)
	switch f[0] {
	case "/help", "/?":
		u.help()
	case "/login":
		pid := ""
		if len(f) > 1 {
			pid = f[1]
		}
		ref, err := u.setup(pid)
		if err != nil {
			if !errors.Is(err, errCanceled) {
				u.failure(err.Error())
			}
			return false
		}
		if err := e.switchModel(ref); err != nil {
			u.failure(err.Error())
		}
	case "/logout":
		if len(f) < 2 {
			u.note("usage: /logout <provider>")
			return false
		}
		if err := cmdLogout(f[1:]); err != nil {
			u.failure(err.Error())
			return false
		}
		u.success("Removed the stored key for " + f[1])
	case "/update":
		if updated, err := runUpdate(f[1:]); err != nil {
			u.failure(err.Error())
		} else if updated {
			u.note("restart agentium to use the new version (/exit, then agentium)")
		}
	case "/config", "/settings":
		u.showConfig(sess.Model, string(gate.GetMode()), a.Reasoning.Effort, e.box)
	case "/effort":
		lvl := ""
		if len(f) > 1 {
			lvl = f[1]
		} else {
			var err error
			lvl, err = u.choose("Reasoning effort", []menuItem{
				{value: "default", label: "Model default"},
				{value: "low", hint: "fastest, cheapest"},
				{value: "medium"},
				{value: "high", hint: "more careful"},
				{value: "xhigh", hint: "much more thinking"},
				{value: "max", hint: "slowest, most thorough"},
			}, firstNonEmpty(a.Reasoning.Effort, "default"), false)
			if err != nil {
				return false
			}
		}
		lvl, err := checkEffort(lvl)
		if err != nil {
			u.failure(err.Error())
			return false
		}
		a.Reasoning.Effort = lvl
		u.success("Effort: " + firstNonEmpty(lvl, "model default"))
	case "/model":
		if len(f) > 1 {
			if err := e.switchModel(f[1]); err != nil {
				u.failure(err.Error())
			}
			return false
		}
		auth, _ := config.LoadAuth()
		cur := e.res.Provider
		name := func(id string) string {
			for _, p := range providerInfo {
				if p.id == id {
					return p.name
				}
			}
			return id
		}
		items := []menuItem{{value: cur, label: name(cur), hint: "current"}}
		specs := provider.Specs(cfg)
		for _, p := range providerInfo {
			if s, ok := specs[p.id]; ok && p.id != cur && !s.NoKey && provider.Key(s, auth) != "" {
				items = append(items, menuItem{value: p.id, label: p.name, hint: "✓ connected"})
			}
		}
		items = append(items, menuItem{value: "+", label: "Connect another provider…"})
		pid, err := u.choose("Model: "+sess.Model+" · choose a provider", items, cur, false)
		if err != nil {
			return false
		}
		if pid == "+" {
			ref, err := u.setup("")
			if err == nil {
				err = e.switchModel(ref)
			}
			if err != nil && !errors.Is(err, errCanceled) {
				u.failure(err.Error())
			}
			return false
		}
		cm := ""
		if pid == cur {
			cm = e.res.Model
		}
		m, err := u.pickModel(pid, cfg, auth, cm)
		if err != nil {
			return false
		}
		if err := e.switchModel(pid + "/" + m); err != nil {
			u.failure(err.Error())
		}
		return false
	case "/mode":
		if len(f) > 1 {
			m, err := policy.CheckMode(f[1])
			if err != nil {
				u.failure(err.Error() + "; mode stays " + string(gate.GetMode()))
				return false
			}
			gate.SetMode(m)
		} else if m, err := u.choose("Approval mode", []menuItem{
			{value: "ask", hint: "confirm every change and command"},
			{value: "auto", hint: "confirm only risky actions (default)"},
			{value: "yolo", hint: "never ask"},
			{value: "plan", hint: "read-only: investigate and propose a plan"},
		}, string(gate.GetMode()), false); err == nil {
			gate.SetMode(policy.ParseMode(m))
		} else {
			return false
		}
		u.success("Mode: " + string(gate.GetMode()))
		return false
	}
	switch f[0] {
	case "/help", "/?", "/login", "/logout", "/update", "/config", "/settings", "/effort", "/model", "/mode":
		return false
	case "/exit", "/quit", "/q":
		return true
	case "/clear", "/new":
		a.Reset()
		*sess = *session.New(sess.Cwd, sess.Model)
		fmt.Fprintln(os.Stderr, "· new conversation")
	case "/sessions":
		list, _ := session.ForCwd(sess.Cwd, 20)
		if len(list) == 0 {
			fmt.Fprintln(os.Stderr, "· no saved sessions here")
		}
		for i, ss := range list {
			mark := " "
			if ss.ID == sess.ID {
				mark = "*"
			}
			fmt.Fprintf(os.Stderr, "%s%2d. %s · %d msgs · %s\n", mark, i+1, ss.Updated.Format("2006-01-02 15:04"), len(ss.Messages), oneLine(ss.Label(), 60))
		}
		fmt.Fprintln(os.Stderr, "· /resume opens a picker · /resume <n or name> · /rename <name> names this one")
	case "/fork":
		// Continue in a copy; the original stays as it was.
		if len(sess.Messages) == 0 {
			u.note("nothing to fork yet")
			return false
		}
		orig := sess.Label()
		saveSession(sess)
		fork := *sess
		fork.ID = session.New(sess.Cwd, sess.Model).ID
		fork.Messages = append([]provider.Message(nil), sess.Messages...)
		fork.Checkpoints = append([]session.Checkpoint(nil), sess.Checkpoints...)
		name := strings.TrimSpace(strings.TrimPrefix(line, "/fork"))
		if name == "" {
			name = "fork of " + oneLine(orig, 40)
		}
		fork.Title = name
		*sess = fork
		saveSession(sess)
		u.success("Now in “" + name + "”" + u.paint(cDim, " · the original is kept: /resume "+oneLine(orig, 30)))
	case "/rename":
		name := strings.TrimSpace(strings.TrimPrefix(line, "/rename"))
		if name == "" {
			u.note("usage: /rename <name>")
			return false
		}
		if r := []rune(name); len(r) > 80 {
			name = string(r[:80])
		}
		sess.Title = name
		saveSession(sess)
		u.success("This session is now “" + name + "”" + u.paint(cDim, " (/resume "+name+" continues it later)"))
	case "/resume":
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
		limit := 20
		if arg != "" {
			limit = 500 // a name may belong to an older conversation
		}
		list, _ := session.ForCwd(sess.Cwd, limit)
		if len(list) == 0 {
			fmt.Fprintln(os.Stderr, "· no saved sessions here")
			return false
		}
		var chosen *session.Session
		if arg != "" {
			chosen = findSession(list, arg)
		} else {
			var items []menuItem
			for i, ss := range list {
				items = append(items, menuItem{value: strconv.Itoa(i), label: oneLine(ss.Label(), 50),
					hint: fmt.Sprintf("%s · %d msgs", ago(ss.Updated), len(ss.Messages))})
			}
			pick, err := u.choose("Resume a conversation", items, "", false)
			if err != nil {
				return false
			}
			i, _ := strconv.Atoi(pick)
			chosen = list[i]
		}
		if chosen == nil {
			fmt.Fprintln(os.Stderr, "· no such session (see /sessions)")
			return false
		}
		hash := sess.SystemHash
		*sess = *chosen
		if _, ok := sess.Own(); !ok {
			// Open in another agentium: continue in a copy.
			sess.ID = session.New(sess.Cwd, sess.Model).ID
			sess.Own()
			u.note("that conversation is open in another agentium: continuing in a copy")
		}
		a.Messages = provider.CloseToolCalls(chosen.Messages)
		a.Env.ForgetReads()
		if chosen.SystemHash != hash {
			for i := range a.Messages {
				a.Messages[i].Raw = nil
			}
		}
		sess.SystemHash = hash
		fmt.Fprintf(os.Stderr, "· resumed “%s” from %s (%d messages)\n", sanitize(oneLine(chosen.Label(), 50)), chosen.Updated.Format("2006-01-02 15:04"), len(chosen.Messages))
		recap(u, a.Messages)
	case "/undo":
		note, err := undoLast(store, sess)
		if err != nil {
			fmt.Fprintln(os.Stderr, "·", err)
			return false
		}
		refreshChanges(sess.Cwd)
		a.Note = note
		saveSession(sess)
	case "/export":
		exportSession(u, a, sess, strings.Join(f[1:], " "))
	case "/memory":
		showMemory(u, e.mem, strings.Join(f[1:], " "))
	case "/diff":
		showDiff(u, sess.Cwd)
	case "/context":
		showContext(u, a)
	case "/compact":
		compactNow(u, a)
		sess.Messages = a.Messages
		saveSession(sess)
	case "/theme":
		pickTheme(u, strings.Join(f[1:], " "))
	case "/btw":
		sideQuestion(u, a, strings.TrimSpace(strings.TrimPrefix(line, "/btw")))
	case "/copy":
		text := ""
		for i := len(a.Messages) - 1; i >= 0; i-- {
			if m := a.Messages[i]; m.Role == provider.RoleAssistant && strings.TrimSpace(m.Text) != "" {
				text = m.Text
				break
			}
		}
		if text == "" {
			fmt.Fprintln(os.Stderr, "· nothing to copy yet")
			return false
		}
		how := copyToClipboard(text)
		u.success(fmt.Sprintf("Copied the last reply (%d characters) %s", len(text), how))
	case "/rewind":
		if store == nil {
			fmt.Fprintln(os.Stderr, "· checkpoints are disabled (needs git, and \"checkpoints\" not false)")
			return false
		}
		if len(sess.Checkpoints) == 0 {
			fmt.Fprintln(os.Stderr, "· nothing to rewind: no turn has changed files yet")
			return false
		}
		var items []menuItem
		for i := len(sess.Checkpoints) - 1; i >= 0; i-- {
			cp := sess.Checkpoints[i]
			items = append(items, menuItem{value: strconv.Itoa(i), label: firstLine(cp.Prompt), hint: cp.Time.Format("15:04")})
		}
		pick, err := u.choose("Rewind: revert the file changes of this turn and every later one", items, "", false)
		if err != nil {
			return false
		}
		k, _ := strconv.Atoi(pick)
		var notes []string
		for len(sess.Checkpoints) > k {
			note, err := undoLast(store, sess)
			if err != nil {
				fmt.Fprintln(os.Stderr, "·", err)
				break
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
		a.Note = strings.Join(notes, "\n")
		saveSession(sess)
	case "/permissions", "/allowed":
		showPermissions(u, e.ap)
	case "/add-dir":
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/add-dir"))
		if arg == "" {
			u.note("usage: /add-dir <path> · workspaces: " + strings.Join(append([]string{gate.Root}, gate.Extra...), ", "))
			return false
		}
		dirs, err := resolveDirs([]string{arg}, gate.Root)
		if err != nil {
			u.failure(err.Error())
			return false
		}
		gate.AddDir(dirs[0])
		gate.Protected = append(gate.Protected, gitProtected(dirs[0])...)
		if a.Env.Sandbox != nil {
			a.Env.Sandbox.Write = append(a.Env.Sandbox.Write, dirs[0])
		}
		a.Note = strings.TrimSpace(a.Note + "\n" + "[agentium] The user added " + dirs[0] + " as a working directory: you may read and change files there too.")
		u.success("Added " + dirs[0] + u.paint(cDim, " · this session; --add-dir or \"dirs\" in the config for every time"))
	case "/hooks":
		var h config.Hooks
		if cfg.Hooks != nil {
			h = *cfg.Hooks
		}
		showHooks(u, h)
	case "/mcp":
		showMCP(u, cfg, e.mcp)
	case "/tools":
		showTools(u, a)
	case "/doctor":
		doctor(u, e, e.mcp)
	case "/usage", "/cost":
		used, spent := a.Totals()
		cost := ""
		if spent > 0 {
			cost = fmt.Sprintf(" · $%.4f", spent)
		}
		fmt.Fprintf(os.Stderr, "· %d turn%s · in %s (cached %s) · out %s%s\n", a.Turns, plural(a.Turns), fmtK(used.Input+used.CacheRead+used.CacheWrite), fmtK(used.CacheRead), fmtK(used.Output), cost)
	default:
		u.failure("unknown command " + f[0] + u.paint(cDim, " · /help lists commands"))
	}
	return false
}

// findSession picks the session arg names: an exact title first, then a
// number from /sessions, then a title or first request containing arg.
func findSession(list []*session.Session, arg string) *session.Session {
	for _, ss := range list {
		if strings.EqualFold(ss.Title, arg) {
			return ss
		}
	}
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= min(len(list), 20) {
		return list[n-1]
	}
	for _, ss := range list {
		if strings.Contains(strings.ToLower(ss.Label()), strings.ToLower(arg)) {
			return ss
		}
	}
	return nil
}

// ago is a time as people say it: "just now", "5 min ago", "yesterday".
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour && t.Day() == time.Now().Day():
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour && t.Day() == time.Now().AddDate(0, 0, -1).Day():
		return "yesterday " + t.Format("15:04")
	case d < 7*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2")
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
