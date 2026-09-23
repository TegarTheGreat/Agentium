// Package lint does fast syntax checks on file contents so an edit that
// breaks a file can be rejected before it lands (the SWE-agent ablation:
// 18.0% vs 10.3% solve rate with vs without this). Go and JSON are parsed
// in-process; Python, JavaScript and shell use their interpreters when
// installed. Unknown file types are not checked.
package lint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Result of a check.
type Result struct {
	Checked bool   // a checker exists for this file type
	OK      bool   // content parsed cleanly
	Msg     string // first error(s), when not OK
}

const timeout = 5 * time.Second

// Check parses content as the language implied by path.
func Check(path string, content []byte) Result {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return goCheck(path, content)
	case ".json":
		return jsonCheck(content)
	case ".py":
		return external(content, "", "python3", "-c", "import ast,sys; ast.parse(sys.stdin.read())")
	case ".sh", ".bash":
		return external(content, "", "bash", "-n")
	case ".js", ".mjs", ".cjs":
		return external(content, filepath.Ext(path), "node", "--check")
	}
	return Result{}
}

func goCheck(path string, content []byte) Result {
	_, err := parser.ParseFile(token.NewFileSet(), filepath.Base(path), content, parser.AllErrors|parser.SkipObjectResolution)
	if err == nil {
		return Result{Checked: true, OK: true}
	}
	var list scanner.ErrorList
	if errors.As(err, &list) && len(list) > 3 {
		list = list[:3]
		err = list
	}
	return Result{Checked: true, Msg: err.Error()}
}

func jsonCheck(content []byte) Result {
	if len(bytes.TrimSpace(content)) == 0 {
		return Result{Checked: true, OK: true}
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(content))
	err := dec.Decode(&v)
	if err == nil && dec.More() {
		err = errors.New("extra data after the top-level value")
	}
	if err == nil {
		return Result{Checked: true, OK: true}
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		line := bytes.Count(content[:min(int(se.Offset), len(content))], []byte("\n")) + 1
		return Result{Checked: true, Msg: fmt.Sprintf("line %d: %v", line, err)}
	}
	return Result{Checked: true, Msg: err.Error()}
}

var lookups sync.Map

func have(bin string) bool {
	if v, ok := lookups.Load(bin); ok {
		return v.(bool)
	}
	_, err := exec.LookPath(bin)
	lookups.Store(bin, err == nil)
	return err == nil
}

// external runs a checker. With ext == "" the content goes on stdin;
// otherwise it is written to a temp file with that extension.
func external(content []byte, ext, bin string, args ...string) Result {
	if !have(bin) {
		return Result{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	if ext == "" {
		cmd = exec.CommandContext(ctx, bin, args...)
		cmd.Stdin = bytes.NewReader(content)
	} else {
		f, err := os.CreateTemp("", "agentium-lint-*"+ext)
		if err != nil {
			return Result{}
		}
		defer os.Remove(f.Name())
		_, werr := f.Write(content)
		f.Close()
		if werr != nil {
			return Result{}
		}
		cmd = exec.CommandContext(ctx, bin, append(args, f.Name())...)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return Result{} // too slow: don't block the edit on the checker
	}
	if err == nil {
		return Result{Checked: true, OK: true}
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return Result{}
	}
	return Result{Checked: true, Msg: tidy(string(out))}
}

// tidy trims checker output to the useful part.
func tidy(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	// Python tracebacks: the last lines carry the location and message.
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	s = strings.Join(lines, "\n")
	if len(s) > 800 {
		s = s[:800] + "…"
	}
	return s
}
