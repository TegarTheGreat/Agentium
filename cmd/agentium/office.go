package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The office: a pixel-art scene at the top of the full-screen sidebar.
// Agentium sits at a desk and visibly does what it is doing — reading a
// document, typing, watching a terminal, browsing, thinking — and each
// sub-agent appears as a staff member at their own desk. Drawn with
// half-block characters (two pixels per cell) at 4 frames a second, and
// only redrawn when a frame changes, so it stays light.

const (
	officeFrame = 250 * time.Millisecond
)

// Activities an actor can show.
const (
	actIdle     = "idle"
	actThink    = "think"
	actRead     = "read"
	actSearch   = "search"
	actWrite    = "write"
	actRun      = "run"
	actWeb      = "web"
	actPlan     = "plan"
	actDelegate = "delegate"
	actWait     = "wait"
	actDone     = "done"
	actFail     = "fail"
)

type actor struct {
	key    string // sub-agents: the task's prompt
	name   string
	title  string
	act    string
	detail string
	since  time.Time
	born   time.Time
	leave  time.Time // when set, the staff member leaves at that time
	shirt  rgb
	hair   rgb
}

type office struct {
	mu    sync.Mutex
	lead  actor
	staff []*actor
	hired int
	done  int // turns finished this session
	start time.Time
}

func newOffice() *office {
	now := time.Now()
	return &office{lead: actor{name: "Agentium", act: actIdle, since: now, born: now.Add(-time.Hour),
		shirt: rgb{70, 130, 220}, hair: rgb{55, 40, 32}}, start: now}
}

var staffLooks = []struct{ shirt, hair rgb }{
	{rgb{230, 126, 70}, rgb{30, 30, 36}},
	{rgb{90, 180, 110}, rgb{150, 90, 40}},
	{rgb{180, 110, 210}, rgb{230, 200, 110}},
	{rgb{220, 90, 110}, rgb{70, 50, 40}},
}

// actFor maps a tool to what the actor visibly does.
func actFor(tool string) string {
	switch tool {
	case "read", "list", "outline", "symbols", "refs", "map":
		return actRead
	case "search", "glob", "grep":
		return actSearch
	case "edit", "write", "patch":
		return actWrite
	case "bash":
		return actRun
	case "fetch", "web_search", "websearch", "web":
		return actWeb
	case "todo", "memory", "remember":
		return actPlan
	case "task":
		return actDelegate
	}
	return actRun
}

func (o *office) setLead(act, detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lead.act != act || o.lead.detail != detail {
		o.lead.act, o.lead.detail, o.lead.since = act, detail, time.Now()
	}
}

// hire seats a sub-agent; a named specialist keeps its name.
func (o *office) hire(key, title, name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	look := staffLooks[o.hired%len(staffLooks)]
	o.hired++
	now := time.Now()
	if name == "" {
		name = fmt.Sprintf("Staff %d", o.hired)
	}
	o.staff = append(o.staff, &actor{key: key, name: truncate(name, 14), title: title, act: actThink,
		since: now, born: now, shirt: look.shirt, hair: look.hair})
}

func (o *office) staffDo(key, act, detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.staff {
		if s.key == key && s.leave.IsZero() {
			s.act, s.detail, s.since = act, detail, time.Now()
			return
		}
	}
}

// dismiss shows the staff member's result briefly, then lets them go.
func (o *office) dismiss(key string, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.staff {
		if s.key == key && s.leave.IsZero() {
			s.act, s.detail, s.since = actDone, "done", time.Now()
			if !ok {
				s.act, s.detail = actFail, "stopped"
			}
			s.leave = time.Now().Add(2 * time.Second)
			return
		}
	}
}

