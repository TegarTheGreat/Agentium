package main

import (
	"fmt"
	"strings"
)

// Each kind of step has its own label and color, drawn as a small chip,
// so a glance tells reading from editing from running a command.

type kindStyle struct {
	label string
	hex   string // truecolor
	ansi  string // other terminals
}

var kindStyles = map[string]kindStyle{
	"read":   {"Read", "#7AA2F7", "34"},
	"search": {"Search", "#5FC4B8", "36"},
	"edit":   {"Edit", "#F2A65A", "33"},
	"bash":   {"Run", "#8BC48A", "32"},
	"fetch":  {"Web", "#C792EA", "35"},
	"task":   {"Staff", "#F28FAD", "35"},
	"oracle": {"Oracle", "#E0AF68", "33"},
	"todo":   {"Plan", "#E8C468", "33"},
}

var kindStylesLight = map[string]string{
	"read": "#3558C8", "search": "#1F7A70", "edit": "#C9701F", "bash": "#2F7D3A",
	"fetch": "#8E44AD", "task": "#B83268", "oracle": "#9A6700", "todo": "#A67C00",
}

func styleFor(tool string) kindStyle {
	if s, ok := kindStyles[tool]; ok {
		if lightTheme {
			s.hex = kindStylesLight[tool]
		}
		return s
	}
	return kindStyle{label: toolLabel(tool), hex: "#9097A3", ansi: "37"}
}

// chip draws a step's kind: colored text on a faint wash of the same
// color (truecolor), or bold colored text.
func (u *ui) chip(tool string) string {
	s := styleFor(tool)
	if !u.color {
		return s.label
	}
	if !truecolorTerm() {
		return "\033[1;" + s.ansi + "m" + s.label + "\033[0m"
	}
	r, g, b := hexRGB(s.hex)
	br, bgc, bb := uint8(20), uint8(22), uint8(27)
	if lightTheme {
		br, bgc, bb = 250, 248, 244
	}
	mix := func(c, base uint8) int { return int(float64(c)*0.2 + float64(base)*0.8) }
	return fmt.Sprintf("\033[48;2;%d;%d;%dm\033[38;2;%d;%d;%dm %s \033[0m", mix(r, br), mix(g, bgc), mix(b, bb), r, g, b, s.label)
}

// kindColor paints s in the kind's color.
func (u *ui) kindColor(tool, s string) string {
	st := styleFor(tool)
	if !u.color {
		return s
	}
	if truecolorTerm() {
		return "\033[" + fgHex(st.hex) + "m" + s + "\033[0m"
	}
	return "\033[" + st.ansi + "m" + s + "\033[0m"
}

// gutter prefixes lines shown under a step (its output).
func (u *ui) gutter(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = "    " + u.paint(cGray, "│") + " " + l
	}
	return out
}

// noteLine formats a notice from Agentium itself (not the model).
func (u *ui) noteLine(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "·"), " ")
	return u.paint(cInk, "ℹ") + " " + u.paint(cGray, s)
}
