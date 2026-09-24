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

func TestAlwaysScope(t *testing.T) {
	for _, c := range []struct{ action, key string }{
		{"bash: npm install three", "bash:npm"},
		{"network: FOO=1 /usr/bin/curl -sS x", "network=FOO=1 /usr/bin/curl -sS x"},
		{"bash: cd /w && go test ./...", "bash:go"},
		{"bash: cd /w && go test && rm -rf ~", "bash=go test && rm -rf ~"},
		{"bash: sudo apt install x", "bash=sudo apt install x"},
		{"bash: cd /other && ls", "bash=cd /other && ls"},
		{"bash: npm test | tee log", "bash=npm test | tee log"},
		{"write: /a/b.go", "write"},
		{"fetch: https://example.com/x?y", "fetch:example.com"},
	} {
		if k, _ := alwaysScope(c.action, "ask mode", "/w"); k != c.key {
			t.Errorf("%q: key %q, want %q", c.action, k, c.key)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.12.1", "0.12.0", true}, {"0.13.0", "0.12.9", true}, {"0.12.0", "0.12.0", false}, {"0.9.0", "0.12.0", false}, {"1.0.0", "0.99.9", true}} {
		if newer(c.a, c.b) != c.want {
			t.Errorf("newer(%s, %s) != %v", c.a, c.b, c.want)
		}
	}
	if !checksumListed("abc  agentium_linux_amd64.tar.gz\n", "agentium_linux_amd64.tar.gz", "ABC") {
		t.Error("checksum line not matched")
	}
}

func TestAlwaysScopeWriteOutside(t *testing.T) {
	if k, _ := alwaysScope("write: /home/u/.bashrc", "outside the workspace", "/w"); k != "write=/home/u/.bashrc" {
		t.Fatalf("outside write scoped too wide: %q", k)
	}
}
