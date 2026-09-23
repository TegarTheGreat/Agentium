// Package config loads Agentium's settings and stored credentials from
// $AGENTIUM_HOME (default ~/.agentium).
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ProviderConf defines or overrides a provider.
type ProviderConf struct {
	Protocol  string            `json:"protocol,omitempty"` // "openai" | "anthropic"
	BaseURL   string            `json:"base_url,omitempty"`
	APIKeyEnv string            `json:"api_key_env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Config is ~/.agentium/config.json.
type Config struct {
	Model     string `json:"model,omitempty"`      // "provider/model"
	FastModel string `json:"fast_model,omitempty"` // reserved for background work
	Mode      string `json:"mode,omitempty"`       // ask | auto | yolo
	MaxTokens int    `json:"max_tokens,omitempty"`
	MaxTurns  int    `json:"max_turns,omitempty"`
	// FetchPrivate lets the fetch tool reach localhost/private networks.
	FetchPrivate bool `json:"fetch_private,omitempty"`
	// Checkpoints snapshot the workspace before each changing turn so it
	// can be undone (default on; needs git).
	Checkpoints *bool        `json:"checkpoints,omitempty"`
	Sandbox     *SandboxConf `json:"sandbox,omitempty"`
	// Verify reminds the model to run a check after changing code
	// (default on).
	Verify *bool `json:"verify,omitempty"`
	// Memory enables cross-session memory and recall (default on).
	Memory *bool `json:"memory,omitempty"`
	// ContextTokens overrides the model's context window.
	ContextTokens int                     `json:"context_tokens,omitempty"`
	Providers     map[string]ProviderConf `json:"providers,omitempty"`
}

// SandboxConf configures confinement of shell commands.
type SandboxConf struct {
	Enabled *bool    `json:"enabled,omitempty"` // default true
	Network string   `json:"network,omitempty"` // ask (default) | allow | deny
	Write   []string `json:"write,omitempty"`   // extra writable directories
}

// Home returns the Agentium state directory.
func Home() string {
	if h := os.Getenv("AGENTIUM_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".agentium"
	}
	return filepath.Join(h, ".agentium")
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads config.json; a missing file yields an empty Config.
func Load() (Config, error) {
	var c Config
	err := readJSON(filepath.Join(Home(), "config.json"), &c)
	return c, err
}

// Credential is a stored secret for one provider.
type Credential struct {
	APIKey string `json:"api_key,omitempty"`
}

// Auth maps provider id to credential.
type Auth map[string]Credential

func authPath() string { return filepath.Join(Home(), "auth.json") }

// LoadAuth reads auth.json.
func LoadAuth() (Auth, error) {
	a := Auth{}
	err := readJSON(authPath(), &a)
	return a, err
}

// SaveAuth writes auth.json with owner-only permissions.
func SaveAuth(a Auth) error {
	return writeJSON(authPath(), a, 0o600)
}
