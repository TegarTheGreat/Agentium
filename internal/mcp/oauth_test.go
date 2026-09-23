package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOAuthServer is an MCP server protected by OAuth, with its own
// authorization server, as the MCP authorization spec describes.
func fakeOAuthServer(t *testing.T) (*httptest.Server, *sync.Map) {
	var srv *httptest.Server
	tokens := &sync.Map{} // access token -> true
	var mu sync.Mutex
	codes := map[string]string{} // code -> code_challenge
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, ok := tokens.Load(tok); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		reply, isReq := handle(body)
		if !isReq {
			w.WriteHeader(202)
			return
		}
		json.NewEncoder(w).Encode(reply)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL + "/auth"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL + "/auth", "authorization_endpoint": srv.URL + "/auth/authorize",
			"token_endpoint": srv.URL + "/auth/token", "registration_endpoint": srv.URL + "/auth/register",
			"code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"client_id": "client-1"})
	})
	mux.HandleFunc("/auth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "client-1" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != srv.URL+"/mcp" {
			http.Error(w, "bad request", 400)
			return
		}
		mu.Lock()
		codes["code-1"] = q.Get("code_challenge")
		mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-1&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		n++
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if _, ok := codes[r.Form.Get("code")]; !ok || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(400)
				return
			}
		}
		tok := fmt.Sprintf("access-%d", n)
		tokens.Store(tok, true)
		json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "refresh_token": "refresh-1", "expires_in": 3600})
	})
	srv = httptest.NewServer(mux)
	return srv, tokens
}

func TestOAuthLoginAndRefresh(t *testing.T) {
	srv, _ := fakeOAuthServer(t)
	defer srv.Close()
	store := &TokenStore{Path: t.TempDir() + "/mcp-auth.json"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := Config{URL: srv.URL + "/mcp", Tokens: store}

	// Without a token: a clear "log in" error.
	if _, err := Start(ctx, "secure", cfg, ""); !errors.Is(err, ErrLoginRequired) || !strings.Contains(err.Error(), "agentium mcp login secure") {
		t.Fatalf("expected login required, got %v", err)
	}
	// The "browser" follows the authorization redirect to our callback.
	browser := func(u string) {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	if err := Login(ctx, cfg.URL, nil, store, browser); err != nil {
		t.Fatal(err)
	}
	c, err := Start(ctx, "secure", cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"authorized"}`))
	c.Close()
	if err != nil || out != "echo: authorized" {
		t.Fatalf("call: %q %v", out, err)
	}
	// An expiring token is refreshed transparently.
	g := store.Get(cfg.URL)
	old := g.AccessToken
	g.Expiry = time.Now().Add(10 * time.Second)
	store.Put(cfg.URL, g)
	tok, err := store.Token(ctx, cfg.URL)
	if err != nil || tok == old {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	if c, err := Start(ctx, "secure", cfg, ""); err != nil {
		t.Fatalf("after refresh: %v", err)
	} else {
		c.Close()
	}
}
