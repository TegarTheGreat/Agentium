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
		{"bash: ./go test", "bash=./go test"},
		{"bash: LD_PRELOAD=/tmp/e.so go test", "bash=LD_PRELOAD=/tmp/e.so go test"},
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

func TestAlwaysScopeRiskyIsExact(t *testing.T) {
	if k, _ := alwaysScope("bash: git push --force origin main", "force push", "/w"); k != "bash=git push --force origin main" {
		t.Fatalf("risky command scoped too wide: %q", k)
	}
}

func TestBubbleWordWrap(t *testing.T) {
	got := wordWrap("Use the task tool twice in parallel: one sub-agent creates hello.py", 20)
	for _, r := range got {
		if strWidth(r) > 20 {
			t.Fatalf("row too wide: %q", r)
		}
	}
	if strings.Join(got, " ") != "Use the task tool twice in parallel: one sub-agent creates hello.py" {
		t.Fatalf("words lost or split: %q", got)
	}
	if got := wordWrap(strings.Repeat("x", 25), 10); len(got) != 3 || got[0] != strings.Repeat("x", 10) {
		t.Fatalf("long word: %q", got)
	}
}

func TestVtermBasics(t *testing.T) {
	v := newVterm(20)
	v.Write([]byte("hello\r\nworld\x1b[1A\r\x1b[2Kbye\n\x1b[31mred\x1b[0m"))
	if v.plain(0) != "bye" || v.plain(1) != "redld" || v.end() != 2 { // "red" overwrites "wor"
		t.Fatalf("got %q %q end=%d", v.plain(0), v.plain(1), v.end())
	}
	if !strings.Contains(v.render(1, 20), "\x1b[0;31mred") {
		t.Fatalf("color lost: %q", v.render(1, 20))
	}
	v.Write([]byte("\r\n" + strings.Repeat("ab", 12))) // wraps at 20
	if v.plain(2) != strings.Repeat("ab", 10) || v.plain(3) != "abab" {
		t.Fatalf("wrap: %q %q", v.plain(2), v.plain(3))
	}
	v.Write([]byte("\x1b[3")) // split escape sequence
	v.Write([]byte("2mG\xe4"))
	v.Write([]byte("\xb8\xad"))
	if v.plain(3) != "ababG中" {
		t.Fatalf("split sequences: %q", v.plain(3))
	}
}
