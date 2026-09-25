package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/mcp"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// mcpState is what became of the configured MCP servers.
type mcpState struct {
	clients []*mcp.Client
	failed  map[string]string // server name → why it did not start
}

// showMCP lists the configured MCP servers: up, down or failed to start,
// their tools, and where each server's log is.
func showMCP(u *ui, cfg config.Config, st *mcpState) {
	fmt.Fprintln(os.Stderr)
	if len(cfg.MCP) == 0 {
		u.note("No MCP servers yet. Add them under \"mcp\" in")
		u.note("  " + filepath.Join(config.Home(), "config.json"))
		u.note(`  local:  "fs": {"command": "npx", "args": ["-y", "server-pkg"]}`)
		u.note(`  remote: "docs": {"url": "https://host/mcp"}`)
		u.note("then restart agentium; agentium mcp login <name> for OAuth.")
		return
	}
	names := make([]string, 0, len(cfg.MCP))
	for n := range cfg.MCP {
		names = append(names, n)
	}
	sort.Strings(names)
	up := map[string]*mcp.Client{}
	if st != nil {
		for _, c := range st.clients {
			up[c.Name] = c
		}
	}
	for _, n := range names {
		sc := cfg.MCP[n]
		where := sc.URL
		if where == "" {
			where = strings.TrimSpace(sc.Command + " " + strings.Join(sc.Args, " "))
		}
		c := up[n]
		switch {
		case c != nil && c.Err() == nil:
			fmt.Fprintf(os.Stderr, "  %s %s %s\n", u.paint(cGreen, "●"), u.paint(cBold, n), u.paint(cDim, fmt.Sprintf("%d tools · %s", len(c.Tools), truncate(where, 50))))
			var tools []string
			for _, t := range c.Tools {
				tools = append(tools, t.Name)
			}
			if len(tools) > 0 {
				u.note("    " + truncate(strings.Join(tools, ", "), termWidth(os.Stderr)-8))
			}
		case c != nil:
			fmt.Fprintf(os.Stderr, "  %s %s %s\n", u.paint(cRed, "●"), u.paint(cBold, n), u.paint(cRed, "stopped: "+firstLine(c.Err().Error())))
		default:
			why := "not started"
			if st != nil && st.failed[n] != "" {
				why = st.failed[n]
			}
			lines := strings.Split(why, "\n")
			head := strings.TrimPrefix(lines[0], "mcp "+n+": ")
			fmt.Fprintf(os.Stderr, "  %s %s %s\n", u.paint(cRed, "●"), u.paint(cBold, n), u.paint(cRed, head))
			for _, l := range lines[1:] {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "server stderr (") {
					u.note("    │ " + truncate(l, termWidth(os.Stderr)-10))
				}
			}
		}
		if sc.URL == "" {
			u.note("    log " + mcpLog(n))
		}
	}
	u.note("Servers start with agentium; after changing the config, restart it.")
}

func mcpLog(name string) string {
	return filepath.Join(config.Home(), "logs", "mcp-"+name+".log")
}

// showTools lists the tools the model can call, built in and from MCP.
func showTools(u *ui, a *agent.Agent) {
	fmt.Fprintln(os.Stderr)
	width := termWidth(os.Stderr) - 1
	var ext []string
	for _, t := range a.Tools {
		name := t.Def.Name
		if strings.HasPrefix(name, "mcp__") {
			ext = append(ext, name)
			continue
		}
		desc := firstLine(t.Def.Description)
		pad := strings.Repeat(" ", max(6-strWidth(styleFor(name).label), 0))
		fmt.Fprintf(os.Stderr, "  %s%s %-8s %s\n", u.chip(name), pad, name, u.paint(cDim, truncate(desc, max(width-24, 20))))
	}
	if len(ext) > 0 {
		fmt.Fprintln(os.Stderr)
		u.note(fmt.Sprintf("%d from MCP servers (/mcp for details):", len(ext)))
		u.note("  " + truncate(strings.Join(ext, ", "), width*3))
	}
}

