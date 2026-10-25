package bridge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxSocketPath is the longest Unix socket path every supported system binds
// (sun_path is 104 bytes on macOS and 108 on Linux and Windows, with the NUL).
const maxSocketPath = 103

// ListenUnix listens on a Unix domain socket (AF_UNIX, which Windows 10 and later
// also provides) that only its owner can connect to. The directory is created
// owner-only when it does not exist; one that exists must not be writable by
// group or others, so nobody else can replace the socket. A stale socket file is
// replaced; a file that is not a socket, and a socket something is listening on,
// are refused. The socket is made 0600 right after it is bound, and the listener
// removes it when closed. On Windows the file mode carries no such bits and the
// application key is the control.
func ListenUnix(path string) (net.Listener, error) {
	if !(filepath.IsAbs(path) || strings.HasPrefix(path, "/")) || len(path) > maxSocketPath {
		return nil, fmt.Errorf("bridge: the socket path must be absolute and at most %d bytes", maxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("bridge: cannot create the socket directory: %w", err)
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("bridge: cannot read the socket directory: %w", err)
	} else if err := socketPermsOK(0o600, fi.Mode(), runtime.GOOS, false); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if !isSocket(fi.Mode()) {
			return nil, errors.New("bridge: the socket path exists and is not a socket, so it was not replaced")
		}
		d := net.Dialer{Timeout: time.Second}
		if c, derr := d.DialContext(context.Background(), "unix", path); derr == nil {
			_ = c.Close()
			return nil, errors.New("bridge: something is already listening on the socket path")
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("bridge: cannot remove the stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("bridge: cannot read the socket path: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("bridge: cannot listen on the socket: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("bridge: cannot restrict the socket to its owner: %w", err)
		}
	}
	return l, nil
}

// ListenLoopback listens on 127.0.0.1 only: the fallback where Unix sockets are
// not available. port 0 picks a free one (read it from the listener's Addr). The
// application key is the only control on this wire, so give it the same care.
func ListenLoopback(port int) (net.Listener, error) {
	if port < 0 || port > 65535 {
		return nil, errors.New("bridge: the port is out of range")
	}
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
}

func isSocket(m fs.FileMode) bool {
	return m&fs.ModeSocket != 0 || (runtime.GOOS == "windows" && m&fs.ModeIrregular != 0)
}

// socketPermsOK is the policy for where a socket lives (SEC-038, SEC-081): the
// socket file is accessible to its owner only, and the directory cannot be
// written by group or others, so another user can neither connect to the socket
// nor replace it. Windows modes carry no such bits, so nothing is checked there.
// file is skipped when checkFile is false (the socket does not exist yet).
func socketPermsOK(file, dir fs.FileMode, goos string, checkFile bool) error {
	if goos == "windows" {
		return nil
	}
	if checkFile && file.Perm()&0o077 != 0 {
		return errors.New("bridge: the socket is accessible to group or others; it must be owner-only (0600)")
	}
	if dir.Perm()&0o022 != 0 && dir&fs.ModeSticky == 0 {
		return errors.New("bridge: the socket directory is writable by group or others; it must not be")
	}
	return nil
}
