package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const redacted = "[REDACTED]"

// Secret holds the application key. Every way of printing it (fmt verbs, JSON,
// text, slog) yields "[REDACTED]". The value is reachable only inside this
// package, where the handshake computes its HMACs.
type Secret struct{ v *[]byte }

// NewSecret wraps key material that the caller obtained from a secret store.
func NewSecret(value []byte) Secret {
	if len(value) == 0 {
		return Secret{}
	}
	b := append([]byte(nil), value...)
	return Secret{v: &b}
}

// IsSet reports whether a key is held.
func (s Secret) IsSet() bool { return s.v != nil && len(*s.v) > 0 }

func (s Secret) value() []byte {
	if s.v == nil {
		return nil
	}
	return *s.v
}

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter so that no verb, flag or width prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// maxKeyFileBytes bounds a key file; an application key is a short token.
const maxKeyFileBytes = 4096

// CheckKeyRef checks the form of a key reference without reading it: env:NAME or
// file:ABSOLUTE_PATH. A literal key is never accepted in configuration (a
// package is signed, distributed and read by people), and no error repeats the
// reference's value, because a key pasted in by mistake would be echoed by the
// very error that rejects it.
func CheckKeyRef(ref string) error {
	switch {
	case strings.HasPrefix(ref, "env:"):
		if !envNameRE.MatchString(strings.TrimPrefix(ref, "env:")) {
			return errors.New("env: needs a valid environment variable name")
		}
	case strings.HasPrefix(ref, "file:"):
		path := strings.TrimPrefix(ref, "file:")
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return errors.New("file: needs an absolute path")
		}
	default:
		return errors.New("a key reference must start with env: or file: (a key itself is never accepted in configuration)")
	}
	return nil
}

// ResolveKey reads the application key a reference names. A file must be a
// regular file that group and others cannot access (SEC-081; Windows modes carry
// no such bits, so the file's ACL is the operator's to restrict there). The key
// is the first line, trimmed, and must be at least MinKeyBytes long.
func ResolveKey(ref string) (Secret, error) {
	if err := CheckKeyRef(ref); err != nil {
		return Secret{}, err
	}
	var text string
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		v, set := os.LookupEnv(name)
		if !set {
			return Secret{}, errors.New("the environment variable the key reference names is not set")
		}
		text = v
	} else {
		var err error
		if text, err = readKeyFile(strings.TrimPrefix(ref, "file:")); err != nil {
			return Secret{}, err
		}
	}
	text = strings.TrimSpace(text)
	if len(text) < MinKeyBytes {
		return Secret{}, fmt.Errorf("the application key is shorter than %d bytes", MinKeyBytes)
	}
	if strings.ContainsAny(text, " \t\r\n\x00") {
		return Secret{}, errors.New("the application key contains whitespace")
	}
	return NewSecret([]byte(text)), nil
}

func readKeyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open the key file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("the key file is not a regular file")
	}
	if err := keyFileModeOK(info.Mode(), runtime.GOOS); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil || len(data) > maxKeyFileBytes {
		return "", errors.New("cannot read the key file, or it is too large")
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return line, nil
}

// keyFileModeOK refuses a key file that group or others can access (SEC-081).
func keyFileModeOK(mode fs.FileMode, goos string) error {
	if goos == "windows" {
		return nil
	}
	if mode.Perm()&0o077 != 0 {
		return errors.New("the key file is accessible to group or others; run chmod 600 on it")
	}
	return nil
}
