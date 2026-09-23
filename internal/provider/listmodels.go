package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
)

// ListModels asks a provider which models it serves (GET /models on
// OpenAI-compatible and Anthropic endpoints). Bedrock and Vertex are not
// listed; callers fall back to the models.dev registry.
func ListModels(ctx context.Context, pid string, cfg config.Config, auth config.Auth) ([]string, error) {
	s, ok := Specs(cfg)[pid]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", pid)
	}
	if s.Protocol != "openai" && s.Protocol != "anthropic" && s.Protocol != "" {
		return nil, fmt.Errorf("%s does not list models", pid)
	}
	if s.BaseURL == "" {
		return nil, fmt.Errorf("%s has no base URL", pid)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := strings.TrimRight(s.BaseURL, "/") + "/models"
	if s.Protocol == "anthropic" {
		url = strings.TrimRight(s.BaseURL, "/") + "/v1/models?limit=100"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	key := Key(s, auth)
	switch {
	case s.Protocol == "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case s.AuthHeader != "":
		req.Header.Set(s.AuthHeader, key)
	case key != "":
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: GET /models: %s", pid, resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct { // Ollama-style
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range body.Data {
		ids = append(ids, d.ID)
	}
	for _, m := range body.Models {
		ids = append(ids, m.Name)
	}
	sort.Strings(ids)
	return ids, nil
}
