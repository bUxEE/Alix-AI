package localai

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

// Secret holds the LocalAI API key. Every way of printing it (fmt verbs,
// JSON, text, slog) yields "[REDACTED]", so a Secret that ends up in a log
// line, an error or a diagnostics dump never reveals the value. The value is
// reachable only through the unexported reveal, used by the transport when it
// attaches the Authorization header.
//
// The value sits behind a pointer on purpose: fmt cannot call Format on an
// unexported struct field, and a reflection dump of such a field would print
// a string's contents but only an address for a pointer.
type Secret struct{ value *string }

// NewSecret wraps a key value that the caller obtained from a secret store.
func NewSecret(value string) Secret {
	if value == "" {
		return Secret{}
	}
	return Secret{value: &value}
}

// IsSet reports whether a key is held.
func (s Secret) IsSet() bool { return s.value != nil && *s.value != "" }

func (s Secret) reveal() string {
	if s.value == nil {
		return ""
	}
	return *s.value
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

// maxSecretFileBytes bounds a key file; a LocalAI key is a short token.
const maxSecretFileBytes = 4096

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ResolveSecret turns a secret reference into a Secret. Two reference forms
// exist:
//
//	env:NAME    the value of the environment variable NAME
//	file:PATH   the first line of the file at PATH (a regular file; on
//	            POSIX systems it must not be accessible to group or others)
//
// The empty reference means "no key". A literal key is never accepted in
// place of a reference, and no error message repeats the reference's value,
// because a literal key pasted into a configuration file would otherwise be
// echoed by the very error that rejects it.
func ResolveSecret(ref string) (Secret, error) {
	switch {
	case ref == "":
		return Secret{}, nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		if !envNameRE.MatchString(name) {
			return Secret{}, errors.New("api key reference: env: needs a valid environment variable name")
		}
		v, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(v) == "" {
			return Secret{}, errors.New("api key reference: the environment variable it names is not set or empty")
		}
		return checkedSecret(v)
	case strings.HasPrefix(ref, "file:"):
		path := strings.TrimPrefix(ref, "file:")
		if path == "" || strings.ContainsRune(path, 0) {
			return Secret{}, errors.New("api key reference: file: needs a path")
		}
		return readSecretFile(path)
	}
	return Secret{}, errors.New("api key reference must start with env: or file: (a key itself is never accepted in configuration)")
}

func readSecretFile(path string) (Secret, error) {
	f, err := os.Open(path)
	if err != nil {
		return Secret{}, errors.New("api key reference: cannot open the key file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Secret{}, errors.New("api key reference: cannot stat the key file")
	}
	if !info.Mode().IsRegular() {
		return Secret{}, errors.New("api key reference: the key file is not a regular file")
	}
	if err := secretFileModeOK(info.Mode(), runtime.GOOS); err != nil {
		return Secret{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return Secret{}, errors.New("api key reference: cannot read the key file")
	}
	if len(data) > maxSecretFileBytes {
		return Secret{}, errors.New("api key reference: the key file is too large")
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return checkedSecret(strings.TrimRight(line, "\r \t"))
}

// secretFileModeOK refuses a key file that group or others can access
// (SEC-081: key and secret files are owner-only). Windows file modes carry no
// such bits, so the check is skipped there; the file's ACL is the operator's
// to restrict.
func secretFileModeOK(mode fs.FileMode, goos string) error {
	if goos == "windows" {
		return nil
	}
	if mode.Perm()&0o077 != 0 {
		return errors.New("api key reference: the key file is accessible to group or others; run chmod 600 on it")
	}
	return nil
}

// checkedSecret rejects values that cannot be a bearer token, notably anything
// with whitespace or control characters, which would split the header.
func checkedSecret(v string) (Secret, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Secret{}, errors.New("api key reference: the key is empty")
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f || r > 0x7e {
			return Secret{}, errors.New("api key reference: the key holds whitespace, control or non-ASCII characters")
		}
	}
	return NewSecret(v), nil
}
