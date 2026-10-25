package upgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-072, SEC-073: a manual rollback restores the backup the last SUCCESSFUL
// upgrade took, not the newest one on disk. After 1 -> 2 succeeds and 2 -> 3
// fails and rolls back, the newest backup holds the version 2 schema; restoring it
// beside the version 1 state and runtime would be exactly the schema and runtime
// mismatch the qualification exists to prevent.
func TestRollbackRestoresTheBackupOfTheLastSuccessfulUpgrade(t *testing.T) {
	f := newFixture(t)
	events, head := f.auditBefore(t)

	u2 := f.upgrader(regV2(t, ""))
	if res, err := u2.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); err != nil || res.Outcome != OutcomeApplied {
		t.Fatalf("first upgrade = %+v, %v", res, err)
	}
	u3 := f.upgrader(regV2(t, "THIS IS NOT SQL;"), binary("3.0.0"))
	res, err := u3.Apply(ctx, Request{Manifest: f.release("3.0.0", []string{"v3.6.0"}, "b2000"), Runtime: &newSet})
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("second upgrade = %+v, %v", res, err)
	}
	if st, _ := u3.LoadState(); st.ReleaseVersion != "2.0.0" {
		t.Fatalf("state after the failed second upgrade = %+v, want 2.0.0", st)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dataDir, dirName, backupsDirName)); len(entries) != 1 {
		t.Errorf("%d backups kept, want only the one the last successful upgrade can be rolled back with", len(entries))
	}

	rb, err := u3.Rollback(ctx)
	if err != nil || rb.Outcome != OutcomeRolledBack {
		t.Fatalf("Rollback = %+v, %v", rb, err)
	}
	f.assertPreUpgradeState(t, events, head)
	if got := f.rt.current(); got.Version != "v3.5.0" || got.Backends["llama-cpp"] != "b1000" {
		t.Errorf("runtime = %+v, want the version 1 set", got)
	}
	if st, _ := u3.LoadState(); st.Runtime == nil || st.Runtime.Version != "v3.5.0" || st.ReleaseVersion != "" {
		t.Errorf("state = %+v, want the version 1 state", st)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dataDir, dirName, backupsDirName)); len(entries) != 0 {
		t.Errorf("%d backups left after the rollback consumed its backup", len(entries))
	}
}

// An upgrade that fails while it records its new state is rolled back without
// losing what the last successful upgrade recorded: previous.json still names
// that upgrade's backup, the backup is kept, and a manual rollback reaches it.
func TestFailedStateWriteKeepsTheLastSuccessfulUpgradesBackup(t *testing.T) {
	f := newFixture(t)
	events, head := f.auditBefore(t)
	u2 := f.upgrader(regV2(t, ""))
	first, err := u2.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if err != nil || first.Outcome != OutcomeApplied {
		t.Fatalf("first upgrade = %+v, %v", first, err)
	}

	v3 := "CREATE TABLE ledger_more (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT;"
	block := filepath.Join(f.dataDir, dirName, stateFile+".tmp")
	u3 := f.upgrader(regV2(t, v3), binary("3.0.0"), func(c *Config) {
		c.Probe = func(context.Context, *store.DB) error { return os.Mkdir(block, 0o700) } // state.json cannot be written
	})
	if _, err := u3.Apply(ctx, Request{Manifest: f.release("3.0.0", []string{"v3.6.0"}, "b2000"), Runtime: &newSet}); !errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("second upgrade: %v, want a rollback that could not record the state", err)
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if _, err := f.upgrader(regV2(t, v3)).Recover(ctx); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("Recover: %v", err)
	}

	var prev Previous
	if found, err := u2.readJSON(previousFile, &prev); !found || err != nil || prev.BackupID != first.BackupID {
		t.Fatalf("previous.json = %+v (found %v, %v), want the backup %s of the first upgrade", prev, found, err, first.BackupID)
	}
	if _, err := os.Stat(first.BackupDir); err != nil {
		t.Fatalf("the backup of the last successful upgrade is gone: %v", err)
	}
	if rb, err := u2.Rollback(ctx); err != nil || rb.Outcome != OutcomeRolledBack {
		t.Fatalf("Rollback = %+v, %v", rb, err)
	}
	f.assertPreUpgradeState(t, events, head)
}

