// Package fsperm makes files and directories owner only and checks that they
// are (SEC-081). On POSIX systems owner only is mode 0700 for a directory and
// 0600 for a file, group and others having no bit. On Windows, where Go's mode
// bits mean nothing and a new file inherits the access rules of its parent, it
// is a DACL whose allow entries, inherited or explicit, name only the user the
// process runs as, LocalSystem or the built-in Administrators (the last two can
// take any file anyway, so an entry for them grants nothing new). Restrict
// writes a protected DACL (no inherited entries) with one entry for the user,
// which what is created in a restricted directory inherits.
//
// The rule of the callers: a directory or file this code creates is restricted
// right after it is created and before anything secret is written into it; an
// existing one is checked, never repaired in place, because someone else may
// already have read it.
package fsperm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotOwnerOnly is wrapped by Check when the path is readable or writable
// by someone other than the owner.
var ErrNotOwnerOnly = errors.New("permissions are not owner only")

// Restrict makes path owner only: mode 0700 (dir) or 0600 (file) on POSIX, a
// protected DACL holding one full-control entry for the current user on
// Windows (a directory's entry is inherited by what is created in it).
func Restrict(path string, dir bool) error { return restrict(path, dir) }

// Check returns an error wrapping ErrNotOwnerOnly when path is not owner only
// (or is not the kind dir says), and another error when it cannot be examined.
func Check(path string, dir bool) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() != dir {
		kind := "a file"
		if dir {
			kind = "a directory"
		}
		return fmt.Errorf("%s is not %s: %w", path, kind, ErrNotOwnerOnly)
	}
	return check(path, st)
}

// MkdirOwnerOnly creates dir when it does not exist (its missing parents with
// mode 0700, the defaults of the platform applying on Windows) and restricts
// dir itself owner only. created reports whether it made dir; an existing dir
// is left exactly as it is (use Check on it).
func MkdirOwnerOnly(dir string) (created bool, err error) {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return false, fmt.Errorf("%s exists and is not a directory", dir)
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil // made meanwhile by someone else: theirs, not ours to change
		}
		return false, err
	}
	if err := restrict(dir, true); err != nil {
		return true, err
	}
	return true, nil
}

// CreateFile creates path, which must not exist (O_EXCL: an existing file is
// an error wrapping os.ErrExist), mode 0600, and restricts it owner only before
// it returns, so nothing is written into it while it is open to others. On a
// failure the file is removed again.
func CreateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := restrict(path, false); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return f, nil
}

// Lock takes an exclusive advisory lock on the file at path (created mode 0600
// when missing; it holds no data) and blocks until it has it. The returned
// function releases it. Processes that Lock the same path exclude each other;
// the lock goes with the process if it dies.
func Lock(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		uerr := unlockFile(f)
		if cerr := f.Close(); uerr == nil {
			uerr = cerr
		}
		return uerr
	}, nil
}
