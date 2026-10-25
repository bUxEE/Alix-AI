package credentials

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultEnvPrefix is the prefix an environment reference must have, so a
// reference cannot name PATH, a cloud key of the host or another service's
// secret.
const DefaultEnvPrefix = "ALIXAI_SECRET_"

var envNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// EnvBackend resolves a reference to an environment variable of the service
// process: for headless installs where a supervisor injects the key. It is read
// only and reads the variable on every call. Only names with the configured
// prefix are readable.
type EnvBackend struct {
	prefix string
	lookup func(string) (string, bool)
}

var _ Backend = (*EnvBackend)(nil)

// NewEnvBackend returns an environment backend; prefix "" means DefaultEnvPrefix.
func NewEnvBackend(prefix string) *EnvBackend {
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}
	return &EnvBackend{prefix: prefix, lookup: os.LookupEnv}
}

// Name implements Backend.
func (*EnvBackend) Name() BackendName { return BackendEnv }

// ReadOnly implements Backend.
func (*EnvBackend) ReadOnly() bool { return true }

// ValidLocator reports whether name is a variable this backend may read.
func (b *EnvBackend) ValidLocator(name string) bool {
	return envNameRE.MatchString(name) && strings.HasPrefix(name, b.prefix) && len(name) > len(b.prefix)
}

// Get implements Backend.
func (b *EnvBackend) Get(ctx context.Context, slot Slot) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !b.ValidLocator(slot.Locator) {
		return "", ErrNotFound
	}
	v, ok := b.lookup(slot.Locator)
	if !ok || strings.TrimSpace(v) == "" {
		return "", ErrNotFound
	}
	return Secret(strings.TrimSpace(v)), nil
}

// Put implements Backend.
func (*EnvBackend) Put(context.Context, Slot, Secret) error { return ErrReadOnly }

// Delete implements Backend: there is nothing the service stores.
func (*EnvBackend) Delete(context.Context, Slot) error { return nil }

// maxSecretFileBytes bounds a secret file: a key or a token, not a document.
const maxSecretFileBytes = 16 << 10

// FileBackend resolves a reference to a file inside one directory: for headless
// installs with mounted secrets (Docker or Podman secrets, systemd credentials,
// Kubernetes). It is read only and reads the file on every call, so replacing the
// file rotates the credential without a restart. The locator is a plain file
// name: no separator, no "..", no link, and the file must be a regular file that
// only the owner can read.
type FileBackend struct{ dir string }

var _ Backend = (*FileBackend)(nil)

var fileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// NewFileBackend returns a file backend over dir, which must exist.
func NewFileBackend(dir string) (*FileBackend, error) {
	if dir == "" {
		return nil, errBackend
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errBackend
	}
	return &FileBackend{dir: abs}, nil
}

// Name implements Backend.
func (*FileBackend) Name() BackendName { return BackendFile }

// ReadOnly implements Backend.
func (*FileBackend) ReadOnly() bool { return true }

// ValidLocator reports whether name is a file name this backend may read.
func (*FileBackend) ValidLocator(name string) bool {
	return fileNameRE.MatchString(name) && !strings.Contains(name, "..")
}

// Get implements Backend.
func (b *FileBackend) Get(ctx context.Context, slot Slot) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !b.ValidLocator(slot.Locator) {
		return "", ErrNotFound
	}
	path := filepath.Join(b.dir, slot.Locator)
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", ErrNotFound
	case err != nil:
		return "", errBackend
	case !st.Mode().IsRegular(): // a link or a special file: refused
		return "", errBackend
	case st.Size() > maxSecretFileBytes:
		return "", errBackend
	}
	if err := checkOwnerOnly(path, false); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errBackend
	}
	defer f.Close()
	if st2, err := f.Stat(); err != nil || !os.SameFile(st, st2) {
		return "", errBackend
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil || len(raw) > maxSecretFileBytes {
		return "", errBackend
	}
	v := strings.TrimRight(string(raw), "\r\n")
	wipe(raw)
	if strings.TrimSpace(v) == "" {
		return "", ErrNotFound
	}
	return Secret(v), nil
}

// Put implements Backend.
func (*FileBackend) Put(context.Context, Slot, Secret) error { return ErrReadOnly }

// Delete implements Backend: the file belongs to whoever provisioned it.
func (*FileBackend) Delete(context.Context, Slot) error { return nil }
