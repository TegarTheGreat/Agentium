//go:build windows

package tool

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows has no process groups that die with their parent: a command's
// children would outlive Agentium. Every started command is put in one
// Job Object created with KILL_ON_JOB_CLOSE, so when Agentium exits (even
// when it crashes) the handle closes and Windows ends them all.

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject    = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	jobOnce                sync.Once
	jobHandle              syscall.Handle
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IoInfo                [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func killOnExitJob() syscall.Handle {
	jobOnce.Do(func() {
		h, _, _ := procCreateJobObject.Call(0, 0)
		if h == 0 {
			return
		}
		var info jobExtendedLimit
		info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
		ok, _, _ := procSetInformationJob.Call(h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if ok == 0 {
			syscall.CloseHandle(syscall.Handle(h))
			return
		}
		jobHandle = syscall.Handle(h) // kept open until the process exits
	})
	return jobHandle
}

// contain puts a started command in the kill-on-exit job. Best effort: a
// child the shell spawned before this runs is not covered, and a failure
// leaves the command running as before.
func contain(cmd *exec.Cmd) {
	job := killOnExitJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	p, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(p)
	procAssignProcessToJob.Call(uintptr(job), uintptr(p))
}
