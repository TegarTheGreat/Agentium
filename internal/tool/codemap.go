package tool

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tegarthegreat/agentium/internal/codemap"
)

const (
	outlineMaxFiles = 400
	outlineMaxBytes = 48 * 1024
	outlineMaxFile  = 2 * 1024 * 1024 // skip generated giants
)

// listFiles lists files under dir (absolute paths, sorted), honouring
// .gitignore when ripgrep is available. keep filters by path.
func listFiles(ctx context.Context, dir string, keep func(string) bool) []string {
	var files []string
	if rg := ripgrep(); rg != "" {
		args := []string{"--files", "--hidden", "-g", "!.git/"}
		for _, s := range secretGlobs {
			args = append(args, "-g", "!"+s)
		}
		cmd := exec.CommandContext(ctx, rg, append(args, dir)...)
		out, _ := cmd.Output()
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" && keep(l) {
				files = append(files, l)
			}
		}
	} else {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				return nil
			}
			if d.IsDir() {
				if p != dir && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			switch d.Name() {
			case ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".env", ".env.local", ".env.production", ".env.development":
				return nil
			}
			if keep(p) {
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

// sourceFiles lists files under dir that codemap understands.
func sourceFiles(ctx context.Context, dir string) []string {
	return listFiles(ctx, dir, codemap.Supported)
}

// codeIndex returns an up-to-date index covering dir and the path prefix
// of dir inside it. The workspace index is kept for the session and
// cached on disk; a directory outside the workspace gets a throwaway one.
func (e *Env) codeIndex(ctx context.Context, dir string) (*codemap.Index, string) {
	if Outside(e.Root, dir) {
		ix := codemap.LoadIndex("", dir)
		ix.Update(sourceFiles(ctx, dir))
		return ix, ""
	}
	e.mu.Lock()
	if e.cix == nil {
		e.cix = codemap.LoadIndex(e.CodeCache, e.Root)
	}
	ix := e.cix
	e.mu.Unlock()
	e.cixMu.Lock()
	defer e.cixMu.Unlock()
	if ix.Update(sourceFiles(ctx, e.Root)) && e.CodeCache != "" {
		_ = ix.Save(e.CodeCache)
	}
	prefix, _ := filepath.Rel(e.Root, dir)
	if prefix == "." {
		prefix = ""
	}
	return ix, prefix
}

// Outside reports whether p is outside root.
func Outside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func underPrefix(rel, prefix string) bool {
	return prefix == "" || rel == prefix || strings.HasPrefix(rel, prefix+string(filepath.Separator))
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

// mapBudget caps a directory outline (~2k tokens). Anything bigger is
// shown as a ranked map: the most depended-on files, and those touched
// this session, first.
const mapBudget = 8 * 1024

// outlineDir renders a map of the source files below dir: the full
// top-level outline when it fits the budget, else a ranked repo map.
func (e *Env) outlineDir(ctx context.Context, dir string) string {
	ix, prefix := e.codeIndex(ctx, dir)
	var full strings.Builder
	files := 0
	ix.Each(func(rel string, f *codemap.FileEntry) {
		if !underPrefix(rel, prefix) || full.Len() > mapBudget {
			return
		}
		files++
		fmt.Fprintf(&full, "== %s (%d lines)\n", display(ix.Root, rel, e.Root), f.Lines)
		var top []codemap.Symbol
		for _, s := range f.Syms {
			if s.Depth == 0 {
				top = append(top, s)
			}
		}
		full.WriteString(codemap.Format(top))
	})
	if files == 0 {
		return "(no source files)"
	}
	if full.Len() <= mapBudget {
		return full.String()
	}
	return e.rankedMap(ix, prefix)
}

func (e *Env) rankedMap(ix *codemap.Index, prefix string) string {
	focus := map[string]bool{}
	e.mu.Lock()
	for p := range e.seen {
		if rel, err := filepath.Rel(ix.Root, p); err == nil && !strings.HasPrefix(rel, "..") {
			focus[rel] = true
		}
	}
	e.mu.Unlock()
	ranked := ix.Rank(prefix, focus)
	var sb strings.Builder
	fmt.Fprintf(&sb, "(%d source files, ranked: most depended-on and recently touched first; top definitions only. Outline a file or subdirectory for more.)\n", len(ranked))
	shown := 0
	for _, r := range ranked {
		var syms []codemap.Symbol
		for _, s := range r.Syms {
			if s.Depth <= 1 && len(syms) < 6 {
				syms = append(syms, s)
			}
		}
		sort.Slice(syms, func(a, b int) bool { return syms[a].Line < syms[b].Line })
		for i := range syms {
			syms[i].Depth = 0
		}
		block := fmt.Sprintf("== %s (%d lines)\n%s", display(ix.Root, r.Rel, e.Root), r.File.Lines, codemap.Format(syms))
		if sb.Len()+len(block) > mapBudget {
			if sb.Len()+len(r.Rel)+4 > mapBudget+1024 {
				break
			}
			continue // a smaller file further down may still fit
		}
		sb.WriteString(block)
		shown++
	}
	if rest := len(ranked) - shown; rest > 0 {
		fmt.Fprintf(&sb, "[... %d more files]\n", rest)
	}
	return sb.String()
}

// display shows rel (relative to indexRoot) relative to the workspace.
func display(indexRoot, rel, root string) string {
	if r, err := filepath.Rel(root, filepath.Join(indexRoot, rel)); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return filepath.Join(indexRoot, rel)
}

// findSymbol returns the definitions named query ("Name" or "Type.Name").
func (e *Env) findSymbol(ctx context.Context, dir, query string) string {
	ix, prefix := e.codeIndex(ctx, dir)
	var out []string
	ix.Each(func(rel string, f *codemap.FileEntry) {
		if !underPrefix(rel, prefix) || len(out) > searchMaxLines {
			return
		}
		for _, s := range f.Syms {
			if s.Match(query) {
				out = append(out, fmt.Sprintf("%s:%d: %s", display(ix.Root, rel, e.Root), s.Line, s.Sig))
			}
		}
	})
	if len(out) == 0 {
		return fmt.Sprintf("(no definition of %s; try search with a pattern)", query)
	}
	return capLines(strings.Join(out, "\n"), searchMaxLines)
}

const refsMax = 40

// findRefs lists where name is used (definitions excluded), each with the
// enclosing function or type, so the model sees callers without reading
// whole files.
func (e *Env) findRefs(ctx context.Context, dir, query string) string {
	name := query
	if _, n, ok := strings.Cut(query, "."); ok {
		name = n
	}
	ix, prefix := e.codeIndex(ctx, dir)
	word := regexp.MustCompile(`(?:^|[^\w$])` + regexp.QuoteMeta(name) + `(?:[^\w$]|$)`)
	var out []string
	files, count := 0, 0
	ix.Each(func(rel string, f *codemap.FileEntry) {
		if ctx.Err() != nil || !underPrefix(rel, prefix) || !f.Uses(name) {
			return
		}
		b, ok := readSource(filepath.Join(ix.Root, rel))
		if !ok {
			return
		}
		defs := map[int]bool{}
		for _, s := range f.Syms {
			if s.Name == name {
				defs[s.Line] = true
			}
		}
		hit := false
		for i, line := range strings.Split(string(b), "\n") {
			if defs[i+1] || !word.MatchString(line) {
				continue
			}
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") {
				continue // comments are not uses
			}
			count++
			hit = true
			if len(out) >= refsMax {
				continue
			}
			in := ""
			if s := f.Enclosing(i + 1); s != nil && s.Name != name {
				in = " [in " + s.Name + "]"
				if s.Recv != "" {
					in = " [in " + strings.TrimLeft(s.Recv, "*") + "." + s.Name + "]"
				}
			}
			if len(t) > 160 {
				t = t[:160] + "…"
			}
			out = append(out, fmt.Sprintf("%s:%d%s: %s", display(ix.Root, rel, e.Root), i+1, in, t))
		}
		if hit {
			files++
		}
	})
	if count == 0 {
		return fmt.Sprintf("(no references to %s)", name)
	}
	head := fmt.Sprintf("%d reference(s) in %d file(s)", count, files)
	if count > len(out) {
		head += fmt.Sprintf("; showing %d, narrow with path", len(out))
	}
	return head + "\n" + strings.Join(out, "\n")
}

const (
	treeMaxLines = 300
	treeDepth    = 2
)

// tree renders dir as an indented tree (gitignore-aware), depth-limited;
// deeper directories are collapsed to a file count.
func tree(ctx context.Context, dir string) (string, error) {
	files := listFiles(ctx, dir, func(string) bool { return true })
	if len(files) == 0 {
		return listDir(dir) // empty, or everything ignored: show raw entries
	}
	type node struct {
		kids  map[string]*node
		files int // files below
	}
	rootN := &node{kids: map[string]*node{}}
	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		n := rootN
		n.files++
		for i, p := range parts {
			if i == len(parts)-1 {
				if n.kids[p] == nil {
					n.kids[p] = nil
				}
				break
			}
			c := n.kids[p]
			if c == nil {
				c = &node{kids: map[string]*node{}}
				n.kids[p] = c
			}
			c.files++
			n = c
		}
	}
	render := func(depth int) []string {
		var lines []string
		var walk func(n *node, indent string, level int)
		walk = func(n *node, indent string, level int) {
			names := make([]string, 0, len(n.kids))
			for k := range n.kids {
				names = append(names, k)
			}
			// Directories first, then files, each alphabetical.
			sort.Slice(names, func(i, j int) bool {
				di, dj := n.kids[names[i]] != nil, n.kids[names[j]] != nil
				if di != dj {
					return di
				}
				return names[i] < names[j]
			})
			for _, k := range names {
				c := n.kids[k]
				if c == nil {
					lines = append(lines, indent+k)
					continue
				}
				if level >= depth {
					lines = append(lines, fmt.Sprintf("%s%s/ (%d files)", indent, k, c.files))
					continue
				}
				lines = append(lines, fmt.Sprintf("%s%s/", indent, k))
				walk(c, indent+"  ", level+1)
			}
		}
		walk(rootN, "", 1)
		return lines
	}
	var lines []string
	for d := treeDepth; d >= 1; d-- {
		if lines = render(d); len(lines) <= treeMaxLines {
			break
		}
	}
	extra := ""
	if len(lines) > treeMaxLines {
		extra = fmt.Sprintf("[... %d more entries; read a subdirectory]\n", len(lines)-treeMaxLines)
		lines = lines[:treeMaxLines]
	}
	return fmt.Sprintf("(%d files; ignored paths hidden; deeper levels collapsed)\n%s\n%s", rootN.files, strings.Join(lines, "\n"), extra), nil
}
