// Package lsp is a minimal Language Server Protocol client: it starts the
// project's language server (gopls, pyright, typescript-language-server,
// rust-analyzer, clangd) when one is installed and asks it for the
// diagnostics of a file after an edit. That catches what a syntax check
// cannot — type errors, wrong imports, calls to things that do not exist —
// the way OpenCode and Claude Code feed LSP errors back to the model.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var debug = os.Getenv("AGENTIUM_LSP_DEBUG") != ""

// Spec describes a language server.
type Spec struct {
	Name     string
	Command  []string
	Language string // LSP languageId
}

// specs maps file extensions to candidate servers, first installed wins.
var specs = map[string][]Spec{
	".go":  {{"gopls", []string{"gopls"}, "go"}},
	".py":  {{"pyright", []string{"pyright-langserver", "--stdio"}, "python"}, {"basedpyright", []string{"basedpyright-langserver", "--stdio"}, "python"}, {"pylsp", []string{"pylsp"}, "python"}},
	".ts":  {{"typescript", []string{"typescript-language-server", "--stdio"}, "typescript"}},
	".tsx": {{"typescript", []string{"typescript-language-server", "--stdio"}, "typescriptreact"}},
	".js":  {{"typescript", []string{"typescript-language-server", "--stdio"}, "javascript"}},
	".jsx": {{"typescript", []string{"typescript-language-server", "--stdio"}, "javascriptreact"}},
	".rs":  {{"rust-analyzer", []string{"rust-analyzer"}, "rust"}},
	".c":   {{"clangd", []string{"clangd"}, "c"}},
	".h":   {{"clangd", []string{"clangd"}, "c"}},
	".cc":  {{"clangd", []string{"clangd"}, "cpp"}},
	".cpp": {{"clangd", []string{"clangd"}, "cpp"}},
	".hpp": {{"clangd", []string{"clangd"}, "cpp"}},
}

// Manager starts servers lazily, one per kind, and stops them at Close.
type Manager struct {
	Root string
	// Env is the environment for servers (credentials removed by caller).
	Env []string

	mu      sync.Mutex
	servers map[string]*server // by spec name; nil entry = unavailable
}

// NewManager returns a manager for a workspace.
func NewManager(root string, env []string) *Manager {
	return &Manager{Root: root, Env: env, servers: map[string]*server{}}
}

const (
	firstWait = 10 * time.Second // a server just started indexes the project
	checkWait = 3 * time.Second
	quiet     = 350 * time.Millisecond // servers often publish twice
	maxShown  = 10
)

// Check sends the new content of path to its language server and returns
// a short report of errors ("" when there is no server, it is too slow,
// or the file is clean).
func (m *Manager) Check(ctx context.Context, path string, content []byte) string {
	if m == nil {
		return ""
	}
	sp, ok := m.spec(path)
	if !ok {
		return ""
	}
	s, fresh, err := m.server(sp)
	if err != nil || s == nil {
		return ""
	}
	wait := checkWait
	if fresh {
		wait = firstWait
	}
	diags, ok := s.diagnose(ctx, path, sp.Language, string(content), wait)
	if !ok {
		return ""
	}
	return format(path, m.Root, sp.Name, diags)
}

func (m *Manager) spec(path string) (Spec, bool) {
	for _, sp := range specs[strings.ToLower(filepath.Ext(path))] {
		if _, err := exec.LookPath(sp.Command[0]); err == nil {
			return sp, true
		}
	}
	return Spec{}, false
}

func (m *Manager) server(sp Spec) (*server, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.servers[sp.Name]; ok {
		if s == nil || s.dead() {
			return nil, false, errors.New("unavailable")
		}
		return s, false, nil
	}
	s, err := start(sp, m.Root, m.Env)
	m.servers[sp.Name] = s // nil on failure: don't retry every edit
	return s, err == nil, err
}

// Close shuts every server down.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if s != nil {
			s.close()
		}
	}
}