// render draws Agentium's desk and up to two staff desks, each with
// who it is and what they are doing written beside it, for a sidebar
// width columns wide.
func (o *office) render(width int, truecolor bool) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	kept := o.staff[:0]
	for _, s := range o.staff {
		if s.leave.IsZero() || now.Before(s.leave) {
			kept = append(kept, s)
		}
	}
	o.staff = kept
	if (o.lead.act == actDone || o.lead.act == actFail) && now.Sub(o.lead.since) > 3*time.Second {
		o.lead.act, o.lead.detail, o.lead.since = actIdle, "", now
	}
	frame := int(now.Sub(o.start) / officeFrame)
	if reduceMotion {
		frame = 0
	}
	var rows []string
	// Agentium's corner, full width, and a line on what it is doing.
	lead := &o.lead
	for _, p := range leadScene(lead, frame, now).halfBlocks(truecolor) {
		rows = append(rows, cropCols(p, width))
	}
	status := "\x1b[1mAgentium\x1b[0m \x1b[" + cAccent + "m" + actWord(lead.act) + "\x1b[0m"
	if lead.act != actIdle {
		status += "\x1b[2m · " + elapsed(now.Sub(lead.since)) + "\x1b[0m"
	}
	rows = append(rows, status)
	if lead.act != actIdle && lead.detail != "" {
		rows = append(rows, "\x1b[2m"+truncate(lead.detail, width)+"\x1b[0m")
	}
	// Staff, two desks side by side.
	var staff []*actor
	for i := 0; i < len(o.staff) && i < 2; i++ {
		staff = append(staff, o.staff[i])
	}
	if len(staff) > 0 {
		rows = append(rows, "")
		var pics [][]string
		for _, s := range staff {
			pics = append(pics, staffScene(s, frame, now).halfBlocks(truecolor))
		}
		colW := staffW + 1
		for j := range pics[0] {
			line := ""
			for k, p := range pics {
				if k > 0 {
					line += " "
				}
				line += p[j]
			}
			if len(pics) == 1 { // one desk: who and what beside it
				s := staff[0]
				side := []string{
					fgColor(s.shirt, truecolor) + "\x1b[1m" + truncate(s.name, width-colW) + "\x1b[0m",
					"\x1b[" + cAccent + "m" + actWord(s.act) + "\x1b[0m\x1b[2m · " + elapsed(now.Sub(s.since)) + "\x1b[0m",
				}
				about := s.detail
				if about == "" || s.act == actThink {
					about = s.title
				}
				for _, l := range wordWrap(about, width-colW) {
					side = append(side, "\x1b[2m"+truncate(l, width-colW)+"\x1b[0m")
				}
				if j > 0 && j-1 < len(side) && j-1 < 4 {
					line += " " + side[j-1]
				}
			}
			rows = append(rows, line)
		}
		if len(pics) == 2 { // names and doings under the desks
			label := func(f func(*actor) string) string {
				a := padTo(f(staff[0]), staffW)
				return a + " " + f(staff[1])
			}
			rows = append(rows, label(func(s *actor) string {
				return fgColor(s.shirt, truecolor) + "\x1b[1m" + truncate(s.name, staffW) + "\x1b[0m"
			}))
			rows = append(rows, label(func(s *actor) string {
				return "\x1b[" + cAccent + "m" + truncate(actWord(s.act)+" · "+elapsed(now.Sub(s.since)), staffW) + "\x1b[0m"
			}))
		}
	}
	if extra := len(o.staff) - 2; extra > 0 {
		rows = append(rows, fmt.Sprintf("\x1b[2m+%d more staff at work\x1b[0m", extra))
	}
	return rows
}

// cropCols keeps the first w cells of a half-block row (for a sidebar
// narrower than the scene).
func cropCols(row string, w int) string {
	if w >= sceneW {
		return row
	}
	var sb strings.Builder
	n := 0
	for i := 0; i < len(row) && n < w; {
		if row[i] == 0x1b {
			j := strings.IndexByte(row[i:], 'm')
			if j < 0 {
				break
			}
			sb.WriteString(row[i : i+j+1])
			i += j + 1
			continue
		}
		_, size := utf8.DecodeRuneInString(row[i:])
		sb.WriteString(row[i : i+size])
		i += size
		n++
	}
	return sb.String() + "\x1b[0m"
}

// staffOf is the color and name of the staff member working on task.
func (o *office) staffOf(task string) (rgb, string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.staff {
		if s.key == task {
			return s.shirt, s.name, true
		}
	}
	return rgb{}, "", false
}

// leadState returns what Agentium is doing.
func (o *office) leadState() actor {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lead
}

// finished counts a finished turn.
func (o *office) finished() {
	o.mu.Lock()
	o.done++
	o.mu.Unlock()
}

func actWord(act string) string {
	switch act {
	case actThink:
		return "thinking"
	case actRead:
		return "reading"
	case actSearch:
		return "searching"
	case actWrite:
		return "writing"
	case actRun:
		return "running"
	case actWeb:
		return "browsing"
	case actPlan:
		return "planning"
	case actDelegate:
		return "briefing"
	case actWait:
		return "asking"
	case actDone:
		return "done"
	case actFail:
		return "stopped"
	}
	return "idle"
}

