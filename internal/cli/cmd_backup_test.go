package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/store"
)

// backupRunCLI runs one command with an isolated configuration registry.
func backupRunCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := &Env{
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &errOut,
		LookupEnv: func(string) (string, bool) { return "", false },
		Config:    config.NewRegistry(),
	}
	code = Default.Run(context.Background(), args, env)
	return code, out.String(), errOut.String()
}

func backupLedgerRows(t *testing.T, dataDir string) int {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName), Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM ledger`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBackupCLIRoundTrip(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	cfg := filepath.Join(base, "alixai.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+fmt.Sprintf("%q", filepath.ToSlash(dataDir))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName), Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE ledger (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO ledger (n) VALUES (?)`, i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if code, _, errOut := backupRunCLI(t, "backup", "create", "-config", cfg); code != ExitUsage {
		t.Fatalf("backup create without -dir = %d (%s), want a usage error", code, errOut)
	}
	backups := filepath.Join(base, "backups")
	code, out, errOut := backupRunCLI(t, "backup", "create", "-config", cfg, "-dir", backups)
	if code != ExitOK {
		t.Fatalf("backup create = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "copy the .pub file somewhere") {
		t.Errorf("no advice about keeping the public key elsewhere: %s", errOut)
	}
	var created struct {
		ID  string `json:"backup_id"`
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Dir == "" {
		t.Fatalf("output %q: %v", out, err)
	}
	pub := filepath.Join(dataDir, "keys", "backup.pub")

	if code, out, errOut := backupRunCLI(t, "backup", "verify", "-dir", created.Dir, "-pubkey", "backup="+pub); code != ExitOK || !strings.Contains(out, `"verified": true`) {
		t.Fatalf("backup verify = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if code, _, errOut := backupRunCLI(t, "backup", "verify", "-dir", created.Dir, "-config", cfg); code != ExitOK {
		t.Fatalf("backup verify with the default key = %d: %s", code, errOut)
	}
	if code, _, _ := backupRunCLI(t, "backup", "verify", "-dir", created.Dir); code != ExitUsage {
		t.Errorf("backup verify with no key = %d, want a usage error", code)
	}

	// The data changes, the service stops, the backup comes back.
	db, err = store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName), Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM ledger WHERE n > 2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if code, out, errOut := backupRunCLI(t, "backup", "restore", "-dir", created.Dir, "-config", cfg); code != ExitOK {
		t.Fatalf("backup restore = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	} else if !strings.Contains(out, "pre_restore_dir") {
		t.Errorf("restore output: %s", out)
	}
	if n := backupLedgerRows(t, dataDir); n != 7 {
		t.Errorf("ledger rows after restore = %d, want 7", n)
	}

	// A modified backup is refused and the data directory is left alone.
	p := filepath.Join(created.Dir, "alixai.db")
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := backupRunCLI(t, "backup", "verify", "-dir", created.Dir, "-pubkey", "backup="+pub); code != ExitError {
		t.Errorf("verify of a modified backup = %d, want %d", code, ExitError)
	}
	if code, _, _ := backupRunCLI(t, "backup", "restore", "-dir", created.Dir, "-config", cfg); code != ExitError {
		t.Errorf("restore of a modified backup = %d, want %d", code, ExitError)
	}
	if n := backupLedgerRows(t, dataDir); n != 7 {
		t.Errorf("ledger rows after a refused restore = %d, want 7", n)
	}
}

// A data directory a restore was cut in is not backed up (nor served) until the
// restore is run again.
func TestBackupCLIRefusesAfterAnInterruptedRestore(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	cfg := filepath.Join(base, "alixai.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+fmt.Sprintf("%q", filepath.ToSlash(dataDir))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName), Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	marker := `{"backup_id": "bkp_01J000000000000000000000AA", "pre_restore": ".pre-restore-pre_01J000000000000000000000AA", "names": ["alixai.db"], "existed": ["alixai.db"]}`
	if err := os.WriteFile(filepath.Join(dataDir, ".restore-in-progress.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := backupRunCLI(t, "backup", "create", "-config", cfg, "-dir", filepath.Join(base, "backups"))
	if code != ExitError || !strings.Contains(errOut, "interrupted") {
		t.Fatalf("backup create = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

func TestUpgradeCLIRefusesUnsignedManifest(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	cfg := filepath.Join(base, "alixai.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+fmt.Sprintf("%q", filepath.ToSlash(dataDir))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName), Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	manifest := filepath.Join(base, "release-manifest.json")
	if err := os.WriteFile(manifest, []byte(`{"format":"alixai.release-manifest.v1","signed":{},"signatures":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := backupRunCLI(t, "upgrade", "apply", "-config", cfg); code != ExitUsage {
		t.Errorf("upgrade apply without -manifest = %d, want a usage error", code)
	}
	code, out, errOut := backupRunCLI(t, "upgrade", "apply", "-config", cfg, "-manifest", manifest)
	if code != ExitError || !strings.Contains(out, `"outcome": "refused"`) {
		t.Fatalf("upgrade apply = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "upgrade", "backups")); !os.IsNotExist(err) {
		t.Errorf("a refused upgrade took a backup (%v)", err)
	}
	if code, out, errOut := backupRunCLI(t, "upgrade", "status", "-config", cfg); code != ExitOK || !strings.Contains(out, `"unfinished": null`) {
		t.Errorf("upgrade status = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if code, out, errOut := backupRunCLI(t, "upgrade", "recover", "-config", cfg); code != ExitOK || !strings.Contains(out, "nothing_to_do") {
		t.Errorf("upgrade recover with nothing to do = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}
