package main

import "testing"

func TestVimKeys(t *testing.T) {
	type step struct {
		keys []string
		buf  string
		pos  int
	}
	run := func(start string, pos int, keys ...string) *editor {
		e := &editor{buf: []rune(start), pos: pos, vim: true, vimNormal: true}
		for _, k := range keys {
			if out := e.vimKey(k); out == "\x1f" { // undo passes to the editor
				n := len(e.undo)
				e.buf, e.pos, e.undo = e.undo[n-1].buf, e.undo[n-1].pos, e.undo[:n-1]
			}
		}
		return e
	}
	for _, c := range []struct {
		start string
		pos   int
		keys  []string
		want  string
		wpos  int
	}{
		{"hello world", 0, []string{"d", "w"}, "world", 0},
		{"hello world", 0, []string{"c", "w"}, " world", 0},
		{"hello world", 6, []string{"x"}, "hello orld", 6},
		{"hello world", 3, []string{"D"}, "hel", 2},
		{"one\ntwo\nthree", 5, []string{"d", "d"}, "one\nthree", 4},
		{"hello world", 0, []string{"w", "d", "e"}, "hello ", 5},
		{"hello world", 10, []string{"b", "b"}, "hello world", 0},
		{"abc", 0, []string{"x", "u"}, "abc", 0},
		{"abc", 0, []string{"~", "~"}, "ABc", 2},
		{"abc", 1, []string{"r", "X"}, "aXc", 1},
		{"abc def", 0, []string{"y", "w", "$", "p"}, "abc defabc ", 10},
		{"a\nb", 2, []string{"g", "g"}, "a\nb", 0},
		// From the audit: e past a word end, dw at a line end, linewise p.
		{"foo bar", 1, []string{"e", "e"}, "foo bar", 6},
		{"foo\nbar", 0, []string{"d", "w"}, "\nbar", 0},
		{"hello", 0, []string{"y", "y", "p"}, "hello\nhello", 6},
		{"a\nb", 0, []string{"d", "d", "p"}, "b\na", 2},
		{"a\n\nb", 2, []string{"r", "x"}, "a\n\nb", 2},
		{"a\n\nb", 2, []string{"~"}, "a\n\nb", 2},
		{"hi", 0, []string{"?"}, "hi", 0},
	} {
		e := run(c.start, c.pos, c.keys...)
		if string(e.buf) != c.want || e.pos != c.wpos {
			t.Errorf("%q %v: got %q pos %d, want %q pos %d", c.start, c.keys, string(e.buf), e.pos, c.want, c.wpos)
		}
	}
	// Esc leaves insert mode; i returns to it; Enter passes through.
	e := &editor{buf: []rune("hi"), pos: 2, vim: true}
	if e.vimKey("\x1b") != "" || !e.vimNormal || e.pos != 1 {
		t.Fatal("esc to normal")
	}
	if e.vimKey("\r") != "\r" || e.vimKey("i") != "" || e.vimNormal {
		t.Fatal("enter passes, i inserts")
	}
	// Esc and d typed fast arrive as Alt-d: still Esc, then d.
	e = &editor{buf: []rune("one two"), pos: 7, vim: true}
	if out := e.vimKey("\x1bd"); out != "" || !e.vimNormal || e.vimPending != "d" {
		t.Fatalf("fast esc: %q normal=%v pending=%q", out, e.vimNormal, e.vimPending)
	}
}
