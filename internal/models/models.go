// Package models is a local cache of the models.dev registry: which
// providers exist (and how to reach them), and per model the context
// window, output limit, pricing and reasoning support.
//
// The raw registry is ~5 MB; it is reduced to the few fields Agentium
// uses and cached under $AGENTIUM_HOME, refreshed in the background when
// older than a week, so startup never waits on the network.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// URL of the registry.
var URL = "https://models.dev/api.json"

const maxAge = 7 * 24 * time.Hour

// Cost is USD per million tokens.
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Model is what Agentium needs to know about a model.
type Model struct {
	ID        string   `json:"id"`
	Context   int      `json:"context,omitempty"`
	Output    int      `json:"output,omitempty"`
	Cost      Cost     `json:"cost,omitempty"`
	Reasoning bool     `json:"reasoning,omitempty"`
	Efforts   []string `json:"efforts,omitempty"` // supported effort values, if any
	Budget    bool     `json:"budget,omitempty"`  // supports budget_tokens thinking
	Tools     bool     `json:"tools,omitempty"`
	// Interleaved names the assistant-message field that must carry the
	// model's reasoning back on later turns (e.g. "reasoning_content").
	Interleaved string `json:"interleaved,omitempty"`
	Released    string `json:"released,omitempty"`
	// Input lists accepted input kinds ("text", "image", ...); nil when
	// the registry did not say (or the cache predates this field).
	Input []string `json:"input,omitempty"`
}

// Provider is one registry provider.
type Provider struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Env      []string         `json:"env,omitempty"`
	API      string           `json:"api,omitempty"`
	Protocol string           `json:"protocol,omitempty"` // openai | anthropic | "" (needs a dedicated client)
	Models   map[string]Model `json:"models"`
}

// Registry is the reduced registry.
type Registry struct {
	Fetched   time.Time            `json:"fetched"`
	Providers map[string]*Provider `json:"providers"`
}

// Reduce converts the raw models.dev JSON into a Registry.
func Reduce(raw []byte) (*Registry, error) {
	var in map[string]struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Env    []string `json:"env"`
		NPM    string   `json:"npm"`
		API    string   `json:"api"`
		Models map[string]struct {
			ID               string `json:"id"`
			Reasoning        bool   `json:"reasoning"`
			ToolCall         bool   `json:"tool_call"`
			ReleaseDate      string `json:"release_date"`
			ReasoningOptions []struct {
				Type   string   `json:"type"`
				Values []string `json:"values"`
			} `json:"reasoning_options"`
			Interleaved json.RawMessage `json:"interleaved"`
			Limit       struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost       Cost `json:"cost"`
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("models.dev: %w", err)
	}
	r := &Registry{Fetched: time.Now(), Providers: map[string]*Provider{}}
	for id, p := range in {
		out := &Provider{ID: id, Name: p.Name, Env: p.Env, API: strings.TrimRight(p.API, "/"), Models: map[string]Model{}}
		switch p.NPM {
		case "@ai-sdk/openai-compatible", "@ai-sdk/openai", "@openrouter/ai-sdk-provider", "@ai-sdk/groq", "@ai-sdk/xai", "@ai-sdk/mistral", "@ai-sdk/deepseek", "@ai-sdk/cerebras", "@ai-sdk/togetherai", "@ai-sdk/fireworks":
			out.Protocol = "openai"
		case "@ai-sdk/anthropic":
			out.Protocol = "anthropic"
		}
		for mid, m := range p.Models {
			mm := Model{ID: mid, Context: m.Limit.Context, Output: m.Limit.Output, Cost: m.Cost,
				Reasoning: m.Reasoning, Tools: m.ToolCall, Released: m.ReleaseDate}
			mm.Input = m.Modalities.Input
			for _, o := range m.ReasoningOptions {
				switch o.Type {
				case "effort":
					mm.Efforts = o.Values
				case "budget_tokens":
					mm.Budget = true
				}
			}
			if len(m.Interleaved) > 0 && m.Interleaved[0] == '{' {
				var f struct {
					Field string `json:"field"`
				}
				if json.Unmarshal(m.Interleaved, &f) == nil {
					mm.Interleaved = f.Field
				}
			}
			out.Models[mid] = mm
		}
		r.Providers[id] = out
	}
	return r, nil
}

// Cache layout (small files, so a lookup reads only what it needs):
//
//	models/providers.json   provider list without models, plus fetch time
//	models/p/<id>.json      one provider's models
func dir(home string) string { return filepath.Join(home, "models") }

type index struct {
	Fetched   time.Time            `json:"fetched"`
	Providers map[string]*Provider `json:"providers"`
	Schema    int                  `json:"schema,omitempty"`
}

