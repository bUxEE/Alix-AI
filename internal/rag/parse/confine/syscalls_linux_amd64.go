//go:build linux && amd64

package confine

import "golang.org/x/sys/unix"

const auditArchX8664 = 0xC000003E

// deniedSyscalls is the seccomp deny list for linux/amd64 and the audit
// architecture the filter insists on (a 32-bit compatibility call is killed).
func deniedSyscalls() ([]uint32, uint32, error) {
	return []uint32{
		// sockets: no network, no local IPC through sockets
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN,
		unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_SENDTO, unix.SYS_RECVFROM, unix.SYS_SENDMSG,
		unix.SYS_RECVMSG, unix.SYS_SENDMMSG, unix.SYS_RECVMMSG,
		// starting programs
		unix.SYS_EXECVE, unix.SYS_EXECVEAT, unix.SYS_FORK, unix.SYS_VFORK,
		// reaching into other processes and the kernel
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KCMP,
		unix.SYS_KEXEC_LOAD, unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
		unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
		// namespaces and mounts
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_SETNS,
		unix.SYS_UNSHARE, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
		// io_uring can perform socket and file operations outside the filter
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	}, auditArchX8664, nil
}
