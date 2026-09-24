// Package sandbox confines shell commands: the workspace (plus temp and
// build caches) is writable, the rest of the filesystem is read-only, and
// Outbound networking is off unless allowed (servers may still listen),
// and credential files stay unreadable. It uses Landlock on Linux and
// sandbox-exec on macOS; no root, containers or extra binaries needed.
//
// On Linux the agent re-executes itself as a tiny helper that applies the
// Landlock domain to its own thread and then execs the shell, so the
// restriction covers the command and all of its children.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Config describes one confinement.
type Config struct {
	Write   []string `json:"write"`   // writable directory trees
	Network bool     `json:"network"` // allow outbound connections (TCP and UDP)
	// ReadDeny lists credential files and directories that stay
	// unreadable inside the sandbox (SSH keys, cloud credentials,
	// Agentium's own keys). Filled in when a command starts.
	ReadDeny []string `json:"read_deny,omitempty"`
	// NetworkUnenforced: this machine cannot block the network, so
	// commands must not be told it is blocked.
	NetworkUnenforced bool `json:"-"`
}

// helperArg is argv[1] of the re-exec helper.
const helperArg = "__agentium_sandbox"

const envKey = "AGENTIUM_SANDBOX"

// Status says what confinement is available on this machine.
type Status struct {
	Available bool   // filesystem confinement works
	Network   bool   // network can be blocked
	Detail    string // human-readable summary
}

// DefaultWrite returns the writable trees for a workspace: the workspace,
// temp dirs, and common build/package caches so builds and tests work.
func DefaultWrite(root string) []string {
	paths := []string{root, os.TempDir(), "/tmp", "/var/tmp",
		"/dev/null", "/dev/zero", "/dev/tty", "/dev/ptmx", "/dev/pts", "/dev/shm"}
	if h, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{
			// Caches only: never directories on PATH (~/go/bin, pnpm's
			// home) or with scripts other tools run later (~/.gradle
			// init.d, ~/.m2 settings), which would let a sandboxed command
			// plant code that runs unconfined.
			".cache", "go/pkg", ".npm", ".yarn/berry/cache", ".pnpm-store", ".cargo/registry", ".cargo/git",
			".rustup/tmp", ".m2/repository", ".gradle/caches", ".gradle/wrapper", ".nuget/packages",
			".bun/install/cache", ".local/share/pnpm/store", "Library/Caches",
		} {
			paths = append(paths, filepath.Join(h, p))
		}
	}
	for _, env := range []string{"GOCACHE", "GOMODCACHE", "npm_config_cache", "XDG_CACHE_HOME", "TMPDIR"} {
		if v := os.Getenv(env); v != "" {
			paths = append(paths, v)
		}
	}
	if v := os.Getenv("GOPATH"); v != "" {
		paths = append(paths, filepath.Join(v, "pkg")) // not GOPATH/bin
	}
	if v := os.Getenv("CARGO_HOME"); v != "" {
		paths = append(paths, filepath.Join(v, "registry"), filepath.Join(v, "git")) // not CARGO_HOME/bin
	}
	return uniqueExisting(paths)
}

// SecretPaths are the credential files and directories under home that
// sandboxed commands may not read, whether or not they exist.
func SecretPaths(home string) []string {
	var out []string
	for _, p := range []string{".ssh", ".aws", ".gnupg", ".azure", ".kube", ".docker/config.json", ".netrc",
		".pgpass", ".git-credentials", ".config/gh", ".config/gcloud", ".config/hub", ".cargo/credentials",
		".cargo/credentials.toml", ".npmrc", ".pypirc", ".gem/credentials", ".terraform.d/credentials.tfrc.json",
		".agentium/auth.json", ".agentium/mcp-auth.json", ".agentium/config.json", "Library/Keychains"} {
		out = append(out, filepath.Join(home, p))
	}
	if h := os.Getenv("AGENTIUM_HOME"); h != "" {
		for _, f := range []string{"auth.json", "mcp-auth.json", "config.json"} {
			out = append(out, filepath.Join(h, f))
		}
	}
	return out
}

func secretPaths() []string {
	h, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	if r, err := filepath.EvalSymlinks(h); err == nil {
		h = r
	}
	return SecretPaths(h)
}

// ReadOnly drops from write every path that is root, inside root, or
// contains root, so the workspace becomes read-only (plan mode). Scratch
// and cache directories elsewhere stay writable.
func ReadOnly(write []string, root string) []string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	var out []string
	for _, p := range write {
		if within(p, root) || within(root, p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func uniqueExisting(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		} else {
			continue // must exist to be added as a rule
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Command builds an exec.Cmd running `shell -c cmdline` inside cfg.
// When confinement is unavailable it returns an unconfined command and
// ok=false so the caller can decide what to do.
func Command(shell, cmdline string, cfg Config) (cmd *exec.Cmd, ok bool, err error) {
	return command(shell, cmdline, cfg)
}

// MaybeRunHelper must be called first thing in main (and TestMain of
// packages that run confined commands). If this process is the sandbox
// helper, it confines itself and execs the target; it never returns then.
func MaybeRunHelper() {
	if len(os.Args) < 3 || os.Args[1] != helperArg {
		return
	}
	var cfg Config
	if err := json.Unmarshal([]byte(os.Getenv(envKey)), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "agentium sandbox: bad config:", err)
		os.Exit(126)
	}
	os.Unsetenv(envKey)
	if err := confineAndExec(cfg, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentium sandbox:", err)
		os.Exit(126)
	}
}
