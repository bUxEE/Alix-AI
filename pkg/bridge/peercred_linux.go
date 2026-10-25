//go:build linux

package bridge

import (
	"net"
	"syscall"
)

// peerCredentials returns the operating system's account of the process on the
// other end of a Unix socket.
func peerCredentials(c net.Conn) (PeerCredentials, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return PeerCredentials{}, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return PeerCredentials{}, false
	}
	var (
		cred *syscall.Ucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || gerr != nil || cred == nil {
		return PeerCredentials{}, false
	}
	return PeerCredentials{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}, true
}
