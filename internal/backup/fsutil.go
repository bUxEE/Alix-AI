package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// copyHashed copies the regular file src to dst (which must not exist) with mode
// 0600, fsyncs it and returns the SHA-256 and size of what it copied. A src
// that is a link or a special file is refused: a backup follows nothing.
func copyHashed(src, dst string) (sum string, size int64, err error) {
	st, err := os.Lstat(src)
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, contracts.Newf(contracts.ErrValidationFailed, "%s is a link or special file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// hashRegular is hashFile for a file that must be a regular file, refused as
// copyHashed refuses it otherwise: a backup follows nothing.
func hashRegular(path string) (string, int64, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, contracts.Newf(contracts.ErrValidationFailed, "%s is a link or special file", path)
	}
	return hashFile(path)
}

// hashFile returns the SHA-256 and size of the file at path.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, fsynced and renamed, with mode 0600.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func unavailable(err error, what string) error {
	return contracts.Wrap(contracts.ErrUnavailable, err, what)
}
