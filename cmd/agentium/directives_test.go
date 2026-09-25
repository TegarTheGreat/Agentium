package main

import (
	"strings"
	"testing"
)

func TestDirectiveFilter(t *testing.T) {
	in := "Done: tests pass.\n@remember Tests run with make test\n- @prefer: short commits\nEmail me @alice later\n```\n@remember inside code stays\n```\n@remembered is a word\n`code` first\n@decide last one"
	want := "Done: tests pass.\nEmail me @alice later\n```\n@remember inside code stays\n```\n@remembered is a word\n`code` first\n"
	for _, chunk := range []int{1, 3, 7, 1000} {
		var got strings.Builder
		f := newDirectiveFilter(func(s string) { got.WriteString(s) })
		for i := 0; i < len(in); i += chunk {
			f.write(in[i:min(len(in), i+chunk)])
		}
		f.flush()
		if got.String() != want {
			t.Fatalf("chunk %d:\n%q\nwant\n%q", chunk, got.String(), want)
		}
	}
	// Ordinary text is not held back: it streams as it comes.
	var got strings.Builder
	f := newDirectiveFilter(func(s string) { got.WriteString(s) })
	f.write("Hello wor")
	if got.String() != "Hello wor" {
		t.Fatalf("held: %q", got.String())
	}
}
