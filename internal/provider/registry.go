package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/models"
)

// Spec describes how to reach one provider.
type Spec struct {
	ID       string
	Protocol string // openai | anthropic | bedrock | vertex
	BaseURL  string
	KeyEnv   []string
	NoKey    bool // local servers, or credentials from the environment
	Default  string
	Headers  map[string]string
	// AuthHeader sends the key in this header verbatim instead of
	// "Authorization: Bearer" (e.g. Azure's api-key).
	AuthHeader string
	// FromRegistry marks providers discovered via models.dev.
	FromRegistry bool
}

// Builtin lists the providers Agentium knows out of the box. Any other
// OpenAI- or Anthropic-compatible endpoint can be added in config.json.
var Builtin = []Spec{
	{ID: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", KeyEnv: []string{"ANTHROPIC_API_KEY"}, Default: "claude-sonnet-5"},
	{ID: "openai", Protocol: "openai", BaseURL: "https://api.openai.com/v1", KeyEnv: []string{"OPENAI_API_KEY"}, Default: "gpt-5.6"},
	{ID: "gemini", Protocol: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, Default: "gemini-3.8-flash"},
	{ID: "openrouter", Protocol: "openai", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: []string{"OPENROUTER_API_KEY"}, Default: "anthropic/claude-sonnet-5"},
	{ID: "groq", Protocol: "openai", BaseURL: "https://api.groq.com/openai/v1", KeyEnv: []string{"GROQ_API_KEY"}},
	{ID: "cerebras", Protocol: "openai", BaseURL: "https://api.cerebras.ai/v1", KeyEnv: []string{"CEREBRAS_API_KEY"}},
	{ID: "deepseek", Protocol: "openai", BaseURL: "https://api.deepseek.com/v1", KeyEnv: []string{"DEEPSEEK_API_KEY"}, Default: "deepseek-flash"},
	{ID: "xai", Protocol: "openai", BaseURL: "https://api.x.ai/v1", KeyEnv: []string{"XAI_API_KEY"}},
	{ID: "mistral", Protocol: "openai", BaseURL: "https://api.mistral.ai/v1", KeyEnv: []string{"MISTRAL_API_KEY"}},
	{ID: "together", Protocol: "openai", BaseURL: "https://api.together.xyz/v1", KeyEnv: []string{"TOGETHER_API_KEY"}},
	{ID: "fireworks", Protocol: "openai", BaseURL: "https://api.fireworks.ai/inference/v1", KeyEnv: []string{"FIREWORKS_API_KEY"}},
	{ID: "moonshot", Protocol: "openai", BaseURL: "https://api.moonshot.ai/v1", KeyEnv: []string{"MOONSHOT_API_KEY"}},
	{ID: "zai", Protocol: "openai", BaseURL: "https://api.z.ai/api/paas/v4", KeyEnv: []string{"ZAI_API_KEY"}},
	{ID: "ollama", Protocol: "openai", BaseURL: "http://localhost:11434/v1", NoKey: true},
	{ID: "lmstudio", Protocol: "openai", BaseURL: "http://localhost:1234/v1", NoKey: true},
	// GitHub Models: official OpenAI-compatible endpoint; a GitHub token
	// (GITHUB_TOKEN, or `gh auth token`) with models access works.
	{ID: "github", Protocol: "openai", BaseURL: "https://models.github.ai/inference", KeyEnv: []string{"GITHUB_TOKEN", "GH_TOKEN"}},
	// Azure OpenAI v1 API: AZURE_OPENAI_ENDPOINT=https://<resource>.openai.azure.com
	{ID: "azure", Protocol: "openai", KeyEnv: []string{"AZURE_OPENAI_API_KEY", "AZURE_API_KEY"}, AuthHeader: "api-key"},
	// Claude on Amazon Bedrock: AWS_BEARER_TOKEN_BEDROCK or AWS access keys; AWS_REGION.
	{ID: "bedrock", Protocol: "bedrock", NoKey: true, Default: "us.anthropic.claude-sonnet-5"},
	// Claude on Google Vertex AI: GOOGLE_CLOUD_PROJECT, region, gcloud credentials.
	{ID: "vertex", Protocol: "vertex", NoKey: true, Default: "claude-sonnet-5"},
}

// registrySkip lists models.dev providers that need a login flow Agentium
// does not implement (subscription OAuth) or a dedicated client.
var registrySkip = map[string]bool{"github-copilot": true, "github-models": true, "amazon-bedrock": true, "google-vertex": true,
	"google-vertex-anthropic": true, "azure": true, "anthropic": true, "openai": true, "google": true}

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
	// Everything models.dev knows that speaks a supported protocol.
	for id, p := range models.Providers(config.Home()) {
		if _, ok := m[id]; ok || registrySkip[id] || p.API == "" || (p.Protocol != "openai" && p.Protocol != "anthropic") {
			continue
		}
		var keys []string
		for _, e := range p.Env {
			if strings.Contains(e, "KEY") || strings.Contains(e, "TOKEN") {
				keys = append(keys, e)
			}
		}
		base := p.API
		if p.Protocol == "anthropic" {
			base = strings.TrimSuffix(base, "/v1")
		}
		m[id] = Spec{ID: id, Protocol: p.Protocol, BaseURL: base, KeyEnv: keys, FromRegistry: true}
	}
	if ep := os.Getenv("AZURE_OPENAI_ENDPOINT"); ep != "" {
		s := m["azure"]
		s.BaseURL = strings.TrimRight(ep, "/") + "/openai/v1"
		m["azure"] = s
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

// Key finds the API key for a provider: env vars first, then auth.json
// (or the OS keychain it points to), then provider-specific helpers.
func Key(s Spec, auth config.Auth) string {
	for _, e := range s.KeyEnv {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	if c, ok := auth[s.ID]; ok {
		if k := c.Secret(s.ID); k != "" {
			return k
		}
	}
	if s.ID == "github" {
		return ghToken()
	}
	return ""
}

var ghToken = sync.OnceValue(func() string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
})

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
	// Info is the model's registry entry (context window, pricing,
	// reasoning support); Known reports whether one was found.
	Info  models.Model
	Known bool
}

// Vision reports whether the model accepts images: from the registry
// when it says, else from well-known multimodal model families.
func (r Resolved) Vision() bool {
	if r.Known && r.Info.Input != nil {
		for _, in := range r.Info.Input {
			if in == "image" {
				return true
			}
		}
		return false
	}
	m := strings.ToLower(r.Model)
	if strings.Contains(m, "o3-mini") || strings.Contains(m, "o1-mini") {
		return false
	}
	for _, f := range []string{"claude", "gpt-4o", "gpt-4.1", "gpt-5", "o3", "o4", "gemini", "grok-4", "pixtral",
		"llava", "vision", "-vl", "llama-4", "kimi-k2.5", "glm-4.5v", "glm-4.6v"} {
		if strings.Contains(m, f) {
			return true
		}
	}
	return false
}

// Reasoning returns request reasoning settings for this model.
func (r Resolved) Reasoning(effort string) Reasoning {
	return Reasoning{Effort: effort, Efforts: r.Info.Efforts, Budget: r.Info.Budget, Interleaved: r.Info.Interleaved}
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
	} else if _, ok := specs[ref]; ok {
		pid = ref // a provider alone: its default model
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
	c, err := client(s, key)
	if err != nil {
		return Resolved{}, fmt.Errorf("provider %s: %w", pid, err)
	}
	info, known := models.Find(config.Home(), pid, model)
	if !known && (pid == "bedrock" || pid == "vertex") {
		// us.anthropic.claude-x / claude-x@version → claude-x
		bare := model[strings.LastIndex(model, ".")+1:]
		bare, _, _ = strings.Cut(bare, "@")
		bare, _, _ = strings.Cut(bare, ":")
		info, known = models.Find(config.Home(), "anthropic", strings.TrimSuffix(bare, "-v1"))
	}
	return Resolved{Provider: pid, Model: model, Client: c, Info: info, Known: known}, nil
}

func client(s Spec, key string) (Client, error) {
	switch s.Protocol {
	case "anthropic":
		return &Anthropic{BaseURL: s.BaseURL, APIKey: key, Headers: s.Headers}, nil
	case "openai", "":
		if s.BaseURL == "" {
			return nil, fmt.Errorf("no base URL (for azure set AZURE_OPENAI_ENDPOINT)")
		}
		c := &OpenAI{BaseURL: s.BaseURL, APIKey: key, Headers: s.Headers}
		if s.AuthHeader != "" {
			c.APIKey = ""
			c.Headers = map[string]string{s.AuthHeader: key}
			for k, v := range s.Headers {
				c.Headers[k] = v
			}
		}
		return c, nil
	case "bedrock":
		return bedrockFromEnv()
	case "vertex":
		return vertexFromEnv()
	}
	return nil, fmt.Errorf("unknown protocol %q", s.Protocol)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func bedrockFromEnv() (Client, error) {
	region := firstEnv("AWS_REGION", "AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}
	b := &Bedrock{Region: region, Bearer: os.Getenv("AWS_BEARER_TOKEN_BEDROCK")}
	if b.Bearer == "" {
		c, ok := AWSCredsFromEnv()
		if !ok {
			return nil, fmt.Errorf("set AWS_BEARER_TOKEN_BEDROCK, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
		}
		b.Creds = c
	}
	return b, nil
}

func vertexFromEnv() (Client, error) {
	project := firstEnv("ANTHROPIC_VERTEX_PROJECT_ID", "GOOGLE_CLOUD_PROJECT", "GOOGLE_VERTEX_PROJECT")
	if project == "" {
		return nil, fmt.Errorf("set GOOGLE_CLOUD_PROJECT (and optionally CLOUD_ML_REGION)")
	}
	region := firstEnv("CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION", "GOOGLE_VERTEX_LOCATION")
	if region == "" {
		region = "global"
	}
	return &Vertex{Project: project, Region: region}, nil
}
