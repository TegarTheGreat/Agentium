package fsx

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestLockSerializesReadModifyWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "n")
	WriteFile(p, []byte("0"), 0o600)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := Lock(p+".lock", 5*time.Second)
			defer unlock()
			b, _ := os.ReadFile(p)
			n, _ := strconv.Atoi(string(b))
			if err := WriteFile(p, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(p); string(b) != "20" {
		t.Fatalf("lost updates: %s", b)
	}
}

func TestWriteFileKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	os.WriteFile(real, []byte("a"), 0o600)
	os.Symlink(real, link)
	if err := WriteFile(link, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	if b, _ := os.ReadFile(real); string(b) != "b" {
		t.Fatalf("target not updated: %s", b)
	}
}
