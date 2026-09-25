package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Night Shift: Agentium's look. A cool, dark room with one warm desk
// lamp: the lamp amber marks Agentium, ink blue marks you, and a few
// muted status colors do the rest. On terminals without truecolor the
// ANSI colors of the user's own theme are kept.

type palette struct {
	accent, ink, green, red, yellow, magenta, cyan, gray, addBG, delBG string
}

var (
	nightShift = palette{
		accent: "#F2A65A", ink: "#7AA2F7", green: "#8BC48A", red: "#E5736B", yellow: "#E8C468",
		magenta: "#C792EA", cyan: "#5FC4B8", gray: "#5C6370", addBG: "#1E2A20", delBG: "#2E1D1E",
	}
	dayShift = palette{
		accent: "#C9701F", ink: "#3558C8", green: "#2F7D3A", red: "#C0392B", yellow: "#A67C00",
		magenta: "#8E44AD", cyan: "#1F7A70", gray: "#8A8F98", addBG: "#E4F3E1", delBG: "#F8E1DE",
	}
	// lightTheme records the chosen variant (the office uses it too).
	lightTheme bool
)

func fgHex(h string) string {
	r, g, b := hexRGB(h)
	return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
}

func bgHex(h string) string {
	r, g, b := hexRGB(h)
	return fmt.Sprintf("48;2;%d;%d;%d", r, g, b)
}

func hexRGB(h string) (r, g, b uint8) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(h, "#"), 16, 32)
	return uint8(v >> 16), uint8(v >> 8), uint8(v)
}

// applyTheme picks the palette: "light", "dark" or "auto" (asks the
// terminal for its background color).
func applyTheme(pref string) {
	bg, bgOK := probeTerminal(time.Second)
	switch strings.ToLower(pref) {
	case "light":
		lightTheme = true
	case "dark":
		lightTheme = false
	default:
		lightTheme = detectLight(bg, bgOK)
	}
	if !truecolorTerm() {
		if lightTheme {
			cAccent, cInk = "33", "34"
			bgAdd, bgDel = "48;5;194", "48;5;224"
		}
		return
	}
	p := nightShift
	if lightTheme {
		p = dayShift
	}
	cAccent, cInk = fgHex(p.accent), fgHex(p.ink)
	cGreen, cRed, cYellow = fgHex(p.green), fgHex(p.red), fgHex(p.yellow)
	cMagenta, cCyan, cBlue, cGray = fgHex(p.magenta), fgHex(p.ink), fgHex(p.ink), fgHex(p.gray)
	bgAdd, bgDel = bgHex(p.addBG), bgHex(p.delBG)
	sgrCyan = "\033[" + cCyan + "m"
}

// detectLight reports a light terminal background, from the terminal's
// reply or COLORFGBG; dark when unknown.
func detectLight(bg [3]uint8, ok bool) bool {
	if ok {
		return 0.2126*float64(bg[0])+0.7152*float64(bg[1])+0.0722*float64(bg[2]) > 140
	}
	if v := os.Getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			return n == 7 || n == 15
		}
	}
	return false
}

// probeTerminal asks the terminal for its background color (OSC 11) and,
// unless COLORTERM already said so, whether it takes 24-bit colors: it
// sets one and reads it back (DECRQSS), which xterm, kitty, WezTerm,
// foot, Ghostty and others answer, over SSH too.
func probeTerminal(timeout time.Duration) (bg [3]uint8, ok bool) {
	if !isTTY(os.Stdin) || !isTTY(os.Stderr) || !lineEditing || dumbTerm() {
		return bg, false
	}
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return bg, false
	}
	defer restore()
	// Then Device Attributes, which every terminal answers: once that
	// reply is in, nothing late can leak into the input (a terminal that
	// knows neither query answers only this one).
	q := "\x1b]11;?\x07"
	// Not under screen or tmux: they answer Device Attributes themselves
	// but may pass the color query on, so its reply could come late and
	// land in the input.
	term := os.Getenv("TERM")
	probe := !truecolor && term != "linux" && !strings.HasPrefix(term, "screen") && !strings.HasPrefix(term, "tmux") &&
		os.Getenv("STY") == "" && os.Getenv("TMUX") == ""
	if probe {
		q += "\x1b[48;2;1;2;3m\x1bP$qm\x1b\\\x1b[0m"
	}
	os.Stderr.WriteString(q + "\x1b[c")
	var reply []byte
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 64)
	for len(reply) < 512 {
		wait := time.Until(deadline)
		if wait <= 0 || !inputReady(os.Stdin, wait) {
			break
		}
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			break
		}
		reply = append(reply, buf[:n]...)
		if da1Done(reply) {
			break
		}
	}
	if sgrEcho(string(reply)) {
		truecolor = true
	} else if probe {
		// A late color reply is swallowed here, not typed into the prompt.
		for inputReady(os.Stdin, 50*time.Millisecond) {
			if n, err := os.Stdin.Read(buf); err != nil || n == 0 {
				break
			} else if sgrEcho(string(buf[:n])) {
				truecolor = true
			}
		}
	}
	bg[0], bg[1], bg[2], ok = parseOSC11(string(reply))
	return bg, ok
}

// sgrEcho reports whether a DECRQSS reply echoes the 24-bit color 1,2,3
// (as "48;2;1;2;3" or "48:2::1:2:3").
func sgrEcho(s string) bool {
	i := strings.Index(s, "\x1bP1$r")
	if i < 0 {
		return false
	}
	s = s[i:]
	return strings.Contains(s, "2;1;2;3") || strings.Contains(s, ":1:2:3")
}

// da1Done reports whether the Device Attributes reply (ESC [ ? … c) has
// arrived.
func da1Done(b []byte) bool {
	i := strings.Index(string(b), "\x1b[?")
	return i >= 0 && strings.Contains(string(b[i:]), "c")
}

// parseOSC11 reads "…rgb:RRRR/GGGG/BBBB…".
func parseOSC11(s string) (r, g, b uint8, ok bool) {
	i := strings.Index(s, "rgb:")
	if i < 0 {
		return 0, 0, 0, false
	}
	parts := strings.Split(strings.TrimRight(s[i+4:], "\x07\x1b\\"), "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var c [3]uint8
	for j, p := range parts {
		if len(p) == 0 {
			return 0, 0, 0, false
		}
		if len(p) > 2 {
			p = p[:2]
		}
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		if len(parts[j]) == 1 {
			v *= 17
		}
		c[j] = uint8(v)
	}
	return c[0], c[1], c[2], true
}
