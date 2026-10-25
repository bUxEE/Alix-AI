package cli

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/fsperm/fspermtest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
)

// minimalConfig is a config file with a data directory and nothing else: what
// the operator commands that only need the store read.
func minimalConfig(t *testing.T, extra string) (cfgPath, dataDir string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	cfgPath = filepath.Join(t.TempDir(), "alixai.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+yamlPath(dataDir)+"\n"+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// openData opens the store of dataDir for a test to look at.
func openData(t *testing.T, dataDir string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// SEC-081: the platform signing key is created owner-only (directory 0700, file
// 0600, set at creation, never widened), its public half is registered for the
// platform in the trust store, and the private key is written to its file and
// nowhere else. Owner only is checked on every platform (internal/fsperm: the
// modes on POSIX, a protected owner-only ACL on Windows, M2); the exact mode
// assertions run on POSIX.
func TestKeysInitCreatesCheckpointKeyOwnerOnly(t *testing.T) {
	cfg, dataDir := minimalConfig(t, "")
	code, out, errOut := runCLI(t, "keys", "init", "-config", cfg)
	if code != ExitOK {
		t.Fatalf("keys init = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	dir := filepath.Join(dataDir, "keys")
	privPath := filepath.Join(dir, defaultCheckpointKeyID+".key")
	pubPath := filepath.Join(dir, defaultCheckpointKeyID+".pub")

	priv, warnings, err := trust.LoadPrivateKeyFile(privPath)
	if err != nil {
		t.Fatalf("the key file cannot be loaded: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the key file loads with warnings: %v", warnings)
	}
	pub := priv.Public().(ed25519.PublicKey)
	if trust.IsTestOnlyKey(pub) {
		t.Fatal("keys init produced a published TEST-ONLY key")
	}
	raw, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := trust.DecodePublicKey(string(raw)); err != nil || !got.Equal(pub) {
		t.Errorf("the .pub file does not hold the public key: %v", err)
	}
	if strings.Contains(string(raw), "PRIVATE") {
		t.Error("the .pub file holds private material")
	}

	for path, isDir := range map[string]bool{dir: true, privPath: false, dataDir: true} {
		if err := fsperm.Check(path, isDir); err != nil {
			t.Errorf("%s is not owner only: %v", path, err)
		}
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{dir: 0o700, privPath: 0o600} {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != want {
				t.Errorf("%s has mode %04o, want %04o", path, st.Mode().Perm(), want)
			}
		}
		if st, err := os.Stat(dataDir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("the data directory is not owner-only: %v %v", st, err)
		}
	}

	// The private key is in its file and in no output.
	pem, _ := os.ReadFile(privPath)
	for name, text := range map[string]string{"stdout": out, "stderr": errOut} {
		if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, strings.TrimSpace(strings.Split(string(pem), "\n")[1])) {
			t.Errorf("%s shows the private key:\n%s", name, text)
		}
	}
	if !strings.Contains(out, privPath) || !strings.Contains(out, trust.EncodePublicKey(pub)) {
		t.Errorf("the output does not say where the key is and what its public half is:\n%s", out)
	}

	// Registered for the platform: that is what lets a checkpoint it signs be believed.
	db := openData(t, dataDir)
	stored, err := trust.NewStore(db, clock.Real{}).LookupKey(context.Background(), trust.PlatformApplicationID, defaultCheckpointKeyID)
	if err != nil || stored.Status != trust.KeyActive || !stored.PublicKey.Equal(pub) {
		t.Fatalf("the trust store holds %+v, %v", stored, err)
	}
}

// keys init is idempotent and never overwrites: a second run keeps the key, and
// a key file next to a fresh database is registered instead of replaced.
func TestKeysInitNeverOverwritesAndRepairsRegistration(t *testing.T) {
	cfg, dataDir := minimalConfig(t, "")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	privPath := filepath.Join(dataDir, "keys", defaultCheckpointKeyID+".key")
	before, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "keys", "init", "-config", cfg)
	if code != ExitOK || !strings.Contains(out, "already present") {
		t.Fatalf("second keys init = %d\n%s\n%s", code, out, errOut)
	}
	if after, _ := os.ReadFile(privPath); string(after) != string(before) {
		t.Fatal("the second keys init changed the key file")
	}

	// A fresh database beside the restored key file: registered, not replaced.
	if err := os.Remove(filepath.Join(dataDir, store.DefaultFileName)); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(filepath.Join(dataDir, store.DefaultFileName+suffix))
	}
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK || !strings.Contains(out, "already present") {
		t.Fatalf("keys init on a fresh database = %d\n%s\n%s", code, out, errOut)
	}
	if after, _ := os.ReadFile(privPath); string(after) != string(before) {
		t.Fatal("repairing the registration changed the key file")
	}
	priv, _, _ := trust.LoadPrivateKeyFile(privPath)
	stored, err := trust.NewStore(openData(t, dataDir), clock.Real{}).LookupKey(context.Background(), trust.PlatformApplicationID, defaultCheckpointKeyID)
	if err != nil || !stored.PublicKey.Equal(priv.Public().(ed25519.PublicKey)) {
		t.Fatalf("the key was not registered again: %+v, %v", stored, err)
	}

	// A key id that is not a safe file name is a usage error, before anything is made.
	if code, _, errOut := runCLI(t, "keys", "init", "-config", cfg, "-key-id", "../escape"); code != ExitUsage {
		t.Errorf("keys init -key-id ../escape = %d (%s), want a usage error", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "escape.key")); err == nil {
		t.Error("a key file was written outside the keys directory")
	}
}

// A key file that other users can read is refused, not used (by mode on POSIX,
// by ACL on Windows).
func TestKeysInitRefusesALooseKeyFile(t *testing.T) {
	cfg, dataDir := minimalConfig(t, "")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	privPath := filepath.Join(dataDir, "keys", defaultCheckpointKeyID+".key")
	fspermtest.Loosen(t, privPath)
	code, _, errOut := runCLI(t, "keys", "init", "-config", cfg)
	if code != ExitError || !strings.Contains(errOut, "accessible by other users") {
		t.Fatalf("keys init with a world-readable key = %d: %s", code, errOut)
	}
}

// The keys that sign backups and integration-audit reports are refused too, like
// the checkpoint key, when other users can read them.
func TestBackupAndAuditSigningKeysRefuseALooseKeyFile(t *testing.T) {
	dir := t.TempDir()
	_, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	privPath, _, err := trust.WriteKeyFiles(dir, "loose", priv)
	if err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, privPath)
	env := &Env{Stderr: &strings.Builder{}}
	if _, err := backupSigner(env, dir, privPath, "loose", false); err == nil || !strings.Contains(err.Error(), "accessible by other users") {
		t.Errorf("backupSigner with a world-readable key = %v", err)
	}
	if _, err := loadSigningKey(env, "loose="+privPath, auditConfig{}); err == nil || !strings.Contains(err.Error(), "accessible by other users") {
		t.Errorf("loadSigningKey with a world-readable key = %v", err)
	}
}

func TestKeysSectionValidates(t *testing.T) {
	good := keysConfig{CheckpointEvery: 10, CheckpointInterval: 1}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid section: %v", err)
	}
	for name, c := range map[string]keysConfig{
		"NUL in the file":   {CheckpointKeyFile: "a\x00b", CheckpointInterval: 1},
		"unsafe key id":     {CheckpointKeyID: "../x", CheckpointInterval: 1},
		"negative every":    {CheckpointEvery: -1, CheckpointInterval: 1},
		"no sweep interval": {},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := keysSection.Validate(); err != nil {
		t.Errorf("the default section is invalid: %v", err)
	}
}
