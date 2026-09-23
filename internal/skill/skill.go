// Package skill finds SKILL.md skills (the Agent Skills format shared with
// Claude Code and Codex) and installs them. Only a one-line index goes into
// the system prompt; the model reads a skill's SKILL.md when a task
// matches it, so unused skills cost almost nothing.
package skill

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Skill is one discovered skill.
type Skill struct {
	Name        string
	Description string
	Path        string // SKILL.md
	Source      string // "user" or "project"
}

// Dir is the skill's directory.
func (s Skill) Dir() string { return filepath.Dir(s.Path) }

const (
	maxDescription = 240
	maxIndex       = 4 * 1024
	maxBody        = 32 * 1024
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether name can be a skill (and slash command) name.
func ValidName(name string) bool { return validName.MatchString(name) }

// UserDir is where installed skills live.
func UserDir(home string) string { return filepath.Join(home, "skills") }

// Location is a directory that may hold skills.
type Location struct {
	Dir    string
	Source string // "user" or "project"
}

// Dirs returns the skill directories for a workspace, lowest priority
// first: user skills, then project skills from the repository root down
// to cwd. A later skill with the same name wins.
func Dirs(home, cwd string) []Location {
	var dirs []Location
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, Location{filepath.Join(h, ".claude", "skills"), "user"})
	}
	dirs = append(dirs, Location{UserDir(home), "user"})
	var chain []string
	for d := cwd; ; {
		chain = append([]string{d}, chain...)
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			break
		}
		p := filepath.Dir(d)
		if p == d {
			chain = []string{cwd} // not a repository: only cwd
			break
		}
		d = p
	}
	for _, d := range chain {
		dirs = append(dirs, Location{filepath.Join(d, ".claude", "skills"), "project"},
			Location{filepath.Join(d, ".agentium", "skills"), "project"})
	}
	return dirs
}

// Discover returns the skills visible from cwd, sorted by name.
func Discover(home, cwd string) []Skill {
	byName := map[string]Skill{}
	seen := map[string]bool{}
	for _, loc := range Dirs(home, cwd) {
		dir := loc.Dir
		if seen[dir] { // e.g. cwd is the home directory
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name(), "SKILL.md")
			s, err := Parse(p)
			if err != nil {
				continue
			}
			s.Source = loc.Source
			byName[s.Name] = s
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Parse reads a SKILL.md's frontmatter. The name defaults to the
// directory name.
func Parse(path string) (Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return Skill{}, err
	}
	defer f.Close()
	s := Skill{Path: path, Name: filepath.Base(filepath.Dir(path))}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	if sc.Scan() && strings.TrimSpace(sc.Text()) == "---" {
		key := "" // key of a multi-line (block or folded) value
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) == "---" {
				break
			}
			if key != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
				if key == "description" {
					s.Description += " " + strings.TrimSpace(line)
				}
				continue
			}
			key = ""
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if v == "" || v == ">" || v == "|" || v == ">-" || v == "|-" {
				key, v = k, ""
			}
			v = strings.Trim(v, `"'`)
			switch k {
			case "name":
				if v != "" {
					s.Name = v
				}
			case "description":
				s.Description = v
			}
		}
	}
	s.Name = strings.ToLower(s.Name)
	if !ValidName(s.Name) {
		return Skill{}, fmt.Errorf("%s: invalid skill name %q", path, s.Name)
	}
	s.Description = oneLine(s.Description, maxDescription)
	return s, nil
}

// Body returns SKILL.md without its frontmatter.
func (s Skill) Body() (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	t := string(b)
	if strings.HasPrefix(t, "---") {
		if i := strings.Index(t[3:], "\n---"); i >= 0 {
			t = t[3+i+4:]
		}
	}
	t = strings.TrimSpace(t)
	if len(t) > maxBody {
		t = t[:maxBody] + "\n[truncated]"
	}
	return t, nil
}

// Prompt is the index added to the system prompt; "" when there are no
// skills. It is stable for a session, so it stays in the cached prefix.
func Prompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nSkills (read the SKILL.md before doing a task that matches one; paths inside it are relative to its directory):")
	for _, s := range skills {
		line := fmt.Sprintf("\n- %s: %s (%s)", s.Name, s.Description, s.Path)
		if sb.Len()+len(line) > maxIndex {
			sb.WriteString("\n- … more skills: run `agentium skills` to list")
			break
		}
		sb.WriteString(line)
	}
	return sb.String()
}

// Find returns the skill called name.
func Find(skills []Skill, name string) (Skill, bool) {
	for _, s := range skills {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Invoke expands "/name rest" into a user message carrying the skill's
// instructions. ok is false when line does not name a skill.
func Invoke(skills []Skill, line string) (msg string, ok bool, err error) {
	if !strings.HasPrefix(line, "/") {
		return "", false, nil
	}
	name, rest, _ := strings.Cut(line[1:], " ")
	s, found := Find(skills, strings.ToLower(name))
	if !found {
		return "", false, nil
	}
	body, err := s.Body()
	if err != nil {
		return "", true, err
	}
	msg = fmt.Sprintf("<skill name=%q dir=%q>\n%s\n</skill>\n\nUse the skill above.", s.Name, s.Dir(), body)
	if rest = strings.TrimSpace(rest); rest != "" {
		msg += " Task: " + rest
	}
	return msg, true, nil
}

// Scripts lists files in a skill directory that can run code, for review
// before installing.
func Scripts(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil // symlinks are not installed
		}
		ext := strings.ToLower(filepath.Ext(p))
		if info.Mode()&0o111 != 0 || ext == ".sh" || ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".rb" || ext == ".pl" {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// ErrExists is returned when installing over an existing skill.
var ErrExists = errors.New("skill already installed (remove it first)")

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
