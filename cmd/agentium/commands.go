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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/bench"
	"github.com/tegarthegreat/agentium/internal/checkpoint"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/mcp"
	"github.com/tegarthegreat/agentium/internal/models"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/sandbox"
	"github.com/tegarthegreat/agentium/internal/session"
	"github.com/tegarthegreat/agentium/internal/tool"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// readSecret reads a line without echo when stdin is a terminal.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	tty := isTTY(os.Stdin)
	if tty {
		if err := exec.Command("stty", "-F", "/dev/tty", "-echo").Run(); err != nil {
			_ = exec.Command("stty", "-f", "/dev/tty", "-echo").Run() // macOS
		}
		defer func() {
			if err := exec.Command("stty", "-F", "/dev/tty", "echo").Run(); err != nil {
				_ = exec.Command("stty", "-f", "/dev/tty", "echo").Run()
			}
			fmt.Fprintln(os.Stderr)
		}()
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	oauth := fs.Bool("oauth", false, "log in through the browser (openrouter)")
	noKeychain := fs.Bool("no-keychain", false, "store the key in auth.json even if an OS keychain is available")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	specs := provider.Specs(cfg)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentium login [--oauth] <provider>   (see `agentium providers`)")
	}
	id := fs.Arg(0)
	if env, ok := searchKeyEnv[id]; ok {
		if _, isProvider := specs[id]; !isProvider {
			// A web search service: its key is for the web_search tool.
			key, err := readSecret(fmt.Sprintf("API key for %s web search: ", id))
			if err != nil {
				return err
			}
			if key == "" {
				return errors.New("empty key")
			}
			where, err := saveKey(searchAuthID(id), key)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ %s web search key saved in %s (%s in the environment takes precedence)\n", id, where, env)
			return nil
		}
	}
	s, ok := specs[id]
	if !ok {
		return fmt.Errorf("unknown provider %q; add it under \"providers\" in %s/config.json (web search keys: brave, tavily, exa, serper)", id, config.Home())
	}
	switch {
	case id == "bedrock":
		fmt.Fprintln(os.Stderr, "bedrock uses AWS credentials from the environment: AWS_BEARER_TOKEN_BEDROCK, or AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN), and AWS_REGION")
		return nil
	case id == "vertex":
		fmt.Fprintln(os.Stderr, "vertex uses Google Cloud credentials: set GOOGLE_CLOUD_PROJECT (and CLOUD_ML_REGION), then `gcloud auth application-default login`")
		return nil
	case s.NoKey:
		fmt.Fprintf(os.Stderr, "%s needs no key (local server at %s)\n", id, s.BaseURL)
		return nil
	case id == "github" && !*oauth:
		fmt.Fprintln(os.Stderr, "tip: with the GitHub CLI installed and logged in (`gh auth login`), no key is needed")
	}
	var key string
	if *oauth {
		if id != "openrouter" {
			return fmt.Errorf("browser login is available for openrouter; for %s paste an API key", id)
		}
		if key, err = openRouterOAuth(context.Background()); err != nil {
			return err
		}
	} else if key, err = readSecret(fmt.Sprintf("API key for %s: ", id)); err != nil {
		return err
	}
	if key == "" {
		return errors.New("empty key")
	}
	where := config.Home() + "/auth.json"
	cred := config.Credential{APIKey: key}
	if !*noKeychain && config.KeychainAvailable() && config.KeychainSet(id, key) == nil {
		cred, where = config.Credential{Keychain: true}, "the OS keychain"
	}
	if err := config.UpdateAuth(func(a config.Auth) { a[id] = cred }); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved %s credentials to %s\n", id, where)
	return nil
}

func cmdLogout(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agentium logout <provider>")
	}
	if _, ok := searchKeyEnv[args[0]]; ok {
		if auth, err := config.LoadAuth(); err == nil {
			if _, stored := auth[searchAuthID(args[0])]; stored {
				args = []string{searchAuthID(args[0])}
			}
		}
	}
	if auth, err := config.LoadAuth(); err == nil {
		if _, ok := auth[args[0]]; !ok {
			var have []string
			for id := range auth {
				have = append(have, id)
			}
			sort.Strings(have)
			if len(have) == 0 {
				return fmt.Errorf("no credentials stored for %s (none are stored)", args[0])
			}
			return fmt.Errorf("no credentials stored for %s (stored: %s)", args[0], strings.Join(have, ", "))
		}
	}
	return config.UpdateAuth(func(a config.Auth) {
		if a[args[0]].Keychain {
			_ = config.KeychainDelete(args[0])
		}
		delete(a, args[0])
	})
}

