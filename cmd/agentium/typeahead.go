package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Type-ahead: while a turn runs, keys are read in raw mode so what the
// user types is shown in the live area instead of being echoed into the
// output. Enter queues the message; it is sent when the turn ends.
// Ctrl-C clears the typed text, or interrupts the turn when there is
// none. An approval prompt takes keys from the same reader.

// startTyping begins reading keys; the returned function stops it.
func (u *ui) startTyping(interrupt func()) (stop func()) {
	noop := func() {}
	if !u.live || !lineEditing || runtime.GOOS == "windows" || !isTTY(os.Stdin) {
		return noop
	}
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return noop
	}
	keys := make(chan string, 16)
	u.mu.Lock()
	u.keys = keys
	u.mu.Unlock()
	os.Stderr.WriteString("\x1b[?2004h") // bracketed paste: a pasted block stays one message
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		// If the reader stops (the terminal went away), a waiting
		// approval prompt must not block forever.
		defer close(keys)
		ed := &editor{in: os.Stdin}
		pasting, lastCR := false, false
		for {
			select {
			case <-done:
				return
			default:
			}
			if !inputReady(os.Stdin, 100*time.Millisecond) {
				continue
			}
			k, err := ed.key()
			if err != nil {
				return
			}
			u.mu.Lock()
			if u.paused { // an approval prompt is waiting for this key
				u.mu.Unlock()
				select {
				case keys <- k:
				default:
				}
				continue
			}
			switch {
			case k == "\x1b[200~":
				pasting = true
			case k == "\x1b[201~":
				pasting = false
			case pasting && (k == "\r" || k == "\n"):
				if k == "\n" && len(u.typing) > 0 && u.typing[len(u.typing)-1] == '\n' && lastCR {
					break // CRLF: one newline
				}
				u.typing = append(u.typing, '\n')
			case k == "\r" || k == "\n":
				if t := strings.TrimSpace(string(u.typing)); t != "" {
					u.queued = append(u.queued, t)
				}
				u.typing = nil
			case k == "\x03":
				if len(u.typing) > 0 {
					u.typing = nil
				} else {
					u.mu.Unlock()
					interrupt()
					continue
				}
			case k == "\x7f" || k == "\x08":
				if len(u.typing) > 0 {
					u.typing = u.typing[:len(u.typing)-1]
				}
			case k == "\x15":
				u.typing = nil
			default:
				if !strings.HasPrefix(k, "\x1b") {
					for _, r := range k {
						switch {
						case r == '\t':
							u.typing = append(u.typing, ' ', ' ')
						case r >= 0x20 && r != 0x7f:
							u.typing = append(u.typing, r)
						}
					}
				}
			}
			lastCR = k == "\r"
			u.clearLive()
			u.drawLive()
			u.mu.Unlock()
		}
	}()
	return func() {
		close(done)
		wg.Wait()
		os.Stderr.WriteString("\x1b[?2004l")
		restore()
		u.mu.Lock()
		u.keys = nil
		u.mu.Unlock()
	}
}

// nextKey reads one key: from the type-ahead reader while a turn runs,
// else directly from the terminal.
func (u *ui) nextKey() (string, error) {
	u.mu.Lock()
	keys := u.keys
	u.mu.Unlock()
	if keys != nil {
		k, ok := <-keys
		if !ok {
			return "", errors.New("terminal input closed")
		}
		return k, nil
	}
	return readKey()
}

// takeQueued returns the next message typed during the last turn (one
// at a time, so a queued /command runs as a command), and the text typed
// but not sent once the queue is empty.
func (u *ui) takeQueued() (next string, ok bool, draft string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.queued) > 0 {
		next, u.queued = u.queued[0], u.queued[1:]
		return next, true, ""
	}
	draft, u.typing = string(u.typing), nil
	return "", false, draft
}

// typeaheadLines renders queued and typed text for the live area.
func (u *ui) typeaheadLines(width int) []string {
	var lines []string
	for _, q := range u.queued {
		lines = append(lines, u.paint(cDim, "  ↳ queued: "+truncate(strings.ReplaceAll(q, "\n", "↵"), width-14)))
	}
	if len(u.typing) > 0 {
		t := strings.ReplaceAll(string(u.typing), "\n", "↵")
		// Show the end of long input, where the cursor is.
		for strWidth(t) > width-4 {
			_, size := utf8.DecodeRuneInString(t)
			t = t[size:]
		}
		lines = append(lines, u.paint(cCyan, "❯ ")+t+u.paint(cDim, "▏"))
	} else if len(u.queued) == 0 && u.keys != nil {
		lines = append(lines, u.paint(cDim, "  type to queue a message · Ctrl-C to interrupt"))
	}
	return lines
}

// drainKeys drops keys buffered for an approval prompt before it showed.
func (u *ui) drainKeys() {
	u.mu.Lock()
	keys := u.keys
	u.mu.Unlock()
	for keys != nil {
		select {
		case <-keys:
		default:
			return
		}
	}
}

// keyWithin reports a key pressed within d, if any (read and dropped).
func (u *ui) keyWithin(d time.Duration) (string, bool) {
	u.mu.Lock()
	keys := u.keys
	u.mu.Unlock()
	if keys != nil {
		select {
		case k, ok := <-keys:
			return k, ok
		case <-time.After(d):
			return "", false
		}
	}
	if !inputReady(os.Stdin, d) {
		return "", false
	}
	k, err := readKey()
	return k, err == nil
}
