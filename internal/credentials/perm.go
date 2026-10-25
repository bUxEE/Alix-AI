package credentials

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"

	"github.com/buxee/alix-ai/internal/fsperm"
)

// Files and directories of this package are owner only (SEC-081, internal/fsperm):
// mode 0700 and 0600 on POSIX systems; on Windows a protected DACL (no inherited
// entries) holding one entry, full control for the user the service runs as. A
// directory or file that already exists with looser permissions is refused, never
// repaired in place: someone else may already have read it.

// restrictOwnerOnly makes path owner only (fsperm.Restrict).
func restrictOwnerOnly(path string, dir bool) error { return fsperm.Restrict(path, dir) }

// checkOwnerOnly refuses a path that is not owner only with
// ErrInsecurePermissions; a path that cannot be examined is errBackend.
func checkOwnerOnly(path string, dir bool) error {
	err := fsperm.Check(path, dir)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fsperm.ErrNotOwnerOnly):
		return ErrInsecurePermissions
	}
	return errBackend
}

// ensureSecureDir creates dir (and its parents, with default permissions) when
// missing, creating dir itself owner only, and refuses an existing dir that is
// not.
func ensureSecureDir(dir string) error {
	st, err := os.Stat(dir)
	switch {
	case err == nil:
		if !st.IsDir() {
			return errBackend
		}
		return checkOwnerOnly(dir, true)
	case !errors.Is(err, os.ErrNotExist):
		return errBackend
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return errBackend
	}
	// Made under a private name, restricted, then renamed into place: a process
	// creating it at the same moment sees either no directory or an owner-only one,
	// never one that is still waiting for its ACL.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errBackend
	}
	tmp := dir + "." + hexString(nonce[:]) + ".tmp"
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return errBackend
	}
	if err := restrictOwnerOnly(tmp, true); err != nil {
		_ = os.Remove(tmp)
		return errBackend
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.Remove(tmp)
		if _, serr := os.Stat(dir); serr != nil {
			return errBackend
		}
		// another process made it first
	}
	return checkOwnerOnly(dir, true)
}

// writeFileOwnerOnly writes data to path through a temporary file in the same
// directory that is owner only from before the first byte is written, fsynced and
// renamed over path. excl makes it fail when path exists (os.ErrExist), without
// a moment in which path exists half written.
func writeFileOwnerOnly(path string, data []byte, excl bool) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errBackend
	}
	tmp := path + "." + hexString(nonce[:]) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errBackend
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := restrictOwnerOnly(tmp, false); err != nil {
		_ = f.Close()
		return errBackend
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errBackend
	}
	if excl {
		// A hard link to the finished file appears complete or not at all, and
		// fails when path exists: of two processes creating the file at once
		// exactly one wins and the other reads its file (the check-then-rename
		// this replaces let the second silently overwrite the first).
		err := os.Link(tmp, path)
		switch {
		case err == nil:
			return nil // the deferred remove drops the temporary name
		case errors.Is(err, os.ErrExist):
			return os.ErrExist
		}
		// A file system without hard links: check and rename, which only the
		// caller's lock (KeyFileSource) keeps from racing.
		if _, err := os.Lstat(path); err == nil {
			return os.ErrExist
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return errBackend
	}
	ok = true
	return nil
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