func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	refresh := fs.Bool("refresh", false, "download the latest registry from models.dev")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home := config.Home()
	if *refresh || len(models.Providers(home)) == 0 {
		fmt.Fprintln(os.Stderr, "fetching models.dev registry …")
		if _, err := models.Refresh(context.Background(), home); err != nil {
			return err
		}
	}
	if fs.NArg() == 0 {
		ps := models.Providers(home)
		fmt.Printf("%d providers in the registry; `agentium models <provider>` lists models\n", len(ps))
		return nil
	}
	pid := fs.Arg(0)
	if a, ok := models.Aliases[pid]; ok {
		pid = a
	}
	list := models.List(home, pid)
	if len(list) == 0 {
		return fmt.Errorf("no models for %q (try `agentium models --refresh`)", fs.Arg(0))
	}
	for _, m := range list {
		if !m.Tools {
			continue
		}
		fmt.Printf("%-40s ctx %-6s out %-6s $%g/$%g per Mtok %s\n", m.ID, fmtK(m.Context), fmtK(m.Output), m.Cost.Input, m.Cost.Output, m.Released)
	}
	return nil
}

func cmdProviders() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	specs := provider.Specs(cfg)
	hidden := 0
	for _, id := range provider.IDs(specs) {
		s := specs[id]
		if s.FromRegistry && provider.Key(s, auth) == "" {
			hidden++
			continue
		}
		status := "no key"
		switch {
		case s.NoKey:
			status = "local"
		case provider.Key(s, auth) != "":
			status = "ready"
		}
		def := s.Default
		if def == "" {
			def = "-"
		}
		fmt.Printf("%-11s %-7s %-9s %-28s %s\n", id, status, s.Protocol, def, s.BaseURL)
	}
	if hidden > 0 {
		fmt.Printf("+ %d more OpenAI/Anthropic-compatible providers from models.dev (set their API key env var to use them)\n", hidden)
	}
	return nil
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	modelRef := fs.String("m", "", "model for live tasks")
	runs := fs.Int("runs", 20, "startup runs")
	asJSON := fs.Bool("json", false, "JSON output")
	only := fs.String("task", "", "run only this live task")
	if err := fs.Parse(args); err != nil {
		return err
	}
	off, err := bench.MeasureOffline(*runs)
	if err != nil {
		return err
	}
	out := map[string]any{"offline": off}
	if !*asJSON {
		fmt.Printf("binary        %.1f MB\n", float64(off.BinaryBytes)/1e6)
		fmt.Printf("startup       median %s · p90 %s (%d runs)\n", off.StartupMedian.Round(10*time.Microsecond), off.StartupP90.Round(10*time.Microsecond), *runs)
		if off.MaxRSSBytes > 0 {
			fmt.Printf("max RSS       %.1f MB\n", float64(off.MaxRSSBytes)/1e6)
		}
		fmt.Printf("prompt        %d chars system + %d chars tools ≈ %d tokens overhead\n", off.PromptChars, off.ToolChars, off.OverheadTokens)
	}
	if *modelRef == "" {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		fmt.Println("\nlive tasks: pass -m provider/model")
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	res, err := provider.Resolve(*modelRef, cfg, auth)
	if err != nil {
		return err
	}
	tasks := bench.Tasks
	if *only != "" {
		tasks = nil
		for _, t := range bench.Tasks {
			if t.Name == *only {
				tasks = append(tasks, t)
			}
		}
		if tasks == nil {
			return fmt.Errorf("unknown task %q", *only)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if !*asJSON {
		fmt.Printf("\nlive: %s/%s\n", res.Provider, res.Model)
	}
	results, err := bench.RunLive(ctx, res.Client, res.Model, tasks, func(r bench.Result) {
		if *asJSON {
			return
		}
		mark := "PASS"
		if !r.Pass {
			mark = "FAIL"
		}
		u := r.Usage
		fmt.Printf("  %-12s %s  %d turns · %d tools · in %s (cached %s) · out %s · %.1fs", r.Name, mark, r.Turns, r.ToolCalls,
			fmtK(u.Input+u.CacheRead+u.CacheWrite), fmtK(u.CacheRead), fmtK(u.Output), r.Elapsed.Seconds())
		if r.Error != "" {
			fmt.Printf("  — %s", firstLine(r.Error))
		}
		fmt.Println()
	})
	var pass, turns, tokIn, tokOut int
	var total time.Duration
	for _, r := range results {
		if r.Pass {
			pass++
		}
		turns += r.Turns
		tokIn += r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheWrite
		tokOut += r.Usage.Output
		total += r.Elapsed
	}
	if *asJSON {
		out["model"] = res.Provider + "/" + res.Model
		out["live"] = results
		if e := json.NewEncoder(os.Stdout).Encode(out); e != nil {
			return e
		}
	} else if len(results) > 0 {
		fmt.Printf("  total        %d/%d pass · %d turns · in %s · out %s · %.1fs\n", pass, len(results), turns, fmtK(tokIn), fmtK(tokOut), total.Seconds())
	}
	return err
}

func openCheckpoints(cfg config.Config, root string) *checkpoint.Store {
	if cfg.Checkpoints != nil && !*cfg.Checkpoints {
		return nil
	}
	s, err := checkpoint.Open(filepath.Join(config.Home(), "checkpoints"), root)
	if err != nil {
		if !errors.Is(err, checkpoint.ErrNoGit) {
			// Undo silently missing is worse than a line saying so.
			fmt.Fprintln(os.Stderr, "· undo is unavailable: "+sanitize(firstLine(err.Error())))
		}
		return nil
	}
	return s
}

// stagedAmong returns the paths among files that git's index has
// changes to (nil outside a git repository).
func stagedAmong(root string, files []string) []string {
	out, err := exec.Command("git", "-C", root, "diff", "--cached", "--name-only", "-z", "--no-renames", "--relative").Output()
	if err != nil {
		return nil
	}
	staged := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		staged[p] = true
	}
	var hit []string
	for _, f := range files {
		if staged[filepath.ToSlash(f)] {
			hit = append(hit, f)
		}
	}
	return hit
}

// undoLast restores the newest checkpoint of sess and returns a note for
// the model describing what was reverted.
func undoLast(store *checkpoint.Store, sess *session.Session) (string, error) {
	if store == nil {
		return "", errors.New("checkpoints are disabled (needs git, and \"checkpoints\" not false)")
	}
	cp, ok := sess.PopCheckpoint()
	if !ok {
		return "", errors.New("nothing to undo")
	}
	files, err := store.Restore(context.Background(), cp.ID, cp.After)
	if err != nil {
		sess.Checkpoints = append(sess.Checkpoints, cp)
		return "", fmt.Errorf("undo failed: %v", err)
	}
	if nested := store.Nested(context.Background()); len(nested) > 0 {
		fmt.Fprintf(os.Stderr, "· note: nested repositories are not covered by undo: %s\n", strings.Join(limitList(nested, 5), ", "))
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "· no file changes to revert for %q\n", firstLine(cp.Prompt))
		return "", nil
	}
	fmt.Fprintf(os.Stderr, "· reverted %d file(s) changed by %q: %s\n", len(files), firstLine(cp.Prompt), strings.Join(limitList(files, 8), ", "))
	if staged := stagedAmong(store.Root, files); len(staged) > 0 {
		// Undo restores files, never your .git: a turn's git add/mv/rm
		// stays in the staging area.
		fmt.Fprintf(os.Stderr, "· git still has staged changes to %s (the turn staged them; undo leaves .git alone): git restore --staged -- <path> unstages them\n", strings.Join(limitList(staged, 5), ", "))
	}
	return fmt.Sprintf("The user undid the file changes from the turn %q. Restored: %s. Re-read files before editing them.",
		firstLine(cp.Prompt), strings.Join(limitList(files, 20), ", ")), nil
}

