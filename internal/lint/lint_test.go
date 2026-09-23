package lint

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	cases := []struct {
		path, good, bad, bin string
	}{
		{"a.go", "package a\n\nfunc F() int { return 1 }\n", "package a\n\nfunc F() int { return 1\n", ""},
		{"a.json", `{"a": [1, 2]}`, `{"a": [1, 2}`, ""},
		{"a.py", "def f():\n    return 1\n", "def f(:\n    return 1\n", "python3"},
		{"a.sh", "if true; then echo ok; fi\n", "if true; then echo ok\n", "bash"},
		{"a.js", "const x = () => 1;\n", "const x = () => {;\n", "node"},
	}
	for _, c := range cases {
		if c.bin != "" {
			if _, err := exec.LookPath(c.bin); err != nil {
				t.Logf("skip %s: %s not installed", c.path, c.bin)
				continue
			}
		}
		if r := Check(c.path, []byte(c.good)); !r.Checked || !r.OK {
			t.Errorf("%s good: %+v", c.path, r)
		}
		r := Check(c.path, []byte(c.bad))
		if !r.Checked || r.OK || r.Msg == "" {
			t.Errorf("%s bad: %+v", c.path, r)
		}
	}
	if r := Check("notes.md", []byte("# anything {")); r.Checked {
		t.Error("unknown types are not checked")
	}
	if r := Check("x.json", []byte("{\n\"a\": 1,\n}")); !strings.Contains(r.Msg, "line") {
		t.Errorf("json error should carry a line: %q", r.Msg)
	}
}
