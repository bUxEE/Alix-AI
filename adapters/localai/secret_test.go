package localai

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSecretNeverPrintsItsValue(t *testing.T) {
	key := newTestKey(t)
	s := NewSecret(key)
	if !s.IsSet() || s.reveal() != key {
		t.Fatal("the secret does not hold its value")
	}
	type holder struct {
		Exported Secret
		private  Secret
		Ptr      *Secret
	}
	h := holder{Exported: s, private: s, Ptr: &s}
	for name, out := range map[string]string{
		"%v":            fmt.Sprintf("%v", s),
		"%s":            fmt.Sprintf("%s", s),
		"%+v":           fmt.Sprintf("%+v", s),
		"%#v":           fmt.Sprintf("%#v", s),
		"%q":            fmt.Sprintf("%q", s),
		"%x":            fmt.Sprintf("%x", s),
		"%d":            fmt.Sprintf("%d", s),
		"%10.3s":        fmt.Sprintf("%10.3s", s),
		"String":        s.String(),
		"struct %+v":    fmt.Sprintf("%+v", h),
		"struct %#v":    fmt.Sprintf("%#v", h),
		"struct %v":     fmt.Sprintf("%v", h),
		"pointer %+v":   fmt.Sprintf("%+v", &s),
		"guard %+v":     fmt.Sprintf("%+v", guardTransport{key: s}),
		"guard %#v":     fmt.Sprintf("%#v", &guardTransport{key: s}),
		"slice %v":      fmt.Sprintf("%v", []Secret{s}),
		"map %v":        fmt.Sprintf("%v", map[string]Secret{"k": s}),
		"error wrap %v": fmt.Errorf("failed with %v", s).Error(),
	} {
		if strings.Contains(out, key) {
			t.Errorf("%s prints the key: %q", name, out)
		}
	}
	j, err := json.Marshal(map[string]any{"key": s, "holder": h})
	if err != nil || strings.Contains(string(j), key) || !strings.Contains(string(j), "[REDACTED]") {
		t.Errorf("json = %s, err = %v", j, err)
	}
	txt, _ := s.MarshalText()
	if string(txt) != "[REDACTED]" {
		t.Errorf("MarshalText = %q", txt)
	}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "key", s, slog.Any("again", s))
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "key", s, slog.Any("again", s))
	if strings.Contains(sb.String(), key) {
		t.Errorf("slog prints the key: %s", sb.String())
	}
	if (Secret{}).IsSet() || NewSecret("").IsSet() {
		t.Error("an empty secret reports as set")
	}
}

func writeKeyFile(t *testing.T, content string, mode fs.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "localai.key")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveSecret(t *testing.T) {
	key := newTestKey(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", key)
	t.Setenv("ALIXAI_TEST_EMPTY_KEY", "  ")
	t.Setenv("ALIXAI_TEST_SPACED_KEY", "two words")

	ok := map[string]string{
		"env:ALIXAI_TEST_LOCALAI_KEY":                             key,
		"file:" + writeKeyFile(t, key+"\n", 0o600):                key,
		"file:" + writeKeyFile(t, key+"\r\nsecond line\n", 0o600): key,
		"file:" + writeKeyFile(t, key, 0o600):                     key,
	}
	for ref, want := range ok {
		s, err := ResolveSecret(ref)
		if err != nil || s.reveal() != want {
			t.Errorf("ResolveSecret(%.12q...) = set %v, err %v", ref, s.IsSet(), err)
		}
	}
	if s, err := ResolveSecret(""); err != nil || s.IsSet() {
		t.Errorf("the empty reference must mean no key: %v, %v", s.IsSet(), err)
	}

	pasted := "sk-live-pasted-literal-key"
	bad := []string{
		pasted,                             // a literal key instead of a reference
		"env:" + pasted,                    // a literal key behind env:
		"literal:" + pasted,                // an unknown scheme
		"env:",                             // no name
		"env:1BAD",                         // not a variable name
		"env:ALIXAI_TEST_NOT_SET_ANYWHERE", // unset
		"env:ALIXAI_TEST_EMPTY_KEY",        // blank
		"env:ALIXAI_TEST_SPACED_KEY",       // whitespace would split the header
		"file:",                            // no path
		"file:" + filepath.Join(t.TempDir(), "missing"),
		"file:" + t.TempDir(), // a directory
		"file:" + writeKeyFile(t, "\n", 0o600),
		"file:" + writeKeyFile(t, "tab\there", 0o600),
		"file:" + writeKeyFile(t, "café", 0o600),
		"file:" + writeKeyFile(t, strings.Repeat("k", 5000), 0o600),
	}
	for _, ref := range bad {
		s, err := ResolveSecret(ref)
		if err == nil || s.IsSet() {
			t.Errorf("ResolveSecret(%.20q...) was accepted", ref)
			continue
		}
		if strings.Contains(err.Error(), pasted) || strings.Contains(err.Error(), "ALIXAI_TEST") {
			t.Errorf("error repeats the reference: %v", err)
		}
	}
}

// SEC-081
// A key file other local users can read is refused. The mode check
// is a pure function so it is tested on every platform.
func TestSecretFileModeIsOwnerOnly(t *testing.T) {
	for _, tt := range []struct {
		mode fs.FileMode
		goos string
		ok   bool
	}{
		{0o600, "linux", true},
		{0o400, "linux", true},
		{0o700, "linux", true},
		{0o640, "linux", false},
		{0o604, "linux", false},
		{0o644, "linux", false},
		{0o666, "darwin", false},
		{0o644, "windows", true}, // no POSIX bits on Windows; the ACL is the operator's
	} {
		if err := secretFileModeOK(tt.mode, tt.goos); (err == nil) != tt.ok {
			t.Errorf("mode %o on %s: err = %v, want ok=%v", tt.mode, tt.goos, err, tt.ok)
		}
	}
	if runtime.GOOS != "windows" {
		p := writeKeyFile(t, "TESTONLY-key", 0o644)
		if _, err := ResolveSecret("file:" + p); err == nil {
			t.Error("a world-readable key file was read")
		}
	}
}
