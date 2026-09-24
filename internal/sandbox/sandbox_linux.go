//go:build linux

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	createRulesetVersion = 1 << 0
	rulePathBeneath      = 1
	prSetNoNewPrivs      = 38
	oPath                = 0x200000 // O_PATH (same value on amd64 and arm64)
)

// Filesystem access rights.
const (
	fsExecute    = 1 << 0
	fsWriteFile  = 1 << 1
	fsReadFile   = 1 << 2
	fsReadDir    = 1 << 3
	fsRemoveDir  = 1 << 4
	fsRemoveFile = 1 << 5
	fsMakeChar   = 1 << 6
	fsMakeDir    = 1 << 7
	fsMakeReg    = 1 << 8
	fsMakeSock   = 1 << 9
	fsMakeFifo   = 1 << 10
	fsMakeBlock  = 1 << 11
	fsMakeSym    = 1 << 12
	fsRefer      = 1 << 13 // ABI 2
	fsTruncate   = 1 << 14 // ABI 3
	fsIoctlDev   = 1 << 15 // ABI 5

	netBindTCP    = 1 << 0 // ABI 4
	netConnectTCP = 1 << 1
	// Landlock scopes (ABI 6).
	scopeAbstractUnixSocket = 1 << 0
	scopeSignal             = 1 << 1

	fileOnly = fsExecute | fsWriteFile | fsReadFile | fsTruncate | fsIoctlDev
)

var abi = sync.OnceValue(func() int {
	r, _, e := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if e != 0 {
		return 0
	}
	return int(r)
})

func handledFS(v int) uint64 {
	h := uint64(1<<13 - 1) // ABI 1: bits 0-12
	if v >= 2 {
		h |= fsRefer
	}
	if v >= 3 {
		h |= fsTruncate
	}
	if v >= 5 {
		h |= fsIoctlDev
	}
	return h
}

// Probe reports what this kernel supports.
func Probe() Status {
	v := abi()
	switch {
	case v <= 0:
		return Status{Detail: "Landlock unavailable (kernel < 5.13 or disabled): commands run unconfined"}
	case v < 4:
		return Status{Available: true, Detail: fmt.Sprintf("Landlock ABI %d: filesystem confined, network NOT blockable (kernel < 6.7)", v)}
	case !datagramFilterSupported():
		return Status{Available: true, Detail: fmt.Sprintf("Landlock ABI %d: filesystem and TCP confined, UDP NOT blockable on %s", v, runtime.GOARCH)}
	}
	return Status{Available: true, Network: true, Detail: fmt.Sprintf("Landlock ABI %d: filesystem and TCP network confined", v)}
}

func command(shell, cmdline string, cfg Config) (*exec.Cmd, bool, error) {
	if abi() <= 0 {
		return exec.Command(shell, "-c", cmdline), false, nil
	}
	self, err := os.Executable()
	if err != nil {
		return exec.Command(shell, "-c", cmdline), false, nil
	}
	cfg.ReadDeny = secretPaths()
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, false, err
	}
	cmd := exec.Command(self, helperArg, shell, "-c", cmdline)
	cmd.Env = append(os.Environ(), envKey+"="+string(b))
	return cmd, true, nil
}

func confineAndExec(cfg Config, argv []string) error {
	runtime.LockOSThread()
	v := abi()
	if v <= 0 {
		return errors.New("landlock unavailable")
	}
	fsAll := handledFS(v)
	attr := make([]byte, 24)
	binary.LittleEndian.PutUint64(attr[0:], fsAll)
	size := uintptr(8)
	if v >= 4 && !cfg.Network {
		// Outbound TCP is confined; listening is not, so servers run and
		// can be opened from a browser.
		binary.LittleEndian.PutUint64(attr[8:], netConnectTCP)
		size = 16
	}
	if v >= 6 {
		// No abstract unix sockets (D-Bus, X11 …) or signals to processes
		// outside the sandbox.
		binary.LittleEndian.PutUint64(attr[16:], scopeAbstractUnixSocket|scopeSignal)
		size = 24
	}
	fd, _, e := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), size, 0)
	if e != 0 {
		return fmt.Errorf("create ruleset: %v", e)
	}
	// Landlock only grants, so "everything but the secrets" is built by
	// granting each sibling along the way to a secret, never the secret.
	read := uint64(fsExecute | fsReadFile | fsReadDir)
	if err := addExcept(int(fd), "/", read, cfg.ReadDeny); err != nil {
		return err
	}
	for _, p := range cfg.Write {
		if err := addExcept(int(fd), p, fsAll, cfg.ReadDeny); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rule %s: %w", p, err)
		}
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	if _, _, e := syscall.Syscall(sysLandlockRestrictSelf, fd, 0, 0); e != 0 {
		return fmt.Errorf("restrict: %v", e)
	}
	syscall.Close(int(fd))
	if !cfg.Network {
		// Landlock confines TCP only: UDP (and so DNS lookups that could
		// carry data out) and raw sockets are refused with seccomp.
		if err := denyDatagrams(); err != nil && err != errUnsupportedArch {
			return fmt.Errorf("seccomp: %w", err)
		}
	}
	return syscall.Exec(argv[0], argv, os.Environ())
}

// addExcept grants access to path, except to the denied paths below it:
// a directory holding a denied path gets rules for its other entries,
// and itself only the right to list its entries (names, not contents).
// Symlinked entries leading to a denied path are skipped, since a rule
// applies to the link's target.
func addExcept(rulesetFD int, path string, access uint64, deny []string) error {
	clean := filepath.Clean(path)
	if denied(clean, deny) {
		return nil
	}
	if !holdsDenied(clean, deny) {
		return addRule(rulesetFD, clean, access)
	}
	_ = addRule(rulesetFD, clean, fsReadDir)
	entries, err := os.ReadDir(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return nil // unreadable: grant nothing below it
	}
	for _, e := range entries {
		p := filepath.Join(clean, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			if t, err := filepath.EvalSymlinks(p); err == nil && (denied(t, deny) || holdsDenied(t, deny)) {
				continue
			}
		}
		// Entries can vanish or be unopenable (sockets); skip them.
		_ = addExcept(rulesetFD, p, access, deny)
	}
	return nil
}

func denied(p string, deny []string) bool {
	for _, d := range deny {
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

func holdsDenied(dir string, deny []string) bool {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, d := range deny {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}

func addRule(rulesetFD int, path string, access uint64) error {
	f, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return os.ErrNotExist
		}
		return err
	}
	defer syscall.Close(f)
	var st syscall.Stat_t
	if err := syscall.Fstat(f, &st); err == nil && st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		access &= fileOnly
	}
	// struct landlock_path_beneath_attr is packed: u64 access, s32 fd.
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint64(buf[0:], access)
	binary.LittleEndian.PutUint32(buf[8:], uint32(int32(f)))
	if _, _, e := syscall.Syscall6(sysLandlockAddRule, uintptr(rulesetFD), rulePathBeneath, uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0); e != 0 {
		return e
	}
	return nil
}
