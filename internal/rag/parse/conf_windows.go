//go:build windows

package parse

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows the host confines the worker with a Job Object, created before the
// process and joined before the host sends it anything (the worker blocks on its
// first read until the host sends Init, so it can do nothing in between):
//
//   - a per-process and per-job memory limit (committed bytes): the process that
//     allocates past it fails its allocation and dies;
//   - a per-process CPU-time limit: the kernel terminates the process at it;
//   - an active-process limit of 1: the worker cannot start a child process;
//   - kill-on-close: if the host dies the kernel kills the worker, so a crashed
//     core leaves no orphan parsers;
//   - die-on-unhandled-exception: a crash ends the process at once, with no
//     error-reporting dialog waiting for a person;
//   - UI restrictions: no clipboard, no desktop switching, no global atoms, no
//     user handles from outside the job, no system parameter changes.
//
// What a Job Object does not do is limit the network or the filesystem. Those
// are not restricted on Windows in this version (ADR 0014); "no network" there
// holds by construction (the worker links no network code) and not by OS
// enforcement.

var (
	modKernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessMemory = modKernel32.NewProc("K32GetProcessMemoryInfo")
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type jobConfinement struct {
	mu     sync.Mutex
	job    windows.Handle
	proc   windows.Handle
	limits hostLimits
	peak   int64
	peakWS int64
	killed bool
	closed bool
}

func newConfinement(cmd *exec.Cmd, lim hostLimits) (confinement, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY |
		windows.JOB_OBJECT_LIMIT_JOB_MEMORY |
		windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS |
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION |
		windows.JOB_OBJECT_LIMIT_PROCESS_TIME
	info.BasicLimitInformation.ActiveProcessLimit = 1
	info.BasicLimitInformation.PerProcessUserTimeLimit = lim.cpu.Milliseconds() * 10000 // 100 ns units
	info.ProcessMemoryLimit = uintptr(lim.memory)
	info.JobMemoryLimit = uintptr(lim.memory)
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("job object limits: %w", err)
	}
	ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: windows.JOB_OBJECT_UILIMIT_DESKTOP |
		windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS | windows.JOB_OBJECT_UILIMIT_EXITWINDOWS |
		windows.JOB_OBJECT_UILIMIT_GLOBALATOMS | windows.JOB_OBJECT_UILIMIT_HANDLES |
		windows.JOB_OBJECT_UILIMIT_READCLIPBOARD | windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
		windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectBasicUIRestrictions,
		uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("job object UI restrictions: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	return &jobConfinement{job: job, limits: lim}, nil
}

func (j *jobConfinement) mechanisms() []string {
	return []string{"job_object:memory", "job_object:cpu_time", "job_object:active_process", "job_object:kill_on_close", "job_object:ui_limits"}
}

func (j *jobConfinement) attach(p *os.Process) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	if err := windows.AssignProcessToJobObject(j.job, h); err != nil {
		windows.CloseHandle(h)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	j.mu.Lock()
	j.proc = h
	j.mu.Unlock()
	return nil
}

func (j *jobConfinement) kill() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.killed {
		return
	}
	j.killed = true
	windows.TerminateJobObject(j.job, 1)
}

// sample reads the job's peak committed memory and the process's peak working
// set. Both are peaks, so reading them late is as good as reading them often.
func (j *jobConfinement) sample() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(j.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err == nil {
		j.peak = max(j.peak, int64(info.PeakProcessMemoryUsed))
	}
	if j.proc != 0 {
		var c processMemoryCounters
		c.cb = uint32(unsafe.Sizeof(c))
		if r, _, _ := procGetProcessMemory.Call(uintptr(j.proc), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r != 0 {
			j.peakWS = max(j.peakWS, int64(c.PeakWorkingSetSize))
		}
	}
}

func (j *jobConfinement) memory() (peak, workingSet int64, kind string) {
	j.sample()
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.peak, j.peakWS, "commit"
}

func (j *jobConfinement) finish(ps *os.ProcessState) {
	j.sample()
}

// memoryExceeded: the job's peak committed memory reached the limit, which is
// how a Go process that failed an allocation under the limit shows up.
func (j *jobConfinement) memoryExceeded() bool {
	j.sample()
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.limits.memory > 0 && j.peak >= j.limits.memory/100*95
}

func (j *jobConfinement) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	if j.proc != 0 {
		windows.CloseHandle(j.proc)
	}
	windows.CloseHandle(j.job) // kill-on-close: anything still running dies here
}

// signalName is empty on Windows: a process has an exit code, not a signal.
func signalName(ps *os.ProcessState) string { return "" }
