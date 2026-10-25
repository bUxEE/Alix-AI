//go:build linux || darwin

package parse

import (
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// On Linux and macOS the host cannot impose limits on a child from outside as a
// Job Object does (setrlimit applies to the calling process, and the worker
// applies its own, see internal/rag/parse/confine). What the host does:
//
//   - starts the worker in its own process group, so that a kill takes the whole
//     group and nothing the worker spawned (it cannot, but the kill does not rely
//     on that) survives it;
//   - watches the worker's resident set (Linux: /proc/<pid>/statm, every 10 ms)
//     and kills it when it passes the memory budget. It is sampling, so a
//     process that allocates faster than the interval overshoots by what it can
//     allocate in one tick; RLIMIT_AS in the worker is the hard cap behind it;
//   - reads the peak resident set from /proc/<pid>/status (VmHWM) while the
//     process lives. After exit, rusage would give it, but on Linux ru_maxrss
//     starts at the host's own high-water mark (see rusageMaxRSSIsTheWorkers).
//
// macOS has no per-process resident-set reader reachable without cgo (the
// libproc calls are C), so there the host has no watchdog and relies on the
// in-process guard, the CPU-time limit and the wall-time kill. See ADR 0014.

type unixConfinement struct {
	limits hostLimits
	pid    atomic.Int64
	peak   atomic.Int64
	hit    atomic.Bool
	exited atomic.Bool
	stop   chan struct{}
	once   sync.Once
	maxrss int64
}

func newConfinement(cmd *exec.Cmd, lim hostLimits) (confinement, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &unixConfinement{limits: lim, stop: make(chan struct{})}, nil
}

func (u *unixConfinement) mechanisms() []string {
	m := []string{"process_group"}
	if rssSupported {
		m = append(m, "rss_watchdog")
	}
	return m
}

func (u *unixConfinement) attach(p *os.Process) error {
	u.pid.Store(int64(p.Pid))
	if rssSupported && u.limits.memory > 0 {
		go u.watch(p.Pid)
	}
	return nil
}

func (u *unixConfinement) watch(pid int) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-u.stop:
			return
		case <-t.C:
			rss, hwm := rssBytes(pid)
			if rss < 0 {
				continue
			}
			u.notePeak(max(rss, hwm))
			if rss > u.limits.memory {
				u.hit.Store(true)
				u.kill()
				return
			}
		}
	}
}

func (u *unixConfinement) kill() {
	pid := int(u.pid.Load())
	// Once reaped, the pid and the group id may belong to another process;
	// until then the leader, a zombie at worst, holds both.
	if pid <= 0 || u.exited.Load() {
		return
	}
	// A negative pid is the process group. The worker is its own group leader.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// notePeak raises the recorded peak.
func (u *unixConfinement) notePeak(v int64) {
	for {
		cur := u.peak.Load()
		if v <= cur || u.peak.CompareAndSwap(cur, v) {
			return
		}
	}
}

// memory is the worker's peak resident set. While the process lives it reads the
// kernel's own high-water mark (VmHWM), so the figure is exact at the moment of
// the call and not a sample; after it has exited the last reading stands.
func (u *unixConfinement) memory() (peak, workingSet int64, kind string) {
	if rssSupported && !u.exited.Load() {
		if pid := int(u.pid.Load()); pid > 0 {
			if rss, hwm := rssBytes(pid); rss >= 0 {
				u.notePeak(max(rss, hwm))
			}
		}
	}
	p := u.peak.Load()
	if rusageMaxRSSIsTheWorkers {
		p = max(p, u.maxrss)
	}
	return p, p, "rss"
}

func (u *unixConfinement) finish(ps *os.ProcessState) {
	u.exited.Store(true)
	if ps == nil {
		return
	}
	if ru, ok := ps.SysUsage().(*syscall.Rusage); ok && ru != nil {
		u.maxrss = maxRSSBytes(ru)
	}
}

func (u *unixConfinement) memoryExceeded() bool { return u.hit.Load() }

func (u *unixConfinement) close() {
	u.once.Do(func() { close(u.stop) })
	u.kill() // a no-op once the worker has been reaped
}

// signalName is the name of the signal that ended the process, or "".
func signalName(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGKILL:
			return "killed"
		case syscall.SIGSEGV:
			return "segv"
		case syscall.SIGABRT:
			return "abort"
		default:
			return ws.Signal().String()
		}
	}
	return ""
}
