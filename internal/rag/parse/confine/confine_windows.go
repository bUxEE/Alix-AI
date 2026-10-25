//go:build windows

package confine

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// This file calls kernel32 through the standard library's syscall package and
// declares the two structures it needs, instead of importing
// golang.org/x/sys/windows: that package links the net package into whatever
// imports it, and the worker's import graph must contain no network code
// (TestParserHasNoNetwork).

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procQueryInformationJob = kernel32.NewProc("QueryInformationJobObject")
	procSetErrorMode        = kernel32.NewProc("SetErrorMode")
)

const (
	jobObjectExtendedLimitInformation = 9

	semFailCriticalErrors = 0x0001
	semNoGPFaultErrorBox  = 0x0002
	semNoOpenFileErrorBox = 0x8000

	jobLimitProcessMemory = 0x00000100
	jobLimitActiveProcess = 0x00000008
	jobLimitKillOnClose   = 0x00002000
	jobLimitProcessTime   = 0x00000002
)

// jobBasicLimit is JOBOBJECT_BASIC_LIMIT_INFORMATION.
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

// jobExtendedLimit is JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IoInfo                [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// Apply, on Windows, verifies rather than installs: the host created a Job
// Object and assigned this process to it before sending Init, and the job is
// what limits memory, CPU time and child processes. The worker reads the limits
// of the job it is actually in (QueryInformationJobObject with a NULL job is
// "the job of the calling process") and compares them with what Init said. A
// process that is in no job, or in a job with other limits, must not parse.
//
// It also turns off the crash and critical-error dialogs, which would otherwise
// block a crashed worker waiting for a click that never comes.
func Apply(o Options) (wire.Sandbox, error) {
	procSetErrorMode.Call(semFailCriticalErrors | semNoGPFaultErrorBox | semNoOpenFileErrorBox)
	var info jobExtendedLimit
	r, _, e := procQueryInformationJob.Call(0, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if r == 0 {
		return wire.Sandbox{}, fmt.Errorf("the worker is not in a job object: %w", e)
	}
	flags := info.Basic.LimitFlags
	sb := wire.Sandbox{Observed: map[string]int64{
		"job_limit_flags":          int64(flags),
		"job_process_memory_bytes": int64(info.ProcessMemoryLimit),
		"job_active_process_limit": int64(info.Basic.ActiveProcessLimit),
		"job_cpu_100ns":            info.Basic.PerProcessUserTimeLimit,
	}}
	const need = jobLimitProcessMemory | jobLimitActiveProcess | jobLimitKillOnClose | jobLimitProcessTime
	switch {
	case flags&need != need:
		return sb, fmt.Errorf("the job object lacks required limits (flags %#x)", flags)
	case o.MemoryBytes > 0 && int64(info.ProcessMemoryLimit) != o.MemoryBytes:
		return sb, fmt.Errorf("the job object memory limit is %d, expected %d", info.ProcessMemoryLimit, o.MemoryBytes)
	case info.Basic.ActiveProcessLimit != 1:
		return sb, fmt.Errorf("the job object allows %d processes, expected 1", info.Basic.ActiveProcessLimit)
	case o.CPUMillis > 0 && info.Basic.PerProcessUserTimeLimit != o.CPUMillis*10000:
		return sb, fmt.Errorf("the job object CPU limit is %d (100 ns units), expected %d", info.Basic.PerProcessUserTimeLimit, o.CPUMillis*10000)
	}
	sb.Applied = []string{wire.JobObject}
	return sb, nil
}
