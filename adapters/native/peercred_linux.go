//go:build linux

package native

import (
	"net"
	"syscall"
)

// peerCreds returns the operating system's account of the process on the other
// end of a Unix socket.
func peerCreds(c net.Conn) (PeerCreds, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return PeerCreds{}, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return PeerCreds{}, false
	}
	var (
		cred *syscall.Ucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || gerr != nil || cred == nil {
		return PeerCreds{}, false
	}
	return PeerCreds{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}, true
}
