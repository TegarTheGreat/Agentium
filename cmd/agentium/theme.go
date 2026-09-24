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
	switch strings.ToLower(pref) {
	case "light":
		lightTheme = true
	case "dark":
		lightTheme = false
	default:
		lightTheme = detectLight()
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

// detectLight reports a light terminal background, from an OSC 11 reply
// or COLORFGBG; dark when unknown.
func detectLight() bool {
	if r, g, b, ok := queryBackground(time.Second); ok {
		return 0.2126*float64(r)+0.7152*float64(g)+0.0722*float64(b) > 140
	}
	if v := os.Getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			return n == 7 || n == 15
		}
	}
	return false
}

// queryBackground asks the terminal for its background color (OSC 11).
func queryBackground(timeout time.Duration) (r, g, b uint8, ok bool) {
	if !isTTY(os.Stdin) || !isTTY(os.Stderr) || !lineEditing {
		return 0, 0, 0, false
	}
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return 0, 0, 0, false
	}
	defer restore()
	// The background query, then Device Attributes, which every terminal
	// answers: once that reply is in, nothing late can leak into the
	// input (a terminal without OSC 11 support answers only the second).
	os.Stderr.WriteString("\x1b]11;?\x07\x1b[c")
	var reply []byte
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 64)
	for len(reply) < 256 {
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
	return parseOSC11(string(reply))
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
