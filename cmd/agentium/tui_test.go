package main

import (
	"strings"
	"testing"
)

func TestApprovalTitle(t *testing.T) {
	for _, c := range []struct{ action, reason, title, what string }{
		{"bash: rm -rf x", "ask mode", "Run this command?", "rm -rf x"},
		{"write: /a/b.go", "ask mode", "Change this file?", "/a/b.go"},
		{"network: npm i", "needs network access", "Allow network access for this command?", "npm i"},
		{"bash: sudo ls", "privilege escalation", "Run this command?  privilege escalation", "sudo ls"},
	} {
		title, what := approvalTitle(c.action, c.reason)
		if title != c.title || what != c.what {
			t.Errorf("%q: got %q / %q", c.action, title, what)
		}
	}
}

func TestLiveToolFeed(t *testing.T) {
	var lt liveTool
	lt.feed([]byte("one\ntwo\n\x1b[32mthree\x1b[0m\nprogress 10%\rprogress 90%"))
	got := strings.Join(lt.lines(), "|")
	if got != "two|three|progress 90%" {
		t.Fatalf("got %q", got)
	}
}

func TestStrWidthIgnoresANSI(t *testing.T) {
	if w := strWidth("\x1b[36m❯\x1b[0m ab"); w != 4 {
		t.Fatalf("width %d", w)
	}
	if s := truncate("abcdefgh", 5); s != "abcd…" || strWidth(s) != 5 {
		t.Fatalf("truncate %q", s)
	}
}