// doctor checks what agentium depends on and says what to fix.
func doctor(u *ui, e *slashEnv, st *mcpState) {
	ok := func(k, v string) {
		fmt.Fprintf(os.Stderr, "  %s %-11s %s\n", u.paint(cGreen, "✓"), k, v)
	}
	warn := func(k, v string) {
		fmt.Fprintf(os.Stderr, "  %s %-11s %s\n", u.paint(cYellow, "!"), k, v)
	}
	bad := func(k, v string) {
		fmt.Fprintf(os.Stderr, "  %s %-11s %s\n", u.paint(cRed, "✗"), k, v)
	}
	fmt.Fprintln(os.Stderr)
	ok("version", fmt.Sprintf("agentium %s · %s/%s", version, runtime.GOOS, runtime.GOARCH))

	// Latest release and the model, checked at the same time.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	latest := make(chan string, 1)
	go func() {
		lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
		defer lcancel()
		v, err := latestVersion(lctx)
		if err != nil {
			latest <- ""
			return
		}
		latest <- v
	}()
	type ping struct {
		d   time.Duration
		err error
	}
	pinged := make(chan ping, 1)
	go func() {
		t0 := time.Now()
		_, err := e.a.Client.Stream(ctx, provider.Request{
			Model:     e.a.Model,
			Messages:  []provider.Message{{Role: provider.RoleUser, Text: "Reply with: ok"}},
			MaxTokens: 16,
		}, nil)
		pinged <- ping{time.Since(t0), err}
	}()

	if _, err := config.Load(); err != nil {
		bad("config", err.Error())
	} else {
		ok("config", filepath.Join(config.Home(), "config.json"))
	}
	if f, err := os.CreateTemp(config.Home(), ".doctor"); err != nil {
		bad("home", config.Home()+" is not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
	}
	u.note("  checking " + e.sess.Model + " …")
	if p := <-pinged; p.err != nil {
		bad("model", e.sess.Model+": "+firstLine(p.err.Error()))
		u.note("              /login to change the key, /model to pick another")
	} else {
		ok("model", fmt.Sprintf("%s answered in %.1fs", e.sess.Model, p.d.Seconds()))
	}
	if v := <-latest; v == "" {
		warn("update", "could not check for a newer release")
	} else if newer(v, version) {
		warn("update", v+" is available · /update")
	} else {
		ok("update", "up to date")
	}

	for _, t := range []struct{ bin, why string }{
		{"git", "diffs, @files, /review, /rewind across branches"},
		{"rg", "faster search (falls back to a built-in search)"},
	} {
		if p, err := exec.LookPath(t.bin); err == nil {
			ok(t.bin, p)
		} else {
			warn(t.bin, "not found · "+t.why)
		}
	}
	if ed := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR")); ed != "" {
		ok("editor", ed+" (ctrl+g)")
	} else {
		warn("editor", "$EDITOR not set · ctrl+g falls back to vi")
	}
	if e.a.Env.Sandbox != nil {
		ok("sandbox", e.box)
	} else {
		warn("sandbox", e.box+" · commands run unconfined")
	}

	term := firstNonEmpty(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM"), "unknown")
	colors := "256 colors"
	if ct := os.Getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
		colors = "truecolor"
	}
	ok("terminal", term+" · "+colors)

	files := agent.ContextFiles(e.cfgRoot())
	if len(files) == 0 {
		warn("project", "no AGENTS.md or CLAUDE.md · /init writes one")
	} else {
		ok("project", strings.Join(files, ", "))
	}

	if len(e.cfg.MCP) > 0 {
		n, down := 0, 0
		if st != nil {
			for _, c := range st.clients {
				if c.Err() == nil {
					n++
				}
			}
		}
		down = len(e.cfg.MCP) - n
		if down > 0 {
			warn("mcp", fmt.Sprintf("%d of %d servers down · /mcp", down, len(e.cfg.MCP)))
		} else {
			ok("mcp", fmt.Sprintf("%d servers up", n))
		}
	}
}

// cfgRoot is the directory the session works in.
func (e *slashEnv) cfgRoot() string { return e.a.Env.Root }