// A crash after an upgrade replaced previous.json and before it removed its
// journal, followed by a recover that rolls the upgrade back, keeps the backup of
// the last successful upgrade and puts its record back: a manual rollback still
// reaches the version before it.
func TestRolledBackRecoveryKeepsTheOlderUpgradesBackup(t *testing.T) {
	f := newFixture(t)
	events, head := f.auditBefore(t)
	first, err := f.upgrader(regV2(t, "")).Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if err != nil || first.Outcome != OutcomeApplied {
		t.Fatalf("first upgrade = %+v, %v", first, err)
	}

	v3 := "CREATE TABLE ledger_more (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT;"
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u3 := f.upgrader(regV2(t, v3), binary("3.0.0"), func(c *Config) {
		c.Probe = func(context.Context, *store.DB) error { cancel(); return errors.New("killed") }
	})
	second, err := u3.Apply(cctx, Request{Manifest: f.release("3.0.0", []string{"v3.6.0"}, "b2000"), Runtime: &newSet})
	if second.Outcome != OutcomeInterrupted {
		t.Fatalf("second upgrade = %+v, %v, want an interruption", second, err)
	}
	// The process died after it recorded the new state and previous.json, before
	// it removed the journal.
	j, err := u3.loadJournal()
	if err != nil || j == nil {
		t.Fatalf("journal = %+v, %v", j, err)
	}
	if err := u3.saveState(j.To); err != nil {
		t.Fatal(err)
	}
	if err := u3.writeJSON(previousFile, Previous{State: j.From, BackupID: j.BackupID}); err != nil {
		t.Fatal(err)
	}

	failing := f.upgrader(regV2(t, v3), func(c *Config) {
		c.Probe = func(context.Context, *store.DB) error { return errors.New("probe fails") }
	})
	if res, err := failing.Recover(ctx); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("Recover = %+v, %v, want a rollback", res, err)
	}
	if st, _ := failing.LoadState(); st.ReleaseVersion != "2.0.0" {
		t.Fatalf("state = %+v, want 2.0.0", st)
	}
	if _, err := os.Stat(first.BackupDir); err != nil {
		t.Fatalf("the backup of the last successful upgrade is gone: %v", err)
	}
	var prev Previous
	if found, err := failing.readJSON(previousFile, &prev); !found || err != nil || prev.BackupID != first.BackupID {
		t.Fatalf("previous.json = %+v (found %v, %v), want the backup %s of the first upgrade", prev, found, err, first.BackupID)
	}
	if rb, err := failing.Rollback(ctx); err != nil || rb.Outcome != OutcomeRolledBack {
		t.Fatalf("Rollback = %+v, %v", rb, err)
	}
	f.assertPreUpgradeState(t, events, head)
}

