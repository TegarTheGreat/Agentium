//go:build darwin

package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// Probe reports what this machine supports.
func Probe() Status {
	if _, err := exec.LookPath(sandboxExec); err != nil {
		return Status{Detail: "sandbox-exec not found: commands run unconfined"}
	}
	return Status{Available: true, Network: true, Detail: "sandbox-exec: filesystem and network confined"}
}

func quote(p string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}

// profile allows everything except writes outside cfg.Write and, unless
// allowed, network access other than local unix sockets.
func profile(cfg Config) string {
	var sb strings.Builder
	sb.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*\n")
	for _, p := range cfg.Write {
		fmt.Fprintf(&sb, "  (subpath %s)\n", quote(p))
	}
	sb.WriteString("  (literal \"/dev/null\") (literal \"/dev/tty\") (regex #\"^/dev/fd/\"))\n")
	if !cfg.Network {
		sb.WriteString("(deny network*)\n(allow network* (remote unix-socket))\n")
	}
	return sb.String()
}

func command(shell, cmdline string, cfg Config) (*exec.Cmd, bool, error) {
	if _, err := exec.LookPath(sandboxExec); err != nil {
		return exec.Command(shell, "-c", cmdline), false, nil
	}
	return exec.Command(sandboxExec, "-p", profile(cfg), shell, "-c", cmdline), true, nil
}

func confineAndExec(Config, []string) error {
	return errors.New("helper not used on macOS")
}
