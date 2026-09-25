package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
)

// Specialist sub-agents: Markdown files in .agentium/agents or
// .claude/agents (the project's or your own), in Claude Code's format:
//
//	---
//	name: reviewer
//	description: reviews a diff for bugs; use after a change
//	tools: Read, Grep, Bash      (optional: all tools when left out)
//	model: anthropic/claude-haiku-4-5   (optional)
//	---
//	You are a careful reviewer …
//
// The agent hands them work through its task tool.

// agentTools maps Claude Code's tool names onto agentium's.
var agentTools = map[string]string{
	"read": "read", "edit": "edit", "write": "edit", "multiedit": "edit", "notebookedit": "edit",
	"bash": "bash", "grep": "search", "glob": "search", "ls": "search", "search": "search",
	"webfetch": "fetch", "websearch": "fetch", "fetch": "fetch", "todowrite": "todo", "todo": "todo",
	"oracle": "oracle",
}

type agentFile struct {
	def     agent.AgentDef
	model   string
	path    string
	project bool // from the repository, not your own folders
}

// loadAgents finds the specialists; agentium's folders win over .claude's
// and the project's over yours.
func loadAgents(cwd string) []agentFile {
	type src struct {
		dir     string
		project bool
	}
	var dirs []src
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, src{filepath.Join(h, ".claude", "agents"), false})
	}
	dirs = append(dirs, src{filepath.Join(config.Home(), "agents"), false})
	root := config.ProjectRoot(cwd)
	for _, d := range []string{root, cwd} {
		dirs = append(dirs, src{filepath.Join(d, ".claude", "agents"), true}, src{filepath.Join(d, ".agentium", "agents"), true})
	}
	byName := map[string]agentFile{}
	for _, sd := range dirs {
		dir := sd.dir
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			b, err := readCapped(p, 64*1024)
			if err != nil {
				continue
			}
			front, body := splitFront(string(b))
			f := agentFile{path: p, project: sd.project}
			f.def.Prompt = strings.TrimSpace(body)
			hasTools := false
			var denied []string
			addTools := func(v string, deny bool) {
				for _, t := range strings.FieldsFunc(strings.Trim(v, "[]"), func(r rune) bool { return r == ',' || r == ' ' }) {
					t = strings.Trim(t, `"'-`)
					if i := strings.IndexByte(t, '('); i > 0 {
						t = t[:i] // Bash(git diff:*): the tool, without its scope
					}
					n := agentTools[strings.ToLower(t)]
					switch {
					case n == "":
					case deny:
						denied = append(denied, n)
					default:
						f.def.Tools = append(f.def.Tools, n)
					}
				}
			}
			listKey := ""
			for _, l := range strings.Split(front, "\n") {
				if t := strings.TrimSpace(l); strings.HasPrefix(t, "- ") && listKey != "" {
					addTools(t[2:], listKey == "disallowedtools") // YAML list form
					continue
				}
				k, v, ok := strings.Cut(l, ":")
				if !ok {
					continue
				}
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				listKey = ""
				switch key := strings.ToLower(strings.TrimSpace(k)); key {
				case "tools", "disallowedtools":
					if key == "tools" {
						hasTools = true
					}
					listKey = key
					addTools(v, key == "disallowedtools")
					continue
				}
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "name":
					f.def.Name = v
				case "description":
					f.def.Description = v
				case "model":
					if v != "inherit" {
						f.model = v
					}
				}
			}
			// A tools list that names nothing agentium has, or a project
			// agent without one, fails closed: read and search only.
			if hasTools && len(f.def.Tools) == 0 || !hasTools && f.project {
				f.def.Tools = []string{"read", "search"}
			}
			if !hasTools && !f.project && len(denied) > 0 {
				f.def.Tools = []string{"read", "edit", "bash", "search", "fetch", "todo", "oracle"}
			}
			if len(f.def.Tools) > 0 { // else: your own agent, every tool
				f.def.Tools = without(uniq(f.def.Tools), denied)
				if len(f.def.Tools) == 0 {
					f.def.Tools = []string{"read", "search"}
				}
			}
			if f.def.Name == "" {
				f.def.Name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			}
			if f.def.Prompt == "" {
				continue
			}
			// Tools that change nothing: it runs read-only.
			f.def.ReadOnly = len(f.def.Tools) > 0
			for _, t := range f.def.Tools {
				if t == "edit" || t == "bash" {
					f.def.ReadOnly = false
				}
			}
			if old, ok := byName[strings.ToLower(f.def.Name)]; ok && !old.project && f.project {
				continue // a repository cannot replace your own agent
			}
			if f.project {
				f.def.Description = "(project) " + f.def.Description
			}
			byName[strings.ToLower(f.def.Name)] = f
		}
	}
	var out []agentFile
	for _, f := range byName {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].def.Name < out[j].def.Name })
	return out
}

// showAgents lists the specialists (/agents).
func showAgents(u *ui, list []agentFile) {
	fmt.Fprintln(os.Stderr)
	if len(list) == 0 {
		u.note("No specialist agents yet. Add one as .agentium/agents/<name>.md (or .claude/agents):")
		u.note("  ---")
		u.note("  name: reviewer")
		u.note("  description: reviews the current diff for bugs")
		u.note("  tools: Read, Grep, Bash")
		u.note("  ---")
		u.note("  You are a careful reviewer. …")
		u.note("then restart agentium; the agent hands it work (or ask: \"have the reviewer check this\").")
		return
	}
	for _, f := range list {
		tools := "all tools"
		if len(f.def.Tools) > 0 {
			tools = strings.Join(f.def.Tools, ", ")
		}
		if f.def.ReadOnly {
			tools += " · read-only"
		}
		model := ""
		if f.model != "" {
			model = " · " + f.model
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", u.paint(cAccent, f.def.Name), u.paint(cDim, oneLine(f.def.Description, 70)))
		u.note("    " + tools + model + " · " + shortPath(f.path))
	}
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func without(xs, drop []string) []string {
	var out []string
	for _, x := range xs {
		keep := true
		for _, d := range drop {
			keep = keep && x != d
		}
		if keep {
			out = append(out, x)
		}
	}
	return out
}