// A journal written before the prior record existed still loads and rolls back
// as it did: the record is read from previous.json.
func TestJournalWithoutPriorStillRollsBack(t *testing.T) {
	f := newFixture(t)
	f.crashDuringMigration(t, regV2(t, "THIS IS NOT SQL;"), regV2(t, ""))
	raw, err := os.ReadFile(filepath.Join(f.dataDir, dirName, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"prior"`) {
		t.Fatalf("the simulated old journal carries a prior record: %s", raw)
	}
	if _, err := f.upgrader(regV2(t, "THIS IS NOT SQL;")).Recover(ctx); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("Recover: %v, want a rollback", err)
	}
}

// schemaIndex is a derived index whose health depends on the schema: with
// ledger_extra (the migrated database) newDamage is damaged and can only be
// re-embedded, and oldPending, which only the new binary can rebuild, is rebuilt.
type schemaIndex struct {
	db   *store.DB
	name string
}

func (s schemaIndex) migrated(ctx context.Context) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'ledger_extra'`).Scan(&n)
	return n == 1
}

func (s schemaIndex) Name() string { return s.name }
func (s schemaIndex) Check(ctx context.Context) error {
	if s.name == "new_damage" && s.migrated(ctx) {
		return ErrDamaged
	}
	return nil
}
func (s schemaIndex) Isolate(context.Context) error { return nil }
func (s schemaIndex) Rebuild(ctx context.Context) error {
	if s.name == "old_pending" && s.migrated(ctx) {
		return nil
	}
	return ErrReembedRequired
}

// The quarantine lives outside the database, so a rollback puts back the
// quarantine of the database it restores: a record the upgrade added goes, one
// it released comes back.
func TestRollbackRestoresTheQuarantine(t *testing.T) {
	for _, manual := range []bool{false, true} {
		f := newFixture(t)
		u := f.upgrader(regV2(t, ""), func(c *Config) {
			c.ExtraDerived = func(db *store.DB) []DerivedIndex {
				return []DerivedIndex{schemaIndex{db, "new_damage"}, schemaIndex{db, "old_pending"}}
			}
			if !manual {
				c.Probe = func(context.Context, *store.DB) error { return errors.New("probe fails") }
			}
		})
		if err := u.Quarantine().Add("old_pending", "isolated before the upgrade"); err != nil {
			t.Fatal(err)
		}
		before, _ := u.Quarantine().List()
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if manual {
			if err != nil {
				t.Fatalf("Apply = %+v, %v", res, err)
			}
			if got, _ := u.Quarantine().List(); len(got) != 1 || got[0].Name != "new_damage" {
				t.Fatalf("quarantine after the upgrade = %+v, want new_damage only", got)
			}
			if res, err = u.Rollback(ctx); err != nil {
				t.Fatalf("Rollback = %+v, %v", res, err)
			}
		} else if !errors.Is(err, ErrRolledBack) {
			t.Fatalf("Apply = %+v, %v, want a rollback", res, err)
		}
		if got, err := u.Quarantine().List(); err != nil || len(got) != 1 || got[0] != before[0] {
			t.Errorf("manual=%v: quarantine after the rollback = %+v (%v), want %+v as it was", manual, got, err, before)
		}
	}
}

// A manual rollback puts the runtime back the way an automatic one does: an
// installation that had no runtime recorded before the upgrade gets the empty set
// again, not the release's set.
func TestManualRollbackPutsTheRuntimeBack(t *testing.T) {
	for _, manual := range []bool{false, true} {
		f := newFixture(t)
		f.rt.cur = RuntimeSet{} // nothing recorded before the upgrade
		var mutate []func(*Config)
		if !manual {
			mutate = append(mutate, func(c *Config) {
				c.Probe = func(context.Context, *store.DB) error { return errors.New("probe fails") }
			})
		}
		u := f.upgrader(regV2(t, ""), mutate...)
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if manual {
			if err != nil || res.Outcome != OutcomeApplied {
				t.Fatalf("Apply = %+v, %v", res, err)
			}
			if got := f.rt.current(); got.Version != "3.6.0" {
				t.Fatalf("runtime after the upgrade = %+v, want the new set", got)
			}
			if res, err = u.Rollback(ctx); err != nil || res.Outcome != OutcomeRolledBack {
				t.Fatalf("Rollback = %+v, %v", res, err)
			}
		} else if !errors.Is(err, ErrRolledBack) {
			t.Fatalf("Apply = %+v, %v, want a rollback", res, err)
		}
		if got := f.rt.current(); got.Runtime != "" {
			t.Errorf("manual=%v: runtime after the rollback = %+v, want the empty set that was recorded before", manual, got)
		}
		if st, _ := u.LoadState(); st.Runtime != nil {
			t.Errorf("manual=%v: state after the rollback = %+v, want no runtime", manual, st)
		}
	}
}

// The journal id names release-<id>.json, so a journal whose id is not one the
// upgrade generates is refused before anything follows it.
func TestJournalWithMalformedIDIsRefused(t *testing.T) {
	f := newFixture(t)
	u := f.upgrader(regV2(t, ""))
	for _, id := range []string{"../../outside", "upg_../x", ""} {
		bad := Journal{ID: id, Phase: PhaseMigrating, BackupID: "bkp_01J000000000000000000000AA", StartedAt: "2026-10-03T12:00:00Z"}
		if err := u.saveJournal(bad); err != nil {
			t.Fatal(err)
		}
		if _, err := u.Status(); err == nil {
			t.Errorf("journal id %q: Status accepted it", id)
		}
		if _, err := u.Recover(ctx); err == nil {
			t.Errorf("journal id %q: Recover accepted it", id)
		}
		var j Journal
		if _, err := u.readJSON(journalFile, &j); err != nil || j.Phase != PhaseMigrating {
			t.Errorf("journal id %q: the journal was acted on (phase %q, %v)", id, j.Phase, err)
		}
	}
}

// Replacing the database under a running service splits its writes from the
// restored file (on Unix a rename does not stop it), so Apply and Rollback refuse
// when a -shm file says another process has the database open.
func TestUpgradeRefusedWhileDatabaseInUse(t *testing.T) {
	f := newFixture(t)
	u := f.upgrader(regV2(t, ""))
	dbPath := filepath.Join(f.dataDir, store.DefaultFileName)
	sum := func() [32]byte {
		b, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := sum()
	shm := dbPath + "-shm"
	if err := os.WriteFile(shm, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if contracts.CodeOf(err) != contracts.ErrConflict || res.Outcome != "" {
		t.Fatalf("Apply = %+v, %v (code %s)", res, err, contracts.CodeOf(err))
	}
	if _, err := u.Rollback(ctx); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("Rollback = %v (code %s)", err, contracts.CodeOf(err))
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, dirName, backupsDirName)); !os.IsNotExist(err) {
		t.Errorf("a refused upgrade took a backup (%v)", err)
	}
	if n := f.rt.activationCount(); n != 0 {
		t.Errorf("the runtime was switched %d times", n)
	}
	if sum() != before {
		t.Error("the database file changed")
	}

	// An operator who knows the service is stopped can say so.
	forced := f.upgrader(regV2(t, ""), func(c *Config) { c.AllowInUse = true })
	if res, err := forced.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); err != nil || res.Outcome != OutcomeApplied {
		t.Fatalf("forced Apply = %+v, %v", res, err)
	}
}

