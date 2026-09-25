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
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*[rRf][a-zA-Z]*\s+|.*\s--(recursive|force)\b|--(recursive|force)\b)`), "recursive/forced delete"},
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
	// Any tmux/screen command talks to a server outside the sandbox (short
	// aliases like "neww" or "send" make listing subcommands unreliable).
	{regexp.MustCompile(`(^|[;&|(\s])(tmux|screen|byobu|zellij)(\s|$)`), "runs a command outside the sandbox"},
	{regexp.MustCompile(`\b(docker|podman|nerdctl)\b.*\s(run|exec|create|start|compose|build)\b`), "runs a container (outside the sandbox)"},
	{regexp.MustCompile(`--unix-socket\b|\b(busctl|dbus-send|gdbus|systemctl)\s`), "talks to a service outside the sandbox"},
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

// argCheck vetoes arguments that make an otherwise read-only command
// write files or run programs. Long options are matched by prefix too,
// since GNU getopt and git accept abbreviations (--o for --output).
var argCheck = map[string]func(args []string) bool{
	"sort": func(a []string) bool {
		return !longOpt(a, "output", "compress-program", "random-source") && !shortFlag(a, 'o')
	},
	"tree": func(a []string) bool { return !longOpt(a, "output") && !shortFlag(a, 'o') },
	"uniq": func(a []string) bool { return positional(a) <= 1 }, // uniq IN OUT writes OUT
	"rg":   func(a []string) bool { return !longOpt(a, "pre", "hostname-bin") },
	"file": func(a []string) bool { return !longOpt(a, "compile") && !shortFlag(a, 'C') },
	"date": func(a []string) bool { return !longOpt(a, "set", "file") && !shortFlag(a, 's') },
	"find": func(a []string) bool { return !anyPrefix(a, "-exec", "-ok", "-delete", "-fprint", "-fls") },
	"fd": func(a []string) bool {
		return !longOpt(a, "exec", "exec-batch") && !shortFlag(a, 'x') && !shortFlag(a, 'X')
	},
	"jq":  func([]string) bool { return true },
	"git": gitReadOnly,
	"go":  goReadOnly,
	"npm": func(a []string) bool {
		return len(a) > 0 && (a[0] == "ls" || a[0] == "view") && !longOpt(a[1:], "prefix", "cache", "userconfig")
	},
	"shasum": func([]string) bool { return true },
}

func gitReadOnly(a []string) bool {
	if len(a) == 0 {
		return false
	}
	sub, args := a[0], a[1:]
	if shortFlag(args, 'O') || longOpt(args, "open-files-in-pager", "output", "ext-diff", "exec", "textconv", "upload-pack") {
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
	// The flag package accepts -x, --x and -x=value alike.
	names := map[string]bool{}
	for _, f := range a[1:] {
		if strings.HasPrefix(f, "-") {
			n, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
			names[n] = true
		}
	}
	switch a[0] {
	case "version", "doc":
		return true
	case "env":
		return !names["w"] && !names["u"]
	case "list", "vet":
		return !names["toolexec"] && !names["vettool"] && !names["exec"] && !names["overlay"]
	}
	return false
}

// ReadOnlyCommand reports whether every part of a shell command only
// reads. It is conservative: anything it does not recognise is not
// read-only. The command is split like a shell would (quotes removed),
// so quoting cannot hide a flag, and expansions it cannot evaluate
// ($var, $(…), {a,b}) are refused.
func ReadOnlyCommand(cmd string) bool {
	if strings.TrimSpace(cmd) == "" || RiskyCommand(cmd) != "" {
		return false
	}
	cmds, ok := splitShell(cmd)
	if !ok || len(cmds) == 0 {
		return false
	}
	for _, ws := range cmds {
		f := make([]string, len(ws))
		for i, w := range ws {
			if w.unsafe {
				return false
			}
			f[i] = w.text
		}
		name := filepath.Base(f[0])
		if strings.Contains(f[0], "=") {
			return false // VAR=value cmd: the environment can change behaviour
		}
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

// longOpt reports a --option that is, or abbreviates, one of names.
func longOpt(args []string, names ...string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") || len(a) < 3 {
			continue
		}
		opt, _, _ := strings.Cut(a[2:], "=")
		for _, n := range names {
			if strings.HasPrefix(n, opt) {
				return true
			}
		}
	}
	return false
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
	switch {
	case filepath.IsAbs(path):
	case strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`):
		// Rooted but without a drive (Windows): the root of root's drive,
		// not a path inside the workspace.
		path = filepath.VolumeName(root) + path
	default:
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
	// Feedback, if set, returns (once) what the user said when declining
	// action.
	Feedback func(action string) string
	// Protected lists more paths git runs or reads settings from (a
	// core.hooksPath such as .husky, included config files); writing them
	// needs approval like .git itself.
	Protected []string
	// Extra are more directories the user added as workspaces
	// (--add-dir): writing inside them is like writing inside Root.
	Extra []string
	// Unconfined means no OS sandbox holds shell commands to the
	// workspace: in auto mode a command that may change things asks.
	Unconfined bool

	allow, deny []rule // the user's permission rules (SetRules)

	mu sync.RWMutex
}

