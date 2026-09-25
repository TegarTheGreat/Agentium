package agent

import (
	"encoding/json"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// Models trained with other agents call their tools by those names
// (Claude Code's Write, Grep, Glob, LS, WebFetch; str_replace; run_command
// ...). Such a call runs as the matching tool here, arguments renamed,
// instead of failing as unknown.

type toolAlias struct {
	to   string
	args func(m map[string]any) map[string]any
}

func pickStr(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && v != "" {
			return v
		}
	}
	return nil
}

func set(out map[string]any, k string, v any) {
	if v != nil {
		out[k] = v
	}
}

var toolAliases = map[string]toolAlias{
	"write": {"edit", func(m map[string]any) map[string]any {
		out := map[string]any{"path": pickStr(m, "path", "file_path", "filename", "file")}
		set(out, "new", pickStr(m, "content", "contents", "text", "new"))
		if out["new"] == nil {
			out["new"] = ""
		}
		return out
	}},
	"str_replace": {"edit", func(m map[string]any) map[string]any {
		out := map[string]any{"path": pickStr(m, "path", "file_path", "filename")}
		set(out, "old", pickStr(m, "old_string", "old_str", "old", "search"))
		set(out, "new", pickStr(m, "new_string", "new_str", "new", "replace"))
		set(out, "all", pickStr(m, "replace_all", "all"))
		return out
	}},
	"multiedit": {"edit", func(m map[string]any) map[string]any {
		out := map[string]any{"path": pickStr(m, "path", "file_path")}
		var parts []any
		edits, _ := m["edits"].([]any)
		for _, e := range edits {
			if em, ok := e.(map[string]any); ok {
				p := map[string]any{"old": pickStr(em, "old_string", "old"), "new": pickStr(em, "new_string", "new")}
				set(p, "all", pickStr(em, "replace_all", "all"))
				parts = append(parts, p)
			}
		}
		out["edits"] = parts
		return out
	}},
	"read_file": {"read", func(m map[string]any) map[string]any {
		out := map[string]any{"path": pickStr(m, "path", "file_path", "filename", "file")}
		set(out, "offset", pickStr(m, "offset", "start_line"))
		set(out, "limit", pickStr(m, "limit"))
		return out
	}},
	"ls": {"read", func(m map[string]any) map[string]any {
		p := pickStr(m, "path", "dir", "directory")
		if p == nil {
			p = "."
		}
		return map[string]any{"path": p}
	}},
	"grep": {"search", func(m map[string]any) map[string]any {
		out := map[string]any{"pattern": pickStr(m, "pattern", "query", "regex")}
		set(out, "path", pickStr(m, "path", "dir"))
		set(out, "glob", pickStr(m, "glob", "include", "file_pattern"))
		set(out, "ignore_case", pickStr(m, "-i", "ignore_case", "case_insensitive"))
		return out
	}},
	"glob": {"search", func(m map[string]any) map[string]any {
		out := map[string]any{"glob": pickStr(m, "pattern", "glob")}
		set(out, "path", pickStr(m, "path", "dir"))
		return out
	}},
	"web_fetch": {"fetch", func(m map[string]any) map[string]any {
		return map[string]any{"url": pickStr(m, "url", "uri")}
	}},
	"websearch": {"web_search", func(m map[string]any) map[string]any {
		out := map[string]any{"query": pickStr(m, "query", "q", "search")}
		set(out, "domains", pickStr(m, "allowed_domains", "domains"))
		set(out, "exclude", pickStr(m, "blocked_domains", "exclude"))
		return out
	}},
	"run": {"bash", func(m map[string]any) map[string]any {
		out := map[string]any{"cmd": pickStr(m, "command", "cmd", "script")}
		set(out, "timeout", pickStr(m, "timeout"))
		return out
	}},
}

// aliasNames lists the other spellings of each alias.
var aliasNames = map[string]string{
	"write": "write", "write_file": "write", "create_file": "write", "file_write": "write",
	"str_replace": "str_replace", "str_replace_editor": "str_replace", "replace_in_file": "str_replace", "edit_file": "str_replace",
	"multiedit": "multiedit", "multi_edit": "multiedit",
	"read_file": "read_file", "view": "read_file", "view_file": "read_file", "cat": "read_file", "open_file": "read_file",
	"ls": "ls", "list_dir": "ls", "list_directory": "ls", "list_files": "ls",
	"grep": "grep", "rg": "grep", "search_files": "grep", "grep_search": "grep",
	"glob": "glob", "find_files": "glob", "file_search": "glob",
	"webfetch": "web_fetch", "web_fetch": "web_fetch", "fetch_url": "web_fetch", "url_fetch": "web_fetch",
	"websearch": "websearch", "search_web": "websearch", "web-search": "websearch", "google_search": "websearch",
	"run": "run", "shell": "run", "execute": "run", "execute_command": "run", "run_command": "run", "terminal": "run", "run_terminal_cmd": "run",
}

// resolveAlias maps a call to an unknown tool onto an available one;
// ok is false when there is no such tool here.
func resolveAlias(c provider.ToolCall, have func(string) bool) (provider.ToolCall, bool) {
	name := strings.ToLower(c.Name)
	if have(name) { // Bash, Read, Edit: only the case differs
		c.Name = name
		return c, true
	}
	a, ok := toolAliases[aliasNames[name]]
	if !ok || !have(a.to) {
		return c, false
	}
	var m map[string]any
	if json.Unmarshal(c.Args, &m) != nil {
		return c, false
	}
	args, err := json.Marshal(a.args(m))
	if err != nil {
		return c, false
	}
	c.Name, c.Args = a.to, args
	return c, true
}
