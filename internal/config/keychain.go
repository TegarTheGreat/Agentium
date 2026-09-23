package config

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

const keychainService = "agentium"

// KeychainAvailable reports whether an OS keychain CLI is usable:
// macOS `security`, or libsecret's `secret-tool` on Linux.
func KeychainAvailable() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("security")
		return err == nil
	case "linux":
		_, err := exec.LookPath("secret-tool")
		return err == nil
	}
	return false
}

// KeychainSet stores secret for provider.
func KeychainSet(provider, secret string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("security", "add-generic-password", "-U", "-a", provider, "-s", keychainService, "-w", secret).Run()
	case "linux":
		cmd := exec.Command("secret-tool", "store", "--label=agentium "+provider, "service", keychainService, "account", provider)
		cmd.Stdin = strings.NewReader(secret)
		return cmd.Run()
	}
	return errors.New("no keychain on this OS")
}

// KeychainGet reads the secret for provider.
func KeychainGet(provider string) (string, error) {
	var out []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		out, err = exec.Command("security", "find-generic-password", "-a", provider, "-s", keychainService, "-w").Output()
	case "linux":
		out, err = exec.Command("secret-tool", "lookup", "service", keychainService, "account", provider).Output()
	default:
		return "", errors.New("no keychain on this OS")
	}
	return string(bytes.TrimSpace(out)), err
}

// KeychainDelete removes the secret for provider.
func KeychainDelete(provider string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("security", "delete-generic-password", "-a", provider, "-s", keychainService).Run()
	case "linux":
		return exec.Command("secret-tool", "clear", "service", keychainService, "account", provider).Run()
	}
	return nil
}
