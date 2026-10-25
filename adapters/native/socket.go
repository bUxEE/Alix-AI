package native

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// PeerCreds is the operating system's account of the process on the other end of
// a Unix socket (SO_PEERCRED): available on Linux only.
type PeerCreds struct {
	UID, GID, PID int
}

func isSocket(m fs.FileMode) bool {
	return m&fs.ModeSocket != 0 || (runtime.GOOS == "windows" && m&fs.ModeIrregular != 0)
}

// socketPermsOK is the policy for the socket the adapter is about to connect to
// (SEC-038, SEC-081): the socket must be accessible to its owner only, and its
// directory must not be writable by group or others, so another local user can
// neither have connected to it nor replaced it with a socket of their own.
// Windows modes carry no such bits, so nothing is checked there; the application
// key authenticates the bridge on every wire.
func socketPermsOK(file, dir fs.FileMode, goos string) error {
	if goos == "windows" {
		return nil
	}
	if !isSocket(file) {
		return errors.New("the path is not a socket")
	}
	if file.Perm()&0o077 != 0 {
		return errors.New("the socket is accessible to group or others")
	}
	if dir.Perm()&0o022 != 0 && dir&fs.ModeSticky == 0 {
		return errors.New("the socket directory is writable by group or others")
	}
	return nil
}

// checkSocket applies socketPermsOK to a path. A path that does not exist is not
// an error here: connecting to it fails as any unreachable bridge does.
func checkSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil
	}
	return socketPermsOK(fi.Mode(), di.Mode(), runtime.GOOS)
}