func limitList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string(nil), xs[:n]...), fmt.Sprintf("… %d more", len(xs)-n))
}

func cmdUndo() error {
	cfg, err := config.Load()
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
	sess, err := session.Latest(cwd)
	if err != nil {
		return err
	}
	if sess == nil {
		return errors.New("no session in this directory")
	}
	note, err := undoLast(openCheckpoints(cfg, cwd), sess)
	if err != nil {
		return err
	}
	if note != "" {
		sess.Note = note
	}
	return sess.Save()
}

func sandboxWanted(cfg config.Config) bool {
	return cfg.Sandbox == nil || cfg.Sandbox.Enabled == nil || *cfg.Sandbox.Enabled
}

// setupSandbox confines env's shell commands when possible.
func setupSandbox(env *tool.Env, cfg config.Config, root string, disabled bool) sandbox.Status {
	if cfg.Sandbox != nil {
		env.PassEnv = cfg.Sandbox.PassEnv
	}
	st := sandbox.Probe()
	if disabled || !sandboxWanted(cfg) || !st.Available {
		return st
	}
	sc := sandbox.Config{Write: sandbox.DefaultWrite(root)}
	if common := gitCommonDir(root); common != "" {
		// A linked worktree commits into the main repository's .git.
		sc.Write = append(sc.Write, common)
	}
	if cfg.Sandbox != nil {
		for _, p := range cfg.Sandbox.Write {
			if strings.HasPrefix(p, "~/") {
				if h, err := os.UserHomeDir(); err == nil {
					p = filepath.Join(h, p[2:])
				}
			}
			sc.Write = append(sc.Write, p)
		}
		env.Net = policy.ParseNet(cfg.Sandbox.Network)
	}
	sc.NetworkUnenforced = !st.Network
	env.Sandbox = &sc
	return st
}

