package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// Terminal presentation: colors, the live status area (what the agent is
// doing right now, with a spinner and a running command's latest output),
// and the permanent one-line record of each finished step.

const (
	cBold    = "1"
	cDim     = "2"
	cRed     = "31"
	cGreen   = "32"
	cYellow  = "33"
	cBlue    = "34"
	cMagenta = "35"
	cCyan    = "36"
	cGray    = "90"
)

// paint wraps s in an SGR color when the UI uses color.
func (u *ui) paint(code, s string) string {
	if !u.color || s == "" {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// liveTool is a tool call in progress.
type liveTool struct {
	call    provider.ToolCall
	name    string
	detail  string
	start   time.Time
	tail    []string // latest complete output lines
	partial string
}

const liveTail = 3

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]`)

// feed adds command output, keeping the last few lines. A carriage
// return (progress bars) replaces the current line.
func (t *liveTool) feed(p []byte) {
	s := ansiRE.ReplaceAllString(strings.ToValidUTF8(string(p), ""), "")
	for _, r := range s {
		switch r {
		case '\n':
			if strings.TrimSpace(t.partial) != "" {
				t.tail = append(t.tail, t.partial)
				if len(t.tail) > liveTail {
					t.tail = t.tail[len(t.tail)-liveTail:]
				}
			}
			t.partial = ""
		case '\r':
			t.partial = ""
		case '\t':
			t.partial += "  "
		default:
			if r >= 0x20 {
				t.partial += string(r)
			}
		}
	}
	if len(t.partial) > 4096 {
		t.partial = t.partial[len(t.partial)-4096:]
	}
}

func (t *liveTool) lines() []string {
	out := append([]string(nil), t.tail...)
	if strings.TrimSpace(t.partial) != "" {
		out = append(out, t.partial)
	}
	if len(out) > liveTail {
		out = out[len(out)-liveTail:]
	}
	return out
}

// startTicker animates the live area while something is in progress.
func (u *ui) startTicker() {
	go func() {
		for range time.Tick(100 * time.Millisecond) {
			u.mu.Lock()
			if u.thinking || len(u.tools) > 0 || u.keys != nil {
				u.frame++
				u.clearLive()
				u.drawLive()
			}
			u.mu.Unlock()
		}
	}()
}

// clearLive erases the live area; the caller holds u.mu.
func (u *ui) clearLive() {
	if u.drawn == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("\r\033[2K")
	for i := 1; i < u.drawn; i++ {
		sb.WriteString("\033[1A\033[2K")
	}
	os.Stderr.WriteString(sb.String())
	u.drawn = 0
}

// drawLive draws the live area below the cursor; the caller holds u.mu.
func (u *ui) drawLive() {
	if !u.live || u.paused || u.midLine || u.drawn > 0 {
		return
	}
	width := termWidth(os.Stderr) - 1
	if width < 20 {
		width = 20
	}
	spin := spinFrames[u.frame%len(spinFrames)]
	var lines []string
	if u.thinking && len(u.tools) == 0 {
		lines = append(lines, u.paint(cCyan, spin)+" "+u.paint(cDim, "Thinking… "+elapsed(time.Since(u.thinkT))))
	}
	for _, t := range u.tools {
		secs := " " + elapsed(time.Since(t.start))
		detail := truncate(t.detail, width-strWidth(t.name)-strWidth(secs)-4)
		lines = append(lines, u.paint(cYellow, spin)+" "+u.paint(cBold, t.name)+" "+detail+u.paint(cDim, secs))
		for _, l := range t.lines() {
			lines = append(lines, u.paint(cDim, "  │ "+truncate(l, width-4)))
		}
	}
	if len(lines) > 0 || len(u.typing) > 0 || len(u.queued) > 0 {
		lines = append(lines, u.typeaheadLines(width)...)
	}
	if len(lines) == 0 {
		return
	}
	os.Stderr.WriteString(strings.Join(lines, "\n"))
	u.drawn = len(lines)
}

func elapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// truncate cuts s to w display columns, adding "…".
func truncate(s string, w int) string {
	if w < 1 {
		return ""
	}
	if strWidth(s) <= w {
		return s
	}
	var sb strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > w-1 {
			break
		}
		sb.WriteRune(r)
		n += rw
	}
	return sb.String() + "…"
}

// permanent prints a finished line above the live area. While an
// approval prompt waits, lines are held and printed after it.
func (u *ui) permanent(s string) {
	if u.paused {
		u.held = append(u.held, s)
		return
	}
	u.clearLive()
	u.endLine()
	fmt.Fprintln(os.Stderr, s)
	u.drawLive()
}

func (u *ui) beginTurn() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.live {
		u.thinking, u.thinkT = true, time.Now()
	}
}

func (u *ui) endTurn() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLive()
	u.thinking, u.tools = false, nil
}

// think shows "Thinking…" again (e.g. before a follow-up model call).
func (u *ui) think() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.live && !u.thinking && len(u.tools) == 0 {
		u.thinking, u.thinkT = true, time.Now()
	}
}

func (u *ui) toolStart(c provider.ToolCall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLive()
	u.endLine()
	u.thinking = false
	u.tools = append(u.tools, &liveTool{call: c, name: toolLabel(c.Name), detail: u.detail(c), start: time.Now()})
	u.drawLive()
}

func (u *ui) toolOutput(c provider.ToolCall, p []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, t := range u.tools {
		if t.call.ID == c.ID {
			t.feed(p)
			return
		}
	}
}

var exitRE = regexp.MustCompile(`\n\[exit (\d+)\]$`)

func (u *ui) toolDone(c provider.ToolCall, out string, err error, d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, t := range u.tools {
		if t.call.ID == c.ID {
			u.tools = append(u.tools[:i], u.tools[i+1:]...)
			break
		}
	}
	icon, fail := u.paint(cGreen, "✓"), ""
	if errors.Is(err, context.Canceled) {
		icon, fail = u.paint(cYellow, "■"), "interrupted"
	} else if err != nil {
		icon, fail = u.paint(cRed, "✗"), firstLine(err.Error())
	} else if m := exitRE.FindStringSubmatch(out); m != nil {
		icon, fail = u.paint(cRed, "✗"), "exit "+m[1]
	}
	width := termWidth(os.Stderr) - 1
	dur := ""
	if d >= 500*time.Millisecond {
		dur = fmt.Sprintf(" %.1fs", d.Seconds())
	}
	name := toolLabel(c.Name)
	detail := truncate(u.detail(c), width-strWidth(name)-len(dur)-len(fail)-6)
	key := name + " " + detail
	if fail == "" && key == u.lastKey && u.afterTool && !u.paused && c.Name != "bash" {
		// The same step again (e.g. several edits to one file): count it
		// on the previous line instead of adding one.
		u.lastCount++
		u.clearLive()
		os.Stderr.WriteString("\033[1A\r\033[2K")
		fmt.Fprintln(os.Stderr, icon+" "+u.paint(cBold, name)+" "+detail+u.paint(cDim, fmt.Sprintf(" ×%d", u.lastCount)))
		u.drawLive()
	} else {
		line := icon + " " + u.paint(cBold, name) + " " + detail + u.paint(cDim, dur)
		if fail != "" {
			line += "  " + u.paint(cRed, fail)
		}
		u.permanent(line)
		u.lastKey, u.lastCount = key, 1
		if fail != "" || u.paused {
			u.lastKey = ""
		}
	}
	u.afterTool = true
	if len(u.tools) == 0 {
		u.thinking, u.thinkT = true, time.Now()
	}
}

func (u *ui) subTool(c provider.ToolCall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastKey = ""
	u.permanent(u.paint(cDim, "  ↳ "+toolLabel(c.Name)+" "+truncate(u.detail(c), termWidth(os.Stderr)-20)))
}

// toolLabel is a tool's display name.
func toolLabel(name string) string {
	switch name {
	case "bash":
		return "Bash"
	case "read":
		return "Read"
	case "edit":
		return "Edit"
	case "search":
		return "Search"
	case "fetch":
		return "Fetch"
	case "todo":
		return "Todo"
	case "task":
		return "Task"
	}
	if strings.HasPrefix(name, "mcp__") {
		return strings.Replace(strings.TrimPrefix(name, "mcp__"), "__", ":", 1)
	}
	return name
}

// detail summarizes a call's arguments for display, with paths relative
// to the workspace.
func (u *ui) detail(c provider.ToolCall) string {
	var m map[string]any
	_ = jsonUnmarshal(c.Args, &m)
	if c.Name == "todo" {
		items, _ := m["items"].([]any)
		done, current := 0, ""
		for _, it := range items {
			im, _ := it.(map[string]any)
			switch im["status"] {
			case "done":
				done++
			case "in_progress":
				if current == "" {
					current, _ = im["text"].(string)
				}
			}
		}
		s := fmt.Sprintf("%d/%d done", done, len(items))
		if current != "" {
			s += " · " + current
		}
		return s
	}
	s := strings.TrimPrefix(summarizeCall(c), c.Name+" ")
	if cmd, _ := m["cmd"].(string); c.Name == "bash" && strings.Contains(strings.TrimSpace(cmd), "\n") {
		// A script: its first line and how much follows.
		lines := strings.Split(strings.TrimSpace(cmd), "\n")
		s = strings.TrimSpace(lines[0]) + fmt.Sprintf("  (+%d lines)", len(lines)-1)
	}
	s = u.relative(s)
	return s
}

// relative strips the workspace from paths and leading "cd <workspace>".
func (u *ui) relative(s string) string {
	if u.cwd != "" {
		s = strings.TrimPrefix(s, "cd "+u.cwd+" && ")
		s = strings.TrimPrefix(s, "cd "+u.cwd+"; ")
		s = strings.ReplaceAll(s, u.cwd+string(filepath.Separator), "")
		if s == u.cwd {
			s = "."
		}
	}
	return s
}
