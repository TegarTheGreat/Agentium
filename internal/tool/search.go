package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tegarthegreat/agentium/internal/policy"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const searchMaxLines = 200

var searchTool = Tool{
	Def: providerDef("search",
		"Find files and text. pattern = regex to grep (file:line:text); omit pattern to list files matching glob; symbol = where a function/type/class is defined (Name or Type.Name); refs = where it is used, with the enclosing function; memory = past decisions, errors and sessions. Respects .gitignore.",
		`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string","description":"e.g. *.go or src/**/*.ts"},"ignore_case":{"type":"boolean"},"symbol":{"type":"string"},"refs":{"type":"string"},"memory":{"type":"string"}}}`),
	Run: func(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Pattern    string `json:"pattern"`
			Path       string `json:"path"`
			Glob       string `json:"glob"`
			IgnoreCase bool   `json:"ignore_case"`
			Symbol     string `json:"symbol"`
			Refs       string `json:"refs"`
			Memory     string `json:"memory"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		if a.Pattern == "" && a.Glob == "" && a.Symbol == "" && a.Refs == "" && a.Memory == "" {
			return "", errors.New("give pattern, glob, symbol, refs or memory")
		}
		if a.Memory != "" {
			if env.Recall == nil {
				return "(memory is off)", nil
			}
			return env.Recall(a.Memory), nil
		}
		dir := env.Root
		if a.Path != "" {
			dir = real(env.abs(a.Path))
		}
		if env.Gate != nil {
			if ok, why := env.Gate.Read(dir); !ok {
				return "", fmt.Errorf("denied (%s)", why)
			}
		}
		if a.Symbol != "" {
			return env.findSymbol(ctx, dir, a.Symbol), nil
		}
		if a.Refs != "" {
			return env.findRefs(ctx, dir, a.Refs), nil
		}
		if rg := ripgrep(); rg != "" {
			return runRipgrep(ctx, rg, env.Root, dir, a.Pattern, a.Glob, a.IgnoreCase)
		}
		return walkSearch(ctx, env.Root, dir, a.Pattern, a.Glob, a.IgnoreCase)
	},
}

var ripgrep = sync.OnceValue(func() string {
	p, _ := exec.LookPath("rg")
	return p
})

func runRipgrep(ctx context.Context, rg, root, dir, pattern, glob string, icase bool) (string, error) {
	// --hidden: dotfiles such as .github/ or .env.example matter in code work.
	args := []string{"--color=never", "--no-messages", "--max-columns=300", "--max-columns-preview", "--hidden", "-g", "!.git/"}
	for _, s := range secretGlobs {
		args = append(args, "-g", "!"+s)
	}
	if pattern == "" {
		args = append(args, "--files")
	} else {
		args = append(args, "-n", "--no-heading")
		if icase {
			args = append(args, "-i")
		}
	}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	if pattern != "" {
		args = append(args, "-e", pattern)
	}
	args = append(args, "--max-count", "200", dir) // one huge file cannot fill the results
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, rg, args...)
	cmd.Dir = root
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// Read as it comes and stop early: a pattern that matches millions of
	// lines must not be held in memory.
	var kept []string
	more, capped := 0, false
	perFile := map[string]int{}
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	prefix := strings.TrimRight(root, string(filepath.Separator)) + string(filepath.Separator)
	for sc.Scan() {
		l := sc.Text()
		path := l
		if i := strings.IndexByte(l, ':'); i > 0 {
			path = l[:i]
		}
		if policy.DotEnv(path) {
			continue
		}
		perFile[path]++
		if len(kept) < searchMaxLines {
			kept = append(kept, strings.ReplaceAll(l, prefix, ""))
			continue
		}
		if more++; more >= searchCountMax {
			capped = true
			cancel()
			break
		}
	}
	_, _ = io.Copy(io.Discard, pipe)
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case capped:
	case errors.As(err, &ee) && ee.ExitCode() == 1 && len(kept) == 0:
		return "(no matches)", nil
	case err != nil && len(kept) == 0:
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return "", fmt.Errorf("%s", s)
		}
		return "", err
	}
	if len(kept) == 0 {
		return "(no matches)", nil
	}
	out := strings.Join(kept, "\n")
	for f, n := range perFile {
		if n >= 200 && pattern != "" {
			out += "\n[" + strings.ReplaceAll(f, prefix, "") + " has more than 200 matches; narrow the search or read it]"
			break
		}
	}
	switch {
	case capped:
		out += fmt.Sprintf("\n[... over %d more; narrow the search]", searchCountMax)
	case more > 0:
		out += fmt.Sprintf("\n[... %d more; narrow the search]", more)
	}
	return out, nil
}

// searchCountMax is how many results past the shown ones are counted
// before the search stops.
const searchCountMax = 20000

func capLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "(no matches)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n[... %d more; narrow the search]", len(lines)-n)
}

// secretGlobs keep credential stores out of search results even when a
// search covers the home directory.
var secretGlobs = []string{".ssh/", ".aws/", ".gnupg/", ".kube/", ".docker/", ".netrc", ".npmrc", ".pypirc",
	".git-credentials", ".config/gcloud/", ".config/gh/", ".agentium/", ".env", ".env.local", ".env.production", ".env.development"}

var skipDirs = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true, ".kube": true, ".docker": true, ".agentium": true, ".git": true, ".hg": true, ".svn": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "dist": true, "build": true, "target": true, "__pycache__": true, ".next": true, ".cache": true}

// walkSearch is the fallback when ripgrep is not installed.
func walkSearch(ctx context.Context, root, dir, pattern, glob string, icase bool) (string, error) {
	var re *regexp.Regexp
	if pattern != "" {
		if icase {
			pattern = "(?i)" + pattern
		}
		var err error
		if re, err = regexp.Compile(pattern); err != nil {
			return "", err
		}
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		switch d.Name() {
		case ".netrc", ".npmrc", ".pypirc", ".git-credentials":
			return nil
		}
		if policy.DotEnv(d.Name()) {
			return nil
		}
		if glob != "" && !globMatch(glob, rel) {
			return nil
		}
		if re == nil {
			out = append(out, rel)
		} else {
			grepFile(p, rel, re, &out)
		}
		if len(out) > searchMaxLines*2 {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return "", err
	}
	return capLines(strings.Join(out, "\n"), searchMaxLines), nil
}

func globMatch(glob, rel string) bool {
	rel = filepath.ToSlash(rel)
	if !strings.Contains(glob, "/") {
		ok, _ := filepath.Match(glob, filepath.Base(rel))
		return ok
	}
	if strings.Contains(glob, "**") {
		parts := strings.SplitN(glob, "**", 2)
		if !strings.HasPrefix(rel, parts[0]) {
			return false
		}
		suffix := strings.TrimPrefix(parts[1], "/")
		ok, _ := filepath.Match(suffix, filepath.Base(rel))
		return ok
	}
	ok, _ := filepath.Match(glob, rel)
	return ok
}

func grepFile(p, rel string, re *regexp.Regexp, out *[]string) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if strings.IndexByte(line, 0) >= 0 {
			return // binary
		}
		if re.MatchString(line) {
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			*out = append(*out, fmt.Sprintf("%s:%d:%s", rel, n, line))
		}
	}
}
