package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var userHome = os.UserHomeDir

const (
	readMaxLines = 2000
	readMaxBytes = 60 * 1024
)

var readTool = Tool{
	Def: providerDef("read",
		"Read a text file (or list a directory). Call several in parallel for several files.",
		`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"1-based start line"},"limit":{"type":"integer"}},"required":["path"]}`),
	Run: func(_ context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		if a.Path == "" {
			return "", errors.New("path is required")
		}
		p := env.abs(a.Path)
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if st.IsDir() {
			return listDir(p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		env.markSeen(p)
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return fmt.Sprintf("(binary file, %d bytes)", len(b)), nil
		}
		return sliceLines(string(b), a.Offset, a.Limit), nil
	},
}

func listDir(p string) (string, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, e := range ents {
		if i >= 500 {
			fmt.Fprintf(&sb, "[... %d more entries]\n", len(ents)-i)
			break
		}
		sb.WriteString(e.Name())
		if e.IsDir() {
			sb.WriteByte('/')
		}
		sb.WriteByte('\n')
	}
	if sb.Len() == 0 {
		return "(empty directory)", nil
	}
	return sb.String(), nil
}

func sliceLines(s string, offset, limit int) string {
	if s == "" {
		return "(empty file)"
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	start := 0
	if offset > 1 {
		start = offset - 1
	}
	if start >= total {
		return fmt.Sprintf("(offset %d is past end of file: %d lines)", offset, total)
	}
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	if end-start > readMaxLines {
		end = start + readMaxLines
	}
	var sb strings.Builder
	for i := start; i < end; i++ {
		if sb.Len()+len(lines[i]) > readMaxBytes {
			end = i
			break
		}
		sb.WriteString(lines[i])
	}
	out := sb.String()
	if start > 0 || end < total {
		out += fmt.Sprintf("\n[lines %d-%d of %d; use offset to read more]", start+1, end, total)
	}
	return out
}

var editTool = Tool{
	Def: providerDef("edit",
		"Replace exact text in a file. old must match exactly once unless all=true. Empty old writes the whole file (creates dirs).",
		`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"all":{"type":"boolean"}},"required":["path","new"]}`),
	Run: func(_ context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Path string `json:"path"`
			Old  string `json:"old"`
			New  string `json:"new"`
			All  bool   `json:"all"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		if a.Path == "" {
			return "", errors.New("path is required")
		}
		p := env.abs(a.Path)
		if env.Gate != nil {
			if ok, why := env.Gate.Write(p); !ok {
				return "", fmt.Errorf("denied (%s); choose another approach or ask the user", why)
			}
		}
		unlock := env.lock(p)
		defer unlock()

		seen, stale := env.freshness(p)
		_, statErr := os.Stat(p)
		exists := statErr == nil
		if exists && stale {
			return "", errors.New("file changed on disk since you read it; read it again before editing")
		}
		if a.Old == "" && exists && !seen {
			return "", errors.New("file exists: read it first, or pass old to change part of it")
		}
		env.mutate()

		if a.Old == "" {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			perm := fs.FileMode(0o644)
			if st, err := os.Stat(p); err == nil {
				perm = st.Mode().Perm()
			}
			if err := os.WriteFile(p, []byte(a.New), perm); err != nil {
				return "", err
			}
			env.markSeen(p)
			return fmt.Sprintf("wrote %s (%d lines)", a.Path, strings.Count(a.New, "\n")+1), nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		s := string(b)
		n := strings.Count(s, a.Old)
		switch {
		case n == 0:
			return "", errors.New("old text not found; read the file again and copy it exactly")
		case n > 1 && !a.All:
			return "", fmt.Errorf("old text matches %d times; add surrounding context or set all=true", n)
		}
		if a.All {
			s = strings.ReplaceAll(s, a.Old, a.New)
		} else {
			s = strings.Replace(s, a.Old, a.New, 1)
		}
		st, _ := os.Stat(p)
		if err := os.WriteFile(p, []byte(s), st.Mode().Perm()); err != nil {
			return "", err
		}
		env.markSeen(p)
		if !a.All {
			n = 1
		}
		return fmt.Sprintf("edited %s (%d replaced)", a.Path, n), nil
	},
}
