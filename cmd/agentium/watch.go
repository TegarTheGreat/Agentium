package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// /watch: comments ending in "AI!" in the project's files are instructions
// ("// handle the empty case AI!"), and "AI?" ones are questions, as in
// aider's watch mode. When a saved file gains one, the comments marked AI
// in it are sent as a message, once the agent is free.

// aiComment matches a comment whose text starts or ends with AI, AI! or AI?.
var aiComment = regexp.MustCompile(`(?:^|\s)(?:#|//|--|;|/\*|<!--|\*)\s*(?:(AI[!?]?)\b\s*(.*?)|(.*?)\s*\b(AI[!?]?))\s*(?:\*/|-->)?\s*$`)

type watcher struct {
	root string
	list func() []string

	mu      sync.Mutex
	mtimes  map[string]time.Time
	seen    map[string]bool // file + comment already sent (or there at start)
	pending string
	stop    chan struct{}
}

func newWatcher(root string, list func() []string) *watcher {
	w := &watcher{root: root, list: list, mtimes: map[string]time.Time{}, seen: map[string]bool{}, stop: make(chan struct{})}
	w.scan(true) // comments already there are not new instructions
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				w.scan(false)
			}
		}
	}()
	return w
}

func (w *watcher) close() { close(w.stop) }

// take returns the message to send, once.
func (w *watcher) take() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.pending
	w.pending = ""
	return m
}

type aiNote struct {
	file string
	line int
	text string
	mark string // "AI", "AI!" or "AI?"
}

func (w *watcher) scan(initial bool) {
	var found []aiNote
	for _, rel := range w.list() {
		p := filepath.Join(w.root, rel)
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
			continue
		}
		w.mu.Lock()
		old, known := w.mtimes[rel]
		w.mtimes[rel] = st.ModTime()
		w.mu.Unlock()
		if known && !st.ModTime().After(old) {
			continue // unchanged since the last look
		}
		notes := aiNotes(p, rel)
		act := false
		w.mu.Lock()
		for _, n := range notes {
			key := rel + "\x00" + n.text
			if !w.seen[key] && n.mark != "AI" {
				act = true
			}
		}
		for _, n := range notes {
			w.seen[rel+"\x00"+n.text] = true
		}
		w.mu.Unlock()
		if act && !initial {
			found = append(found, notes...)
		}
	}
	if len(found) == 0 {
		return
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].file < found[j].file })
	var sb strings.Builder
	ask := false
	sb.WriteString("[/watch] I left instructions in code comments marked AI:\n")
	for _, n := range found {
		fmt.Fprintf(&sb, "- %s:%d: %s (%s)\n", n.file, n.line, n.text, n.mark)
		ask = ask || n.mark == "AI?"
	}
	sb.WriteString("Do what the AI! comments ask, using the other AI comments as context")
	if ask {
		sb.WriteString(", and answer the AI? questions in your reply")
	}
	sb.WriteString(". Then remove those AI comments from the code.")
	w.mu.Lock()
	if w.pending != "" {
		w.pending += "\n\n"
	}
	w.pending += sb.String()
	w.mu.Unlock()
}

// aiNotes lists the AI comments in a text file.
func aiNotes(path, rel string) []aiNote {
	b, err := os.ReadFile(path)
	if err != nil || strings.ContainsRune(string(b[:min(len(b), 8000)]), 0) {
		return nil
	}
	var out []aiNote
	for i, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, "AI") {
			continue
		}
		m := aiComment.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		mark, text := m[1], m[2]
		if mark == "" {
			mark, text = m[4], m[3]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			text = strings.TrimSpace(l)
		}
		out = append(out, aiNote{file: rel, line: i + 1, text: text, mark: mark})
	}
	return out
}
