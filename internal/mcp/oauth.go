package mcp

// OAuth 2.1 for remote MCP servers, as the MCP authorization spec lays it
// out: the server answers 401 and names its protected-resource metadata;
// that names the authorization server; the client registers itself
// dynamically (RFC 7591), sends the user through a PKCE login in the
// browser, and keeps the tokens, refreshing them when they expire.

import (
	"github.com/tegarthegreat/agentium/internal/fsx"

	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrLoginRequired means the server wants OAuth and no usable token is
// stored; `agentium mcp login <name>` fixes it.
var ErrLoginRequired = errors.New("the server requires login")

// Grant is what is stored for one server.
type Grant struct {
	ClientID      string    `json:"client_id"`
	TokenEndpoint string    `json:"token_endpoint"`
	Resource      string    `json:"resource,omitempty"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	Expiry        time.Time `json:"expiry,omitempty"`
}

// TokenStore keeps grants in a 0600 JSON file, keyed by server URL. The
// file is changed only under a cross-process lock, and a refresh happens
// once per server even when calls race (a refresh token may be single
// use, and reusing one can revoke the whole grant).
type TokenStore struct {
	Path string
	mu   sync.Mutex // guards refreshing
}

func (s *TokenStore) lock() func() { return fsx.Lock(s.Path+".lock", 10*time.Second) }

// load reads the grants; a file that does not parse is an error, so it
// is never overwritten with an empty set.
func (s *TokenStore) load() (map[string]*Grant, error) {
	m := map[string]*Grant{}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (fix or delete it): %w", s.Path, err)
	}
	if m == nil {
		m = map[string]*Grant{}
	}
	return m, nil
}

// Get returns the stored grant for a server, if any.
func (s *TokenStore) Get(serverURL string) *Grant {
	m, err := s.load()
	if err != nil {
		return nil
	}
	return m[serverURL]
}

// Put stores (or, with nil, removes) a grant.
func (s *TokenStore) Put(serverURL string, g *Grant) error {
	defer s.lock()()
	m, err := s.load()
	if err != nil {
		return err
	}
	if g == nil {
		delete(m, serverURL)
	} else {
		m[serverURL] = g
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return fsx.WriteFile(s.Path, b, 0o600)
}

// Token returns a valid access token for serverURL, refreshing it when it
// is about to expire. ErrLoginRequired when there is none.
func (s *TokenStore) Token(ctx context.Context, serverURL string) (string, error) {
	g := s.Get(serverURL)
	if g == nil || g.AccessToken == "" {
		return "", ErrLoginRequired
	}
	if g.Expiry.IsZero() || time.Until(g.Expiry) > time.Minute {
		return g.AccessToken, nil
	}
	// One refresh at a time, across goroutines and processes; whoever
	// waited re-reads the grant, which another refresher may have renewed.
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock := fsx.Lock(s.Path+".refresh.lock", 30*time.Second)
	defer unlock()
	g = s.Get(serverURL)
	if g == nil || g.AccessToken == "" {
		return "", ErrLoginRequired
	}
	if g.Expiry.IsZero() || time.Until(g.Expiry) > time.Minute {
		return g.AccessToken, nil
	}
	if g.RefreshToken == "" {
		return "", ErrLoginRequired
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.RefreshToken}, "client_id": {g.ClientID}}
	if g.Resource != "" {
		form.Set("resource", g.Resource)
	}
	tok, err := tokenRequest(ctx, g.TokenEndpoint, form)
	if err != nil {
		return "", fmt.Errorf("%w (refresh failed: %v)", ErrLoginRequired, err)
	}
	g.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		g.RefreshToken = tok.RefreshToken
	}
	g.Expiry = tok.expiry()
	if err := s.Put(serverURL, g); err != nil {
		return "", err
	}
	return g.AccessToken, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (t tokenResponse) expiry() time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

func tokenRequest(ctx context.Context, endpoint string, form url.Values) (tokenResponse, error) {
	var t tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return t, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return t, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(b, &t)
	if resp.StatusCode/100 != 2 || t.AccessToken == "" {
		if t.Error != "" {
			return t, fmt.Errorf("%s: %s", t.Error, t.Description)
		}
		return t, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, b)
	}
	return t, nil
}

// Metadata is what discovery finds out about a server's authorization.
type Metadata struct {
	Resource              string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	Scopes                []string
}

var resourceMetadataRe = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// Discover finds the authorization server for a protected MCP server.
func Discover(ctx context.Context, serverURL string, headers map[string]string) (*Metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"ping"}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	su, _ := url.Parse(serverURL)
	origin := su.Scheme + "://" + su.Host
	var candidates []string
	if m := resourceMetadataRe.FindStringSubmatch(resp.Header.Get("WWW-Authenticate")); m != nil {
		candidates = append(candidates, m[1])
	}
	if p := strings.TrimSuffix(su.Path, "/"); p != "" {
		candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+p)
	}
	candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		Scopes               []string `json:"scopes_supported"`
	}
	for _, c := range candidates {
		if getJSON(ctx, c, &prm) == nil && len(prm.AuthorizationServers) > 0 {
			break
		}
	}
	md := &Metadata{Resource: prm.Resource, Scopes: prm.Scopes}
	issuer := origin // servers that predate protected-resource metadata
	if len(prm.AuthorizationServers) > 0 {
		issuer = strings.TrimSuffix(prm.AuthorizationServers[0], "/")
	}
	iu, err := url.Parse(issuer)
	if err != nil {
		return nil, err
	}
	iorigin, ipath := iu.Scheme+"://"+iu.Host, strings.TrimSuffix(iu.Path, "/")
	var asm struct {
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RegistrationEndpoint  string   `json:"registration_endpoint"`
		Scopes                []string `json:"scopes_supported"`
	}
	for _, c := range []string{
		iorigin + "/.well-known/oauth-authorization-server" + ipath,
		iorigin + "/.well-known/openid-configuration" + ipath,
		issuer + "/.well-known/openid-configuration",
	} {
		if getJSON(ctx, c, &asm) == nil && asm.AuthorizationEndpoint != "" && asm.TokenEndpoint != "" {
			break
		}
	}
	if asm.AuthorizationEndpoint == "" || asm.TokenEndpoint == "" {
		return nil, fmt.Errorf("no OAuth authorization server metadata found for %s", serverURL)
	}
	md.AuthorizationEndpoint, md.TokenEndpoint, md.RegistrationEndpoint = asm.AuthorizationEndpoint, asm.TokenEndpoint, asm.RegistrationEndpoint
	if len(md.Scopes) == 0 {
		md.Scopes = asm.Scopes
	}
	return md, nil
}

func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func randomString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Login runs the browser flow for serverURL and stores the grant.
// openBrowser is called with the URL the user must visit.
func Login(ctx context.Context, serverURL string, headers map[string]string, store *TokenStore, openBrowser func(string)) error {
	md, err := Discover(ctx, serverURL, headers)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	clientID := ""
	if old := store.Get(serverURL); old != nil && old.ClientID != "" && old.TokenEndpoint == md.TokenEndpoint {
		clientID = old.ClientID
	}
	if md.RegistrationEndpoint != "" {
		// Redirect ports change between runs; register for this one.
		body, _ := json.Marshal(map[string]any{
			"client_name": "Agentium", "redirect_uris": []string{redirect},
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
			"token_endpoint_auth_method": "none",
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("client registration: %v", err)
		}
		var reg struct {
			ClientID string `json:"client_id"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 || json.Unmarshal(b, &reg) != nil || reg.ClientID == "" {
			return fmt.Errorf("client registration failed: HTTP %d: %.200s", resp.StatusCode, b)
		}
		clientID = reg.ClientID
	}
	if clientID == "" {
		return errors.New("the authorization server does not support dynamic client registration; configure an Authorization header instead")
	}

	verifier := randomString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString()
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state},
	}
	if md.Resource != "" {
		q.Set("resource", md.Resource)
	}
	if len(md.Scopes) > 0 {
		q.Set("scope", strings.Join(md.Scopes, " "))
	}
	sep := "?"
	if strings.Contains(md.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	authURL := md.AuthorizationEndpoint + sep + q.Encode()

	type result struct{ code, err string }
	got := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		qs := r.URL.Query()
		res := result{code: qs.Get("code"), err: qs.Get("error")}
		if qs.Get("state") != state {
			res = result{err: "state mismatch"}
		}
		if res.err != "" {
			fmt.Fprintf(w, "Login failed: %s. You can close this tab.", res.err)
		} else {
			fmt.Fprint(w, "Agentium is logged in. You can close this tab.")
		}
		select {
		case got <- res:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	openBrowser(authURL)

	var res result
	select {
	case res = <-got:
	case <-ctx.Done():
		return errors.New("timed out waiting for the browser login")
	}
	if res.err != "" || res.code == "" {
		return fmt.Errorf("login failed: %s", firstNonEmpty(res.err, "no code"))
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {res.code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier}}
	if md.Resource != "" {
		form.Set("resource", md.Resource)
	}
	tok, err := tokenRequest(ctx, md.TokenEndpoint, form)
	if err != nil {
		return fmt.Errorf("token exchange: %v", err)
	}
	return store.Put(serverURL, &Grant{ClientID: clientID, TokenEndpoint: md.TokenEndpoint, Resource: md.Resource,
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, Expiry: tok.expiry()})
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
