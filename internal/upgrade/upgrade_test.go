package upgrade

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	newSet = RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"llama-cpp": "b2000"}}
	oldSet = RuntimeSet{Runtime: "localai", Version: "v3.5.0", Backends: map[string]string{"llama-cpp": "b1000"}}
)

// good is a release qualified for newSet.
func (f *fixture) good() []byte { return f.release("2.0.0", []string{"v3.6.0"}, "b2000") }

func (f *fixture) preRestoreDirs() []string {
	m, _ := filepath.Glob(filepath.Join(f.dataDir, ".pre-restore-*"))
	return m
}

// assertPreUpgradeState checks that the database is exactly what the v1 fixture
// wrote: one ledger migration, no ledger_extra, ten rows and the audit chain
// the fixture appended.
func (f *fixture) assertPreUpgradeState(t *testing.T, wantEvents int64, wantHead string) {
	t.Helper()
	db := f.openWith(regV1(t))
	defer db.Close()
	if n := applied(t, db); n != 1 {
		t.Errorf("ledger migrations applied = %d, want 1", n)
	}
	if hasTable(t, db, "ledger_extra") {
		t.Error("ledger_extra exists: the migration was not rolled back")
	}
	if n := count(t, db, `SELECT count(*) FROM ledger`); n != 10 {
		t.Errorf("ledger rows = %d, want 10", n)
	}
	if events, head := auditHead(t, db); events != wantEvents || head != wantHead {
		t.Errorf("audit chain = %d events head %.12s, want %d events head %.12s", events, head, wantEvents, wantHead)
	}
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Errorf("integrity: %v", err)
	}
}

func (f *fixture) auditBefore(t *testing.T) (int64, string) {
	db := f.openWith(regV1(t))
	defer db.Close()
	return auditHead(t, db)
}

