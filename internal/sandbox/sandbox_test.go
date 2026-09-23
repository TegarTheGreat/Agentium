package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	MaybeRunHelper()
	os.Exit(m.Run())
}

func run(t *testing.T, dir, script string, cfg Config) (string, error) {
	t.Helper()
	cmd, ok, err := Command("/bin/sh", script, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("sandbox unavailable: " + Probe().Detail)
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestFilesystemConfinement(t *testing.T) {
	work, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("safe"), 0o644)
	// Only the workspace is writable; the other temp dir is not.
	cfg := Config{Write: []string{work, "/dev"}}

	out, err := run(t, work, "echo ok > inside.txt && cat inside.txt && cat "+filepath.Join(outside, "victim.txt"), cfg)
	if err != nil || !strings.Contains(out, "ok") || !strings.Contains(out, "safe") {
		t.Fatalf("workspace write / outside read should work: %v %q", err, out)
	}
	for _, script := range []string{
		"echo pwned > " + filepath.Join(outside, "victim.txt"),
		"rm -f " + filepath.Join(outside, "victim.txt"),
		"mkdir " + filepath.Join(outside, "newdir"),
		"python3 -c \"open('" + filepath.Join(outside, "victim.txt") + "','w').write('x')\" 2>&1 || exit 1",
	} {
		if _, err := run(t, work, script, cfg); err == nil {
			t.Errorf("should be denied: %s", script)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim.txt")); string(b) != "safe" {
		t.Fatalf("outside file modified: %q", b)
	}
	// Children inherit the confinement.
	if _, err := run(t, work, "sh -c 'sh -c \"echo x > "+filepath.Join(outside, "deep.txt")+"\"'", cfg); err == nil {
		t.Error("grandchild escaped the sandbox")
	}
}

func TestNetworkConfinement(t *testing.T) {
	if !Probe().Network {
		t.Skip("network confinement unavailable: " + Probe().Detail)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello\n"))
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	work, _ := filepath.EvalSymlinks(t.TempDir())
	script := `python3 -c "import socket; s=socket.create_connection(('127.0.0.1',` + strconv.Itoa(port) + `),2); print(s.recv(10).decode())"`
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		if _, err := os.Stat("/usr/local/bin/python3"); err != nil {
			t.Skip("python3 needed for the TCP probe")
		}
	}
	if out, err := run(t, work, script, Config{Write: []string{work, "/dev"}}); err == nil {
		t.Fatalf("TCP connect should be blocked, got %q", out)
	}
	if out, err := run(t, work, script, Config{Write: []string{work, "/dev"}, Network: true}); err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("TCP connect should work when allowed: %v %q", err, out)
	}
}

func TestDefaultWrite(t *testing.T) {
	work, _ := filepath.EvalSymlinks(t.TempDir())
	w := DefaultWrite(work)
	found := false
	for _, p := range w {
		if p == work {
			found = true
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("non-existent path in write list: %s", p)
		}
	}
	if !found {
		t.Fatalf("workspace missing from %v", w)
	}
}
