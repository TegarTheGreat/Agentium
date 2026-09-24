//go:build linux

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
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
	ruleNetPort   = 2

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
	if !cfg.Network {
		cfg.LocalPorts = listeningPorts()
	}
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
	attr := make([]byte, 16)
	binary.LittleEndian.PutUint64(attr[0:], fsAll)
	size := uintptr(8)
	if v >= 4 && !cfg.Network {
		// Only outbound connections are confined: servers may listen.
		binary.LittleEndian.PutUint64(attr[8:], netConnectTCP)
		size = 16
	}
	fd, _, e := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), size, 0)
	if e != 0 {
		return fmt.Errorf("create ruleset: %v", e)
	}
	read := uint64(fsExecute | fsReadFile | fsReadDir)
	if err := addRule(int(fd), "/", read); err != nil {
		return err
	}
	for _, p := range cfg.Write {
		if err := addRule(int(fd), p, fsAll); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rule %s: %w", p, err)
		}
	}
	if v >= 4 && !cfg.Network {
		for _, port := range cfg.LocalPorts {
			// struct landlock_net_port_attr: u64 allowed_access, u64 port.
			buf := make([]byte, 16)
			binary.LittleEndian.PutUint64(buf[0:], netConnectTCP)
			binary.LittleEndian.PutUint64(buf[8:], uint64(port))
			if _, _, e := syscall.Syscall6(sysLandlockAddRule, fd, ruleNetPort, uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0); e != 0 {
				return fmt.Errorf("port rule %d: %v", port, e)
			}
		}
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	if _, _, e := syscall.Syscall(sysLandlockRestrictSelf, fd, 0, 0); e != 0 {
		return fmt.Errorf("restrict: %v", e)
	}
	syscall.Close(int(fd))
	return syscall.Exec(argv[0], argv, os.Environ())
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

// remotePorts are never opened to commands without network access, even
// when something listens on them locally: they are how data would leave
// the machine (ssh, mail, DNS, web).
var remotePorts = map[int]bool{21: true, 22: true, 23: true, 25: true, 53: true, 80: true, 443: true,
	465: true, 587: true, 853: true, 993: true, 995: true, 1080: true, 3128: true, 8443: true}

// listeningPorts returns the TCP ports something on this machine listens
// on, from /proc/net/tcp and tcp6.
func listeningPorts() []int {
	seen := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fs := strings.Fields(line)
			if len(fs) < 4 || fs[3] != "0A" { // 0A = LISTEN
				continue
			}
			i := strings.LastIndexByte(fs[1], ':')
			if i < 0 {
				continue
			}
			p, err := strconv.ParseUint(fs[1][i+1:], 16, 16)
			if err == nil && p > 0 && !remotePorts[int(p)] {
				seen[int(p)] = true
			}
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}
