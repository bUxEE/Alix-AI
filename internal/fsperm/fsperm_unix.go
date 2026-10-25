//go:build !windows

package fsperm

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func restrict(path string, dir bool) error {
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}

func check(path string, st os.FileInfo) error {
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible by other users (mode %04o): %w", path, st.Mode().Perm(), ErrNotOwnerOnly)
	}
	return nil
}

func lockFile(f *os.File) error   { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
