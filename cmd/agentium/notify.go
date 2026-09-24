package main

import (
	"os"
	"strings"
)

// Getting the user's attention: the terminal title shows what Agentium is
// doing (a glance at a background tab is enough), and a bell plus a
// desktop notification (where the terminal supports OSC 9) says when it
// needs an answer or has finished a long turn.

// realTTY is the terminal itself, also in the full screen.
func realTTY() *os.File {
	if f := activeFS(); f != nil {
		return f.tty
	}
	return os.Stderr
}

// setTitle sets the terminal title: "◆ agentium · working" and the like.
func (u *ui) setTitle(state string) {
	if !u.live || os.Getenv("AGENTIUM_NO_TITLE") != "" {
		return
	}
	glyph := map[string]string{"working": "◆", "needs you": "✋", "ready": "◇"}[state]
	realTTY().WriteString("\x1b]0;" + strings.TrimSpace(glyph+" agentium · "+state) + "\x07")
}

// notify rings the bell and, where supported, shows a notification.
func (u *ui) notify(msg string) {
	if !u.live || os.Getenv("AGENTIUM_NO_NOTIFY") != "" {
		return
	}
	s := "\x07"
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app", "ghostty", "WezTerm":
		s += "\x1b]9;" + msg + "\x07"
	}
	if strings.Contains(os.Getenv("TERM"), "kitty") {
		s += "\x1b]99;;" + msg + "\x1b\\"
	}
	realTTY().WriteString(s)
}
