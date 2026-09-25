package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFileSuggestionsLargeProject(t *testing.T) {
	c := &completer{listed: time.Now()}
	for i := 0; i < 60000; i++ {
		c.files = append(c.files, fmt.Sprintf("pkg/m%d/file%d.go", i%50, i))
	}
	c.files = append(c.files, "internal/zebra/handler.go") // past the old 20k cap
	got := c.fileSuggestions("zebhand")
	if len(got) == 0 || got[0].label != "handler.go" {
		t.Fatalf("got %v", got)
	}
	got = c.fileSuggestions("file1")
	if len(got) != 50 {
		t.Fatalf("got %d suggestions, want 50", len(got))
	}
	for i := 1; i < len(got); i++ {
		a, _ := fuzzyScore(strings.TrimSpace(got[i-1].insert[1:]), "file1")
		b, _ := fuzzyScore(strings.TrimSpace(got[i].insert[1:]), "file1")
		if a < b {
			t.Fatalf("not ranked: %q before %q", got[i-1].insert, got[i].insert)
		}
	}
}
