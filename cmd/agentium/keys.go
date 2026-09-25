package main

import (
	"fmt"
	"strings"
)

// Your own shortcuts: "keys": {"ctrl+x": "/diff", "f5": "run the tests"}
// in the config sends the command or message when the key is pressed in
// an empty message box.

// keySeq turns a key name into what the terminal sends, "" if unknown.
func keySeq(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if c, ok := strings.CutPrefix(n, "ctrl+"); ok && len(c) == 1 && c[0] >= 'a' && c[0] <= 'z' {
		return string(rune(c[0] - 'a' + 1))
	}
	if c, ok := strings.CutPrefix(n, "alt+"); ok && len(c) == 1 && c[0] > ' ' && c[0] < 0x7f {
		return "\x1b" + c
	}
	fkeys := map[string]string{
		"f1": "\x1bOP", "f2": "\x1bOQ", "f3": "\x1bOR", "f4": "\x1bOS",
		"f5": "\x1b[15~", "f6": "\x1b[17~", "f7": "\x1b[18~", "f8": "\x1b[19~",
		"f9": "\x1b[20~", "f10": "\x1b[21~", "f11": "\x1b[23~", "f12": "\x1b[24~",
	}
	return fkeys[n]
}

// reservedKeys are agentium's own and cannot be remapped.
var reservedKeys = map[string]bool{
	"\x01": true, "\x02": true, "\x03": true, "\x04": true, "\x05": true, "\x06": true, "\x07": true,
	"\x08": true, "\t": true, "\n": true, "\x0b": true, "\x0c": true, "\r": true, "\x0e": true,
	"\x0f": true, "\x10": true, "\x12": true, "\x13": true, "\x14": true, "\x15": true, "\x17": true,
	"\x19": true, "\x1bp": true, "\x1bt": true, "\x1bb": true, "\x1bf": true, "\x1bd": true,
}

// userKeys maps the configured shortcuts to their key sequences, with a
// note for each one that cannot be used.
func userKeys(cfg map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	var bad []string
	for name, action := range cfg {
		seq := keySeq(name)
		switch {
		case seq == "":
			bad = append(bad, fmt.Sprintf("keys: %q is not a key agentium knows (ctrl+a…z, alt+x, f1…f12)", name))
		case reservedKeys[seq]:
			bad = append(bad, fmt.Sprintf("keys: %s is taken by agentium", name))
		case strings.TrimSpace(action) == "":
		default:
			out[seq] = strings.TrimSpace(action)
		}
	}
	return out, bad
}
