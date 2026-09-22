package provider

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tegarthegreat/agentium/internal/config"
)

// Spec describes how to reach one provider.
type Spec struct {
	ID       string
	Protocol string // "openai" | "anthropic"
	BaseURL  string
	KeyEnv   []string
	NoKey    bool // local servers
	Default  string
	Headers  map[string]string
}

// Builtin lists the providers Agentium knows out of the box. Any other
// OpenAI- or Anthropic-compatible endpoint can be added in config.json.
var Builtin = []Spec{
	{ID: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", KeyEnv: []string{"ANTHROPIC_API_KEY"}, Default: "claude-sonnet-5"},
	{ID: "openai", Protocol: "openai", BaseURL: "https://api.openai.com/v1", KeyEnv: []string{"OPENAI_API_KEY"}, Default: "gpt-5.5"},
	{ID: "gemini", Protocol: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, Default: "gemini-3.5-flash"},
	{ID: "openrouter", Protocol: "openai", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: []string{"OPENROUTER_API_KEY"}, Default: "anthropic/claude-sonnet-5"},
	{ID: "groq", Protocol: "openai", BaseURL: "https://api.groq.com/openai/v1", KeyEnv: []string{"GROQ_API_KEY"}},
	{ID: "cerebras", Protocol: "openai", BaseURL: "https://api.cerebras.ai/v1", KeyEnv: []string{"CEREBRAS_API_KEY"}},
	{ID: "deepseek", Protocol: "openai", BaseURL: "https://api.deepseek.com/v1", KeyEnv: []string{"DEEPSEEK_API_KEY"}, Default: "deepseek-chat"},
	{ID: "xai", Protocol: "openai", BaseURL: "https://api.x.ai/v1", KeyEnv: []string{"XAI_API_KEY"}},
	{ID: "mistral", Protocol: "openai", BaseURL: "https://api.mistral.ai/v1", KeyEnv: []string{"MISTRAL_API_KEY"}},
	{ID: "together", Protocol: "openai", BaseURL: "https://api.together.xyz/v1", KeyEnv: []string{"TOGETHER_API_KEY"}},
	{ID: "fireworks", Protocol: "openai", BaseURL: "https://api.fireworks.ai/inference/v1", KeyEnv: []string{"FIREWORKS_API_KEY"}},
	{ID: "moonshot", Protocol: "openai", BaseURL: "https://api.moonshot.ai/v1", KeyEnv: []string{"MOONSHOT_API_KEY"}},
	{ID: "zai", Protocol: "openai", BaseURL: "https://api.z.ai/api/paas/v4", KeyEnv: []string{"ZAI_API_KEY"}},
	{ID: "ollama", Protocol: "openai", BaseURL: "http://localhost:11434/v1", NoKey: true},
	{ID: "lmstudio", Protocol: "openai", BaseURL: "http://localhost:1234/v1", NoKey: true},
}

// Specs returns builtin specs merged with config overrides and additions.
func Specs(cfg config.Config) map[string]Spec {
	m := make(map[string]Spec, len(Builtin)+len(cfg.Providers))
	for _, s := range Builtin {
		m[s.ID] = s
	}
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		s := m["ollama"]
		if !strings.HasPrefix(h, "http") {
			h = "http://" + h
		}
		s.BaseURL = strings.TrimRight(h, "/") + "/v1"
		m["ollama"] = s
	}
	for id, pc := range cfg.Providers {
		s, ok := m[id]
		if !ok {
			s = Spec{ID: id, Protocol: "openai", NoKey: pc.APIKeyEnv == ""}
		}
		if pc.Protocol != "" {
			s.Protocol = pc.Protocol
		}
		if pc.BaseURL != "" {
			s.BaseURL = pc.BaseURL
		}
		if pc.APIKeyEnv != "" {
			s.KeyEnv = []string{pc.APIKeyEnv}
			s.NoKey = false
		}
		if pc.Headers != nil {
			s.Headers = pc.Headers
		}
		m[id] = s
	}
	return m
}

// IDs returns sorted provider ids.
func IDs(specs map[string]Spec) []string {
	ids := make([]string, 0, len(specs))
	for id := range specs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Key finds the API key for a provider: env vars first, then auth.json.
func Key(s Spec, auth config.Auth) string {
	for _, e := range s.KeyEnv {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return auth[s.ID].APIKey
}

// guess maps a bare model name to a provider.
func guess(model string) string {
	switch {
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return "openai"
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	case strings.HasPrefix(model, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(model, "grok"):
		return "xai"
	}
	return ""
}

// Resolved is a ready-to-use client and model name.
type Resolved struct {
	Provider string
	Model    string
	Client   Client
}

// Resolve turns "provider/model" (or a bare model, or "") into a client.
// An empty ref picks the first provider that has credentials.
func Resolve(ref string, cfg config.Config, auth config.Auth) (Resolved, error) {
	specs := Specs(cfg)
	var pid, model string
	if ref == "" {
		for _, s := range Builtin {
			if !s.NoKey && Key(specs[s.ID], auth) != "" && s.Default != "" {
				pid, model = s.ID, s.Default
				break
			}
		}
		if pid == "" {
			return Resolved{}, fmt.Errorf("no model configured: set e.g. ANTHROPIC_API_KEY or OPENAI_API_KEY, run `agentium login <provider>`, or pass -m ollama/<model>")
		}
	} else if i := strings.Index(ref, "/"); i > 0 {
		if _, ok := specs[ref[:i]]; ok {
			pid, model = ref[:i], ref[i+1:]
		}
	}
	if pid == "" {
		pid, model = guess(ref), ref
		if pid == "" {
			return Resolved{}, fmt.Errorf("unknown provider for %q; use provider/model (providers: %s)", ref, strings.Join(IDs(specs), ", "))
		}
	}
	s := specs[pid]
	if model == "" {
		model = s.Default
	}
	if model == "" {
		return Resolved{}, fmt.Errorf("no model given for provider %s", pid)
	}
	key := Key(s, auth)
	if key == "" && !s.NoKey {
		env := ""
		if len(s.KeyEnv) > 0 {
			env = s.KeyEnv[0] + " or "
		}
		return Resolved{}, fmt.Errorf("no credentials for %s: set %s run `agentium login %s`", pid, env, pid)
	}
	var c Client
	switch s.Protocol {
	case "anthropic":
		c = &Anthropic{BaseURL: s.BaseURL, APIKey: key, Headers: s.Headers}
	case "openai", "":
		c = &OpenAI{BaseURL: s.BaseURL, APIKey: key, Headers: s.Headers}
	default:
		return Resolved{}, fmt.Errorf("provider %s: unknown protocol %q", pid, s.Protocol)
	}
	return Resolved{Provider: pid, Model: model, Client: c}, nil
}
