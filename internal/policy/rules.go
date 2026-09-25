package policy

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Rules are the user's standing permissions (settings "permissions", or
// --allow/--deny), as in Claude Code: "bash(go test*)", "edit(src/**)",
// "read(secrets/**)", "mcp(github__*)", or a bare kind for all of it.
// A deny always wins, in every mode; an allow skips the question.
type Rules struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type rule struct {
	kind string
	re   *regexp.Regexp // nil: every subject
	text string
}

func parseRules(list []string, path bool) []rule {
	var out []rule
	for _, r := range list {
		r = strings.TrimSpace(r)
		kind, pat, hasPat := strings.Cut(r, "(")
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "write" {
			kind = "edit"
		}
		ru := rule{kind: kind, text: r}
		if hasPat {
			pat = strings.TrimSuffix(strings.TrimSpace(pat), ")")
			ru.re = globRE(pat, path && kind != "bash" && kind != "mcp")
		}
		out = append(out, ru)
	}
	return out
}

// globRE turns a pattern into a regexp: for paths * stays within a
// folder and ** crosses them; for commands and names * matches anything.
func globRE(p string, path bool) *regexp.Regexp {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			sb.WriteString(".*")
			i++
			if i+1 < len(p) && p[i+1] == '/' {
				i++ // "**/" also matches no folder at all
				sb.WriteString("(?:/)?")
			}
		case c == '*' && path:
			sb.WriteString("[^/]*")
		case c == '*':
			sb.WriteString(".*")
		case c == '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return regexp.MustCompile(`^$.`) // matches nothing
	}
	return re
}

func (r rule) matches(kind, subject string) bool {
	return r.kind == kind && (r.re == nil || r.re.MatchString(subject))
}

// splitCommand cuts a shell line at &&, ||, ;, | and newlines: each part
// must be allowed on its own ("go test && rm -rf ~" is not "go test").
func splitCommand(cmd string) []string {
	var parts []string
	for _, p := range regexp.MustCompile(`&&|\|\||;|\||\n`).Split(cmd, -1) {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// ruleFor reports what the rules say about an action: deny names the rule
// that forbids it; allowed means a rule lets it run without asking.
func (g *Gate) ruleFor(kind, subject string) (deny string, allowed bool) {
	g.mu.RLock()
	allow, denies := g.allow, g.deny
	g.mu.RUnlock()
	if len(allow) == 0 && len(denies) == 0 {
		return "", false
	}
	subjects := []string{subject}
	if kind == "bash" {
		subjects = splitCommand(subject)
	}
	if kind == "edit" || kind == "read" {
		if rel, err := filepath.Rel(g.Root, subject); err == nil && !strings.HasPrefix(rel, "..") {
			subjects = append(subjects, filepath.ToSlash(rel))
		}
		subjects = append(subjects, filepath.ToSlash(subject))
	}
	for _, r := range denies {
		for _, s := range subjects {
			if r.matches(kind, s) {
				return r.text, false
			}
		}
	}
	if kind == "bash" {
		// Every part must be allowed, and nothing may hide a command.
		if strings.Contains(subject, "$(") || strings.Contains(subject, "`") || len(subjects) == 0 {
			return "", false
		}
		for _, s := range subjects {
			ok := false
			for _, r := range allow {
				if r.matches(kind, s) {
					ok = true
					break
				}
			}
			if !ok {
				return "", false
			}
		}
		return "", true
	}
	for _, r := range allow {
		for _, s := range subjects {
			if r.matches(kind, s) {
				return "", true
			}
		}
	}
	return "", false
}

// SetRules installs the user's permission rules.
func (g *Gate) SetRules(r Rules) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allow = append(g.allow, parseRules(r.Allow, true)...)
	g.deny = append(g.deny, parseRules(r.Deny, true)...)
}
