// Package config loads Agentium's settings and stored credentials from
// $AGENTIUM_HOME (default ~/.agentium).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tegarthegreat/agentium/internal/fsx"
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
	UI        string `json:"ui,omitempty"`         // fullscreen (default) | classic
	Theme     string `json:"theme,omitempty"`      // auto (default) | dark | light
	Mouse     *bool  `json:"mouse,omitempty"`      // full screen: wheel scrolling (default); false keeps native selection
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
	// LSP enables language-server diagnostics after edits (default true
	// when a server for the language is installed).
	LSP *bool `json:"lsp,omitempty"`
	// Effort sets reasoning effort: minimal|low|medium|high|xhigh|max.
	Effort string `json:"effort,omitempty"`
	// Fast requests the provider's fast output mode where available.
	Fast bool `json:"fast,omitempty"`
	// Fallback lists models to switch to when the main one is rate
	// limited or down, e.g. ["openrouter/anthropic/claude-sonnet-5"].
	Fallback []string `json:"fallback,omitempty"`
	Hooks    *Hooks   `json:"hooks,omitempty"`
	// MCP servers, by name.
	MCP map[string]MCPServer `json:"mcp,omitempty"`
	// ContextTokens overrides the model's context window.
	ContextTokens int                     `json:"context_tokens,omitempty"`
	Providers     map[string]ProviderConf `json:"providers,omitempty"`
}

// Hooks are user commands run at fixed points.
type Hooks struct {
	// PostEdit runs after each successful edit; {path} is the file.
	PostEdit []string `json:"post_edit,omitempty"`
	// Stop runs after each finished turn (e.g. a desktop notification).
	Stop []string `json:"stop,omitempty"`
}

// MCPServer is an MCP server: a local command (stdio) or a remote URL
// (Streamable HTTP; "type": "sse" for the legacy transport). Values of
// env and headers may use $VARS.
type MCPServer struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Type    string            `json:"type,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Literal: env and header values are not $VAR-expanded (set for
	// servers an editor passes over ACP).
	Literal bool `json:"-"`
}

// SandboxConf configures confinement of shell commands.
type SandboxConf struct {
	Enabled *bool    `json:"enabled,omitempty"` // default true
	Network string   `json:"network,omitempty"` // ask (default) | allow | deny
	Write   []string `json:"write,omitempty"`   // extra writable directories
	// PassEnv lists credential-looking environment variables commands
	// may still see (e.g. "GITHUB_TOKEN"); all others are removed.
	PassEnv []string `json:"pass_env,omitempty"`
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
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFile(path, b, perm)
}

// lock serializes read-modify-write of Agentium's state files across
// processes (two sessions, or a session and `agentium login`).
// Lock files live in Home()/locks, which sandboxed commands cannot read
// (and so cannot hold a lock to stall the agent).
func lock(path string) func() {
	return fsx.Lock(filepath.Join(Home(), "locks", filepath.Base(path)+".lock"), 5*time.Second)
}

// Load reads config.json; a missing file yields an empty Config.
func Load() (Config, error) {
	var c Config
	err := readJSON(filepath.Join(Home(), "config.json"), &c)
	return c, err
}

// Set changes one top-level field of config.json (nil removes it),
// leaving every other field, including unknown ones, as written.
func Set(key string, value any) error {
	path := filepath.Join(Home(), "config.json")
	defer lock(path)()
	m := map[string]json.RawMessage{}
	if err := readJSON(path, &m); err != nil {
		return err
	}
	if m == nil { // the file holds "null"
		m = map[string]json.RawMessage{}
	}
	if value == nil {
		delete(m, key)
	} else {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		m[key] = b
	}
	return writeJSON(path, m, 0o600)
}

// Credential is a stored secret for one provider. With Keychain set the
// secret lives in the OS keychain and APIKey is empty.
type Credential struct {
	APIKey   string `json:"api_key,omitempty"`
	Keychain bool   `json:"keychain,omitempty"`
}

// Secret returns the credential's key, reading the keychain if needed.
func (c Credential) Secret(provider string) string {
	if c.APIKey != "" {
		return c.APIKey
	}
	if c.Keychain {
		k, _ := KeychainGet(provider)
		return k
	}
	return ""
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

// UpdateAuth changes auth.json under the cross-process lock, so two
// processes storing different keys never lose one.
func UpdateAuth(f func(Auth)) error {
	defer lock(authPath())()
	a, err := LoadAuth()
	if err != nil {
		return err
	}
	if a == nil {
		a = Auth{}
	}
	f(a)
	return SaveAuth(a)
}

// ProjectRoot is the top of the git repository containing dir, or dir
// itself outside a repository. Project state (memory, code index) is
// keyed by it, so every subdirectory of a repo shares one memory.
func ProjectRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			return dir
		}
		d = p
	}
}

// ProjectDir is the state directory for a project root.
func ProjectDir(root string) string {
	h := sha256.Sum256([]byte(root))
	return filepath.Join(Home(), "projects", hex.EncodeToString(h[:8]))
}
