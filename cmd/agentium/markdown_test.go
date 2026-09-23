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
