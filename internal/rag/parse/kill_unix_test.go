//go:build linux || darwin

package parse

import (
	"os/exec"
	"syscall"
	"testing"
)

// killProcess ends a process from outside, the way an administrator or an
// out-of-memory killer might.
func killProcess(t testing.TB, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
}

// Once the worker has been reaped its pid can belong to another process, even
// another group leader: the confinement must not signal it or its group.
func TestConfinementDoesNotKillAfterReap(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep command here")
	}
	other := exec.Command(sleep, "30") // stands for the process that reused the pid
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	u := &unixConfinement{stop: make(chan struct{})}
	u.pid.Store(int64(other.Process.Pid))
	u.finish(nil) // the worker exited and was reaped
	u.kill()
	u.close()
	// A SIGKILL sent above would win over this SIGTERM.
	_ = other.Process.Signal(syscall.SIGTERM)
	_ = other.Wait()
	if ws, ok := other.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("the other process ended with %v, want SIGTERM: the confinement signalled it after the reap", other.ProcessState)
	}
}