// Recover of an unfinished upgrade migrates the database or restores it from the
// backup, so it refuses the same way while the database is in use.
func TestRecoverRefusedWhileDatabaseInUse(t *testing.T) {
	f := newFixture(t)
	broken := func() *store.Registry { return regV2(t, "THIS IS NOT SQL;") }
	f.crashDuringMigration(t, broken(), regV2(t, ""))
	dbPath := filepath.Join(f.dataDir, store.DefaultFileName)
	sum := func() [32]byte {
		b, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := sum()
	if err := os.WriteFile(dbPath+"-shm", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	u := f.upgrader(broken())
	if _, err := u.Recover(ctx); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("Recover = %v (code %s)", err, contracts.CodeOf(err))
	}
	if s, _ := u.Status(); s.Unfinished == nil || s.Unfinished.Phase != PhaseMigrating {
		t.Errorf("status = %+v, want the journal untouched in phase migrating", s)
	}
	if n := f.rt.activationCount(); n != 0 {
		t.Errorf("the runtime was switched %d times", n)
	}
	if sum() != before {
		t.Error("the database file changed")
	}

	forced := f.upgrader(broken(), func(c *Config) { c.AllowInUse = true })
	if res, err := forced.Recover(ctx); !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
		t.Fatalf("forced Recover = %+v, %v", res, err)
	}
}