// startMCP starts the configured MCP servers concurrently (10s budget) and
// returns those that came up. Only users who configure MCP pay this cost.
func startMCP(servers map[string]config.MCPServer, dir string, report func(string)) *mcpState {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type res struct {
		name string
		c    *mcp.Client
		err  error
	}
	ch := make(chan res, len(servers))
	for name, sc := range servers {
		go func(name string, sc config.MCPServer) {
			c, err := mcp.Start(ctx, name, mcp.Config{Command: sc.Command, Args: sc.Args, Env: sc.Env,
				URL: sc.URL, Type: sc.Type, Headers: sc.Headers, Tokens: mcpTokens(), Literal: sc.Literal,
				LogPath: filepath.Join(config.Home(), "logs", "mcp-"+name+".log")}, dir)
			ch <- res{name, c, err}
		}(name, sc)
	}
	st := &mcpState{failed: map[string]string{}}
	var out []*mcp.Client
	for range servers {
		r := <-ch
		if r.err != nil {
			report("mcp: " + r.err.Error())
			st.failed[r.name] = r.err.Error()
			continue
		}
		report(fmt.Sprintf("mcp: %s ready (%d tools)", r.c.Name, len(r.c.Tools)))
		out = append(out, r.c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name }) // stable tool order = cacheable prefix
	st.clients = out
	return st
}

// runStopHooks runs the user's post-turn commands in the background.
func runStopHooks(hooks []string, dir string) {
	for _, h := range hooks {
		cmd := exec.Command("sh", "-c", h)
		cmd.Dir = dir
		_ = cmd.Start()
		go func() { _ = cmd.Wait() }()
	}
}

// codeCache is where the code index for cwd is cached.
func codeCache(cwd string) string {
	h := sha256.Sum256([]byte(cwd))
	return filepath.Join(config.ProjectDir(config.ProjectRoot(cwd)), "codemap-"+hex.EncodeToString(h[:6])+".gob")
}

// mcpTokens is where OAuth grants for remote MCP servers are kept.
// mcpTokens is the one token store of this process: its refresh lock
// must be shared by every server and call.
var mcpTokens = sync.OnceValue(func() *mcp.TokenStore {
	return &mcp.TokenStore{Path: filepath.Join(config.Home(), "mcp-auth.json")}
})

