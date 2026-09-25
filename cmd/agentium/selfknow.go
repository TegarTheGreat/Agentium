package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/skill"
)

// selfPrompt tells the model about Agentium itself: where its settings,
// memory, skills and sessions live, and how the user drives it, so it can
// answer "where are my skills?" or "how do I add an MCP server?" instead
// of guessing. Short, and stable for the session (it is cached).
func selfPrompt(cwd string) string {
	home := config.Home()
	root := config.ProjectRoot(cwd)
	proj := config.ProjectDir(root)
	userHome, _ := os.UserHomeDir()
	short := func(p string) string {
		if userHome != "" && strings.HasPrefix(p, userHome) {
			return "~" + strings.TrimPrefix(p, userHome)
		}
		return p
	}
	var skillDirs []string
	for _, d := range skill.Dirs(home, cwd) {
		skillDirs = append(skillDirs, short(d.Dir))
	}
	return fmt.Sprintf(`

About you (Agentium %s; for questions about yourself — never guess, check these files or the docs at https://github.com/TegarTheGreat/Agentium):
- Settings %s (model, subagent_model, mode, theme, ui, mcp servers, providers, hooks, fallback); API keys in the OS keychain or %s (never read or print it)
- Your instructions for every project: %s; for this project: AGENTS.md (or CLAUDE.md, GEMINI.md) in the repository, from its root down to the current folder
- Memory: user preferences %s (every project); this project (%s): %s (MEMORY.md, decisions, journal). Projects do not share notes.
- Skills (a folder with SKILL.md): %s
- Sessions %s; checkpoints for /undo %s
- The user drives you with /commands (/help lists them: /model /login /mode /undo /rewind /diff /context /compact /memory /btw /skills …), @file mentions, !shell commands, and shell subcommands (agentium help, login <provider>, models, skills, mcp, update, tidy).
- MCP servers: in the settings, "mcp": {"<name>": {"command": "npx", "args": ["-y", "pkg"], "env": {"TOKEN": "$TOKEN"}}} or {"<name>": {"url": "https://host/mcp"}}; remote OAuth with "agentium mcp login <name>"; loaded when a session starts. There is no /mcp command.
- Skills: "agentium skills add <dir | git URL | owner/repo>", or create <skills dir>/<name>/SKILL.md; /<name> runs one. Custom commands: .agentium/commands/<name>.md (or .claude/commands, in the project or %s) whose text is sent as the message, $ARGUMENTS replaced; /init writes AGENTS.md, /review reviews the diff. Hooks (config.json only): "hooks": {"post_edit": ["gofmt -w {path}"], "stop": ["…"], "pre_tool": [{"match": "bash", "command": "…"}] (call JSON on stdin; exit 2 blocks, stderr says why), "user_prompt": ["…"] (prints context; exit 2 stops the send), "session_start": ["…"]}. /commit, /pr, /fork; "agentium --worktree NAME" works in its own git worktree (branch agentium/NAME); /permissions lists approvals kept per project; /doctor checks the setup. Shortcuts: "keys": {"ctrl+x": "/diff", "f5": "run the tests"} (ctrl+letter, alt+key, f1-f12; agentium's own keys are kept). Other settings: "vim", "dirs", "status_line", "suggest", "subagent_model", "oracle_model" (a stronger model the agent consults through its oracle tool). Providers: "agentium login <provider>" or /login; custom ones under "providers".`,
		version, short(filepath.Join(home, "config.json")), short(filepath.Join(home, "auth.json")),
		short(filepath.Join(home, "AGENTS.md")), short(filepath.Join(home, "USER.md")), short(root), short(proj),
		strings.Join(skillDirs, ", "), short(filepath.Join(home, "sessions")), short(filepath.Join(home, "checkpoints")), short(filepath.Join(home, "commands")))
}

// showMemory prints what Agentium remembers and where it is kept;
// "/memory edit" opens the preferences, "/memory edit project" this
// project's notes, in $EDITOR.
func showMemory(u *ui, m *memCtl, arg string) {
	if m == nil {
		u.note("memory is off (\"memory\": false in the config)")
		return
	}
	st := m.store
	switch strings.TrimSpace(arg) {
	case "edit", "edit prefs", "edit preferences":
		if err := editFile(st.UserPath); err != nil {
			u.failure(err.Error())
			return
		}
		u.success("Saved " + st.UserPath + u.paint(cDim, " (takes effect in the next session)"))
		return
	case "edit project":
		if err := editFile(st.MemoryPath); err != nil {
			u.failure(err.Error())
			return
		}
		u.success("Saved " + st.MemoryPath + u.paint(cDim, " (takes effect in the next session)"))
		return
	}
	var sb strings.Builder
	section := func(title, path, body string) {
		sb.WriteString("\n  " + u.paint(cBold, title) + u.paint(cDim, "  "+path) + "\n")
		body = strings.TrimSpace(body)
		if body == "" {
			sb.WriteString(u.paint(cDim, "    (empty)") + "\n")
			return
		}
		for _, l := range strings.Split(body, "\n") {
			sb.WriteString("    " + sanitize(l) + "\n")
		}
	}
	files := agent.ContextFiles(m.cwd)
	var lines []string
	for _, f := range files {
		lines = append(lines, shortPath(f))
	}
	section("Instructions in use", "AGENTS.md · CLAUDE.md · GEMINI.md", strings.Join(lines, "\n"))
	section("Preferences · every project", st.UserPath, readFile(st.UserPath))
	section("Notes · this project ("+shortPath(st.Root)+")", st.MemoryPath, readFile(st.MemoryPath))
	var ds []string
	for _, d := range st.Decisions() {
		if d.Status == "active" {
			ds = append(ds, d.ID+" "+d.Text)
		}
	}
	section("Decisions · this project", filepath.Dir(st.MemoryPath), strings.Join(ds, "\n"))
	sb.WriteString(u.paint(cDim, "\n  /memory edit · /memory edit project open them in $EDITOR · ask me to @forget something · agentium tidy cleans up") + "\n")
	os.Stderr.WriteString(sb.String())
}
