//go:build linux || darwin

package parse

import "syscall"

// processAlive reports whether a process with this id is still running (a
// reaped child no longer exists; a zombie would still answer, but the host
// waits for every worker it starts).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
