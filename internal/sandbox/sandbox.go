// Package sandbox confines shell commands: the workspace (plus temp and
// build caches) is writable, the rest of the filesystem is read-only, and
// TCP networking is off unless allowed. It uses Landlock on Linux and
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
	Network bool     `json:"network"` // allow TCP connect/bind
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
			".cache", "go", ".npm", ".yarn", ".pnpm-store", ".cargo/registry", ".cargo/git",
			".rustup/tmp", ".m2", ".gradle", ".nuget", ".bun/install/cache", ".local/share/pnpm",
			"Library/Caches",
		} {
			paths = append(paths, filepath.Join(h, p))
		}
	}
	for _, env := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "npm_config_cache", "CARGO_HOME", "XDG_CACHE_HOME", "TMPDIR"} {
		if v := os.Getenv(env); v != "" {
			paths = append(paths, v)
		}
	}
	return uniqueExisting(paths)
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
