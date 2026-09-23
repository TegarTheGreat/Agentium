package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	cmd.Env = append(os.Environ(), "AGENTIUM_HOME="+home, "FAKE_KEY=k", "NO_COLOR=1", "AGENTIUM_OFFLINE=1")
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

func TestUndo(t *testing.T) {
	rec := &recorder{}
	srv := fakeModel(t, rec)
	defer srv.Close()
	home := setupHome(t, srv.URL)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("original"), 0o644)

	if _, stderr, err := runBin(t, home, dir, "", "-q", "-m", "fakeoai/m", "create hello.txt"); err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatal("agent should have created hello.txt")
	}
	_, stderr, err := runBin(t, home, dir, "", "undo")
	if err != nil || !strings.Contains(stderr, "reverted 1 file") {
		t.Fatalf("undo: %v %q", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); !os.IsNotExist(err) {
		t.Fatal("undo should remove the created file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keep.txt")); string(b) != "original" {
		t.Fatal("undo must not touch unrelated files")
	}
	if _, stderr, _ = runBin(t, home, dir, "", "undo"); !strings.Contains(stderr, "nothing to undo") {
		t.Fatalf("second undo: %q", stderr)
	}
	// The model is told about the undo on the next continued turn.
	before := len(rec.all())
	if _, stderr, err = runBin(t, home, dir, "", "-q", "-c", "-m", "fakeoai/m", "next"); err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	b, _ := json.Marshal(rec.all()[before])
	if !strings.Contains(string(b), "undid the file changes") {
		t.Fatalf("undo note not delivered: %s", b)
	}
}

func TestMemoryAcrossSessions(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		rec.add(body)
		w.Header().Set("Content-Type", "text/event-stream")
		reply := "Noted.\n@remember deploys run scripts/ship.sh from the repo root\n@decide indent with tabs — matches gofmt"
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\ndata: [DONE]\n\n", reply)
	}))
	defer srv.Close()
	home := setupHome(t, srv.URL)
	dir, _ := filepath.EvalSymlinks(t.TempDir())

	_, stderr, err := runBin(t, home, dir, "", "-m", "fakeoai/m", "remember how we deploy the ship script")
	if err != nil || !strings.Contains(stderr, "remembered: deploys run scripts/ship.sh") || !strings.Contains(stderr, "decision D-001") {
		t.Fatalf("first session: %v\n%s", err, stderr)
	}
	// A brand-new session (no -c) sees the memory snapshot in its system
	// prompt and gets the earlier exchange recalled for a related question.
	before := len(rec.all())
	_, stderr, err = runBin(t, home, dir, "", "-m", "fakeoai/m", "which ship script deploys?")
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	b, _ := json.Marshal(rec.all()[before])
	req := string(b)
	for _, want := range []string{"scripts/ship.sh", "D-001", "@remember", "\\u003crecall"} {
		if !strings.Contains(req, want) {
			t.Errorf("second session request missing %q", want)
		}
	}
	if !strings.Contains(stderr, "recalled") {
		t.Errorf("recall notice missing: %s", stderr)
	}
	// Memory can be switched off.
	os.WriteFile(filepath.Join(home, "config.json"), []byte(fmt.Sprintf(`{"memory":false,"providers":{"fakeoai":{"base_url":%q,"api_key_env":"FAKE_KEY"}}}`, srv.URL+"/v1")), 0o600)
	before = len(rec.all())
	runBin(t, home, dir, "", "-m", "fakeoai/m", "which ship script deploys?")
	b, _ = json.Marshal(rec.all()[before])
	if strings.Contains(string(b), "scripts/ship.sh") {
		t.Error("memory disabled but still injected")
	}
}

func TestTidy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		out := "=== USER.md\n- prefers short answers\n=== MEMORY.md\n- deploy with scripts/ship.sh\n- tests: make test"
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\ndata: [DONE]\n\n", out)
	}))
	defer srv.Close()
	home := setupHome(t, srv.URL)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	_, stderr, err := runBin(t, home, dir, "", "tidy", "--yes", "-m", "fakeoai/m")
	if err != nil || !strings.Contains(stderr, "+ - deploy with scripts/ship.sh") || !strings.Contains(stderr, "memory updated") {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "USER.md")); !strings.Contains(string(b), "short answers") {
		t.Fatalf("USER.md = %q", b)
	}
	_, stderr, _ = runBin(t, home, dir, "", "tidy", "--yes", "-m", "fakeoai/m")
	if !strings.Contains(stderr, "already tidy") {
		t.Fatalf("second tidy: %s", stderr)
	}
}

