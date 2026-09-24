package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// Ctrl-O shows the full output of recent steps; Ctrl-G edits the message
// in $VISUAL or $EDITOR.

type stepOutput struct {
	title string
	lines []string
}

const keptOutputs = 20

// keepOutput remembers a step's output for the viewer; the caller holds
// u.mu.
func (u *ui) keepOutput(c provider.ToolCall, out string) {
	if strings.TrimSpace(out) == "" || c.Name == "todo" {
		return
	}
	if len(out) > 256<<10 {
		out = out[:256<<10] + "\n… (clipped)"
	}
	title := toolLabel(c.Name) + " " + u.detail(c)
	u.outputs = append(u.outputs, stepOutput{title: title, lines: strings.Split(sanitizeKeepTabs(out), "\n")})
	if len(u.outputs) > keptOutputs {
		u.outputs = u.outputs[len(u.outputs)-keptOutputs:]
	}
}

func sanitizeKeepTabs(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 && r != '\n' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// openViewer shows the latest step's output: in the full screen as a
// pager, inline by printing it.
func (u *ui) openViewer() {
	u.mu.Lock()
	outs := append([]stepOutput(nil), u.outputs...)
	u.mu.Unlock()
	if len(outs) == 0 {
		return
	}
	if f := activeFS(); f != nil {
		f.mu.Lock()
		f.pager = &pager{outs: outs, cur: len(outs) - 1}
		f.dirty = true
		f.mu.Unlock()
		return
	}
	o := outs[len(outs)-1]
	var sb strings.Builder
	sb.WriteString("\r\x1b[K" + u.paint(cBold, "  "+o.title) + "\r\n")
	for _, l := range o.lines {
		sb.WriteString(u.paint(cDim, "  │ ") + l + "\r\n")
	}
	os.Stderr.WriteString(sb.String())
}

type pager struct {
	outs []stepOutput
	cur  int
	top  int
	h    int
}

// key handles a key; it reports whether the pager closes.
func (p *pager) key(k string) bool {
	lines := len(p.outs[p.cur].lines)
	switch k {
	case "q", "\x1b", "\x0f", "\x03":
		return true
	case "\x1b[A", "k":
		p.top--
	case "\x1b[B", "j", "\r":
		p.top++
	case "\x1b[5~", "b":
		p.top -= p.h - 2
	case "\x1b[6~", " ":
		p.top += p.h - 2
	case "g", "\x1b[H":
		p.top = 0
	case "G", "\x1b[F":
		p.top = lines
	case "\x1b[D", "h":
		if p.cur > 0 {
			p.cur, p.top = p.cur-1, 0
		}
	case "\x1b[C", "l":
		if p.cur < len(p.outs)-1 {
			p.cur, p.top = p.cur+1, 0
		}
	case "\x1b[<64":
	}
	if strings.HasPrefix(k, "\x1b[<64;") {
		p.top -= 3
	} else if strings.HasPrefix(k, "\x1b[<65;") {
		p.top += 3
	}
	return false
}

// rows draws the pager in w columns and h rows.
func (p *pager) rows(w, h int) []string {
	p.h = h - 2
	o := p.outs[p.cur]
	p.top = min(max(p.top, 0), max(len(o.lines)-p.h, 0))
	head := fmt.Sprintf(" %s  %s", truncate(o.title, w-30), sgr(cGray)+fmt.Sprintf("%d/%d · ←→ step · q close", p.cur+1, len(p.outs))+"\x1b[0m")
	rows := []string{padTo("\x1b[1m"+head+"\x1b[0m", w), sgr(cGray) + strings.Repeat("─", w) + "\x1b[0m"}
	numW := len(fmt.Sprint(len(o.lines)))
	for i := p.top; i < len(o.lines) && len(rows) < h; i++ {
		l := truncate(o.lines[i], w-numW-3)
		rows = append(rows, padTo(" "+sgr(cGray)+fmt.Sprintf("%*d", numW, i+1)+"\x1b[0m "+l, w))
	}
	for len(rows) < h {
		rows = append(rows, strings.Repeat(" ", w))
	}
	return rows
}

// externalEdit lets the user write the message in their editor.
func externalEdit(text string) (string, error) {
	ed := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if ed == "" {
		for _, c := range []string{"nano", "vim", "vi"} {
			if _, err := exec.LookPath(c); err == nil {
				ed = c
				break
			}
		}
	}
	if ed == "" {
		return text, fmt.Errorf("set $EDITOR to write messages in an editor")
	}
	tmp, err := os.CreateTemp("", "agentium-*.md")
	if err != nil {
		return text, err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(text)
	tmp.Close()
	f := activeFS()
	out := os.Stderr
	if f != nil {
		out = f.tty
		f.suspend()
		defer f.resume()
	}
	err = withCookedTerm(os.Stdin, func() error {
		parts := strings.Fields(ed)
		cmd := exec.Command(parts[0], append(parts[1:], tmp.Name())...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, out
		return cmd.Run()
	})
	if err != nil {
		return text, err
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return text, err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// copyToClipboard copies text with the system clipboard tool when there
// is one, and always with OSC 52 (which also works over SSH); it says how.
func copyToClipboard(text string) string {
	how := "via the terminal (OSC 52)"
	for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}, {"clip.exe"}} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if c[0] != "pbcopy" && c[0] != "clip.exe" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			how = "to the clipboard"
			break
		}
	}
	tty := os.Stderr
	if f := activeFS(); f != nil {
		tty = f.tty
	}
	tty.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07")
	return how
}
