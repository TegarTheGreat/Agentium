package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// Directive is one memory instruction found in a model reply.
type Directive struct {
	Kind string // remember | prefer | decide | forget
	Text string
}

var directiveLine = regexp.MustCompile(`^\s*(?:[-*]\s*)?@(remember|prefer|decide|forget)\s*:?\s+(.+?)\s*$`)

// Parse extracts directives from text, one per line.
func Parse(text string) []Directive {
	var out []Directive
	inFence := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue // examples inside code blocks are not directives
		}
		if m := directiveLine.FindStringSubmatch(l); m != nil {
			out = append(out, Directive{Kind: m[1], Text: m[2]})
		}
	}
	return out
}

// Suspicious content should never become a standing instruction: memory
// is injected into every future prompt, so a poisoned entry (e.g. from a
// fetched page) would act like a persistent prompt injection.
var suspicious = regexp.MustCompile(`(?i)ignore (all |any )?(previous|prior|above)|disregard (the )?(system|previous)|system prompt|you are now|new instructions|curl[^|]*\|\s*(ba|z)?sh|base64 -d|exfiltrat|send (the |your )?(key|token|secret|credential)`)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(sk|pk|rk)-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key)\s*[:=]\s*\S{6,}`),
}

// Redact masks things that look like credentials.
func Redact(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// Apply executes directives and returns one short report line per
// directive for the user.
func (s *Store) Apply(ds []Directive) []string {
	var out []string
	for _, d := range ds {
		text := strings.TrimSpace(Redact(d.Text))
		if len(text) > 300 {
			text = text[:300]
		}
		if suspicious.MatchString(text) {
			out = append(out, fmt.Sprintf("ignored @%s (looks like an instruction injection): %s", d.Kind, clip(text, 60)))
			continue
		}
		var err error
		switch d.Kind {
		case "remember":
			var upd bool
			upd, err = s.Remember(text)
			if err == nil {
				out = append(out, pick(upd, "updated note: ", "remembered: ")+clip(text, 80))
			}
		case "prefer":
			var upd bool
			upd, err = s.Prefer(text)
			if err == nil {
				out = append(out, pick(upd, "preference updated: ", "preference saved: ")+clip(text, 80))
			}
		case "decide":
			var id string
			id, err = s.Decide(text)
			if err == nil {
				out = append(out, "decision "+id+": "+clip(text, 80))
			}
		case "forget":
			var n int
			n, err = s.Forget(text)
			if err == nil {
				out = append(out, fmt.Sprintf("forgot %d matching %q", n, clip(text, 40)))
			}
		}
		if err == ErrFull {
			_ = s.Journal("@pending " + d.Kind + " " + text)
			out = append(out, "memory full, kept in journal instead (run `agentium tidy`): "+clip(text, 60))
		} else if err != nil {
			out = append(out, "memory error: "+err.Error())
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

func pick(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// Hold records directives from a turn that read untrusted content (web
// pages, MCP output) as pending journal entries instead of applying them:
// memory is injected into every future prompt, so it must not be writable
// by whatever a fetched page says. `agentium tidy` reviews pending items.
func (s *Store) Hold(ds []Directive) []string {
	var out []string
	for _, d := range ds {
		text := clip(strings.TrimSpace(Redact(d.Text)), 300)
		if suspicious.MatchString(text) {
			out = append(out, fmt.Sprintf("ignored @%s (looks like an instruction injection): %s", d.Kind, clip(text, 60)))
			continue
		}
		_ = s.Journal("@pending " + d.Kind + " " + text)
		out = append(out, "held for review (this turn read web/MCP content; run `agentium tidy`): "+clip(text, 60))
	}
	return out
}
