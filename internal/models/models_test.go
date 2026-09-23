package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func fixture(t *testing.T) []byte {
	b, err := os.ReadFile("testdata/api.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReduceAndLookup(t *testing.T) {
	r, err := Reduce(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r.Lookup("anthropic", "claude-opus-5-5")
	if !ok || m.Context != 1_000_000 || m.Output != 128_000 || m.Cost.Input != 4 || m.Cost.CacheRead != 0.2 {
		t.Fatalf("opus 5.5 = %+v", m)
	}
	if len(m.Efforts) == 0 || m.Budget {
		t.Fatalf("opus 5.5 reasoning options = %+v", m)
	}
	if h, _ := r.Lookup("anthropic", "claude-haiku-4-5"); !h.Budget {
		t.Fatalf("haiku should use budget thinking: %+v", h)
	}
	if d, _ := r.Lookup("deepseek", "deepseek-v4-flash"); d.Interleaved != "reasoning_content" {
		t.Fatalf("deepseek interleaved = %q", d.Interleaved)
	}
	// Built-in alias gemini → google; bare lookup via another provider.
	if _, ok := r.Lookup("gemini", "gemini-3.8-flash"); !ok {
		t.Fatal("alias lookup failed")
	}
	if _, ok := r.Lookup("openrouter", "anthropic/claude-sonnet-5"); !ok {
		t.Fatal("bare-id fallback failed")
	}
	if r.Providers["tokengo"].Protocol != "openai" || r.Providers["anthropic"].Protocol != "anthropic" {
		t.Fatal("protocol mapping")
	}
	if p := m.Price(1_000_000, 100_000, 0, 0); p != 4+2 {
		t.Fatalf("price = %v", p)
	}
}

func TestRefreshAndLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fixture(t)) }))
	defer srv.Close()
	old := URL
	URL = srv.URL
	defer func() { URL = old }()
	home := t.TempDir()
	if _, err := Refresh(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	idx = nil
	mu.Unlock()
	if m, ok := Find(home, "anthropic", "claude-sonnet-5"); !ok || m.Context == 0 {
		t.Fatal("find from cache failed")
	}
	if _, ok := Find(home, "openrouter", "openai/gpt-5.6"); !ok {
		t.Fatal("bare fallback from cache failed")
	}
	ps := Providers(home)
	if ps["tokengo"] == nil || ps["tokengo"].API == "" || ps["tokengo"].Models != nil {
		t.Fatalf("provider index = %+v", ps["tokengo"])
	}
}
