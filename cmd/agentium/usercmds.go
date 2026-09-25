package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tegarthegreat/agentium/internal/config"
)

// Prompt commands: /init and /review are built in; custom ones are
// Markdown files in .agentium/commands or .claude/commands (the project's
// or your own), whose text becomes the message, with $ARGUMENTS replaced
// by what follows the command.

const initPrompt = `Study this repository and write AGENTS.md at its root: the instructions a new engineer (or coding agent) needs. Cover, briefly and concretely: what the project is; how to build, run, test and lint (exact commands, verified by running them where cheap); the layout (main directories and entry points); conventions you can see in the code (style, naming, error handling, testing patterns); and pitfalls. If AGENTS.md or CLAUDE.md exists, improve it instead of starting over. Keep it under about 100 lines.`

const reviewPrompt = `Review the current changes (git diff HEAD, plus untracked files; if there are none, the commits on this branch that are not on the default branch) as a careful senior reviewer. Look for bugs, missed edge cases, security problems, broken error handling, leftover debug code and missing tests. Report findings as a numbered list, most serious first, each with file:line, what is wrong and a concrete fix; say plainly if you find nothing. Do not change any file.`

const securityPrompt = `Do a security review of the current changes (git diff HEAD, plus untracked files; if there are none, the commits on this branch that are not on the default branch). Read the surrounding code to see how data flows in. Look for vulnerabilities an attacker could actually exploit: injection (SQL, shell, template, path traversal), broken authentication or authorization, secrets or keys in code or logs, unsafe deserialization, SSRF, XSS and unsafe HTML, weak or misused cryptography, insecure defaults, missing validation at trust boundaries, and dependencies added with known problems. Leave out theoretical issues, denial of service, rate limiting and style. For each finding give severity (high, medium, low), file:line, how it would be exploited, and a concrete fix, most serious first; report only what you are confident about, and say plainly if you find nothing. Do not change any file.`

const commitPrompt = `Commit the current changes. Look at git status and git diff (staged and not), and git log -5 for this repository's message style. Stage the files that belong to the change (never secrets, .env files, build output or unrelated files; say which you left out and why), then commit with a concise message in the repository's style: a short summary line, and a body only if it helps. Do not push, amend or rewrite history.`

const prPrompt = `Open a pull request for the current branch. Check git status (commit first only if I asked for it; otherwise stop and tell me what is uncommitted), push the branch to its remote, and create the pull request with the gh CLI if it is available: a clear title and a description of what changed and why, with how it was tested. If gh is missing or not logged in, push and give me the URL to open it. Never force-push.`

type userCmd struct {
	name, desc, path string
	hint             string // argument-hint: what to type after it
	personal         bool   // in your home folder, not a repository's
}

// userCommands finds the custom commands; a project's win over yours.
func userCommands(cwd string) []userCmd {
	dirs := []string{filepath.Join(config.Home(), "commands")}
	if h, err := os.UserHomeDir(); err == nil {
		// Agentium's own folder wins over .claude's at the same level.
		dirs = append([]string{filepath.Join(h, ".claude", "commands")}, dirs...)
	}
	personal := len(dirs)
	root := config.ProjectRoot(cwd)
	for _, d := range []string{root, cwd} {
		dirs = append(dirs, filepath.Join(d, ".claude", "commands"), filepath.Join(d, ".agentium", "commands"))
	}
	byName := map[string]userCmd{}
	own := map[string]bool{}
	for di, dir := range dirs {
		real := dir
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			real = r
		}
		if di < personal {
			own[real] = true
		} else if own[real] {
			continue // your home folder is the project: still yours
		}
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
			desc, hint := commandDesc(p)
			byName[name] = userCmd{name: name, desc: desc, hint: hint, path: p, personal: di < personal}
		}
	}
	var out []userCmd
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// commandDesc is the front matter's description (or the first line) and
// argument-hint.
func commandDesc(path string) (desc, hint string) {
	b, err := readCapped(path, 1<<20)
	if err != nil {
		return "", ""
	}
	front, body := splitFront(string(b))
	for _, l := range strings.Split(front, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "description:"); ok && desc == "" {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		} else if v, ok := strings.CutPrefix(l, "argument-hint:"); ok {
			hint = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if desc == "" {
		desc = firstLine(strings.TrimSpace(body))
	}
	return sanitize(oneLine(desc, 200)), sanitize(oneLine(hint, 80))
}

// builtinCommand reports whether /name is one of agentium's own.
func builtinCommand(name string) bool {
	switch name {
	case "plan", "go", "skills", "settings", "new", "quit", "q", "?", "init", "review", "security-review", "commit", "pr", "allowed", "vim", "add-dir", "watch", "handoff", "agents", "style", "bug", "release-notes":
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
	return expandCommandShell(cwd, line, nil)
}

// expandCommandShell is expandCommand that also fills a custom command's
// !`command` spans with their output, through shell; a nil shell (or one
// that returns nil) leaves them as written.
func expandCommandShell(cwd, line string, shell func(c userCmd, cmds []string) []string) (msg string, ok bool) {
	if !strings.HasPrefix(line, "/") {
		return "", false
	}
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
	case "security-review":
		return withArgs(securityPrompt), true
	case "commit":
		return withArgs(commitPrompt), true
	case "pr":
		return withArgs(prPrompt), true
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
		// The file's !`commands` are held out while the arguments go in
		// (so nothing typed is run, and $1 in their output stays), then
		// filled with their output.
		spans := bangCmd.FindAllString(body, -1)
		n := 0
		body = bangCmd.ReplaceAllStringFunc(body, func(string) string { n++; return "\x00" + strconv.Itoa(n-1) + "\x00" })
		args = strings.ReplaceAll(args, "\x00", "")
		msg := withArgs(strings.TrimSpace(body))
		if strings.Contains(body, "$ARGUMENTS") || args != "" && positional.MatchString(body) {
			msg = strings.TrimSpace(expandArgs(body, args))
		}
		fill := spans
		if len(spans) > 0 && shell != nil {
			if outs := shell(c, bangCommands(strings.Join(spans, "\n"))); len(outs) == len(spans) {
				fill = outs
			}
		}
		msg = held.ReplaceAllStringFunc(msg, func(m string) string {
			i, _ := strconv.Atoi(m[1 : len(m)-1])
			return fill[i]
		})
		return msg, true // "" for an empty file: the caller says so
	}
	return "", false
}

// positional matches $1 … $9 in a command file (not $10).
var positional = regexp.MustCompile(`\$[1-9]\b`)

// held marks a !`command` span while the arguments go in.
var held = regexp.MustCompile("\x00[0-9]+\x00")

// expandArgs fills $ARGUMENTS with all the arguments and $1 … $9 with
// each one; "quoted words" count as one.
func expandArgs(body, args string) string {
	words := splitArgs(args)
	body = positional.ReplaceAllStringFunc(body, func(m string) string {
		if i := int(m[1] - '1'); i < len(words) {
			return words[i]
		}
		return ""
	})
	return strings.ReplaceAll(body, "$ARGUMENTS", args)
}

// splitArgs splits at spaces, keeping "double" or 'single' quoted words.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	quote, in := rune(0), false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, in = r, true
		case quote == 0 && (r == ' ' || r == '\t'):
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}
