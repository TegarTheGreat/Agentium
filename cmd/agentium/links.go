package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Clickable file names (OSC 8 hyperlinks): in terminals that support
// them (iTerm2, WezTerm, kitty, Ghostty, VS Code, Windows Terminal,
// GNOME Terminal …) a ctrl- or cmd-click opens the file. Others show
// plain text. AGENTIUM_NO_LINKS=1 turns them off.

var linksOn = os.Getenv("AGENTIUM_NO_LINKS") == "" && os.Getenv("TERM") != "linux" && os.Getenv("TERM") != "dumb"

// fileLink makes text a link to the file at path (relative to cwd).
func fileLink(cwd, path, text string) string {
	if !linksOn || path == "" || text == "" {
		return text
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	host, _ := os.Hostname()
	u := url.URL{Scheme: "file", Host: host, Path: filepath.ToSlash(path)}
	if strings.ContainsAny(u.String(), "\x1b\x07") {
		return text
	}
	return "\x1b]8;;" + u.String() + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// fileLinkURL makes text a link to a web address.
func fileLinkURL(uri, text string) string {
	if !linksOn || strings.ContainsAny(uri, "\x1b\x07") {
		return text
	}
	return "\x1b]8;;" + uri + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}
