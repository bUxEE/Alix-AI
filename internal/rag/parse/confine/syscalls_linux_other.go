//go:build linux && !amd64 && !arm64

package confine

import "errors"

// deniedSyscalls has no list for this architecture: system call numbers differ
// per architecture and a wrong list is worse than none. The worker still gets
// no_new_privs, resource limits and Landlock, and reports seccomp as missing.
func deniedSyscalls() ([]uint32, uint32, error) {
	return nil, 0, errors.New("no seccomp deny list for this CPU architecture")
}
