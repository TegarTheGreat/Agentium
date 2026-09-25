package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/session"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// Commands gathered from the other agent CLIs: ! runs a shell command
// yourself, /diff shows what changed, /compact and /context manage the
// context window, /theme switches the palette, /btw asks a side question.

// shellCommand runs a command the user typed after "!" and tells the agent
// about it with the next message.
func shellCommand(u *ui, a *agent.Agent, cwd, cmdline string) {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmdline)
	if isWindows() {
		c = exec.CommandContext(ctx, "cmd", "/c", cmdline)
	}
	c.Dir = cwd
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	start := time.Now()
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
		out.WriteString(err.Error())
	}
	text := out.String()
	args, _ := json.Marshal(map[string]string{"cmd": cmdline})
	call := provider.ToolCall{Name: "bash", Args: args}
	icon := u.paint(cGreen, "✓")
	if code != 0 {
		icon = u.paint(cRed, "✗")
	}
	width := termWidth(os.Stderr) - 1
	fmt.Fprintf(os.Stderr, "  %s %s %s%s\n", icon, u.chip("bash"), truncate(u.detail(call), width-30),
		u.paint(cDim, fmt.Sprintf(" %.1fs · exit %d · you ran this", time.Since(start).Seconds(), code)))
	lines := strings.Split(strings.TrimRight(ansiRE.ReplaceAllString(text, ""), "\n"), "\n")
	if len(lines) > 20 {
		lines = append([]string{fmt.Sprintf("… %d earlier lines · ctrl+o shows all", len(lines)-20)}, lines[len(lines)-20:]...)
	}
	for _, l := range u.gutter(lines) {
		if strings.TrimSpace(text) != "" {
			fmt.Fprintln(os.Stderr, truncate(l, width+20))
		}
	}
	u.mu.Lock()
	u.keepOutput(call, text)
	u.mu.Unlock()
	note := fmt.Sprintf("The user ran `%s` in the shell (exit %d). Output:\n%s", cmdline, code, tool.Clip(text, 6000))
	a.Note = strings.TrimSpace(a.Note + "\n\n" + note)
}

func isWindows() bool { return os.PathSeparator == '\\' }

// showDiff shows the working tree's changes against HEAD.
func showDiff(u *ui, cwd string) {
	cmd := exec.Command("git", "-c", "core.fsmonitor=false", "-c", "core.pager=cat", "diff", "HEAD", "--no-color", "--no-ext-diff")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		cmd = exec.Command("git", "-c", "core.fsmonitor=false", "diff", "--no-color", "--no-ext-diff")
		cmd.Dir = cwd
		out, err = cmd.Output()
	}
	if err != nil {
		u.failure("no git repository here, or git failed")
		return
	}
	untracked, _ := exec.Command("git", "-C", cwd, "ls-files", "--others", "--exclude-standard").Output()
	text := string(out)
	if u := strings.TrimSpace(string(untracked)); u != "" {
		text += "\nNew files (untracked):\n" + u + "\n"
	}
	if strings.TrimSpace(text) == "" {
		u.note("no changes against HEAD")
		return
	}
	lines := strings.Split(sanitizeKeepTabs(strings.TrimRight(text, "\n")), "\n")
	if f := activeFS(); f != nil {
		f.mu.Lock()
		f.pager = &pager{outs: []stepOutput{{title: "Changes against HEAD", lines: lines, diff: true}}}
		f.dirty = true
		f.mu.Unlock()
		return
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(u.diffLineColor(l) + "\n")
	}
	os.Stderr.WriteString(sb.String())
}

// diffLineColor colors a unified diff line.
func (u *ui) diffLineColor(l string) string {
	switch {
	case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "diff "):
		return u.paint(cBold, l)
	case strings.HasPrefix(l, "+"):
		return u.paint(cGreen, l)
	case strings.HasPrefix(l, "-"):
		return u.paint(cRed, l)
	case strings.HasPrefix(l, "@@"):
		return u.paint(cCyan, l)
	}
	return l
}

// showContext draws what fills the context window.
func showContext(u *ui, a *agent.Agent) {
	sys, tools, conv := a.ContextBreakdown()
	used, limit := a.ContextUsed()
	if used == 0 {
		used = sys + tools + conv
	}
	if limit <= 0 {
		limit = max(used, 1)
	}
	bar := func(n int, color string) string {
		cells := min(n*40/limit, 40)
		if n > 0 && cells == 0 {
			cells = 1
		}
		return u.paint(color, strings.Repeat("█", cells))
	}
	pct := used * 100 / limit
	var sb strings.Builder
	sb.WriteString("\n  " + u.paint(cBold, "Context") + u.paint(cDim, fmt.Sprintf("  %s of %s tokens (%d%%)", fmtK(used), fmtK(limit), pct)) + "\n  ")
	sb.WriteString(bar(sys, cMagenta) + bar(tools, cCyan) + bar(conv, cAccent))
	sb.WriteString(u.paint(cGray, strings.Repeat("░", max(40-min(sys*40/limit, 40)-min(tools*40/limit, 40)-min(conv*40/limit, 40), 0))) + "\n")
	row := func(color, name string, n int) {
		sb.WriteString("  " + u.paint(color, "█") + " " + fmt.Sprintf("%-14s", name) + u.paint(cDim, fmtK(n)+" tokens") + "\n")
	}
	row(cMagenta, "instructions", sys)
	row(cCyan, "tools", tools)
	row(cAccent, "conversation", conv)
	sb.WriteString(u.paint(cDim, fmt.Sprintf("  %d messages · /compact summarizes older ones · /clear starts over", len(a.Messages))) + "\n")
	os.Stderr.WriteString(sb.String())
}

