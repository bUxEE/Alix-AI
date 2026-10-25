//go:build windows

package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGuard owns the job object created for one child. The job kills every
// member when it is closed, so LocalAI's backend processes cannot outlive it,
// and carries the memory limit.
type procGuard struct {
	mu  sync.Mutex
	job windows.Handle
}

// currentEUID: Windows has no uid; the HC-41 root guard does not apply.
func currentEUID() int { return -1 }

// baseEnv is what a Windows process needs to run at all.
func baseEnv() []string {
	if v, ok := os.LookupEnv("SystemRoot"); ok {
		return []string{"SystemRoot=" + v}
	}
	return nil
}

func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func limitsSupported(l Limits) error {
	if l.MaxOpenFiles != 0 {
		return errors.New("max_open_files cannot be enforced on Windows")
	}
	return nil
}

// guardProcess puts the child in a job object with KILL_ON_JOB_CLOSE and, when
// asked, a per-process memory limit.
func guardProcess(pid int, l Limits) (*procGuard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	g := &procGuard{job: job}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if l.MaxMemoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = uintptr(l.MaxMemoryBytes)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return g, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return g, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return g, err
	}
	return g, nil
}

// terminate: Windows has no polite signal for a console-less process.
func (g *procGuard) terminate(int) bool { return false }

// kill terminates every process of the job.
func (g *procGuard) kill(int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
}

// sweep closes the job, which kills any member still alive.
func (g *procGuard) sweep() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}
