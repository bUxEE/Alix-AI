//go:build linux

package confine

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// A seccomp-bpf filter built by hand (a few dozen instructions, no dependency),
// installed on every thread with SECCOMP_FILTER_FLAG_TSYNC. It is a deny list:
// every system call is allowed except the ones named, which fail with EPERM.
// The list is what a text extractor never needs and an exploit would want:
// creating or using sockets (no network, whatever else is true), starting
// programs, tracing, namespaces and mounts, and io_uring (which can issue
// network and file operations around a seccomp filter). A deny list rather than
// an allow list because the Go runtime's own system calls change between Go and
// kernel versions; an allow list that is one entry short kills the worker.
//
// Together with Landlock this is the Linux "no network, no filesystem" layer.
// If the kernel or the container's own seccomp profile refuses to install it,
// the worker reports the mechanism as missing and runs without it; the host
// records the lower sandbox level.

const (
	retAllow = 0x7fff0000
	retKill  = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	retErrno = 0x00050000
)

func setNoNewPrivs() error {
	if _, _, errno := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// applySeccomp installs the filter and returns the mechanism names it provides.
func applySeccomp() ([]string, error) {
	denied, arch, err := deniedSyscalls()
	if err != nil {
		return nil, err
	}
	prog := buildFilter(denied, arch)
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := syscall.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return nil, fmt.Errorf("seccomp: %v", errno)
	}
	return []string{wire.SeccompNet, wire.SeccompExec}, nil
}

// buildFilter assembles: refuse a foreign architecture, then for each denied
// system call number jump to a return of EPERM, else allow.
func buildFilter(denied []uint32, arch uint32) []unix.SockFilter {
	const (
		ldAbs  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq    = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ret    = unix.BPF_RET | unix.BPF_K
		offNr  = 0 // seccomp_data.nr
		offArc = 4 // seccomp_data.arch
	)
	var p []unix.SockFilter
	p = append(p,
		unix.SockFilter{Code: ldAbs, K: offArc},
		unix.SockFilter{Code: jeq, Jt: 1, Jf: 0, K: arch}, // right architecture: skip the kill
		unix.SockFilter{Code: ret, K: retKill},
		unix.SockFilter{Code: ldAbs, K: offNr},
	)
	n := len(denied)
	for i, nr := range denied {
		// a match jumps over the remaining compares and the allow, to the errno return
		p = append(p, unix.SockFilter{Code: jeq, Jt: uint8(n - i), Jf: 0, K: nr})
	}
	p = append(p,
		unix.SockFilter{Code: ret, K: retAllow},
		unix.SockFilter{Code: ret, K: retErrno | uint32(syscall.EPERM)},
	)
	return p
}
