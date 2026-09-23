package main

import (
	"bytes"
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
	"os/exec"
	"runtime"
	"time"
)

// OpenRouter's PKCE flow is designed for third-party apps: the user
// approves in the browser, OpenRouter redirects to a localhost callback
// with a code, and the code is exchanged for a user-scoped API key.
var (
	openRouterAuthURL = "https://openrouter.ai/auth"
	openRouterKeyURL  = "https://openrouter.ai/api/v1/auth/keys"
	openBrowser       = func(u string) {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", u)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
		default:
			cmd = exec.Command("xdg-open", u)
		}
		_ = cmd.Start()
	}
)

func pkcePair() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(h[:]), nil
}

func openRouterOAuth(ctx context.Context) (string, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:3000")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
	}
	defer ln.Close()
	callback := fmt.Sprintf("http://localhost:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	codes := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if r.URL.Path != "/callback" || code == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "Agentium is logged in to OpenRouter. You can close this tab.")
		select {
		case codes <- code:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	u := openRouterAuthURL + "?" + url.Values{
		"callback_url":          {callback},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	fmt.Fprintf(os.Stderr, "Opening your browser to approve Agentium on OpenRouter.\nIf it does not open, visit:\n  %s\n", u)
	openBrowser(u)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var code string
	select {
	case code = <-codes:
	case <-ctx.Done():
		return "", errors.New("timed out waiting for the browser approval")
	}
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "code_challenge_method": "S256"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterKeyURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("key exchange failed: HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Key == "" {
		return "", fmt.Errorf("key exchange: unexpected response %s", b)
	}
	return out.Key, nil
}
