package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSnapshotRestore(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	w := func(p, c string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(c), 0o644)
	}
	r := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	// The project has its own git repo; the shadow store must not touch it.
	exec.Command("git", "-C", root, "init", "-q").Run()
	w(".gitignore", "secret.env\n")
	w("keep.txt", "v1")
	w("del.txt", "will be deleted")
	w("secret.env", "TOKEN=1")

	s, err := Open(t.TempDir(), root)
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	id, err := s.Snapshot(ctx, "turn 1")
	if err != nil {
		t.Fatal(err)
	}
	w("keep.txt", "v2")
	os.Remove(filepath.Join(root, "del.txt"))
	w("new/dir/added.txt", "new")
	w("secret.env", "TOKEN=2")

	changed, _ := s.Changed(ctx, id)
	if len(changed) != 3 {
		t.Fatalf("changed = %v", changed)
	}
	touched, err := s.Restore(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 3 {
		t.Fatalf("touched = %v", touched)
	}
	if r("keep.txt") != "v1" || r("del.txt") != "will be deleted" || r("new/dir/added.txt") != "<missing>" {
		t.Fatalf("restore wrong: keep=%q del=%q added=%q", r("keep.txt"), r("del.txt"), r("new/dir/added.txt"))
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("empty dirs created since the snapshot should be removed")
	}
	if r("secret.env") != "TOKEN=2" {
		t.Fatal("ignored files are not tracked and must be left alone")
	}
	// Project repo untouched: no commits were made in it.
	if out, _ := exec.Command("git", "-C", root, "log", "--oneline").CombinedOutput(); len(out) > 0 && string(out[:5]) != "fatal" {
		t.Fatalf("project repo got commits: %s", out)
	}
	// Nothing changed: restore is a no-op.
	id2, _ := s.Snapshot(ctx, "turn 2")
	if touched, err := s.Restore(ctx, id2, ""); err != nil || len(touched) != 0 {
		t.Fatalf("noop restore: %v %v", touched, err)
	}
}

func TestRestoreOnlyTheTurn(t *testing.T) {
	root := t.TempDir()
	s, err := Open(t.TempDir(), root)
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("v1"), 0o644)
	before, _ := s.Snapshot(ctx, "before")
	// The turn changes a.go and creates b"weird\tname".go.
	os.WriteFile(filepath.Join(root, "a.go"), []byte("v2"), 0o644)
	weird := filepath.Join(root, "b\"weird\tname.go")
	os.WriteFile(weird, []byte("x"), 0o644)
	after, _ := s.Snapshot(ctx, "after")
	// Later the user creates notes.md.
	os.WriteFile(filepath.Join(root, "notes.md"), []byte("mine"), 0o644)
	if _, err := s.Restore(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.go")); string(b) != "v1" {
		t.Errorf("a.go not restored: %q", b)
	}
	if _, err := os.Stat(weird); err == nil {
		t.Error("file with an unusual name created by the turn survived undo")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes.md")); string(b) != "mine" {
		t.Error("undo deleted a file the user created after the turn")
	}
}