func TestUpgradeApplies(t *testing.T) {
	f := newFixture(t)
	events, head := f.auditBefore(t)
	u := f.upgrader(regV2(t, ""))

	res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if err != nil || res.Outcome != OutcomeApplied {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	st, err := u.LoadState()
	if err != nil || st.ReleaseVersion != "2.0.0" || st.Runtime == nil || st.Runtime.Version != "3.6.0" {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if got := f.rt.current(); got.Version != "3.6.0" {
		t.Errorf("runtime = %+v, want the new set", got)
	}
	db := f.openWith(regV2(t, ""))
	if n := applied(t, db); n != 2 {
		t.Errorf("ledger migrations applied = %d, want 2", n)
	}
	if n := count(t, db, `SELECT count(*) FROM ledger_extra`); n != 10 {
		t.Errorf("ledger_extra rows = %d, want 10", n)
	}
	if e, h := auditHead(t, db); e != events || h != head {
		t.Errorf("audit chain changed by the upgrade: %d events head %.12s", e, h)
	}
	if s, err := u.Status(); err != nil || s.Unfinished != nil || len(s.Quarantined) != 0 {
		t.Errorf("status = %+v, %v", s, err)
	}
	db.Close() // the service is stopped before a rollback replaces the database
	if entries, _ := os.ReadDir(filepath.Join(f.dataDir, dirName, backupsDirName)); len(entries) != 1 {
		t.Errorf("%d pre-upgrade backups kept, want 1", len(entries))
	}

	// The same signed manifest again, and an older one, are refused: without
	// update metadata a replay would be a downgrade.
	for name, raw := range map[string][]byte{"replay": f.good(), "older": f.release("1.9.0", []string{"v3.6.0"}, "b2000")} {
		res, err := u.Apply(ctx, Request{Manifest: raw, Runtime: &newSet})
		if !errors.Is(err, ErrRefused) || res.Outcome != OutcomeRefused || contracts.CodeOf(err) != contracts.ErrConflict || !strings.Contains(err.Error(), "not newer") {
			t.Errorf("%s: res = %+v, err = %v (code %s)", name, res, err, contracts.CodeOf(err))
		}
	}

	// An operator rollback goes back to v1: database, runtime, state.
	rb, err := u.Rollback(ctx)
	if err != nil || rb.Outcome != OutcomeRolledBack {
		t.Fatalf("Rollback = %+v, %v", rb, err)
	}
	f.assertPreUpgradeState(t, events, head)
	if got := f.rt.current(); got.Version != "v3.5.0" {
		t.Errorf("runtime after rollback = %+v, want the previous set", got)
	}
	if st, _ := u.LoadState(); st.Runtime == nil || st.Runtime.Version != "v3.5.0" {
		t.Errorf("state after rollback = %+v", st)
	}
	if _, err := u.Rollback(ctx); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second rollback: %v", err)
	}
}

// SEC-072, SEC-073: an upgrade that fails after it started leaves the previous
// database, runtime and state, and receipts and audit events survive.
func TestRollbackAfterFailedUpgrade(t *testing.T) {
	t.Run("migration fails", func(t *testing.T) {
		f := newFixture(t)
		events, head := f.auditBefore(t)
		u := f.upgrader(regV2(t, "THIS IS NOT SQL;"))
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		f.assertPreUpgradeState(t, events, head)
		if st, _ := u.LoadState(); st.ReleaseVersion != "" {
			t.Errorf("state = %+v, want none", st)
		}
		if s, _ := u.Status(); s.Unfinished != nil {
			t.Errorf("the journal was left behind: %+v", s.Unfinished)
		}
		if got := f.rt.current(); got.Version != "v3.5.0" {
			t.Errorf("runtime = %+v, want the previous set", got)
		}
		if len(f.preRestoreDirs()) != 1 {
			t.Errorf("pre-restore directories = %v, want exactly one (what was in place is kept)", f.preRestoreDirs())
		}
		// The next attempt with a fixed binary goes through.
		u2 := f.upgrader(regV2(t, ""))
		if res, err := u2.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); err != nil || res.Outcome != OutcomeApplied {
			t.Fatalf("second attempt = %+v, %v", res, err)
		}
	})

	t.Run("health probe fails after the runtime was switched", func(t *testing.T) {
		f := newFixture(t)
		events, head := f.auditBefore(t)
		u := f.upgrader(regV2(t, ""), func(c *Config) {
			c.Probe = func(_ context.Context, db *store.DB) error { return errors.New("the application does not answer") }
		})
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		f.assertPreUpgradeState(t, events, head)
		if n := f.rt.activationCount(); n != 2 {
			t.Errorf("runtime activations = %d, want 2 (new set, then the previous one put back)", n)
		}
		if got := f.rt.current(); got.Version != "v3.5.0" || got.Backends["llama-cpp"] != "b1000" {
			t.Errorf("runtime = %+v, want the previous set", got)
		}
	})

	// SEC-072: a LocalAI or backend version the release was not qualified for is
	// refused before anything changes.
	t.Run("unqualified runtime is refused", func(t *testing.T) {
		for name, req := range map[string]Request{
			"runtime version": {Runtime: &RuntimeSet{Runtime: "localai", Version: "3.9.9", Backends: map[string]string{"llama-cpp": "b2000"}}},
			"backend version": {Runtime: &RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"llama-cpp": "b9999"}}},
			"unknown backend": {Runtime: &RuntimeSet{Runtime: "localai", Version: "3.6.0", Backends: map[string]string{"evil-backend": "b2000"}}},
			"running runtime": {}, // keeps LocalAI v3.5.0, which this release does not list
			"another runtime": {Runtime: &RuntimeSet{Runtime: "other", Version: "3.6.0"}},
			"empty version":   {Runtime: &RuntimeSet{Runtime: "localai"}},
		} {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				events, head := f.auditBefore(t)
				u := f.upgrader(regV2(t, ""))
				req.Manifest = f.good()
				res, err := u.Apply(ctx, req)
				if !errors.Is(err, ErrRefused) || res.Outcome != OutcomeRefused {
					t.Fatalf("Apply = %+v, %v", res, err)
				}
				if name != "empty version" && contracts.CodeOf(err) != contracts.ErrUnavailable {
					t.Errorf("code = %s, want %s: an unqualified combination reports unavailable", contracts.CodeOf(err), contracts.ErrUnavailable)
				}
				f.assertPreUpgradeState(t, events, head)
				if n := f.rt.activationCount(); n != 0 {
					t.Errorf("the runtime was switched %d times by a refused upgrade", n)
				}
				if _, err := os.Stat(filepath.Join(f.dataDir, dirName, backupsDirName)); !os.IsNotExist(err) {
					t.Errorf("a refused upgrade took a backup or touched the data directory (%v)", err)
				}
				if s, _ := u.Status(); s.Unfinished != nil || s.State.ReleaseVersion != "" {
					t.Errorf("status = %+v", s)
				}
			})
		}
	})

	// SEC-072: the runtime that is running after the switch is not the qualified
	// one (a backend replaced behind the upgrade's back): rolled back, the
	// previous set restored.
	t.Run("runtime drifts to an unqualified backend", func(t *testing.T) {
		f := newFixture(t)
		events, head := f.auditBefore(t)
		f.rt.afterActivate = func(rs RuntimeSet) *RuntimeSet {
			if rs.Version == "3.6.0" {
				d := clone(rs)
				d.Backends["llama-cpp"] = "b9999"
				return &d
			}
			return nil
		}
		u := f.upgrader(regV2(t, ""))
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		if contracts.CodeOf(res.Cause) != contracts.ErrUnavailable {
			t.Errorf("cause code = %s (%v), want %s", contracts.CodeOf(res.Cause), res.Cause, contracts.ErrUnavailable)
		}
		f.assertPreUpgradeState(t, events, head)
		if got, _ := f.rt.Observe(ctx); got.Version != "v3.5.0" || got.Backends["llama-cpp"] != "b1000" {
			t.Errorf("runtime = %+v, want the previous qualified set", got)
		}
	})

	// A pre-upgrade backup that does not verify is never restored: the rollback
	// stops, says so, and keeps the journal and the backup.
	t.Run("tampered backup is not restored", func(t *testing.T) {
		f := newFixture(t)
		u := f.upgrader(regV2(t, ""), func(c *Config) {
			c.Probe = func(_ context.Context, db *store.DB) error {
				p := filepath.Join(f.dataDir, dirName, backupsDirName)
				entries, _ := os.ReadDir(p)
				db2 := filepath.Join(p, entries[0].Name(), "alixai.db")
				b, _ := os.ReadFile(db2)
				b[len(b)/2] ^= 0xff
				_ = os.WriteFile(db2, b, 0o600)
				return errors.New("probe fails")
			}
		})
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if !errors.Is(err, ErrRollbackFailed) || res.Outcome != OutcomeRollbackFailed {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		if s, _ := u.Status(); s.Unfinished == nil {
			t.Error("the journal was dropped although the rollback did not complete")
		}
		if _, err := u.Apply(ctx, Request{Manifest: f.good()}); contracts.CodeOf(err) != contracts.ErrConflict {
			t.Errorf("a new upgrade over an unfinished one: %v", err)
		}
	})
}

