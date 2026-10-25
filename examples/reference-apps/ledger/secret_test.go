package ledger

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadSecretRef(t *testing.T) {
	t.Setenv("LEDGER_TEST_SECRET", "s3cret-value")
	if b, err := ReadSecretRef("env:LEDGER_TEST_SECRET"); err != nil || string(b) != "s3cret-value" {
		t.Fatalf("env = %q, %v", b, err)
	}
	for _, bad := range []string{"", "plain-secret", "env:LEDGER_TEST_UNSET_VARIABLE", "file:relative/path", "file:" + filepath.Join(t.TempDir(), "missing"), "cred:ledger-docs"} {
		if b, err := ReadSecretRef(bad); err == nil {
			t.Errorf("ReadSecretRef(%q) = %q, want an error", bad, b)
		}
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadSecretRef("file:" + path); err != nil || string(b) != "file-secret" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecretRef("file:" + path); err == nil {
			t.Fatal("a secret file readable by others was read")
		}
	}
}
