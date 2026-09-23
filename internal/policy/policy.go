// Package policy decides, deterministically, which actions need the user's
// approval. The model never gets to decide this.
package policy

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Mode controls how much is gated.
type Mode string

const (
	Ask  Mode = "ask"  // approve every bash command and every write
	Auto Mode = "auto" // approve only risky actions (default)
	Yolo Mode = "yolo" // never ask
)

// ParseMode maps a string to a Mode, defaulting to Auto.
func ParseMode(s string) Mode {
	switch Mode(strings.ToLower(s)) {
	case Ask:
		return Ask
	case Yolo:
		return Yolo
	}
	return Auto
}

var risky = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*[rRf][a-zA-Z]*\s+)`), "recursive/forced delete"},
	{regexp.MustCompile(`\bgit\s+push\b.*(\s-f\b|--force)`), "force push"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "discards local changes"},
	{regexp.MustCompile(`\bgit\s+clean\s+-[a-zA-Z]*f`), "deletes untracked files"},
	{regexp.MustCompile(`\bgit\s+(checkout|restore)\s+(--\s+)?\.(\s|$)`), "discards local changes"},
	{regexp.MustCompile(`\bgit\s+branch\s+-D\b`), "deletes a branch"},
	{regexp.MustCompile(`\bsudo\b`), "runs as root"},
	{regexp.MustCompile(`\b(mkfs|fdisk|parted|shutdown|reboot|halt|poweroff)\b`), "system-level command"},
	{regexp.MustCompile(`\bdd\s+.*\bof=`), "raw disk write"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|disk)`), "raw disk write"},
	{regexp.MustCompile(`\bchmod\s+(-R\s+)?[0-7]*777\b`), "world-writable permissions"},
	{regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z)?sh\b`), "pipes a download into a shell"},
	{regexp.MustCompile(`:\(\)\s*\{`), "fork bomb"},
	{regexp.MustCompile(`\b(npm|pnpm|yarn)\s+publish\b|\bcargo\s+publish\b|\btwine\s+upload\b`), "publishes a package"},
	{regexp.MustCompile(`\bdocker\s+system\s+prune\b|\bkubectl\s+delete\b|\bterraform\s+(destroy|apply)\b`), "destroys or changes infrastructure"},
	{regexp.MustCompile(`\bDROP\s+(TABLE|DATABASE)\b`), "drops data"},
}

// RiskyCommand returns a reason if cmd looks destructive, else "".
func RiskyCommand(cmd string) string {
	for _, r := range risky {
		if r.re.MatchString(cmd) {
			return r.reason
		}
	}
	return ""
}

// Outside reports whether path resolves outside root.
func Outside(root, path string) bool {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Approver asks the user. It returns true when the action may proceed.
type Approver func(action, reason string) bool

// Gate combines a mode, a workspace root and an approver. Mode may be
// changed while tools run, so access it through Get/SetMode after setup.
type Gate struct {
	Mode    Mode
	Root    string
	Approve Approver // nil means deny whatever needs approval

	mu sync.RWMutex
}

// GetMode returns the current mode.
func (g *Gate) GetMode() Mode {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.Mode
}

// SetMode changes the mode.
func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	g.Mode = m
	g.mu.Unlock()
}

func (g *Gate) ask(action, reason string) bool {
	if g.Approve == nil {
		return false
	}
	return g.Approve(action, reason)
}

// Bash reports whether cmd may run.
func (g *Gate) Bash(cmd string) (bool, string) {
	mode := g.GetMode()
	if mode == Yolo {
		return true, ""
	}
	reason := RiskyCommand(cmd)
	if reason == "" && mode == Ask {
		reason = "ask mode"
	}
	if reason == "" {
		return true, ""
	}
	return g.ask("bash: "+cmd, reason), reason
}

// Write reports whether path may be written.
func (g *Gate) Write(path string) (bool, string) {
	mode := g.GetMode()
	if mode == Yolo {
		return true, ""
	}
	reason := ""
	if Outside(g.Root, path) {
		reason = "outside workspace"
	} else if mode == Ask {
		reason = "ask mode"
	}
	if reason == "" {
		return true, ""
	}
	return g.ask("write: "+path, reason), reason
}

// NetPolicy controls network access for sandboxed shell commands.
type NetPolicy string

const (
	NetAsk   NetPolicy = "ask"   // approve each command that asks for network (default)
	NetAllow NetPolicy = "allow" // always allow
	NetDeny  NetPolicy = "deny"  // never allow
)

// ParseNet maps a string to a NetPolicy, defaulting to NetAsk.
func ParseNet(s string) NetPolicy {
	switch NetPolicy(strings.ToLower(s)) {
	case NetAllow:
		return NetAllow
	case NetDeny:
		return NetDeny
	}
	return NetAsk
}

// Net reports whether cmd may run with network access.
func (g *Gate) Net(cmd string, p NetPolicy) (bool, string) {
	switch {
	case p == NetDeny:
		return false, "network disabled by config"
	case p == NetAllow || g.GetMode() == Yolo:
		return true, ""
	}
	return g.ask("network: "+cmd, "needs network access"), "needs network access"
}
