package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tegarthegreat/agentium/internal/config"
)

// Prompt commands: /init and /review are built in; custom ones are
// Markdown files in .agentium/commands or .claude/commands (the project's
// or your own), whose text becomes the message, with $ARGUMENTS replaced
// by what follows the command.

const initPrompt = `Study this repository and write AGENTS.md at its root: the instructions a new engineer (or coding agent) needs. Cover, briefly and concretely: what the project is; how to build, run, test and lint (exact commands, verified by running them where cheap); the layout (main directories and entry points); conventions you can see in the code (style, naming, error handling, testing patterns); and pitfalls. If AGENTS.md or CLAUDE.md exists, improve it instead of starting over. Keep it under about 100 lines.`

const reviewPrompt = `Review the current changes (git diff HEAD, plus untracked files) as a careful senior reviewer. Look for bugs, missed edge cases, security problems, broken error handling, leftover debug code and missing tests. Report findings as a numbered list, most serious first, each with file:line, what is wrong and a concrete fix; say plainly if you find nothing. Do not change any file.`

type userCmd struct {
	name, desc, path string
}

// userCommands finds the custom commands; a project's win over yours.
func userCommands(cwd string) []userCmd {
	dirs := []string{filepath.Join(config.Home(), "commands")}
	if h, err := os.UserHomeDir(); err == nil {
		// Agentium's own folder wins over .claude's at the same level.
		dirs = append([]string{filepath.Join(h, ".claude", "commands")}, dirs...)
	}
	root := config.ProjectRoot(cwd)
	for _, d := range []string{root, cwd} {
		dirs = append(dirs, filepath.Join(d, ".claude", "commands"), filepath.Join(d, ".agentium", "commands"))
	}
	byName := map[string]userCmd{}
	for _, dir := range dirs {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			ext := filepath.Ext(e.Name())
			if e.IsDir() || !strings.EqualFold(ext, ".md") {
				continue
			}
			name := strings.ToLower(strings.TrimSuffix(e.Name(), ext))
			if builtinCommand(name) || name == "" {
				continue // a repository's file cannot take over /undo or /exit
			}
			p := filepath.Join(dir, e.Name())
			byName[name] = userCmd{name: name, desc: commandDesc(p), path: p}
		}
	}
	var out []userCmd
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// commandDesc is the front matter's description, or the first line.
func commandDesc(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	front, body := splitFront(string(b))
	for _, l := range strings.Split(front, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "description:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return firstLine(strings.TrimSpace(body))
}

// builtinCommand reports whether /name is one of agentium's own.
func builtinCommand(name string) bool {
	switch name {
	case "plan", "go", "skills", "settings", "new", "quit", "q", "?", "init", "review":
		return true
	}
	for _, c := range slashCommands {
		if c.name == "/"+name {
			return true
		}
	}
	return false
}

func splitFront(s string) (front, body string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if i := strings.Index(rest, "\n---"); i >= 0 {
			return rest[:i], strings.TrimLeft(rest[i+4:], "\n")
		}
	}
	return "", s
}

// expandCommand turns /init, /review and custom commands into the message
// to send; ok is false for anything else.
func expandCommand(cwd, line string) (msg string, ok bool) {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	args = strings.TrimSpace(args)
	withArgs := func(p string) string {
		if args != "" {
			p += "\n\n" + args
		}
		return p
	}
	switch strings.ToLower(name) {
	case "init":
		return withArgs(initPrompt), true
	case "review":
		return withArgs(reviewPrompt), true
	}
	for _, c := range userCommands(cwd) {
		if c.name != strings.ToLower(name) {
			continue
		}
		b, err := os.ReadFile(c.path)
		if err != nil {
			return "", false
		}
		_, body := splitFront(string(b))
		msg := withArgs(strings.TrimSpace(body))
		if strings.Contains(body, "$ARGUMENTS") {
			msg = strings.TrimSpace(strings.ReplaceAll(body, "$ARGUMENTS", args))
		}
		return msg, true // "" for an empty file: the caller says so
	}
	return "", false
}
