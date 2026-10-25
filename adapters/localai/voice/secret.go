package voice

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

// apiKey holds the LocalAI API key. Every way of printing it yields
// "[REDACTED]". Package localai's Secret cannot be used here (its value is
// reachable only inside that package), so the voice client keeps its own.
type apiKey struct{ value *string }

func newAPIKey(v string) apiKey {
	if v == "" {
		return apiKey{}
	}
	return apiKey{value: &v}
}

func (k apiKey) isSet() bool { return k.value != nil && *k.value != "" }

func (k apiKey) reveal() string {
	if k.value == nil {
		return ""
	}
	return *k.value
}

func (apiKey) String() string               { return redacted }
func (apiKey) GoString() string             { return redacted }
func (apiKey) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (apiKey) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (apiKey) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (apiKey) LogValue() slog.Value         { return slog.StringValue(redacted) }

const maxKeyFileBytes = 4096

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// resolveKeyRef turns "env:NAME" or "file:PATH" into a key, with the rules of
// localai.ResolveSecret. The empty reference means no key. Error messages
// never repeat the reference's value.
func resolveKeyRef(ref string) (apiKey, error) {
	switch {
	case ref == "":
		return apiKey{}, nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		if !envNameRE.MatchString(name) {
			return apiKey{}, errors.New("api key reference: env: needs a valid environment variable name")
		}
		v, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(v) == "" {
			return apiKey{}, errors.New("api key reference: the environment variable it names is not set or empty")
		}
		return checkedKey(v)
	case strings.HasPrefix(ref, "file:"):
		return readKeyFile(strings.TrimPrefix(ref, "file:"))
	}
	return apiKey{}, errors.New("api key reference must start with env: or file:")
}

func readKeyFile(path string) (apiKey, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return apiKey{}, errors.New("api key reference: file: needs a path")
	}
	f, err := os.Open(path)
	if err != nil {
		return apiKey{}, errors.New("api key reference: cannot open the key file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return apiKey{}, errors.New("api key reference: the key file is not a regular file")
	}
	if err := keyFileModeOK(info.Mode(), runtime.GOOS); err != nil {
		return apiKey{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil || len(data) > maxKeyFileBytes {
		return apiKey{}, errors.New("api key reference: cannot read the key file")
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

// checkedKey rejects values that cannot be a bearer token (whitespace,
// control or non-ASCII characters would split the header).
func checkedKey(v string) (apiKey, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return apiKey{}, errors.New("api key reference: the key is empty")
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f || r > 0x7e {
			return apiKey{}, errors.New("api key reference: the key holds whitespace, control or non-ASCII characters")
		}
	}
	return newAPIKey(v), nil
}
