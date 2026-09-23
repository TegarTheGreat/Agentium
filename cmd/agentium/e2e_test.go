package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func buildBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agentium-bin-")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "agentium")
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

// recorder collects request bodies from the fake server.
type recorder struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (r *recorder) add(b map[string]any) {
	r.mu.Lock()
	r.reqs = append(r.reqs, b)
	r.mu.Unlock()
}

func (r *recorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.reqs...)
}

// fakeModel answers like a model: first turn creates hello.txt with the
// edit tool, the next turn replies with text. It speaks both protocols.
func fakeModel(t *testing.T, rec *recorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		rec.add(body)
		hasResult := strings.Contains(string(b), `"tool_result"`) || strings.Contains(string(b), `"role":"tool"`)
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/messages"):
			if !hasResult {
				fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":50}}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"edit\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"hello.txt\\\",\\\"new\\\":\\\"hi from anthropic\\\"}\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":20}}\n\n")
			} else {
				fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":60}}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Created hello.txt.\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n")
			}
			fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if !hasResult {
				fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"edit","arguments":"{\"path\":\"hello.txt\",\"new\":\"hi from openai\"}"}}]}}]}`+"\n\n")
			} else {
				fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Created hello.txt."}}]}`+"\n\n")
			}
			fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6}}`+"\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
}

func setupHome(t *testing.T, srvURL string) string {
	t.Helper()
	home := t.TempDir()
	cfg := fmt.Sprintf(`{"providers":{
		"fakeant":{"protocol":"anthropic","base_url":%q,"api_key_env":"FAKE_KEY"},
		"fakeoai":{"protocol":"openai","base_url":%q,"api_key_env":"FAKE_KEY"}}}`, srvURL, srvURL+"/v1")
	os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600)
	return home
}

func runBin(t *testing.T, home, dir, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AGENTIUM_HOME="+home, "FAKE_KEY=k", "NO_COLOR=1")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func TestEndToEnd(t *testing.T) {
	rec := &recorder{}
	srv := fakeModel(t, rec)
	defer srv.Close()
	home := setupHome(t, srv.URL)

	for _, tc := range []struct{ model, want string }{
		{"fakeant/m1", "hi from anthropic"},
		{"fakeoai/m2", "hi from openai"},
	} {
		dir := t.TempDir()
		stdout, stderr, err := runBin(t, home, dir, "", "-m", tc.model, "create hello.txt")
		if err != nil {
			t.Fatalf("%s: %v\nstderr: %s", tc.model, err, stderr)
		}
		if strings.TrimSpace(stdout) != "Created hello.txt." {
			t.Fatalf("%s stdout = %q", tc.model, stdout)
		}
		if !strings.Contains(stderr, "› edit hello.txt") || !strings.Contains(stderr, "2 turns") {
			t.Fatalf("%s stderr = %q", tc.model, stderr)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
		if string(b) != tc.want {
			t.Fatalf("%s hello.txt = %q", tc.model, b)
		}
	}
	if n := len(rec.all()); n != 4 {
		t.Fatalf("requests = %d", n)
	}
}

func TestStdinPromptQuietAndContinue(t *testing.T) {
	rec := &recorder{}
	srv := fakeModel(t, rec)
	defer srv.Close()
	home := setupHome(t, srv.URL)
	dir := t.TempDir()

	stdout, stderr, err := runBin(t, home, dir, "create hello.txt\n", "-q", "-m", "fakeoai/m")
	if err != nil || strings.TrimSpace(stdout) != "Created hello.txt." || stderr != "" {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	// -c continues the saved session: the previous exchange is resent.
	before := len(rec.all())
	if _, stderr, err = runBin(t, home, dir, "", "-q", "-c", "-m", "fakeoai/m", "and again"); err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	msgs := rec.all()[before]["messages"].([]any)
	if len(msgs) < 5 {
		t.Fatalf("continued session should carry history, got %d messages", len(msgs))
	}
}

func TestCLIBasics(t *testing.T) {
	home := t.TempDir()
	out, _, err := runBin(t, home, t.TempDir(), "", "version")
	if err != nil || !strings.HasPrefix(out, "agentium ") {
		t.Fatalf("version: %q %v", out, err)
	}
	out, _, err = runBin(t, home, t.TempDir(), "", "providers")
	if err != nil || !strings.Contains(out, "anthropic") || !strings.Contains(out, "ollama") {
		t.Fatalf("providers: %q %v", out, err)
	}
	_, stderr, err := runBin(t, home, t.TempDir(), "", "-m", "nosuch-model", "hi")
	if err == nil || !strings.Contains(stderr, "unknown provider") {
		t.Fatalf("bad model: %q %v", stderr, err)
	}
	// login reads the key from stdin and stores it with 0600.
	if _, stderr, err = runBin(t, home, t.TempDir(), "sk-test\n", "login", "openai"); err != nil {
		t.Fatalf("login: %v %s", err, stderr)
	}
	st, err := os.Stat(filepath.Join(home, "auth.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json perms: %v %v", st, err)
	}
	out, _, _ = runBin(t, home, t.TempDir(), "", "providers")
	if !strings.Contains(out, "openai      ready") {
		t.Fatalf("after login: %q", out)
	}
	out, _, err = runBin(t, home, t.TempDir(), "", "bench", "-runs", "3")
	if err != nil || !strings.Contains(out, "startup") || !strings.Contains(out, "tokens overhead") {
		t.Fatalf("bench: %q %v", out, err)
	}
}
