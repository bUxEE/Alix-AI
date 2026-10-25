//go:build unix

package supervisor

import (
	"os"
	"os/exec"
	"syscall"
)

// procGuard owns the process group created for one child. LocalAI starts its
// backends as child processes; signalling the group reaches them too.
type procGuard struct {
	pgid int
}

func currentEUID() int { return os.Geteuid() }

// baseEnv is what a child needs beyond its configured environment: nothing.
func baseEnv() []string { return nil }

// prepareCmd puts the child in its own process group.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// guardProcess applies limits to the freshly started child (pid is its group
// id too, because of Setpgid).
func guardProcess(pid int, l Limits) (*procGuard, error) {
	g := &procGuard{pgid: pid}
	if err := applyLimits(pid, l); err != nil {
		return g, err
	}
	return g, nil
}

// terminate sends SIGTERM to the group and reports whether it was delivered.
func (g *procGuard) terminate(int) bool {
	return syscall.Kill(-g.pgid, syscall.SIGTERM) == nil
}

// kill sends SIGKILL to the group.
func (g *procGuard) kill(int) {
	_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
}

// sweep kills what is left of the group once its leader has exited.
func (g *procGuard) sweep() {
	_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
}
