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
//
// Only edits count: comments that arrive with a checkout, pull or merge
// (git's HEAD moved), in vendored or dependency folders, or in prose files
// are not instructions from the user.

// aiComment matches a comment whose text starts or ends with AI, AI! or AI?.
var aiComment = regexp.MustCompile(`(?:^|\s)(?:#|//|--|;|/\*|<!--|\*)\s*(?:(AI[!?]|AI\b)\s*(.*?)|(.*?)\s*(?:\b|\s)(AI[!?]|AI\b))\s*(?:\*/|-->)?\s*$`)

// watchSkipDirs hold code the user did not write.
var watchSkipDirs = []string{"vendor/", "node_modules/", "third_party/", ".git/", "dist/", "build/"}

const watchMaxFiles = 20000

type watcher struct {
	root string
	list func() []string

	mu      sync.Mutex
	ready   bool
	mtimes  map[string]time.Time
	seen    map[string]bool // file + comment already sent (or there at start)
	pending []aiNote
	heads   time.Time // latest mtime of git's HEAD files
	stop    chan struct{}
}

func newWatcher(root string, list func() []string) *watcher {
	w := &watcher{root: root, list: list, mtimes: map[string]time.Time{}, seen: map[string]bool{}, stop: make(chan struct{})}
	go func() {
		w.scan(true) // comments already there are not new instructions
		t := time.NewTicker(2 * time.Second)
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

type aiNote struct {
	file string
	line int
	text string
	mark string // "AI", "AI!" or "AI?"
}

// take returns the message to send, once: the pending comments that are
// still in their files (the user or a turn may have handled some).
func (w *watcher) take() string {
	w.mu.Lock()
	notes := w.pending
	w.pending = nil
	w.mu.Unlock()
	if len(notes) == 0 {
		return ""
	}
	still := map[string]bool{}
	for _, n := range notes {
		for _, m := range aiNotes(filepath.Join(w.root, n.file), n.file) {
			still[m.file+"\x00"+m.text] = true
		}
	}
	var live []aiNote
	act := false
	for _, n := range notes {
		if still[n.file+"\x00"+n.text] {
			live = append(live, n)
			act = act || n.mark != "AI"
		}
	}
	if !act {
		return ""
	}
	return watchMessage(live)
}

// watchMessage quotes the comments as file content, for the agent.
func watchMessage(notes []aiNote) string {
	var sb strings.Builder
	ask := false
	sb.WriteString("[/watch] Comments marked AI were saved in these files:\n")
	for _, n := range notes {
		fmt.Fprintf(&sb, "- %s:%d (%s): %q\n", n.file, n.line, n.mark, n.text)
		ask = ask || n.mark == "AI?"
	}
	sb.WriteString("Do what the AI! comments ask, using the other AI comments as context")
	if ask {
		sb.WriteString(", and answer the AI? questions in your reply")
	}
	sb.WriteString(". Then remove those AI comments from the code.")
	return sb.String()
}

// gitHeads is the latest change to git's HEAD files: a checkout, pull or
// merge brings other people's comments.
func gitHeads(root string) time.Time {
	var t time.Time
	for _, f := range []string{"HEAD", "ORIG_HEAD", "FETCH_HEAD", "MERGE_HEAD"} {
		if st, err := os.Stat(filepath.Join(root, ".git", f)); err == nil && st.ModTime().After(t) {
			t = st.ModTime()
		}
	}
	return t
}

func skipWatch(rel string) bool {
	r := filepath.ToSlash(rel)
	for _, d := range watchSkipDirs {
		if strings.HasPrefix(r, d) || strings.Contains(r, "/"+d) {
			return true
		}
	}
	switch strings.ToLower(filepath.Ext(r)) {
	case ".md", ".markdown", ".txt", ".rst", ".adoc", ".log", ".csv", ".json", ".lock", ".svg":
		return true // prose and data, not code comments
	}
	return false
}

func (w *watcher) scan(initial bool) {
	files := w.list()
	full := len(files) >= watchMaxFiles // the list is cut: new names may be old files
	heads := gitHeads(w.root)
	w.mu.Lock()
	moved := !initial && heads.After(w.heads)
	w.heads = heads
	w.mu.Unlock()
	var found []aiNote
	for _, rel := range files {
		if skipWatch(rel) {
			continue
		}
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
		quiet := initial || moved || (!known && full)
		act := false
		w.mu.Lock()
		for _, n := range notes {
			if !w.seen[rel+"\x00"+n.text] && n.mark != "AI" {
				act = true
			}
			w.seen[rel+"\x00"+n.text] = true
		}
		w.mu.Unlock()
		if act && !quiet {
			found = append(found, notes...)
		}
	}
	w.mu.Lock()
	w.ready = true
	if len(found) > 0 {
		sort.SliceStable(found, func(i, j int) bool { return found[i].file < found[j].file })
		w.pending = append(w.pending, found...)
	}
	w.mu.Unlock()
}

// aiNotes lists the AI comments in a text file.
func aiNotes(path, rel string) []aiNote {
	b, err := readCapped(path, 1<<20)
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