// compactNow summarizes the older conversation on request.
func compactNow(u *ui, a *agent.Agent) {
	_, _, before := a.ContextBreakdown()
	u.note("summarizing the older part of the conversation…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := a.Compact(ctx); err != nil {
		u.failure("compact: " + err.Error())
		return
	}
	_, _, after := a.ContextBreakdown()
	u.success(fmt.Sprintf("Compacted the conversation: %s → %s tokens", fmtK(before), fmtK(after)))
}

// pickTheme switches the palette and saves the choice.
func pickTheme(u *ui, arg string) {
	if arg == "" {
		v, err := u.choose("Theme", []menuItem{
			{value: "auto", label: "auto", hint: "follow the terminal's background"},
			{value: "dark", label: "dark", hint: "Night Shift"},
			{value: "light", label: "light", hint: "Day Shift"},
		}, "auto", false)
		if err != nil {
			return
		}
		arg = v
	}
	applyTheme(arg)
	_ = config.Set("theme", arg)
	if f := activeFS(); f != nil {
		f.redraw()
	}
	u.success("Theme: " + arg + u.paint(cDim, " (saved; earlier lines keep their colors)"))
}

// sideQuestion answers a quick question about the conversation without
// adding it to the history or running tools (/btw).
func sideQuestion(u *ui, a *agent.Agent, q string) {
	if strings.TrimSpace(q) == "" {
		u.note("usage: /btw <question> — answered on the side, not added to the conversation")
		return
	}
	msgs := append(append([]provider.Message(nil), a.Messages...), provider.Message{Role: provider.RoleUser,
		Text: "[side question; answer briefly from what you already know, without tools]\n" + q})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	u.note("asking on the side…")
	resp, err := a.Client.Stream(ctx, provider.Request{Model: a.Model, System: a.System, Messages: msgs,
		Tools: tool.Defs(a.Tools), MaxTokens: 2000}, nil)
	if err != nil {
		u.failure("btw: " + firstLine(err.Error()))
		return
	}
	var sb strings.Builder
	sb.WriteString("  " + u.paint(cInk, "◇ btw") + u.paint(cDim, " · not added to the conversation") + "\n")
	for _, l := range wordWrap(strings.TrimSpace(resp.Text), termWidth(os.Stderr)-8) {
		sb.WriteString("  " + u.paint(cInk, "│") + " " + l + "\n")
	}
	os.Stderr.WriteString(sb.String())
}

// exportSession writes the conversation as Markdown (/export [file]).
func exportSession(u *ui, a *agent.Agent, sess *session.Session, arg string) {
	path := strings.TrimSpace(arg)
	if path == "" {
		path = "agentium-" + sess.ID + ".md"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(sess.Cwd, path)
	}
	if strings.HasSuffix(strings.ToLower(path), ".html") {
		// 0600: step outputs can hold things only you should see.
		if err := os.WriteFile(path, []byte(exportHTML(a.Messages, sess)), 0o600); err != nil {
			u.failure("export: " + err.Error())
			return
		}
		u.success("Saved the conversation as a page: " + shortPath(path) + u.paint(cDim, " · open it in a browser; check the step outputs before you share it"))
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Agentium session %s\n\n_%s · %s_\n", sess.ID, sess.Model, shortPath(sess.Cwd))
	for _, m := range a.Messages {
		switch m.Role {
		case provider.RoleUser:
			if strings.HasPrefix(m.Text, "[agentium]") {
				continue
			}
			sb.WriteString("\n## You\n\n" + strings.TrimSpace(firstNonEmpty(m.Typed, provider.UserWords(m.Text))) + "\n")
		case provider.RoleAssistant:
			if t := strings.TrimSpace(m.Text); t != "" {
				sb.WriteString("\n## Agentium\n\n" + t + "\n")
			}
			for _, c := range m.ToolCalls {
				sb.WriteString("\n- `" + strings.ReplaceAll(summarizeCall(c), "`", "'") + "`\n")
			}
		}
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		u.failure("export: " + err.Error())
		return
	}
	u.success("Saved the conversation to " + shortPath(path))
}
