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

func TestCommandSteps(t *testing.T) {
	join := func(ss []cmdStep) string {
		var b []string
		for _, s := range ss {
			b = append(b, s.text+"⟨"+s.op+"⟩")
		}
		return strings.Join(b, " ")
	}
	for in, want := range map[string]string{
		"sed -n 1,40p README.md; echo ===; ls joss":          "sed -n 1,40p README.md⟨;⟩ echo ===⟨;⟩ ls joss⟨⟩",
		"go test ./... 2>&1 | tail -20 && echo ok || echo x": "go test ./... 2>&1⟨|⟩ tail -20⟨&&⟩ echo ok⟨||⟩ echo x⟨⟩",
		`echo "a; b" && grep 'x|y' f`:                        `echo "a; b"⟨&&⟩ grep 'x|y' f⟨⟩`,
		"(cd sub; make) && echo $(date; true)":               "(cd sub; make)⟨&&⟩ echo $(date; true)⟨⟩",
		"ls":                                                 "ls⟨⟩",
	} {
		if got := join(commandSteps("bash: x", in)); got != want {
			t.Errorf("%q:\n got %s\nwant %s", in, got, want)
		}
	}
	if got := commandSteps("bash: x", "cat <<EOF; a\nEOF"); len(got) != 1 {
		t.Error("heredoc split")
	}
}
