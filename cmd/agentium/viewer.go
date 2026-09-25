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
	diff  bool // color as a unified diff
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

// keepThought keeps what the model thought before a step, for the viewer
// (Claude Code's "ctrl+o to see thinking").
func (u *ui) keepThought(t string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var lines []string
	for _, l := range strings.Split(sanitizeKeepTabs(t), "\n") {
		lines = append(lines, wordWrap(l, max(termWidth(os.Stderr)-10, 30))...)
	}
	words := len(strings.Fields(t))
	u.outputs = append(u.outputs, stepOutput{title: fmt.Sprintf("Thinking · %d words", words), lines: lines})
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
		if r < 0x20 && r != '\n' || r == 0x7f || r >= 0x80 && r < 0xa0 { // C0, DEL, C1
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
	if f := activeFS(); f != nil {
		f.mu.Lock()
		// The conversation so far, as it is on screen.
		tr := stepOutput{title: "Conversation"}
		for i := 0; i < f.vt.end(); i++ {
			l, _ := f.vt.renderW(i, 1<<16)
			tr.lines = append(tr.lines, l)
		}
		p := &pager{outs: outs, cur: len(outs) - 1, tr: tr, hit: -1}
		if len(outs) == 0 {
			p.showTr, p.top = true, len(tr.lines)
		}
		f.pager = p
		f.dirty = true
		f.mu.Unlock()
		return
	}
	if len(outs) == 0 {
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

// pager shows the full output of recent steps, or the whole conversation
// (t), with search (/, n, N), jumps between your messages ([ ]) and the
// text in $EDITOR (e).
type pager struct {
	outs   []stepOutput
	cur    int
	tr     stepOutput // the conversation
	showTr bool
	top    int
	h      int

	typing bool   // reading a search query
	query  []rune // the search
	hit    int    // line of the current match, -1 for none
	status string // a note in the header (no match …)
	edit   string // text to open in $EDITOR once the lock is released
}

func (p *pager) view() stepOutput {
	if p.showTr || len(p.outs) == 0 {
		return p.tr
	}
	return p.outs[p.cur]
}

// find moves to the next (dir 1) or previous (-1) line matching the
// query, starting after from.
func (p *pager) find(from, dir int) {
	lines := p.view().lines
	q := strings.ToLower(string(p.query))
	p.status = ""
	if q == "" || len(lines) == 0 {
		return
	}
	for k := 1; k <= len(lines); k++ {
		i := ((from+dir*k)%len(lines) + len(lines)) % len(lines)
		if strings.Contains(strings.ToLower(stripANSI(lines[i])), q) {
			p.hit = i
			p.top = i - p.h/3
			if (dir > 0 && i <= from) || (dir < 0 && i >= from) {
				p.status = "wrapped"
			}
			return
		}
	}
	p.hit, p.status = -1, "no match"
}

// jump moves to the next or previous message you sent.
func (p *pager) jump(dir int) {
	lines := p.view().lines
	mine := func(i int) bool { return i >= 0 && i < len(lines) && strings.HasPrefix(stripANSI(lines[i]), "▌") }
	for i := p.top + dir; i >= 0 && i < len(lines); i += dir {
		if mine(i) && !mine(i-1) { // the first line of a message
			p.top = i
			return
		}
	}
}

// key handles a key; it reports whether the pager closes.
func (p *pager) key(k string) bool {
	if p.typing {
		switch {
		case k == "\r" || k == "\n":
			p.typing = false
			p.find(p.top-1, 1)
		case k == "\x1b" || k == "\x03":
			p.typing, p.query = false, nil
		case k == "\x7f" || k == "\x08":
			if len(p.query) > 0 {
				p.query = p.query[:len(p.query)-1]
			}
		case len(k) > 0 && k[0] >= 0x20 && k[0] != 0x7f:
			p.query = append(p.query, []rune(k)...)
		}
		return false
	}
	lines := len(p.view().lines)
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
		if !p.showTr && p.cur > 0 {
			p.cur, p.top, p.hit = p.cur-1, 0, -1
		}
	case "\x1b[C", "l":
		if !p.showTr && p.cur < len(p.outs)-1 {
			p.cur, p.top, p.hit = p.cur+1, 0, -1
		}
	case "t":
		if len(p.outs) > 0 {
			p.showTr, p.hit = !p.showTr, -1
			p.top = 0
			if p.showTr {
				p.top = len(p.tr.lines)
			}
		}
	case "/":
		p.typing, p.query, p.status = true, nil, ""
	case "n":
		p.find(max(p.hit, p.top), 1)
	case "N":
		from := p.hit
		if from < 0 {
			from = p.top + p.h
		}
		p.find(from, -1)
	case "[":
		p.jump(-1)
	case "]":
		p.jump(1)
	case "e":
		var sb strings.Builder
		for _, l := range p.view().lines {
			sb.WriteString(stripANSI(l) + "\n")
		}
		p.edit = sb.String()
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
	o := p.view()
	p.top = min(max(p.top, 0), max(len(o.lines)-p.h, 0))
	where := fmt.Sprintf("t conversation · ←→ step %d/%d", p.cur+1, len(p.outs))
	if p.showTr || len(p.outs) == 0 {
		where = "[ ] your messages"
		if len(p.outs) > 0 {
			where += " · t step outputs"
		}
	}
	hints := "/ search · " + where + " · e editor · q close"
	switch {
	case p.typing:
		hints = "search: " + string(p.query) + "▏ · enter find · esc cancel"
	case len(p.query) > 0:
		hints = "“" + string(p.query) + "” " + p.status + " · n next · N previous · " + hints
	}
	head := fmt.Sprintf(" %s  %s", truncate(o.title, max(w/3, 12)), sgr(cGray)+truncate(hints, max(w-w/3-6, 10))+"\x1b[0m")
	rows := []string{padTo("\x1b[1m"+head+"\x1b[0m", w), sgr(cGray) + strings.Repeat("─", w) + "\x1b[0m"}
	numW := len(fmt.Sprint(len(o.lines)))
	q := strings.ToLower(string(p.query))
	for i := p.top; i < len(o.lines) && len(rows) < h; i++ {
		l := truncate(o.lines[i], w-numW-3)
		if o.diff {
			l = (&ui{color: true}).diffLineColor(l)
		}
		num := sgr(cGray) + fmt.Sprintf("%*d", numW, i+1) + "\x1b[0m"
		if q != "" && !p.typing && strings.Contains(strings.ToLower(stripANSI(o.lines[i])), q) {
			num = sgr(cAccent) + fmt.Sprintf("%*d", numW, i+1) + "\x1b[0m" // a match
			if i == p.hit {
				num = "\x1b[7m" + num
			}
		}
		rows = append(rows, padTo(" "+num+" "+l, w))
	}
	for len(rows) < h {
		rows = append(rows, strings.Repeat(" ", w))
	}
	return rows
}

// editFile opens path in the user's editor.
func editFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text, err := externalEdit(string(b))
	if err != nil {
		return err
	}
	if text == strings.TrimRight(string(b), "\n") {
		return nil
	}
	return os.WriteFile(path, []byte(text+"\n"), 0o600)
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

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