// schema is bumped when the cached fields change, forcing a refresh.
const schema = 2

var (
	mu      sync.Mutex
	idxHome string
	idx     *index
	perProv = map[string]map[string]Model{}
)

func loadIndex(h string) *index {
	mu.Lock()
	defer mu.Unlock()
	if idx != nil && idxHome == h {
		return idx
	}
	ix := &index{Providers: map[string]*Provider{}}
	if b, err := os.ReadFile(filepath.Join(dir(h), "providers.json")); err == nil {
		_ = json.Unmarshal(b, ix)
		if ix.Providers == nil {
			ix.Providers = map[string]*Provider{}
		}
	}
	idx, idxHome, perProv = ix, h, map[string]map[string]Model{}
	if (time.Since(ix.Fetched) > maxAge || ix.Schema < schema) && os.Getenv("AGENTIUM_OFFLINE") == "" {
		go func() { _, _ = Refresh(context.Background(), h) }()
	}
	return ix
}

// Providers returns the cached provider list (without models). A stale
// or missing cache is refreshed in the background.
func Providers(h string) map[string]*Provider {
	return loadIndex(h).Providers
}

func providerModels(h, id string) map[string]Model {
	loadIndex(h)
	mu.Lock()
	defer mu.Unlock()
	if m, ok := perProv[id]; ok {
		return m
	}
	m := map[string]Model{}
	if b, err := os.ReadFile(filepath.Join(dir(h), "p", filepath.Base(id)+".json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	perProv[id] = m
	return m
}

// Refresh downloads the registry and rewrites the cache.
func Refresh(ctx context.Context, h string) (*Registry, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := URL
	if v := os.Getenv("AGENTIUM_MODELS_URL"); v != "" {
		u = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models.dev: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	r, err := Reduce(raw)
	if err != nil {
		return nil, err
	}
	if err := r.save(h); err != nil {
		return nil, err
	}
	mu.Lock()
	if idxHome == h {
		idx = nil // reload lazily
	}
	mu.Unlock()
	return r, nil
}

func writeJSON(p string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (r *Registry) save(h string) error {
	if err := os.MkdirAll(filepath.Join(dir(h), "p"), 0o700); err != nil {
		return err
	}
	ix := index{Fetched: r.Fetched, Providers: map[string]*Provider{}, Schema: schema}
	for id, p := range r.Providers {
		cp := *p
		cp.Models = nil
		ix.Providers[id] = &cp
		if err := writeJSON(filepath.Join(dir(h), "p", filepath.Base(id)+".json"), p.Models); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(dir(h), "providers.json"), ix)
}

// Aliases maps Agentium's built-in provider ids to models.dev ids.
var Aliases = map[string]string{"gemini": "google", "together": "togetherai", "moonshot": "moonshotai",
	"bedrock": "amazon-bedrock", "vertex": "google-vertex-anthropic", "github": "github-models"}

// Lookup finds a model, trying the provider's registry entry first and
// then any provider that lists the bare model id.
func (r *Registry) Lookup(provider, model string) (Model, bool) {
	if r == nil {
		return Model{}, false
	}
	pid := provider
	if a, ok := Aliases[pid]; ok {
		pid = a
	}
	if p := r.Providers[pid]; p != nil {
		if m, ok := p.Models[model]; ok {
			return m, true
		}
	}
	bare := model
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	for _, id := range []string{"anthropic", "openai", "google", "xai", "deepseek", "mistral"} {
		if p := r.Providers[id]; p != nil {
			if m, ok := p.Models[bare]; ok {
				return m, true
			}
		}
	}
	return Model{}, false
}

// Find looks a model up in the cache under home, like Registry.Lookup.
func Find(h, provider, model string) (Model, bool) {
	pid := provider
	if a, ok := Aliases[pid]; ok {
		pid = a
	}
	if m, ok := providerModels(h, pid)[model]; ok {
		return m, true
	}
	bare := model
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	for _, id := range []string{"anthropic", "openai", "google", "xai", "deepseek", "mistral"} {
		if m, ok := providerModels(h, id)[bare]; ok {
			return m, true
		}
	}
	return Model{}, false
}

// Price returns the USD cost of the given token counts.
func (m Model) Price(input, output, cacheRead, cacheWrite int) float64 {
	c := m.Cost
	return (float64(input)*c.Input + float64(output)*c.Output + float64(cacheRead)*c.CacheRead + float64(cacheWrite)*c.CacheWrite) / 1e6
}

// List returns a provider's models from the cache, newest first.
func List(h, provider string) []Model {
	ms := providerModels(h, provider)
	out := make([]Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Released != out[j].Released {
			return out[i].Released > out[j].Released
		}
		return out[i].ID < out[j].ID
	})
	return out
}