func actSentence(act string) string {
	switch act {
	case actThink:
		return "Thinking…"
	case actRead:
		return "Reading"
	case actSearch:
		return "Searching"
	case actWrite:
		return "Writing"
	case actRun:
		return "Running"
	case actWeb:
		return "Browsing"
	case actPlan:
		return "Planning"
	case actDelegate:
		return "Handing work to staff"
	case actWait:
		return "Waiting for your answer"
	case actDone:
		return "Done"
	case actFail:
		return "Stopped"
	}
	return "Ready for your next message"
}

// --- pixels -----------------------------------------------------------

type rgb struct{ r, g, b uint8 }

type canvas struct {
	w, h int
	px   []rgb
	set  []bool
}

func newCanvas(w, h int) *canvas {
	return &canvas{w: w, h: h, px: make([]rgb, w*h), set: make([]bool, w*h)}
}

func (c *canvas) dot(x, y int, col rgb) {
	if x >= 0 && y >= 0 && x < c.w && y < c.h {
		c.px[y*c.w+x], c.set[y*c.w+x] = col, true
	}
}

func (c *canvas) rect(x, y, w, h int, col rgb) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			c.dot(i, j, col)
		}
	}
}

// pattern draws rows of a small bitmap ('#' set) in one color.
func (c *canvas) pattern(x, y int, col rgb, rows ...string) {
	for j, r := range rows {
		for i, ch := range r {
			if ch == '#' {
				c.dot(x+i, y+j, col)
			}
		}
	}
}

// halfBlocks turns pixel pairs into "▀"/"▄" cells.
func (c *canvas) halfBlocks(truecolor bool) []string {
	var rows []string
	for y := 0; y+1 < c.h; y += 2 {
		var sb strings.Builder
		for x := 0; x < c.w; x++ {
			ti, bi := y*c.w+x, (y+1)*c.w+x
			top, bot := c.set[ti], c.set[bi]
			switch {
			case !top && !bot:
				sb.WriteString("\x1b[0m ")
			case top && bot:
				sb.WriteString(fgColor(c.px[ti], truecolor) + bgColor(c.px[bi], truecolor) + "▀")
			case top:
				sb.WriteString("\x1b[49m" + fgColor(c.px[ti], truecolor) + "▀")
			default:
				sb.WriteString("\x1b[49m" + fgColor(c.px[bi], truecolor) + "▄")
			}
		}
		sb.WriteString("\x1b[0m")
		rows = append(rows, sb.String())
	}
	return rows
}

func fgColor(c rgb, tc bool) string {
	if tc {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", to256(c))
}

func bgColor(c rgb, tc bool) string {
	if tc {
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[48;5;%dm", to256(c))
}

// to256 picks the nearest color of the 6×6×6 cube.
func to256(c rgb) int {
	q := func(v uint8) int { return (int(v)*5 + 127) / 255 }
	return 16 + 36*q(c.r) + 6*q(c.g) + q(c.b)
}

func truecolorTerm() bool { return truecolor }

// truecolor: the terminal takes 24-bit colors, from COLORTERM or, since
// SSH rarely forwards that, from asking the terminal (see probeTerminal).
var truecolor = func() bool {
	ct := os.Getenv("COLORTERM")
	return ct == "truecolor" || ct == "24bit"
}()

var (
	colSkin    = rgb{241, 194, 150}
	colEye     = rgb{40, 34, 40}
	colDesk    = rgb{150, 104, 64}
	colLeg     = rgb{100, 68, 42}
	colFrame   = rgb{74, 78, 92}
	colPaper   = rgb{236, 236, 228}
	colInk     = rgb{140, 140, 150}
	colKeys    = rgb{58, 60, 70}
	colFolder  = rgb{236, 190, 70}
	colGreen   = rgb{90, 210, 120}
	colRed     = rgb{230, 80, 80}
	colYellow  = rgb{245, 205, 80}
	colBlue    = rgb{80, 150, 240}
	colScreen  = rgb{28, 34, 48}
	colTerm    = rgb{14, 16, 20}
	colCodeA   = rgb{120, 180, 250}
	colCodeB   = rgb{230, 160, 90}
	colCodeC   = rgb{170, 130, 230}
	colBoard   = rgb{150, 110, 70}
	colBubble  = rgb{250, 250, 250}
	colMagnify = rgb{180, 220, 250}
)