// cmdMCP: agentium mcp [list | login <name> | logout <name>].
func cmdMCP(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store := mcpTokens()
	if len(args) == 0 || args[0] == "list" {
		if len(cfg.MCP) == 0 {
			fmt.Fprintln(os.Stderr, `no MCP servers configured ("mcp" in ~/.agentium/config.json)`)
			return nil
		}
		names := make([]string, 0, len(cfg.MCP))
		for n := range cfg.MCP {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			sc := cfg.MCP[n]
			switch {
			case sc.URL == "":
				fmt.Printf("%-16s stdio  %s\n", n, sc.Command)
			case store.Get(sc.URL) != nil:
				fmt.Printf("%-16s remote %s (logged in)\n", n, sc.URL)
			default:
				fmt.Printf("%-16s remote %s\n", n, sc.URL)
			}
		}
		return nil
	}
	if len(args) != 2 || args[0] != "login" && args[0] != "logout" {
		return errors.New("usage: agentium mcp [list | login <name> | logout <name>]")
	}
	sc, ok := cfg.MCP[args[1]]
	if !ok || sc.URL == "" {
		return fmt.Errorf("no remote MCP server %q in config", args[1])
	}
	if args[0] == "logout" {
		if err := store.Put(sc.URL, nil); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "logged out of", args[1])
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = mcp.Login(ctx, sc.URL, sc.Headers, store, func(u string) {
		fmt.Fprintf(os.Stderr, "Opening your browser to log in to %s.\nIf it does not open, visit:\n  %s\n", args[1], u)
		openBrowser(u)
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "logged in to", args[1])
	return nil
}

// gitProtected lists hook directories and included config files outside
// .git that git runs or reads (Husky's .husky, includes): edits to them
// need approval like .git itself.
func gitProtected(root string) []string {
	hooks, includes := tool.GitExtras(root)
	return append(hooks, includes...)
}

// subModel resolves cfg.SubagentModel: the model sub-agents run on, with
// its own limits, price and reasoning effort. known is false when its
// price is unknown (its spending then counts as 0).
func subModel(cfg config.Config, auth config.Auth, effort string) (*agent.SubModel, bool, error) {
	sr, err := provider.Resolve(cfg.SubagentModel, cfg, auth)
	if err != nil {
		return nil, false, err
	}
	info, known := sr.Info, sr.Known
	return &agent.SubModel{Client: sr.Client, Model: sr.Model, MaxOutput: info.Output,
		ContextTokens: firstPositive(info.Context, provider.ContextWindow(sr.Model)),
		Reasoning:     sr.Reasoning(effort),
		Cost: func(us provider.Usage) float64 {
			if !known {
				return 0
			}
			return info.Price(us.Input, us.Output, us.CacheRead, us.CacheWrite)
		}}, known, nil
}

// oracleModel resolves cfg.OracleModel for the oracle tool.
func oracleModel(cfg config.Config, auth config.Auth) (*agent.Oracle, error) {
	or, err := provider.Resolve(cfg.OracleModel, cfg, auth)
	if err != nil {
		return nil, err
	}
	info, known := or.Info, or.Known
	return &agent.Oracle{Client: or.Client, Model: or.Model, Reasoning: or.Reasoning("high"), MaxOutput: info.Output,
		Cost: func(us provider.Usage) float64 {
			if !known {
				return 0
			}
			return info.Price(us.Input, us.Output, us.CacheRead, us.CacheWrite)
		}}, nil
}

// filesChanged lists the files a run changed, relative to the project:
// from the checkpoint taken before its first change (so files a command
// wrote count too), else from its successful edits.
func filesChanged(root string, edited []string, store *checkpoint.Store, sess *session.Session, cpBefore int) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(p string) {
		if filepath.IsAbs(p) {
			if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
				p = rel
			}
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if p != "." && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if store != nil && len(sess.Checkpoints) > cpBefore {
		if files, err := store.Changed(context.Background(), sess.Checkpoints[cpBefore].ID); err == nil {
			for _, f := range files {
				add(f)
			}
		}
	}
	for _, f := range edited {
		add(f)
	}
	sort.Strings(out)
	return out
}