// AddDir adds a workspace directory.
func (g *Gate) AddDir(dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Extra = append(g.Extra, dir)
}

// Dirs returns the added workspace directories.
func (g *Gate) Dirs() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]string(nil), g.Extra...)
}

// Inside reports whether path is in the workspace or an added directory.
func (g *Gate) Inside(path string) bool { return !g.outside(path) }

// outside reports whether path is outside Root and every added directory.
func (g *Gate) outside(path string) bool {
	if !Outside(g.Root, path) {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, d := range g.Extra {
		if !Outside(d, path) {
			return false
		}
	}
	return true
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

// askWhy asks, and on a refusal says so in the reason (with the user's
// words, if they gave any), so the model knows it was a decision.
func (g *Gate) askWhy(action, reason string) (bool, string) {
	if g.ask(action, reason) {
		return true, reason
	}
	if g.Approve == nil {
		return false, reason
	}
	if g.Feedback != nil {
		if fb := strings.TrimSpace(g.Feedback(action)); fb != "" {
			return false, "the user declined and said: " + fb
		}
	}
	return false, "the user declined (" + reason + ")"
}

// PlanNote tells the model what plan mode means; it is added to the user
// message (not the system prompt, which must stay cacheable).
const PlanNote = "[plan mode: read-only. Investigate as needed, then reply with a short numbered plan: files to change, what changes, how to verify. Do not try to edit files.]"

// planReason is given when plan mode blocks a change.
const planReason = "plan mode is read-only; propose the change in your plan instead"

// Bash reports whether cmd may run. In plan mode only read-only commands
// pass; the shell tool relaxes that when a sandbox enforces read-only.
func (g *Gate) Bash(cmd string) (bool, string) {
	deny, allowed := g.ruleFor("bash", cmd)
	if deny != "" {
		return false, "blocked by your permission rule " + deny
	}
	mode := g.GetMode()
	if mode == Yolo {
		return true, ""
	}
	if allowed && mode != Plan {
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
	if reason == "" && g.Unconfined && !ReadOnlyCommand(cmd) {
		reason = "no sandbox here: the command is not confined to the project"
	}
	if reason == "" {
		return true, ""
	}
	return g.askWhy("bash: "+cmd, reason)
}

// Write reports whether path may be written.
func (g *Gate) Write(path string) (bool, string) {
	deny, allowed := g.ruleFor("edit", path)
	if deny != "" {
		return false, "blocked by your permission rule " + deny
	}
	mode := g.GetMode()
	if mode == Yolo {
		return true, ""
	}
	if allowed && mode != Plan {
		return true, ""
	}
	if mode == Plan {
		return false, planReason
	}
	reason := ""
	if g.outside(path) {
		reason = "outside workspace"
	} else if inGitDir(path) || g.protected(path) {
		// .git/config and hooks hold programs git runs later, outside the
		// sandbox; the shell's git guard covers bash, this covers edit.
		reason = "git internals (config and hooks run programs)"
	} else if mode == Ask {
		reason = "ask mode"
	}
	if reason == "" {
		return true, ""
	}
	return g.askWhy("write: "+path, reason)
}

func (g *Gate) protected(path string) bool {
	for _, p := range g.Protected {
		if rel, err := filepath.Rel(p, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
	return g.askWhy("network: "+cmd, "needs network access")
}

var secretPath = regexp.MustCompile(`/\.(ssh|aws|gnupg|kube|docker|netrc|npmrc|pypirc|git-credentials|config/gcloud|config/gh|agentium/auth\.json)(/|$)|^/etc/(shadow|gshadow|sudoers)`)

// Read reports whether path may be read. Credential stores (SSH keys,
// cloud credentials, ...) need approval: their content would be sent to
// the model provider.
func (g *Gate) Read(path string) (bool, string) {
	if deny, _ := g.ruleFor("read", path); deny != "" {
		return false, "blocked by your permission rule " + deny
	}
	if g.GetMode() == Yolo || !secretPath.MatchString(filepath.ToSlash(path)) && !dotEnv(path) {
		return true, ""
	}
	return g.askWhy("read: "+path, "credential file")
}

// External reports whether an external (MCP) tool may run. Ask mode gates
// them; in auto mode the user opted in by configuring the server. Plan
// mode asks too, since an MCP tool may change things.
func (g *Gate) External(name string) (bool, string) {
	deny, allowed := g.ruleFor("mcp", name)
	if deny != "" {
		return false, "blocked by your permission rule " + deny
	}
	if allowed && g.GetMode() != Plan {
		return true, ""
	}
	switch g.GetMode() {
	case Ask:
		return g.askWhy("mcp: "+name, "ask mode")
	case Plan:
		return g.askWhy("mcp: "+name, "plan mode")
	}
	return true, ""
}

// secretEnv matches environment variable names that usually hold
// credentials, as whole name parts (so GIT_AUTHOR_NAME, GOPRIVATE and
// TOKENIZERS_PARALLELISM are not caught). Child processes (shell
// commands, MCP servers) do not get them: a prompt-injected `env` would
// otherwise hand the model every API key of the session.
var secretEnv = regexp.MustCompile(`(?i)(^|_)(API_?KEY|APIKEY|KEY|KEYS|TOKEN|TOKENS|SECRET|SECRETS|PASSWORD|PASSWD|PWD|PASS|PASSPHRASE|CREDENTIALS?|AUTH|DSN|PRIVATE_KEY|ACCESS_KEY|SESSION_KEY|CONNECTION_STRING|DATABASE_URL)($|_)`)

// userPassURL matches URL values with embedded credentials
// (postgres://user:pass@host), whatever the variable is called.
var userPassURL = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`)

// notSecret are names that match the pattern but are not credentials.
var notSecret = map[string]bool{"PWD": true, "OLDPWD": true, "SSH_AUTH_SOCK": true, "GPG_TTY": true}

func secretVar(k, v string) bool {
	if notSecret[strings.ToUpper(k)] {
		return false
	}
	return secretEnv.MatchString(k) || userPassURL.MatchString(v)
}

// ScrubEnv returns env without credential-looking variables, except the
// names in allow (config "sandbox.pass_env").
func ScrubEnv(env []string, allow []string) []string {
	keep := map[string]bool{}
	for _, a := range allow {
		keep[a] = true
	}
	// GIT_CONFIG_COUNT/KEY_n/VALUE_n only work together: keep the group
	// whole, or drop it whole when a value carries a credential (e.g. an
	// http.extraHeader with a token).
	gitCfgSecret := false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_VALUE_") && (secretValue.MatchString(v) || strings.Contains(strings.ToLower(v), "authorization")) {
			gitCfgSecret = true
		}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			if !gitCfgSecret {
				out = append(out, kv)
			}
			continue
		}
		if secretVar(k, v) && !keep[k] {
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
		if len(v) >= 12 && secretVar(k, v) && strings.Contains(s, v) {
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
		return g.askWhy("fetch: "+url, "ask mode")
	}
	return true, ""
}

// DotEnv reports .env files holding real values (.env, .env.local,
// .env.staging, .envrc), not templates (.env.example, .env.sample).
func DotEnv(path string) bool { return dotEnv(path) }

func dotEnv(path string) bool {
	b := filepath.Base(path)
	if b == ".envrc" {
		return true
	}
	if b != ".env" && !strings.HasPrefix(b, ".env.") {
		return false
	}
	switch strings.TrimPrefix(b, ".env.") {
	case "example", "sample", "template", "dist", "defaults":
		return false
	}
	return true
}

func inGitDir(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		// Case-insensitive: macOS and Windows file systems treat .GIT as
		// .git; Windows also ignores trailing dots and spaces.
		if strings.EqualFold(strings.TrimRight(part, ". "), ".git") {
			return true
		}
	}
	return false
}
