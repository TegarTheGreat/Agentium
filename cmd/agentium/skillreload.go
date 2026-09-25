package main

import (
	"strings"

	"github.com/tegarthegreat/agentium/internal/skill"
)

// skillDiff names the skills that appeared and went away.
func skillDiff(old, cur []skill.Skill) (added, removed []string) {
	had := map[string]bool{}
	for _, s := range old {
		had[s.Name] = true
	}
	has := map[string]bool{}
	for _, s := range cur {
		has[s.Name] = true
		if !had[s.Name] {
			added = append(added, "/"+s.Name)
		}
	}
	for _, s := range old {
		if !has[s.Name] {
			removed = append(removed, "/"+s.Name)
		}
	}
	return added, removed
}

func skillChangeNote(added, removed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "new "+strings.Join(added, " "))
	}
	if len(removed) > 0 {
		parts = append(parts, "gone "+strings.Join(removed, " "))
	}
	if len(parts) == 0 {
		return "skills updated"
	}
	return sanitize("skills updated: " + strings.Join(parts, " · "))
}

// skillKey identifies the skill set: names, descriptions and places.
func skillKey(ss []skill.Skill) string {
	var sb strings.Builder
	for _, s := range ss {
		sb.WriteString(s.Name + "\x00" + s.Description + "\x00" + s.Path + "\x01")
	}
	return sb.String()
}
