// Package policy decides, deterministically, which actions need the user's
// approval. The model never gets to decide this.
package policy

import (
	"os"
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
	Plan Mode = "plan" // read-only: investigate and propose, never change files
)

// ParseMode maps a string to a Mode, defaulting to Auto.
func ParseMode(s string) Mode {
	switch Mode(strings.ToLower(s)) {
	case Ask:
		return Ask
	case Yolo:
		return Yolo
	case Plan:
		return Plan
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
	{regexp.MustCompile(`\bfind\b.*\s-(delete|exec\s+rm)\b`), "bulk delete"},
	{regexp.MustCompile(`\bxargs\b.*\brm\b`), "bulk delete"},
	{regexp.MustCompile(`\bgit\s+push\b.*\s\+\S`), "force push"},
	{regexp.MustCompile(`\b(truncate|shred|wipefs)\s`), "destroys file contents"},
	{regexp.MustCompile(`\bchmod\s+(-R\s+)?0*00\b|\bchmod\s+[0-7]*\s+-R\b.*`), "changes permissions recursively"},
	{regexp.MustCompile(`(~|\$HOME|\$\{HOME\}|/home/[^/\s]+|/root)/\.(ssh|aws|gnupg|kube|docker|netrc|npmrc|pypirc|git-credentials|config/gcloud|config/gh|agentium/auth)`), "touches credentials"},
	{regexp.MustCompile(`/etc/(shadow|sudoers)`), "touches system secrets"},
	{regexp.MustCompile(`\b(shutil\.rmtree|os\.remove|os\.unlink|fs\.rmSync|rimraf|unlink\s+glob)\b`), "deletes files from a script"},
	{regexp.MustCompile(`\b(nc|ncat|socat|telnet)\s+\S+\s+\d+`), "raw network connection"},
	// These hand a command to an already-running process outside the
	// sandbox (tmux server, container daemon, init system, desktop).
	{regexp.MustCompile(`\b(tmux|screen)\b.*\b(run-shell|send-keys|new-window|new-session|split-window|-X\s+stuff)\b`), "runs a command outside the sandbox"},
	{regexp.MustCompile(`\b(docker|podman|nerdctl)\s+(run|exec|create|start|compose|build)\b`), "runs a container (outside the sandbox)"},
	{regexp.MustCompile(`\b(systemd-run|launchctl|osascript|crontab)\s|(^|[;&|]\s*)(at|batch)\s`), "schedules or runs a command outside the sandbox"},
	{regexp.MustCompile(`\b(xdg-open|gio\s+open)\b|(^|[;&|]\s*)open\s+-a\b`), "opens a program outside the sandbox"},
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

// readOnlyCmds are commands that only inspect. Used in plan mode when no
// sandbox can enforce read-only execution.
var readOnlyCmds = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true, "rg": true, "egrep": true,
	"fgrep": true, "find": true, "fd": true, "tree": true, "file": true, "stat": true, "du": true, "df": true,
	"pwd": true, "echo": true, "printf": true, "which": true, "type": true, "uname": true,
	"date": true, "sort": true, "uniq": true, "cut": true, "tr": true, "nl": true, "diff": true, "cmp": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true, "true": true, "jq": true,
	"column": true, "md5sum": true, "sha256sum": true, "sha1sum": true,
}

var (
	// Command substitution and variables could smuggle arguments past
	// the checks below.
	unsafeShell      = regexp.MustCompile("`|\\$|<\\(|>\\(")
	harmlessRedirect = regexp.MustCompile(`\d?>&\d|\d?>\s*/dev/null`)
	cmdSep           = regexp.MustCompile(`\|\||&&|[|;&\n]`)
	unquote          = strings.NewReplacer("'", "", `"`, "", `\`, "")
)

// argCheck vetoes arguments that make an otherwise read-only command
// write files or run programs.
var argCheck = map[string]func(args []string) bool{
	"sort": func(a []string) bool {
		return !hasPrefix(a, "--output") && !hasPrefix(a, "--compress-program") && !shortFlag(a, 'o')
	},
	"tree":   func(a []string) bool { return !hasPrefix(a, "--output") && !shortFlag(a, 'o') },
	"uniq":   func(a []string) bool { return positional(a) <= 1 }, // uniq IN OUT writes OUT
	"rg":     func(a []string) bool { return !hasPrefix(a, "--pre") && !hasPrefix(a, "--hostname-bin") },
	"file":   func(a []string) bool { return !hasFlag(a, "-C", "--compile") },
	"date":   func(a []string) bool { return !hasPrefix(a, "-s") && !hasPrefix(a, "--set") },
	"find":   func(a []string) bool { return !anyPrefix(a, "-exec", "-ok", "-delete", "-fprint", "-fls") },
	"fd":     func(a []string) bool { return !anyPrefix(a, "-x", "-X", "--exec") },
	"jq":     func([]string) bool { return true },
	"git":    gitReadOnly,
	"go":     goReadOnly,
	"npm":    func(a []string) bool { return len(a) > 0 && (a[0] == "ls" || a[0] == "view") },
	"shasum": func([]string) bool { return true },
}

func gitReadOnly(a []string) bool {
	if len(a) == 0 {
		return false
	}
	sub, args := a[0], a[1:]
	if anyPrefix(args, "-O", "--open-files-in-pager", "--output", "--ext-diff", "--exec") {
		return false
	}
	switch sub {
	case "status", "log", "diff", "show", "blame", "grep", "ls-files", "rev-parse", "describe", "shortlog":
		return true
	case "branch":
		return onlyFlags(args, "-a", "-r", "-v", "-vv", "--list", "--show-current", "--all", "--remotes")
	case "remote":
		return len(args) == 0 || len(args) == 1 && args[0] == "-v"
	}
	return false
}

func goReadOnly(a []string) bool {
	if len(a) == 0 {
		return false
	}
	args := a[1:]
	switch a[0] {
	case "version", "doc":
		return true
	case "env":
		return !hasFlag(args, "-w", "-u")
	case "list", "vet":
		return !anyPrefix(args, "-toolexec", "-vettool", "-exec", "--toolexec", "--vettool", "--exec")
	}
	return false
}

// ReadOnlyCommand reports whether every part of a shell pipeline only
// reads. It is conservative: anything it does not recognise is not
// read-only, and quoting cannot hide a flag from the checks.
func ReadOnlyCommand(cmd string) bool {
	if strings.TrimSpace(cmd) == "" || unsafeShell.MatchString(cmd) || RiskyCommand(cmd) != "" {
		return false
	}
	// Allow harmless redirections, reject any other.
	c := harmlessRedirect.ReplaceAllString(cmd, " ")
	if strings.ContainsAny(c, ">") {
		return false
	}
	for _, part := range cmdSep.Split(c, -1) {
		f := strings.Fields(unquote.Replace(part))
		if len(f) == 0 {
			continue
		}
		name := filepath.Base(f[0])
		check, special := argCheck[name]
		switch {
		case special:
			if !check(f[1:]) {
				return false
			}
		case !readOnlyCmds[name]:
			return false
		}
	}
	return true
}

// shortFlag reports a single-dash flag group containing c (-o, -uo).
func shortFlag(args []string, c byte) bool {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a[1:], c) >= 0 {
			return true
		}
	}
	return false
}

func positional(args []string) int {
	n := 0
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			n++
		}
	}
	return n
}

func anyPrefix(args []string, prefixes ...string) bool {
	for _, p := range prefixes {
		if hasPrefix(args, p) {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

func onlyFlags(args []string, flags ...string) bool {
	for _, a := range args {
		if !hasFlag(flags, a) {
			return false
		}
	}
	return true
}

func hasPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
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

// PlanNote tells the model what plan mode means; it is added to the user
// message (not the system prompt, which must stay cacheable).
const PlanNote = "[plan mode: read-only. Investigate as needed, then reply with a short numbered plan: files to change, what changes, how to verify. Do not try to edit files.]"

// planReason is given when plan mode blocks a change.
const planReason = "plan mode is read-only; propose the change in your plan instead"

// Bash reports whether cmd may run. In plan mode only read-only commands
// pass; the shell tool relaxes that when a sandbox enforces read-only.
func (g *Gate) Bash(cmd string) (bool, string) {
	mode := g.GetMode()
	if mode == Yolo {
		return true, ""
	}
	if mode == Plan {
		if ReadOnlyCommand(cmd) {
			return true, ""
		}
		return false, planReason
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
	if mode == Plan {
		return false, planReason
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

var secretPath = regexp.MustCompile(`/\.(ssh|aws|gnupg|kube|docker|netrc|npmrc|pypirc|git-credentials|config/gcloud|config/gh|agentium/auth\.json)(/|$)|^/etc/(shadow|gshadow|sudoers)`)

// Read reports whether path may be read. Credential stores (SSH keys,
// cloud credentials, ...) need approval: their content would be sent to
// the model provider.
func (g *Gate) Read(path string) (bool, string) {
	if g.GetMode() == Yolo || !secretPath.MatchString(filepath.ToSlash(path)) && !dotEnv(path) {
		return true, ""
	}
	return g.ask("read: "+path, "credential file"), "credential file"
}

// External reports whether an external (MCP) tool may run. Ask mode gates
// them; in auto mode the user opted in by configuring the server. Plan
// mode asks too, since an MCP tool may change things.
func (g *Gate) External(name string) (bool, string) {
	switch g.GetMode() {
	case Ask:
		return g.ask("mcp: "+name, "ask mode"), "ask mode"
	case Plan:
		return g.ask("mcp: "+name, "plan mode"), "plan mode"
	}
	return true, ""
}

// secretEnv matches environment variable names that usually hold
// credentials. Child processes (shell commands, MCP servers) do not get
// them: a prompt-injected `env` or `printenv` would otherwise hand the
// model — and anything it can reach — every API key of the session.
var secretEnv = regexp.MustCompile(`(?i)(API_?KEY|_KEY$|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|PRIVATE|ACCESS_KEY|SESSION_KEY|AUTH)`)

// ScrubEnv returns env without credential-looking variables, except the
// names in allow (config "sandbox.pass_env").
func ScrubEnv(env []string, allow []string) []string {
	keep := map[string]bool{}
	for _, a := range allow {
		keep[a] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if secretEnv.MatchString(k) && !keep[k] && k != "SSH_AUTH_SOCK" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// secretValue matches strings shaped like credentials (API keys, tokens,
// private keys).
var secretValue = regexp.MustCompile(`\b(sk|pk|rk)-[A-Za-z0-9_-]{16,}|\bsk-ant-[A-Za-z0-9_-]{16,}|\bAKIA[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bxox[abpr]-[A-Za-z0-9-]{10,}|\bAIza[0-9A-Za-z_-]{30,}|-----BEGIN [A-Z ]*PRIVATE KEY`)

// CarriesSecret reports whether s (e.g. a URL about to be fetched)
// contains a credential: something shaped like one, or the value of a
// credential-looking environment variable of this process. Sending it
// anywhere would leak it.
func CarriesSecret(s string) bool {
	if secretValue.MatchString(s) {
		return true
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if len(v) >= 12 && secretEnv.MatchString(k) && strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// Fetch reports whether url may be fetched. A URL carrying a credential
// is always refused; ask mode asks for every fetch.
func (g *Gate) Fetch(url string) (bool, string) {
	if CarriesSecret(url) {
		return false, "the URL contains a credential"
	}
	if g.GetMode() == Ask {
		return g.ask("fetch: "+url, "ask mode"), "ask mode"
	}
	return true, ""
}

// dotEnv reports .env files holding real values (.env, .env.local,
// .env.production), not templates (.env.example, .env.sample).
func dotEnv(path string) bool {
	b := filepath.Base(path)
	if b != ".env" && !strings.HasPrefix(b, ".env.") {
		return false
	}
	switch strings.TrimPrefix(b, ".env.") {
	case "example", "sample", "template", "dist", "defaults":
		return false
	}
	return true
}
