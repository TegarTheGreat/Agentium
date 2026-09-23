package main

import (
	"strings"
	"testing"
)

func render(chunks ...string) string {
	var sb strings.Builder
	m := newMD(&sb)
	for _, c := range chunks {
		m.Write(c)
	}
	m.Flush()
	return strings.NewReplacer(sgrReset, "</>", sgrBold, "<b>", sgrCyan, "<c>", sgrDim, "<d>").Replace(sb.String())
}

func TestMarkdownStream(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"Plain text, no marks."}, "Plain text, no marks.\n"},
		{[]string{"## Sum", "mary\nok"}, "</><b>Summary</>\nok\n"},
		{[]string{"- one\n", "* two **big** `x*y`\n"}, "• one\n• two </><b>big</> </><c>x*y</>\n"},
		{[]string{"```go\nfunc a() { *p = **q }\n```\nafter"}, "<d>```go</>\nfunc a() { *p = **q }\n<d>```</>\nafter\n"},
		{[]string{"> note\n---\n"}, "</><d>│ note</>\n<d>" + strings.Repeat("─", 40) + "</>\n"},
		{[]string{"a * b and snake_case_name"}, "a * b and snake_case_name\n"},
		{[]string{"-", "-", "flag is fine"}, "--flag is fine\n"},
		{[]string{"**unclosed bold\nnext"}, "</><b>unclosed bold</>\nnext\n"},
		{[]string{"a ** b and src/**/*.go stay"}, "a ** b and src/**/*.go stay\n"},
		{[]string{"see (**this**) now"}, "see (</><b>this</>) now\n"},
		{[]string{"**[link](x)** and ***both***"}, "</><b>[link](x)</> and </><b>both</>\n"},
		{[]string{"---\r\nx\r\n"}, "<d>" + strings.Repeat("─", 40) + "</>\nx\n"},
		{[]string{"````md\n```go\nx := **y**\n```\n````\nafter **b**"}, "<d>````md</>\n```go\nx := **y**\n```\n<d>````</>\nafter </><b>b</>\n"},
	}
	for _, c := range cases {
		if got := render(c.in...); got != c.want {
			t.Errorf("%q:\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestMarkdownStreamsPartialLines(t *testing.T) {
	var sb strings.Builder
	m := newMD(&sb)
	m.Write("Hello wor")
	if sb.String() != "Hello wor" {
		t.Fatalf("plain text must stream immediately, got %q", sb.String())
	}
	if !m.Pending() {
		t.Fatal("mid-line output must report pending")
	}
}

func TestMarkdownEndResetsFence(t *testing.T) {
	var sb strings.Builder
	m := newMD(&sb)
	m.Write("```\ncut off inside code")
	m.End()
	sb.Reset()
	m.Write("**next** reply\n")
	if got := sb.String(); !strings.Contains(got, sgrBold+"next") {
		t.Fatalf("fence leaked into the next reply: %q", got)
	}
}

func TestRuneWidth(t *testing.T) {
	if strWidth("✅⭐") != 4 || strWidth("🫠") != 2 {
		t.Fatal("emoji width")
	}
	if strWidth("日本語") != 6 || strWidth("abc") != 3 || strWidth("👍") != 2 || strWidth("é") != 1 || strWidth("é") != 1 {
		t.Fatal("runeWidth")
	}
}
