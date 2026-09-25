package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPyrightDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("pyright-langserver"); err != nil {
		t.Skip("pyright not installed")
	}
	root := t.TempDir()
	p := filepath.Join(root, "app.py")
	good := []byte("def add(a: int, b: int) -> int:\n    return a + b\n\nprint(add(1, 2))\n")
	os.WriteFile(p, good, 0o644)
	m := NewManager(root, os.Environ())
	defer m.Close()
	if out := m.Check(context.Background(), p, good); out != "" {
		t.Fatalf("clean file reported: %q", out)
	}
	bad := []byte("def add(a: int, b: int) -> int:\n    return a + b\n\nprint(ad(1, 2))\nimport nosuchmodule_xyz\n")
	os.WriteFile(p, bad, 0o644)
	out := m.Check(context.Background(), p, bad)
	if !strings.Contains(out, "pyright reports") || !strings.Contains(out, "4:7") || !strings.Contains(out, "\"ad\" is not defined") {
		t.Fatalf("diagnostics: %q", out)
	}
	if m.Check(context.Background(), filepath.Join(root, "x.unknown"), nil) != "" {
		t.Fatal("unknown language must be silent")
	}
}

func TestGoplsDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(root, "util.go"), []byte("package x\n\nfunc Double(n int) int { return n * 2 }\n"), 0o644)
	p := filepath.Join(root, "main.go")
	// Syntactically fine, but a type error across files: only a type
	// checker sees it.
	bad := []byte("package x\n\nfunc Use() string { return Double(2) }\n")
	os.WriteFile(p, bad, 0o644)
	m := NewManager(root, os.Environ())
	defer m.Close()
	out := m.Check(context.Background(), p, bad)
	if !strings.Contains(out, "gopls reports") || !strings.Contains(out, "3:") {
		t.Fatalf("diagnostics: %q", out)
	}
}

func TestReportOnlyNewErrors(t *testing.T) {
	m := NewManager("/w", nil)
	d := func(line int, msg string) Diagnostic {
		var x Diagnostic
		x.Range.Start.Line, x.Severity, x.Message = line, 1, msg
		return x
	}
	first := m.report("/w/a.py", "pyright", []Diagnostic{d(10, `"batched" is unknown import symbol`)})
	if !strings.Contains(first, "batched") {
		t.Fatalf("first check lists standing errors: %q", first)
	}
	// Same standing error, moved by an edit: nothing new to say.
	if got := m.report("/w/a.py", "pyright", []Diagnostic{d(12, `"batched" is unknown import symbol`)}); got != "" {
		t.Fatalf("repeated a known error: %q", got)
	}
	got := m.report("/w/a.py", "pyright", []Diagnostic{d(12, `"batched" is unknown import symbol`), d(40, `"x" is not defined`)})
	if !strings.Contains(got, `"x" is not defined`) || strings.Contains(got, "batched") || !strings.Contains(got, "+1 error(s) reported earlier") {
		t.Fatalf("new error: %q", got)
	}
}
