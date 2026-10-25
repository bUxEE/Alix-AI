//go:build linux && arm64

package confine

import "golang.org/x/sys/unix"

const auditArchAarch64 = 0xC00000B7

// deniedSyscalls is the seccomp deny list for linux/arm64 and the audit
// architecture the filter insists on. arm64 has no fork or vfork (the C library
// uses clone), which the filter leaves alone because the Go runtime creates
// threads with it; execve is what matters for "no program is started".
func deniedSyscalls() ([]uint32, uint32, error) {
	return []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN,
		unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_SENDTO, unix.SYS_RECVFROM, unix.SYS_SENDMSG,
		unix.SYS_RECVMSG, unix.SYS_SENDMMSG, unix.SYS_RECVMMSG,
		unix.SYS_EXECVE, unix.SYS_EXECVEAT,
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KCMP,
		unix.SYS_KEXEC_LOAD, unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
		unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_SETNS,
		unix.SYS_UNSHARE, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	}, auditArchAarch64, nil
}
