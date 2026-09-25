package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// The office: a small pixel-art strip at the top of the full-screen UI.
// Agentium sits at a desk and visibly does what it is doing — reading a
// document, typing, watching a terminal, browsing, thinking — and each
// sub-agent appears as a staff member at their own desk. Drawn with
// half-block characters (two pixels per cell) at 4 frames a second, and
// only redrawn when a frame changes, so it stays light.

const (
	officeRows  = 7 // 6 rows of pixels (12 px) and a label row
	deskW       = 17
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

func (o *office) hire(key, title string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	look := staffLooks[o.hired%len(staffLooks)]
	o.hired++
	now := time.Now()
	o.staff = append(o.staff, &actor{key: key, name: fmt.Sprintf("Staff %d", o.hired), title: title, act: actThink,
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
	actors := []*actor{&o.lead}
	for i := 0; i < len(o.staff) && i < 2; i++ {
		actors = append(actors, o.staff[i])
	}
	textW := width - deskW - 1
	var rows []string
	for i, a := range actors {
		c := newCanvas(deskW, 12)
		drawDesk(c, 0, a, frame, now)
		px := c.halfBlocks(truecolor)
		// Beside the desk: who, what (and for how long), on what.
		name := "\x1b[1m" + a.name + "\x1b[0m"
		if i > 0 {
			name = fgColor(a.shirt, truecolor) + "\x1b[1m" + truncate(a.name, textW) + "\x1b[0m"
		}
		doing := "\x1b[" + cAccent + "m" + actWord(a.act) + "\x1b[0m"
		if a.act != actIdle {
			doing += "\x1b[2m · " + elapsed(now.Sub(a.since)) + "\x1b[0m"
		}
		side := []string{"", name, doing}
		about := a.detail
		if i > 0 && (about == "" || a.act == actThink) {
			about = a.title
		}
		if a.act != actIdle {
			for j, l := range wordWrap(about, textW) {
				if j == 2 {
					break
				}
				side = append(side, "\x1b[2m"+truncate(l, textW)+"\x1b[0m")
			}
		}
		for j, p := range px {
			t := ""
			if j < len(side) {
				t = side[j]
			}
			rows = append(rows, p+" "+t)
		}
	}
	if extra := len(o.staff) - 2; extra > 0 {
		rows = append(rows, fmt.Sprintf("\x1b[2m+%d more staff at work\x1b[0m", extra))
	}
	return rows
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

// drawDesk draws one actor at their desk; x0 is the left edge.
func drawDesk(c *canvas, x0 int, a *actor, frame int, now time.Time) {
	// Desk and monitor.
	c.rect(x0, 9, deskW-1, 1, colDesk)
	c.rect(x0+1, 10, 1, 2, colLeg)
	c.rect(x0+deskW-3, 10, 1, 2, colLeg)
	c.rect(x0+10, 3, 7, 5, colFrame)
	c.rect(x0+13, 8, 1, 1, colFrame)
	c.rect(x0+12, 8, 3, 1, colFrame)
	drawScreen(c, x0+11, 4, a.act, frame)

	// A new staff member walks in from the right.
	dx := 0
	if age := now.Sub(a.born); age < 1200*time.Millisecond {
		dx = int((1200*time.Millisecond - age) / (150 * time.Millisecond))
	}
	x := x0 + dx

	// Head and hair.
	c.pattern(x+3, 0, a.hair, "###")
	c.pattern(x+2, 1, a.hair, "#####")
	c.rect(x+2, 2, 5, 3, colSkin)
	c.dot(x+2, 2, a.hair)
	c.dot(x+6, 2, a.hair)
	// Eyes look at the screen while working, blink now and then.
	look := 0
	if a.act != actIdle && a.act != actWait && a.act != actDone && a.act != actFail {
		look = 1
	}
	if frame%17 != 0 {
		c.dot(x+3+look, 3, colEye)
		c.dot(x+5+look, 3, colEye)
	}
	// Body.
	c.rect(x+3, 5, 3, 1, a.shirt)
	c.rect(x+1, 6, 7, 3, a.shirt)

	hands := func(lx, ly, rx, ry int) {
		c.dot(x+lx, ly, colSkin)
		c.dot(x+rx, ry, colSkin)
	}
	switch a.act {
	case actWrite, actRun:
		c.rect(x0+7, 8, 4, 1, colKeys)
		// Typing: the hands take turns.
		if frame%2 == 0 {
			hands(7, 7, 9, 8)
		} else {
			hands(7, 8, 9, 7)
		}
	case actRead:
		// Holding a document; the page turns every few seconds.
		c.rect(x+6, 4, 4, 4, colPaper)
		for j := 0; j < 3; j++ {
			w := 2 + (j+frame/12)%2
			c.rect(x+7, 5+j, w, 1, colInk)
		}
		hands(6, 7, 9, 7)
	case actSearch:
		// A magnifying glass that sweeps a little.
		off := []int{0, 1, 0, -1}[frame%4]
		c.pattern(x+6+off, 3, colFrame, ".#.", "#.#", ".#.")
		c.dot(x+7+off, 4, colMagnify)
		c.dot(x+6+off, 6, colFrame)
		hands(5, 7, 6+off, 7)
	case actWeb:
		hands(7, 8, 8, 8)
	case actPlan:
		// A clipboard, ticked off line by line.
		c.rect(x+6, 3, 4, 5, colBoard)
		c.rect(x+7, 4, 2, 3, colPaper)
		for j := 0; j <= (frame/4)%3; j++ {
			c.dot(x+7, 4+j, colGreen)
		}
		hands(6, 7, 9, 7)
	case actDelegate:
		// Holding out a folder for a colleague.
		reach := (frame / 2) % 2
		c.rect(x+7+reach, 5, 3, 2, colFolder)
		hands(6+reach, 6, 9+reach, 7)
	case actThink:
		hands(1, 8, 6, 5) // hand to chin
		for i := 0; i <= frame%4 && i < 3; i++ {
			c.dot(x+8+2*i, 1-i/2, colBubble)
		}
	case actWait:
		c.pattern(x+8, 0, colYellow, "###", "..#", ".##", "...", ".#.")
		hands(1, 8, 7, 8)
	case actDone:
		c.dot(x+0, 4, colSkin) // arms up
		c.dot(x+8, 4, colSkin)
		c.dot(x+0, 5, a.shirt)
		c.dot(x+8, 5, a.shirt)
	case actFail:
		c.pattern(x+8, 0, colRed, "#", "#", "#", ".", "#")
		hands(1, 8, 7, 8)
	default:
		hands(1, 8, 7, 8)
	}
}

// drawScreen fills the 5×3 monitor screen.
func drawScreen(c *canvas, x, y int, act string, frame int) {
	switch act {
	case actWrite:
		c.rect(x, y, 5, 3, colScreen)
		n := frame % 8
		cols := []rgb{colCodeA, colCodeB, colCodeC}
		for j := 0; j < 3; j++ {
			w := min(max(n-2*j, 0), 4)
			c.rect(x+j%2, y+j, w, 1, cols[j])
		}
	case actRun:
		c.rect(x, y, 5, 3, colTerm)
		for j := 0; j < 3; j++ {
			w := 1 + (frame+j*3)%4
			c.rect(x, y+j, w, 1, colGreen)
		}
		if frame%2 == 0 {
			c.dot(x+4, y+2, colBubble)
		}
	case actRead, actSearch:
		c.rect(x, y, 5, 3, colPaper)
		for j := 0; j < 3; j++ {
			c.rect(x+1, y+j, 1+(j+frame/3)%3, 1, colInk)
		}
	case actWeb:
		c.rect(x, y, 5, 3, colBlue)
		for j := 0; j < 3; j++ {
			c.dot(x+(frame+j*2)%5, y+j, colGreen)
			c.dot(x+(frame+j*2+1)%5, y+j, colGreen)
		}
	case actDone:
		c.rect(x, y, 5, 3, colScreen)
		c.pattern(x, y, colGreen, "....#", "#.#..", ".#...")
	case actFail:
		c.rect(x, y, 5, 3, colScreen)
		c.pattern(x+1, y, colRed, "#.#", ".#.", "#.#")
	case actThink:
		c.rect(x, y, 5, 3, colScreen)
		c.dot(x+1+frame%3, y+1, colCodeA)
	default:
		c.rect(x, y, 5, 3, colScreen)
		c.dot(x+2, y+1, colCodeC)
	}
}
