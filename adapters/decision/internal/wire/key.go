package wire

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strings"
)

const redacted = "[REDACTED]"

// Key holds an API key. Every way of printing it yields "[REDACTED]"; the value
// is reachable only through the unexported reveal, used when the guard attaches
// the Authorization header.
type Key struct{ value *string }

// NewKey wraps a key value obtained from a secret store.
func NewKey(v string) Key {
	if v == "" {
		return Key{}
	}
	return Key{value: &v}
}

// IsSet reports whether a key is held.
func (k Key) IsSet() bool { return k.value != nil && *k.value != "" }

func (k Key) reveal() string {
	if k.value == nil {
		return ""
	}
	return *k.value
}

func (Key) String() string               { return redacted }
func (Key) GoString() string             { return redacted }
func (Key) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (Key) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Key) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Key) LogValue() slog.Value         { return slog.StringValue(redacted) }

const maxKeyFileBytes = 4096

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidKeyRef reports whether ref has the form of a key reference ("env:NAME",
// "file:PATH") without reading it. The empty reference is valid (no key).
func ValidKeyRef(ref string) bool {
	return ref == "" || strings.HasPrefix(ref, "env:") || strings.HasPrefix(ref, "file:")
}

// ResolveKeyRef turns "env:NAME" or "file:PATH" into a key, with the rules of
// localai.ResolveSecret. The empty reference means no key. A literal key is
// never accepted and no message repeats the reference.
func ResolveKeyRef(ref string) (Key, error) {
	switch {
	case ref == "":
		return Key{}, nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		if !envNameRE.MatchString(name) {
			return Key{}, errors.New("api key reference: env: needs a valid environment variable name")
		}
		v, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(v) == "" {
			return Key{}, errors.New("api key reference: the environment variable it names is not set or empty")
		}
		return checkedKey(v)
	case strings.HasPrefix(ref, "file:"):
		return readKeyFile(strings.TrimPrefix(ref, "file:"))
	}
	return Key{}, errors.New("api key reference must start with env: or file:")
}

func readKeyFile(path string) (Key, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return Key{}, errors.New("api key reference: file: needs a path")
	}
	f, err := os.Open(path)
	if err != nil {
		return Key{}, errors.New("api key reference: cannot open the key file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Key{}, errors.New("api key reference: the key file is not a regular file")
	}
	if err := keyFileModeOK(info.Mode(), runtime.GOOS); err != nil {
		return Key{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil || len(data) > maxKeyFileBytes {
		return Key{}, errors.New("api key reference: cannot read the key file")
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return checkedKey(strings.TrimRight(line, "\r \t"))
}

// keyFileModeOK refuses a key file that group or others can access (SEC-081).
// Windows file modes carry no such bits, so the check is skipped there.
func keyFileModeOK(mode fs.FileMode, goos string) error {
	if goos != "windows" && mode.Perm()&0o077 != 0 {
		return errors.New("api key reference: the key file is accessible to group or others; run chmod 600 on it")
	}
	return nil
}

func checkedKey(v string) (Key, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Key{}, errors.New("api key reference: the key is empty")
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f || r > 0x7e {
			return Key{}, errors.New("api key reference: the key holds whitespace, control or non-ASCII characters")
		}
	}
	return NewKey(v), nil
}
