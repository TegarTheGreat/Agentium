package tool

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tegarthegreat/agentium/internal/codemap"
)

const (
	outlineMaxFiles = 400
	outlineMaxBytes = 48 * 1024
	outlineMaxFile  = 2 * 1024 * 1024 // skip generated giants
)

// sourceFiles lists files under dir that codemap understands, honouring
// .gitignore when ripgrep is available.
func sourceFiles(ctx context.Context, dir string) []string {
	var files []string
	if rg := ripgrep(); rg != "" {
		args := []string{"--files", "--hidden", "-g", "!.git/"}
		for _, s := range secretGlobs {
			args = append(args, "-g", "!"+s)
		}
		cmd := exec.CommandContext(ctx, rg, append(args, dir)...)
		out, _ := cmd.Output()
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" && codemap.Supported(l) {
				files = append(files, l)
			}
		}
	} else {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				return nil
			}
			if d.IsDir() {
				if p != dir && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if codemap.Supported(p) {
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

func readSource(p string) ([]byte, bool) {
	st, err := os.Stat(p)
	if err != nil || st.Size() > outlineMaxFile {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return nil, false
	}
	return b, true
}

// outlineFile renders one file's outline.
func outlineFile(p string) (string, error) {
	if !codemap.Supported(p) {
		return "", fmt.Errorf("no outline for %s files; read it instead", filepath.Ext(p))
	}
	b, ok := readSource(p)
	if !ok {
		return "", fmt.Errorf("cannot outline %s (binary or too large)", p)
	}
	syms := codemap.Outline(p, b)
	lines := bytes.Count(b, []byte("\n")) + 1
	if len(syms) == 0 {
		return fmt.Sprintf("(%d lines, no definitions found)", lines), nil
	}
	return fmt.Sprintf("(%d lines; read with offset/limit for bodies)\n%s", lines, codemap.Format(syms)), nil
}

// outlineDir renders a map of every source file below dir.
func outlineDir(ctx context.Context, root, dir string) string {
	files := sourceFiles(ctx, dir)
	var sb strings.Builder
	for i, f := range files {
		if i >= outlineMaxFiles || sb.Len() > outlineMaxBytes {
			fmt.Fprintf(&sb, "[... %d more files; outline a subdirectory]\n", len(files)-i)
			break
		}
		b, ok := readSource(f)
		if !ok {
			continue
		}
		syms := codemap.Outline(f, b)
		rel, _ := filepath.Rel(root, f)
		fmt.Fprintf(&sb, "== %s\n", rel)
		// Top-level definitions only keep a directory map compact.
		var top []codemap.Symbol
		for _, s := range syms {
			if s.Depth == 0 {
				top = append(top, s)
			}
		}
		sb.WriteString(codemap.Format(top))
	}
	if sb.Len() == 0 {
		return "(no source files)"
	}
	return sb.String()
}

// findSymbol returns the definitions named query ("Name" or "Type.Name").
func findSymbol(ctx context.Context, root, dir, query string) string {
	name := query
	if _, n, ok := strings.Cut(query, "."); ok {
		name = n
	}
	needle := []byte(name)
	var out []string
	for _, f := range sourceFiles(ctx, dir) {
		if ctx.Err() != nil {
			break
		}
		b, ok := readSource(f)
		if !ok || !bytes.Contains(b, needle) {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		for _, s := range codemap.Outline(f, b) {
			if s.Match(query) {
				out = append(out, fmt.Sprintf("%s:%d: %s", rel, s.Line, s.Sig))
			}
		}
		if len(out) > searchMaxLines {
			break
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("(no definition of %s; try search with a pattern)", query)
	}
	return capLines(strings.Join(out, "\n"), searchMaxLines)
}
