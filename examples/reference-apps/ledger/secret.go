package ledger

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ReadSecretRef reads a secret from a reference: env:NAME (the variable's value) or
// file:/absolute/path (the file's content, trimmed). It is how the application is
// given its bridge key, its event key and its document API credential: nothing secret
// is ever a command-line argument, a default or a fixture. On POSIX a file must not be
// readable by group or others; Windows file modes carry no such bits.
func ReadSecretRef(ref string) ([]byte, error) {
	switch {
	case strings.HasPrefix(ref, "env:"):
		v, ok := os.LookupEnv(strings.TrimPrefix(ref, "env:"))
		if !ok || v == "" {
			return nil, errors.New("ledger: the environment variable of the secret reference is not set")
		}
		return []byte(v), nil
	case strings.HasPrefix(ref, "file:"):
		path := strings.TrimPrefix(ref, "file:")
		if !filepath.IsAbs(path) && !strings.HasPrefix(path, "/") {
			return nil, errors.New("ledger: a file secret reference must be an absolute path")
		}
		fi, err := os.Stat(path)
		if err != nil {
			return nil, errors.New("ledger: the secret file cannot be read")
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&fs.FileMode(0o077) != 0 {
			return nil, errors.New("ledger: the secret file must not be readable by group or others")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("ledger: the secret file cannot be read")
		}
		return []byte(strings.TrimSpace(string(b))), nil
	}
	return nil, errors.New("ledger: a secret reference is env:NAME or file:/absolute/path")
}
