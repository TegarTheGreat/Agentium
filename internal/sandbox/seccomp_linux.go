//go:build linux

package sandbox

import (
	"runtime"
	"syscall"
	"unsafe"
)

// denyDatagrams installs a seccomp filter that refuses UDP and raw
// sockets (EACCES) and io_uring (ENOSYS, it could bypass the checks), for
// this process and everything it starts. no_new_privs must already be
// set. Other architectures run without it.
func denyDatagrams() error {
	var arch, sysSocket uint32
	switch runtime.GOARCH {
	case "amd64":
		arch, sysSocket = 0xc000003e, 41 // AUDIT_ARCH_X86_64
	case "arm64":
		arch, sysSocket = 0xc00000b7, 198 // AUDIT_ARCH_AARCH64
	default:
		return nil
	}
	const (
		sysIoUringSetup = 425
		afInet          = 2
		afInet6         = 10
		sockDgram       = 2
		sockRaw         = 3
		retAllow        = 0x7fff0000
		retErrno        = 0x00050000
		eacces          = 13
		enosys          = 38
	)
	p := &bpf{}
	p.ld(4) // arch
	p.jeq(arch, "", "deny")
	p.ld(0) // syscall number
	if runtime.GOARCH == "amd64" {
		p.jge(0x40000000, "deny", "") // x32 ABI
	}
	p.jeq(sysSocket, "socket", "")
	p.jeq(sysIoUringSetup, "enosys", "")
	p.ret(retAllow)
	p.label("socket")
	p.ld(16) // args[0]: domain
	p.jeq(afInet, "type", "")
	p.jeq(afInet6, "type", "allow")
	p.label("type")
	p.ld(24)   // args[1]: type
	p.and(0xf) // without SOCK_NONBLOCK / SOCK_CLOEXEC
	p.jeq(sockDgram, "deny", "")
	p.jeq(sockRaw, "deny", "allow")
	p.label("allow")
	p.ret(retAllow)
	p.label("deny")
	p.ret(retErrno | eacces)
	p.label("enosys")
	p.ret(retErrno | enosys)
	prog := p.assemble()

	fprog := struct {
		len    uint16
		_      [6]byte
		filter *sockFilter
	}{len: uint16(len(prog)), filter: &prog[0]}
	const prSetSeccomp, seccompModeFilter = 22, 2
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

// bpf is a tiny classic-BPF assembler with forward jumps to labels.
type bpf struct {
	ins    []sockFilter
	jumps  []jump
	labels map[string]int
}

type jump struct {
	at     int
	jt, jf string
}

func (b *bpf) emit(code uint16, k uint32) { b.ins = append(b.ins, sockFilter{code: code, k: k}) }
func (b *bpf) ld(off uint32)              { b.emit(0x20, off) } // BPF_LD|BPF_W|BPF_ABS
func (b *bpf) and(k uint32)               { b.emit(0x54, k) }   // BPF_ALU|BPF_AND|BPF_K
func (b *bpf) ret(k uint32)               { b.emit(0x06, k) }   // BPF_RET|BPF_K
func (b *bpf) jeq(k uint32, jt, jf string) {
	b.jumps = append(b.jumps, jump{len(b.ins), jt, jf})
	b.emit(0x15, k) // BPF_JMP|BPF_JEQ|BPF_K
}
func (b *bpf) jge(k uint32, jt, jf string) {
	b.jumps = append(b.jumps, jump{len(b.ins), jt, jf})
	b.emit(0x35, k) // BPF_JMP|BPF_JGE|BPF_K
}
func (b *bpf) label(name string) {
	if b.labels == nil {
		b.labels = map[string]int{}
	}
	b.labels[name] = len(b.ins)
}

// assemble resolves labels; "" means the next instruction.
func (b *bpf) assemble() []sockFilter {
	off := func(from int, l string) uint8 {
		if l == "" {
			return 0
		}
		to, ok := b.labels[l]
		if !ok || to <= from || to-from-1 > 255 {
			panic("bpf: bad jump to " + l)
		}
		return uint8(to - from - 1)
	}
	for _, j := range b.jumps {
		b.ins[j.at].jt = off(j.at, j.jt)
		b.ins[j.at].jf = off(j.at, j.jf)
	}
	return b.ins
}
