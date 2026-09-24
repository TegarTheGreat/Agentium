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
	box := Config{Write: []string{work, "/dev"}}
	// Without network, even a local listening port cannot be reached: a
	// port rule cannot tell hosts apart (a local proxy would be a way out).
	if out, err := run(t, work, script, box); err == nil {
		t.Fatalf("TCP connect should be blocked, got %q", out)
	}
	if out, err := run(t, work, script, Config{Write: box.Write, Network: true}); err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("TCP connect should work when allowed: %v %q", err, out)
	}
	// Servers may listen without network access.
	listen := `python3 -c "import socket; s=socket.socket(); s.bind(('127.0.0.1',0)); s.listen(); print('listening')"`
	if out, err := run(t, work, listen, box); err != nil || !strings.Contains(out, "listening") {
		t.Fatalf("listening should be allowed: %v %q", err, out)
	}
	// UDP (DNS lookups carrying data) is refused too, when the kernel
	// filter applies.
	udp := `python3 -c "
import socket
try:
    socket.socket(socket.AF_INET, socket.SOCK_DGRAM).sendto(b'x', ('192.0.2.1', 53))
    print('sent')
except OSError as e:
    print('errno', e.errno)
"`
	if got, _ := run(t, work, udp, box); !strings.Contains(got, "errno") {
		t.Fatalf("UDP should be denied without network, got %q", got)
	}
	if got, _ := run(t, work, udp, Config{Write: box.Write, Network: true}); !strings.Contains(got, "sent") {
		t.Fatalf("UDP should work with network, got %q", got)
	}
}

func TestSecretsUnreadable(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("PRIVATE"), 0o600)
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PUBLIC"), 0o644)
	work := filepath.Join(home, "work")
	os.MkdirAll(work, 0o755)
	box := Config{Write: []string{work, "/dev"}}
	if out, _ := run(t, work, "cat "+filepath.Join(home, ".ssh", "id_ed25519"), box); strings.Contains(out, "PRIVATE") {
		t.Fatal("an SSH key was readable in the sandbox")
	}
	if out, err := run(t, work, "cat "+filepath.Join(home, ".bashrc"), box); err != nil || !strings.Contains(out, "PUBLIC") {
		t.Fatalf("ordinary home files should stay readable: %v %q", err, out)
	}
	// Even when the workspace is the home directory itself.
	if out, _ := run(t, home, "cat .ssh/id_ed25519", Config{Write: []string{home, "/dev"}}); strings.Contains(out, "PRIVATE") {
		t.Fatal("an SSH key was readable with home as the workspace")
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

func TestReadOnlyDropsWorkspace(t *testing.T) {
	got := ReadOnly([]string{"/tmp", "/tmp/w", "/tmp/w/sub", "/home/u/.cache", "/dev/null"}, "/tmp/w")
	want := []string{"/home/u/.cache", "/dev/null"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}