// crashDuringMigration does what Apply does up to the migration, applies only
// the migrations of partial (the process dies after them) and returns the
// journal it left.
// A signal during the upgrade is an interruption, not a failure: the journal
// stays and Recover rolls the upgrade forward.
func TestInterruptedUpgradeIsRecovered(t *testing.T) {
	f := newFixture(t)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u := f.upgrader(regV2(t, ""), func(c *Config) {
		c.Probe = func(context.Context, *store.DB) error {
			cancel()
			return errors.New("interrupted")
		}
	})
	res, err := u.Apply(cctx, Request{Manifest: f.good(), Runtime: &newSet})
	if res.Outcome != OutcomeInterrupted || contracts.CodeOf(err) != contracts.ErrTimeout {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if s, _ := u.Status(); s.Unfinished == nil {
		t.Fatal("the journal was dropped by an interruption")
	}
	if _, err := u.Apply(ctx, Request{Manifest: f.good()}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a new upgrade over an interrupted one: %v", err)
	}
	// The restarted process has no hook.
	u2 := f.upgrader(regV2(t, ""))
	res, err = u2.Recover(ctx)
	if err != nil || res.Outcome != OutcomeRecovered {
		t.Fatalf("Recover = %+v, %v", res, err)
	}
	if st, _ := u2.LoadState(); st.ReleaseVersion != "2.0.0" {
		t.Errorf("state = %+v", st)
	}
}

// SEC-081: the upgrade's own files are owner-only.
func TestUpgradeFilesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes; Windows relies on the data directory's ACL")
	}
	f := newFixture(t)
	u := f.upgrader(regV2(t, ""))
	if _, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); err != nil {
		t.Fatal(err)
	}
	if err := u.Quarantine().Add("some_fts", "test"); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(filepath.Join(f.dataDir, dirName), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFileRuntime(t *testing.T) {
	r := FileRuntime{Path: RuntimeFile(t.TempDir())}
	if got, err := r.Observe(ctx); err != nil || got.Runtime != "" {
		t.Fatalf("with no file: %+v, %v", got, err)
	}
	if err := r.Activate(ctx, newSet); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Observe(ctx); err != nil || got.Version != "3.6.0" || got.Backends["llama-cpp"] != "b2000" {
		t.Fatalf("after Activate: %+v, %v", got, err)
	}
	if err := r.Activate(ctx, RuntimeSet{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Observe(ctx); got.Runtime != "" {
		t.Errorf("the empty set did not clear the record: %+v", got)
	}
	os.WriteFile(r.Path, []byte(`{"runtime":"localai","surprise":1}`), 0o600)
	if _, err := r.Observe(ctx); err == nil {
		t.Error("a record with an unknown field was accepted")
	}
}

func (f *fixture) crashDuringMigration(t *testing.T, full *store.Registry, partial *store.Registry) {
	t.Helper()
	u := f.upgrader(full)
	raw := f.good()
	rel, err := u.releaseVerifier().VerifyRelease(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := u.preBackup(ctx, &Result{})
	if err != nil {
		t.Fatal(err)
	}
	j := Journal{
		ID: "upg_01J000000000000000000000AA", Phase: PhaseMigrating, From: State{Runtime: &RuntimeSet{Runtime: "localai", Version: "v3.5.0", Backends: map[string]string{"llama-cpp": "b1000"}}},
		To: State{ReleaseVersion: rel.Version, Runtime: &newSet}, SwitchRuntime: true, BackupID: id, StartedAt: "2026-10-03T12:00:00Z",
	}
	if err := u.writeRaw(releaseFile(j.ID), raw); err != nil {
		t.Fatal(err)
	}
	if err := u.saveJournal(j); err != nil {
		t.Fatal(err)
	}
	db := f.openWith(partial)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// SEC-073: a migration killed midway recovers on restart, or rolls back to the
// pre-upgrade backup.
func TestInterruptedMigrationRecovers(t *testing.T) {
	partial := func(t *testing.T) *store.Registry { return regV2(t, "") } // ledger 0001 and 0002 only

	t.Run("rolls forward", func(t *testing.T) {
		f := newFixture(t)
		events, head := f.auditBefore(t)
		f.crashDuringMigration(t, regV2(t, "CREATE TABLE ledger_more (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT;"), partial(t))

		// The restarted process: a new Upgrader, the full registry.
		db := f.openWith(partial(t))
		if applied(t, db) != 2 {
			t.Fatalf("the simulated crash left %d migrations, want 2", applied(t, db))
		}
		db.Close()
		u := f.upgrader(regV2(t, "CREATE TABLE ledger_more (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT;"))
		if s, _ := u.Status(); s.Unfinished == nil || s.Unfinished.Phase != PhaseMigrating {
			t.Fatalf("status = %+v, want an unfinished upgrade in phase migrating", s)
		}
		res, err := u.Recover(ctx)
		if err != nil || res.Outcome != OutcomeRecovered {
			t.Fatalf("Recover = %+v, %v", res, err)
		}
		db = f.openWith(regV2(t, "CREATE TABLE ledger_more (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT;"))
		defer db.Close()
		if n := applied(t, db); n != 3 {
			t.Errorf("ledger migrations applied = %d, want 3", n)
		}
		if n := count(t, db, `SELECT count(*) FROM ledger`); n != 10 {
			t.Errorf("ledger rows = %d, want 10", n)
		}
		if e, h := auditHead(t, db); e != events || h != head {
			t.Errorf("audit chain = %d events head %.12s, want %d events head %.12s", e, h, events, head)
		}
		if err := db.IntegrityCheck(ctx); err != nil {
			t.Errorf("integrity: %v", err)
		}
		if st, _ := u.LoadState(); st.ReleaseVersion != "2.0.0" {
			t.Errorf("state = %+v", st)
		}
		if s, _ := u.Status(); s.Unfinished != nil {
			t.Errorf("the journal was left behind: %+v", s.Unfinished)
		}
		if got := f.rt.current(); got.Version != "3.6.0" {
			t.Errorf("runtime = %+v, want the new set", got)
		}
		if res, err := u.Recover(ctx); err != nil || res.Outcome != OutcomeNothing {
			t.Errorf("a second Recover = %+v, %v", res, err)
		}
	})

	t.Run("rolls back when it cannot complete", func(t *testing.T) {
		f := newFixture(t)
		events, head := f.auditBefore(t)
		broken := regV2(t, "THIS IS NOT SQL;")
		f.crashDuringMigration(t, broken, partial(t))

		u := f.upgrader(regV2(t, "THIS IS NOT SQL;"))
		res, err := u.Recover(ctx)
		if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
			t.Fatalf("Recover = %+v, %v", res, err)
		}
		f.assertPreUpgradeState(t, events, head)
		if st, _ := u.LoadState(); st.ReleaseVersion != "" {
			t.Errorf("state = %+v, want none", st)
		}
		if s, _ := u.Status(); s.Unfinished != nil {
			t.Errorf("the journal was left behind: %+v", s.Unfinished)
		}
		if got := f.rt.current(); got.Version != "v3.5.0" {
			t.Errorf("runtime = %+v, want the previous set", got)
		}
	})

	t.Run("an interrupted rollback is finished", func(t *testing.T) {
		for _, phase := range []string{PhaseRollingBack, PhaseRestored} {
			t.Run(phase, func(t *testing.T) {
				f := newFixture(t)
				events, head := f.auditBefore(t)
				f.crashDuringMigration(t, regV2(t, "THIS IS NOT SQL;"), partial(t))
				u := f.upgrader(regV2(t, "THIS IS NOT SQL;"))
				j, err := u.loadJournal()
				if err != nil || j == nil {
					t.Fatal(err)
				}
				j.Phase = phase
				if phase == PhaseRestored {
					// The database was already restored; only the runtime is still on the new set.
					if _, err := u.Recover(ctx); err == nil {
						t.Fatal("setup: expected a rollback")
					}
					if err := u.saveJournal(*j); err != nil {
						t.Fatal(err)
					}
					f.rt.cur = clone(newSet)
				} else if err := u.saveJournal(*j); err != nil {
					t.Fatal(err)
				}
				res, err := u.Recover(ctx)
				if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
					t.Fatalf("Recover = %+v, %v", res, err)
				}
				f.assertPreUpgradeState(t, events, head)
				if got := f.rt.current(); got.Version != "v3.5.0" {
					t.Errorf("runtime = %+v, want the previous set", got)
				}
			})
		}
	})

	t.Run("a checksum drift in an applied migration refuses start", func(t *testing.T) {
		f := newFixture(t)
		edited := registry(t, map[string]map[string]string{
			"ledger": {"0001_init.sql": ledgerV1 + "\n-- edited after it was applied\n"},
			"notes":  {"0001_init.sql": notesMigration},
		}, "ledger", "notes")
		u := f.upgrader(edited)
		res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
		if !errors.Is(err, ErrRolledBack) {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		if res.Cause == nil || !strings.Contains(res.Cause.Error(), "checksum mismatch") {
			t.Errorf("cause = %v, want a migration checksum mismatch", res.Cause)
		}
	})
}