func TestModelsRegistryAndCost(t *testing.T) {
	fixture, err := os.ReadFile("../../internal/models/testdata/api.json")
	if err != nil {
		t.Fatal(err)
	}
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fixture) }))
	defer reg.Close()
	home := t.TempDir()
	cmd := exec.Command(buildBinary(t), "models", "--refresh", "anthropic")
	cmd.Env = append(os.Environ(), "AGENTIUM_HOME="+home, "AGENTIUM_MODELS_URL="+reg.URL)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "claude-opus-5-5") || !strings.Contains(string(out), "$4/$20") {
		t.Fatalf("models: %v\n%s", err, out)
	}
	// Registry providers (tokengo) become usable by setting their key env.
	cmd = exec.Command(buildBinary(t), "providers")
	cmd.Env = append(os.Environ(), "AGENTIUM_HOME="+home, "AGENTIUM_OFFLINE=1", "TOKENGO_API_KEY=x")
	out, _ = cmd.CombinedOutput()
	if !strings.Contains(string(out), "tokengo     ready") {
		t.Fatalf("providers:\n%s", out)
	}
	// A known model gets its price in the stats line.
	var gotEffort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		if oc, ok := body["output_config"].(map[string]any); ok {
			gotEffort, _ = oc["effort"].(string)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1000000}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	os.WriteFile(filepath.Join(home, "config.json"), []byte(fmt.Sprintf(`{"effort":"xhigh","providers":{"anthropic":{"base_url":%q}}}`, srv.URL)), 0o600)
	cmd = exec.Command(buildBinary(t), "-m", "anthropic/claude-opus-5-5", "hi")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "AGENTIUM_HOME="+home, "AGENTIUM_OFFLINE=1", "ANTHROPIC_API_KEY=k", "NO_COLOR=1")
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "$4.0000") || gotEffort != "xhigh" {
		t.Fatalf("cost/effort: %v effort=%q\n%s", err, gotEffort, out)
	}
}

func TestOpenRouterOAuth(t *testing.T) {
	var gotVerifier string
	keySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "the-code" || body["code_challenge_method"] != "S256" {
			http.Error(w, "bad", 400)
			return
		}
		gotVerifier = body["code_verifier"]
		fmt.Fprint(w, `{"key":"sk-or-test"}`)
	}))
	defer keySrv.Close()
	oldAuth, oldKey, oldOpen := openRouterAuthURL, openRouterKeyURL, openBrowser
	defer func() { openRouterAuthURL, openRouterKeyURL, openBrowser = oldAuth, oldKey, oldOpen }()
	openRouterKeyURL = keySrv.URL
	openRouterAuthURL = "https://openrouter.example/auth"
	// The "browser" follows the auth URL straight to the callback.
	openBrowser = func(u string) {
		pu, _ := url.Parse(u)
		cb := pu.Query().Get("callback_url")
		if pu.Query().Get("code_challenge") == "" {
			return
		}
		go http.Get(cb + "?code=the-code")
	}
	key, err := openRouterOAuth(context.Background())
	if err != nil || key != "sk-or-test" || len(gotVerifier) < 43 {
		t.Fatalf("key=%q err=%v verifier=%q", key, err, gotVerifier)
	}
}

func TestJSONMode(t *testing.T) {
	rec := &recorder{}
	srv := fakeModel(t, rec)
	defer srv.Close()
	home := setupHome(t, srv.URL)
	dir := t.TempDir()
	stdout, stderr, err := runBin(t, home, dir, "", "--json", "-m", "fakeoai/m", "create hello.txt")
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	var types []string
	var result map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		types = append(types, ev["type"].(string))
		if ev["type"] == "result" {
			result = ev
		}
	}
	got := strings.Join(types, ",")
	if !strings.HasPrefix(got, "session,tool_call,tool_result,") || !strings.HasSuffix(got, "result") {
		t.Fatalf("events = %s", got)
	}
	if result["ok"] != true || result["text"] != "Created hello.txt." || result["files_changed"].([]any)[0] != "hello.txt" {
		t.Fatalf("result = %v", result)
	}
	// Exit code 2 when a limit stops the run.
	_, _, err = runBin(t, home, t.TempDir(), "", "--json", "--max-turns", "1", "-m", "fakeoai/m", "create hello.txt")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("max-turns exit: %v", err)
	}
}
