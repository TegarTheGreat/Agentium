package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/checkpoint"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/session"
)

// turnStart is a message the user typed, where a turn begins.
type turnStart struct {
	index int    // in the conversation
	words string // what the user typed
}

// userTurns lists the turns still in the conversation (compaction folds
// older ones into its summary).
func userTurns(msgs []provider.Message) []turnStart {
	var out []turnStart
	for i, m := range msgs {
		if m.Role != provider.RoleUser {
			continue
		}
		t := m.Text
		if strings.HasPrefix(t, "[agentium]") || strings.HasPrefix(t, "[Summary of the earlier") ||
			strings.HasPrefix(t, "[The user sent this while") || strings.HasPrefix(t, "[The user approved") {
			continue
		}
		if w := provider.UserWords(t); w != "" {
			out = append(out, turnStart{i, w})
		}
	}
	return out
}

// rewind goes back to before one of the user's messages (Esc Esc): the
// conversation, the files, or both, as in Claude Code; the message comes
// back into the composer to edit and send again (as in Codex).
func rewind(u *ui, a *agent.Agent, sess *session.Session, store *checkpoint.Store, ed *editor) {
	turns := userTurns(a.Messages)
	if len(turns) == 0 {
		u.note("nothing to rewind")
		return
	}
	var items []menuItem
	for i := len(turns) - 1; i >= 0; i-- {
		items = append(items, menuItem{value: strconv.Itoa(i), label: oneLine(turns[i].words, 60),
			hint: fmt.Sprintf("message %d", i+1)})
	}
	pick, err := u.choose("Rewind to before which message?", items, "", false)
	if err != nil {
		return
	}
	k, _ := strconv.Atoi(pick)
	// The checkpoints of that turn and every later one.
	later := map[string]bool{}
	for _, t := range turns[k:] {
		later[strings.TrimSpace(t.words)] = true
	}
	undoable := 0
	for i := len(sess.Checkpoints) - 1; i >= 0 && later[strings.TrimSpace(sess.Checkpoints[i].Prompt)]; i-- {
		undoable++
	}
	what := []menuItem{{value: "chat", label: "Conversation only", hint: "files stay as they are"}}
	if store != nil && undoable > 0 {
		what = append([]menuItem{{value: "both", label: "Conversation and files", hint: fmt.Sprintf("undo %d turn(s) of file changes", undoable)}},
			append(what, menuItem{value: "code", label: "Files only", hint: "keep the conversation"})...)
	}
	choice, err := u.choose("Restore", what, "", false)
	if err != nil {
		return
	}
	if choice == "both" || choice == "code" {
		var notes []string
		for n := 0; n < undoable; n++ {
			note, err := undoLast(store, sess)
			if err != nil {
				fmt.Fprintln(os.Stderr, "·", err)
				break
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
		a.Env.ForgetReads()
		if choice == "code" {
			a.Note = strings.Join(notes, "\n")
		}
	}
	if choice == "both" || choice == "chat" {
		a.Messages = append([]provider.Message(nil), a.Messages[:turns[k].index]...)
		sess.Messages = a.Messages
		a.Note = ""
		ed.draft = []rune(turns[k].words) // edit it and send again
	}
	_ = sess.Save()
	refreshChanges(sess.Cwd)
	switch choice {
	case "both":
		u.success("Rewound the conversation and the files · your message is back in the box")
	case "chat":
		u.success("Rewound the conversation (files unchanged) · your message is back in the box")
	case "code":
		u.success("Rewound the files; the conversation stays")
	}
}

// refreshChanges sets the side panel's change list from git's view of
// the working tree, after files were put back.
func refreshChanges(dir string) {
	f := activeFS()
	if f == nil {
		return
	}
	out, err := git(dir, "diff", "--numstat", "HEAD")
	if err != nil {
		return
	}
	var cs []fileChange
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.Fields(l)
		if len(p) < 3 {
			continue
		}
		add, _ := strconv.Atoi(p[0])
		del, _ := strconv.Atoi(p[1])
		cs = append(cs, fileChange{strings.Join(p[2:], " "), add, del})
	}
	f.resetChanges(cs)
}
