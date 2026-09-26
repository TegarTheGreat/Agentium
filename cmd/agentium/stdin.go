package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stdinPiped reports whether stdin is a pipe or a file (not a terminal,
// not /dev/null or another device).
func stdinPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	m := fi.Mode()
	return m&os.ModeNamedPipe != 0 || m.IsRegular() && fi.Size() > 0
}

// maxPiped is how much piped text goes into the prompt itself; the rest
// is saved to a file the agent can read.
const maxPiped = 200 << 10

// readPiped reads stdin to its end. A pipe that stays silent gets a note
// on stderr, so a caller that left stdin open knows why nothing happens.
func readPiped(r io.Reader) (string, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			fmt.Fprintln(os.Stderr, "agentium: waiting for input on stdin (run with </dev/null to skip it)")
		}
	}()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	return string(b), nil
}

// attachPiped adds piped text to the prompt; very long text is cut, with
// the whole of it saved to a temporary file named in the prompt.
func attachPiped(prompt, piped string) string {
	if strings.TrimSpace(piped) == "" {
		return prompt
	}
	body, note := piped, ""
	if len(piped) > maxPiped {
		note = fmt.Sprintf("\n[stdin was %d bytes; this is the first %d", len(piped), maxPiped)
		f, err := os.CreateTemp("", "agentium-stdin-*.txt")
		if err == nil {
			_, err = f.WriteString(piped)
			f.Close()
		}
		if err == nil {
			note += "; all of it is in " + filepath.ToSlash(f.Name())
		}
		note += "]"
		body = strings.ToValidUTF8(piped[:maxPiped], "")
	}
	return prompt + "\n\n<stdin>\n" + strings.TrimRight(body, "\n") + "\n</stdin>" + note
}
