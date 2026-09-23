package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/skill"
)

// builtinSlash are REPL commands that a skill cannot shadow.
var builtinSlash = map[string]bool{
	"exit": true, "quit": true, "q": true, "clear": true, "new": true, "model": true, "mode": true,
	"usage": true, "undo": true, "sessions": true, "resume": true, "plan": true, "go": true, "skills": true, "help": true,
}

// skillCall reports whether a REPL line invokes a skill.
func skillCall(skills []skill.Skill, line string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	name = strings.ToLower(name)
	_, ok := skill.Find(skills, name)
	return strings.HasPrefix(line, "/") && ok && !builtinSlash[name]
}

func printSkills(skills []skill.Skill) {
	if len(skills) == 0 {
		fmt.Fprintf(os.Stderr, "no skills. Add one with `agentium skills add <dir|git-url|owner/repo>` or put <name>/SKILL.md in .agentium/skills\n")
		return
	}
	for _, s := range skills {
		fmt.Fprintf(os.Stderr, "/%-20s %-7s %s\n", s.Name, s.Source, oneLine(s.Description, 70))
	}
}

func cmdSkills(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home := config.Home()
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		printSkills(skill.Discover(home, cwd))
		return nil
	case "show":
		if len(args) != 1 {
			return errors.New("usage: agentium skills show <name>")
		}
		s, ok := skill.Find(skill.Discover(home, cwd), strings.ToLower(args[0]))
		if !ok {
			return fmt.Errorf("no skill %q", args[0])
		}
		b, err := os.ReadFile(s.Path)
		if err != nil {
			return err
		}
		fmt.Printf("# %s (%s) — %s\n", s.Name, s.Source, s.Path)
		if o := skill.Origin(s); o != "" {
			fmt.Printf("# from %s\n", o)
		}
		if sc := skill.Scripts(s.Dir()); len(sc) > 0 {
			fmt.Printf("# scripts: %s\n", strings.Join(sc, ", "))
		}
		fmt.Println()
		fmt.Print(string(b))
		return nil
	case "remove", "rm":
		if len(args) != 1 {
			return errors.New("usage: agentium skills remove <name>")
		}
		if err := skill.Remove(home, strings.ToLower(args[0])); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "removed", args[0])
		return nil
	case "add", "install":
		return skillsAdd(home, args)
	}
	return errors.New("usage: agentium skills [list | show <name> | add <dir|git-url|owner/repo[#ref]> [--yes] | remove <name>]")
}

// skillsAdd fetches a source, shows what it contains, and installs only
// after the user has reviewed it. A skill is instructions plus optional
// scripts the agent may run, so it is treated like code from the internet.
func skillsAdd(home string, args []string) error {
	yes := false
	var src string
	var only []string
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case src == "":
			src = a
		default:
			only = append(only, strings.ToLower(a))
		}
	}
	if src == "" {
		return errors.New("usage: agentium skills add <dir|git-url|owner/repo[#ref]> [name...] [--yes]")
	}
	fmt.Fprintln(os.Stderr, "fetching", src, "…")
	st, err := skill.Stage(src)
	if err != nil {
		return err
	}
	defer st.Close()
	found := st.Found()
	if len(only) > 0 {
		var keep []skill.Skill
		for _, s := range found {
			for _, n := range only {
				if s.Name == n {
					keep = append(keep, s)
				}
			}
		}
		found = keep
	}
	if len(found) == 0 {
		return fmt.Errorf("no SKILL.md found in %s", src)
	}
	if st.Commit != "" {
		fmt.Fprintf(os.Stderr, "source %s @ %s (pinned)\n", st.Source, st.Commit[:12])
	}
	for _, s := range found {
		fmt.Fprintf(os.Stderr, "\n  %s — %s\n", s.Name, oneLine(s.Description, 100))
		if sc := skill.Scripts(s.Dir()); len(sc) > 0 {
			fmt.Fprintf(os.Stderr, "  ! contains scripts the agent may run: %s\n", strings.Join(sc, ", "))
		}
	}
	fmt.Fprintln(os.Stderr, "\nSkills are instructions the agent follows. Review them first: `agentium skills show <name>` after install, or read the source.")
	if !yes {
		if !isTTY(os.Stdin) {
			return errors.New("not a terminal: pass --yes to install")
		}
		fmt.Fprintf(os.Stderr, "install %d skill%s? [y/N] ", len(found), plural(len(found)))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "nothing installed")
			return nil
		}
	}
	var failed int
	for _, s := range found {
		dst, err := skill.Install(home, st, s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", s.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(os.Stderr, "  installed /%s → %s\n", s.Name, dst)
	}
	if failed > 0 {
		return fmt.Errorf("%d skill%s not installed", failed, plural(failed))
	}
	return nil
}