// Diagnostic is one LSP diagnostic.
type Diagnostic struct {
	Range struct {
		Start struct{ Line, Character int } `json:"start"`
	} `json:"range"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
}

func format(path, root, server string, ds []Diagnostic) string {
	var errs []Diagnostic
	for _, d := range ds {
		if d.Severity == 1 || d.Severity == 0 {
			errs = append(errs, d)
		}
	}
	if len(errs) == 0 {
		return ""
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Range.Start.Line < errs[j].Range.Start.Line })
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n%s reports %d error(s) in %s:", server, len(errs), rel)
	for i, d := range errs {
		if i == maxShown {
			fmt.Fprintf(&sb, "\n  … %d more", len(errs)-maxShown)
			break
		}
		msg := strings.Join(strings.Fields(d.Message), " ")
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		fmt.Fprintf(&sb, "\n  %d:%d: %s", d.Range.Start.Line+1, d.Range.Start.Character+1, msg)
	}
	return sb.String()
}

// server is one running language server.
type server struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	wmu    sync.Mutex
	nextID int

	mu       sync.Mutex
	pending  map[int]chan json.RawMessage
	versions map[string]int           // open documents
	diags    map[string][]Diagnostic  // latest per URI
	notify   map[string]chan struct{} // closed on the next publish for a URI
	done     chan struct{}
}

func start(sp Spec, root string, env []string) (*server, error) {
	cmd := exec.Command(sp.Command[0], sp.Command[1:]...)
	cmd.Dir = root
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // discarded: servers are chatty
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{cmd: cmd, in: in, pending: map[int]chan json.RawMessage{}, versions: map[string]int{},
		diags: map[string][]Diagnostic{}, notify: map[string]chan struct{}{}, done: make(chan struct{})}
	go s.readLoop(bufio.NewReader(out))
	go func() { cmd.Wait(); close(s.done) }()

	rootURI := uri(root)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = s.call(ctx, "initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": rootURI, "rootPath": root,
		"workspaceFolders": []map[string]string{{"uri": rootURI, "name": filepath.Base(root)}},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{"relatedInformation": false},
			},
			// Advertising workspace configuration or folders makes pyright wait
			// for exchanges that never come; it then publishes nothing.
			"workspace": map[string]any{"configuration": false, "workspaceFolders": false},
		},
	})
	if err != nil {
		s.close()
		return nil, err
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	return s, nil
}

func uri(p string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String()
}

func (s *server) dead() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *server) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if debug {
		fmt.Fprintf(os.Stderr, "%s lsp -> %.120s\n", time.Now().Format("05.000"), b)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = fmt.Fprintf(s.in, "Content-Length: %d\r\n\r\n%s", len(b), b)
	return err
}

func (s *server) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		return nil, errors.New("language server exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) readLoop(r *bufio.Reader) {
	for {
		n := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
				n, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if n < 0 || n > 64<<20 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		if debug {
			fmt.Fprintf(os.Stderr, "%s lsp <- %.200s\n", time.Now().Format("05.000"), body)
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			s.answer(msg.ID, msg.Method, msg.Params)
		case msg.Method == "textDocument/publishDiagnostics":
			var p struct {
				URI         string       `json:"uri"`
				Version     *int         `json:"version"`
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				s.mu.Lock()
				if p.Version != nil && *p.Version < s.versions[p.URI] {
					// A late publish for an older version of the file: its
					// errors may be ones the latest edit fixed.
					s.mu.Unlock()
					continue
				}
				s.diags[p.URI] = p.Diagnostics
				if ch, ok := s.notify[p.URI]; ok {
					close(ch)
					delete(s.notify, p.URI)
				}
				s.mu.Unlock()
			}
		case len(msg.ID) > 0:
			id, _ := strconv.Atoi(string(msg.ID))
			s.mu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- msg.Result
			}
		}
	}
}

// answer replies to requests servers send to the client; servers stall
// when these go unanswered.
func (s *server) answer(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	if method == "workspace/configuration" {
		var p struct {
			Items []any `json:"items"`
		}
		json.Unmarshal(params, &p)
		result = make([]any, len(p.Items))
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// diagnose syncs the document and waits for the server's diagnostics.
func (s *server) diagnose(ctx context.Context, path, lang, text string, wait time.Duration) ([]Diagnostic, bool) {
	u := uri(path)
	ch := make(chan struct{})
	s.mu.Lock()
	s.notify[u] = ch
	v, open := s.versions[u]
	v++
	s.versions[u] = v
	delete(s.diags, u) // results for the previous text no longer apply
	s.mu.Unlock()
	if !open {
		s.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
			"textDocument": map[string]any{"uri": u, "languageId": lang, "version": v, "text": text}}})
	} else {
		s.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange", "params": map[string]any{
			"textDocument":   map[string]any{"uri": u, "version": v},
			"contentChanges": []map[string]any{{"text": text}}}})
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didSave", "params": map[string]any{
		"textDocument": map[string]any{"uri": u}}})
	timer := time.NewTimer(wait)
	defer timer.Stop()
wait:
	for {
		select {
		case <-ch:
			break wait
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		case <-s.done:
			return nil, false
		}
	}
	// Let a second, fuller publish arrive (servers often send a quick
	// syntax pass, then the type check).
	for {
		next := make(chan struct{})
		s.mu.Lock()
		s.notify[u] = next
		s.mu.Unlock()
		select {
		case <-next:
			continue
		case <-time.After(quiet):
		case <-timer.C:
		}
		break
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.notify, u)
	return append([]Diagnostic(nil), s.diags[u]...), true
}

func (s *server) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !s.dead() {
		s.call(ctx, "shutdown", nil)
		s.send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		if s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}
	}
}
