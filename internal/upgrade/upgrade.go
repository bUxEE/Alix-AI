package upgrade

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Sentinels for errors.Is on what Apply, Recover and Rollback return.
var (
	// ErrRefused: the upgrade was refused before anything was changed.
	ErrRefused = errors.New("upgrade refused")
	// ErrRolledBack: the upgrade failed after it started and the previous
	// database and runtime were restored.
	ErrRolledBack = errors.New("upgrade failed and was rolled back")
	// ErrRollbackFailed: the upgrade failed and the previous state could not be
	// restored; the journal and the pre-upgrade backup are kept for the operator.
	ErrRollbackFailed = errors.New("upgrade failed and rollback did not complete")
)

// Runtime is the inference runtime an installation uses (LocalAI today). The
// upgrade only asks it what runs and tells it which set to run; it never
// imports the runtime's code (adapters/localai implements this in the
// composition root).
type Runtime interface {
	// Observe returns the runtime and backend versions that are running now.
	Observe(ctx context.Context) (RuntimeSet, error)
	// Activate makes rs the set that runs: staged, switched, restarted. It is
	// also how a rollback puts the previous set back, so it must be idempotent.
	// The empty set (no Runtime name) means "no runtime recorded": a rollback of
	// the first upgrade of an installation that had none activates it.
	Activate(ctx context.Context, rs RuntimeSet) error
}

// Config configures an Upgrader.
type Config struct {
	// DataDir holds the database, the file trees and the upgrade directory.
	DataDir string
	// Migrations is the registry of the NEW binary: the migrations an upgrade
	// applies. Nil means store.Default.
	Migrations *store.Registry
	// Store sizes the database connections; zero fields take the defaults.
	Store store.Config
	// Clock is injected; nil means the wall clock.
	Clock clock.Clock
	// Logger receives progress (never data). Nil discards.
	Logger *slog.Logger

	// ReleaseKeys are the keys trusted for release manifests: release keys of
	// the platform application (trust.PurposeRelease), plus the built-in
	// release key that release.Verify always adds. The published TEST-ONLY key
	// needs an explicit allow in the KeyLookup itself
	// (release.StaticKeys.AllowTestOnly or trust.AllowTestOnlyKeys): nothing
	// here widens it.
	ReleaseKeys trust.KeyLookup
	// BackupSigner signs the pre-upgrade backup. Its public half is also what
	// verifies it before a rollback restores it, unless BackupKeys is set.
	BackupSigner trust.SigningKey
	// BackupKeys verifies the pre-upgrade backup. Nil derives it from
	// BackupSigner.
	BackupKeys trust.KeyLookup
	// BackupTrees are the file trees the backup includes; nil means
	// backup.DefaultTrees.
	BackupTrees []string
	// Version is the software version of the binary that runs the upgrade
	// (internal/version), recorded in the backup manifest. A release is applied
	// only by its own binary: its version, or Executable listed among its
	// binaries (Release.MatchesBinary).
	Version string
	// Executable is the file of the binary that runs the upgrade. Empty means
	// os.Executable().
	Executable string
	// AllowInUse lets Apply, Recover and Rollback proceed although the database looks in
	// use (its -shm file exists: a running service, or one that crashed). Replacing
	// the files under a running service splits its writes from the restored
	// database, so this is for after the service is stopped.
	AllowInUse bool

	// Runtime, when set, is switched and observed. Nil means the installation
	// has no managed runtime and only the database is upgraded.
	Runtime Runtime
	// Models restricts the qualification check to these registry model IDs.
	// Empty means every model of the release for the runtime.
	Models []string
	// Probe is an additional health check run on the migrated database.
	Probe func(ctx context.Context, db *store.DB) error
	// ExtraDerived lists derived indexes beyond the FTS5 tables and the vector
	// index the upgrade knows itself, for the health probe to check and rebuild.
	// An index with the same name as a built-in one replaces it.
	ExtraDerived func(db *store.DB) []DerivedIndex
	// VectorRebuild recreates the vector index by embedding the chunks again
	// (VectorIndex). Nil leaves an isolated vector index in the quarantine until
	// the embedding job that re-embeds it releases it.
	VectorRebuild VectorRebuild
}

// Upgrader runs staged upgrades, recovery and rollback of one data directory.
type Upgrader struct {
	cfg        Config
	reg        *store.Registry
	quarantine *Quarantine
}

// New validates cfg and returns an Upgrader.
func New(cfg Config) (*Upgrader, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, contracts.New(contracts.ErrValidationFailed, "upgrade: no data directory")
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "upgrade: data directory invalid")
	}
	cfg.DataDir = abs
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.BackupKeys == nil && len(cfg.BackupSigner.Key) == ed25519.PrivateKeySize {
		pub := cfg.BackupSigner.Key.Public().(ed25519.PublicKey)
		cfg.BackupKeys = backup.StaticKeys{Keys: map[string]ed25519.PublicKey{cfg.BackupSigner.KeyID: pub}}
	}
	if cfg.BackupKeys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "upgrade: no backup signing key and no backup verification keys")
	}
	reg := cfg.Migrations
	if reg == nil {
		reg = store.Default
	}
	// The vector index is derived data too: it is wired in front of whatever the
	// composition root adds, so no caller has to remember it.
	vectors, extra := VectorDerived(cfg.VectorRebuild), cfg.ExtraDerived
	cfg.ExtraDerived = func(db *store.DB) []DerivedIndex {
		out := vectors(db)
		if extra != nil {
			out = append(out, extra(db)...)
		}
		return out
	}
	return &Upgrader{cfg: cfg, reg: reg, quarantine: NewQuarantine(cfg.DataDir, cfg.Clock)}, nil
}

// Quarantine returns the set of isolated derived indexes of this data directory.
func (u *Upgrader) Quarantine() *Quarantine { return u.quarantine }

// Request is one upgrade.
type Request struct {
	// Manifest is the release manifest, as written by tools/release, signed.
	Manifest []byte
	// ArtifactDir, when set, is a directory whose files must match the
	// manifest's artifacts exactly.
	ArtifactDir string
	// Runtime, when set, is the runtime set to switch to. Nil leaves the
	// runtime as it is, which must still be qualified by the release.
	Runtime *RuntimeSet
}

// Outcome is how an operation ended.
type Outcome string

// Outcomes.
const (
	OutcomeApplied        Outcome = "applied"
	OutcomeRefused        Outcome = "refused"
	OutcomeRolledBack     Outcome = "rolled_back"
	OutcomeInterrupted    Outcome = "interrupted"
	OutcomeRollbackFailed Outcome = "rollback_failed"
	OutcomeRecovered      Outcome = "recovered"
	OutcomeNothing        Outcome = "nothing_to_do"
)

// Result describes what an operation did. The error returned beside it is nil
// only for applied, recovered, nothing_to_do and an operator-requested rollback.
type Result struct {
	Outcome   Outcome
	From, To  string // release versions
	BackupID  string
	BackupDir string
	// Derived lists the derived indexes the probe found damaged and rebuilt.
	Derived DerivedReport
	// Cause is why the operation was refused or rolled back.
	Cause error
}

func (u *Upgrader) dbPath() string { return filepath.Join(u.cfg.DataDir, store.DefaultFileName) }

func (u *Upgrader) openDB(ctx context.Context, reg *store.Registry) (*store.DB, error) {
	return store.Open(ctx, store.Options{Path: u.dbPath(), Config: u.cfg.Store, Migrations: reg, Clock: u.cfg.Clock, Logger: u.cfg.Logger})
}

func (u *Upgrader) releaseVerifier() ReleaseVerifier {
	return ReleaseVerifier{Keys: u.cfg.ReleaseKeys, Clock: u.cfg.Clock}
}

func releaseFile(id string) string { return "release-" + id + ".json" }

// executable is the file of the running binary, for Release.MatchesBinary.
func (u *Upgrader) executable() string {
	if u.cfg.Executable != "" {
		return u.cfg.Executable
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// Apply performs a staged upgrade: verify the signed release manifest, refuse a
// downgrade or replay, a release that is not this binary's (MatchesBinary) and a
// runtime the release was not qualified for (before anything changes), take a signed pre-upgrade backup, apply the migrations,
// switch the runtime, run the health probe, and record the new state. If any
// step after the backup fails, the database and runtime are restored from the
// backup and the error wraps ErrRolledBack. Every step is journaled outside the
// database, so a crash at any point is resolved by Recover.
func (u *Upgrader) Apply(ctx context.Context, req Request) (Result, error) {
	var res Result
	if err := u.assertStopped(); err != nil {
		return res, err
	}
	if err := backup.InterruptedRestore(u.cfg.DataDir); err != nil {
		return res, err
	}
	if err := u.ensureDir(); err != nil {
		return res, err
	}
	if j, err := u.loadJournal(); err != nil {
		return res, err
	} else if j != nil {
		return res, contracts.Newf(contracts.ErrConflict, "an earlier upgrade did not finish (phase %s); run recover first", j.Phase)
	}
	st, err := u.LoadState()
	if err != nil {
		return res, err
	}
	res.From = st.ReleaseVersion
	refuse := func(cause error) (Result, error) {
		res.Outcome, res.Cause = OutcomeRefused, cause
		return res, wrapOutcome(ErrRefused, contracts.CodeOf(cause), cause)
	}

	rel, err := u.releaseVerifier().VerifyRelease(ctx, req.Manifest)
	if err != nil {
		return refuse(err)
	}
	res.To = rel.Version
	if st.ReleaseVersion != "" && compareVersions(rel.Version, st.ReleaseVersion) <= 0 {
		return refuse(contracts.Newf(contracts.ErrConflict,
			"release %s is not newer than the installed %s; a signed older manifest replayed is a downgrade attack, use rollback to go back", rel.Version, st.ReleaseVersion))
	}
	if err := rel.MatchesBinary(u.cfg.Version, u.executable()); err != nil {
		return refuse(err)
	}
	if req.ArtifactDir != "" {
		if err := rel.VerifyArtifacts(req.ArtifactDir); err != nil {
			return refuse(err)
		}
	}

	var observedNow *RuntimeSet
	if u.cfg.Runtime != nil {
		cur, err := u.cfg.Runtime.Observe(ctx)
		if err != nil {
			return refuse(contracts.Wrap(contracts.ErrUnavailable, err, "the running runtime cannot be observed, so a rollback could not be guaranteed"))
		}
		if cur.Runtime != "" { // an empty set means none is recorded yet
			observedNow = &cur
		}
	}
	var target *RuntimeSet
	switchRuntime := false
	switch {
	case req.Runtime != nil:
		if u.cfg.Runtime == nil {
			return refuse(contracts.New(contracts.ErrValidationFailed, "a runtime set was requested but no runtime is managed here"))
		}
		t := *req.Runtime
		target, switchRuntime = &t, true
	case observedNow != nil:
		target = observedNow
	default:
		target = st.Runtime
	}
	from := st
	if observedNow != nil {
		from.Runtime = observedNow
	}
	if target != nil {
		// SEC-072: the runtime this upgrade would leave running must be one the
		// release was qualified for. Checked before anything is changed.
		if err := rel.Qualify(*target, u.cfg.Models...); err != nil {
			return refuse(err)
		}
	}

	backupDir, backupID, err := u.preBackup(ctx, &res)
	if err != nil {
		return refuse(err)
	}
	res.BackupID, res.BackupDir = backupID, backupDir
	prior := &Previous{}
	if _, err := u.readJSON(previousFile, prior); err != nil {
		prior = nil // unreadable: a rollback reads previous.json, as it always did
	}
	quarantined, err := u.quarantine.List() // as it is in the backup just taken
	if err != nil {
		return refuse(err)
	}
	if quarantined == nil {
		quarantined = []QuarantineEntry{}
	}
	j := Journal{
		ID: ids.NewGenerator(u.cfg.Clock).New("upg"), Phase: PhaseBackedUp,
		From: from, To: State{ReleaseVersion: rel.Version, Runtime: target}, SwitchRuntime: switchRuntime,
		BackupID: backupID, StartedAt: u.now().Format(timeLayout), Prior: prior, Quarantine: &quarantined,
	}
	if err := u.writeRaw(releaseFile(j.ID), req.Manifest); err != nil {
		return refuse(err)
	}
	if err := u.saveJournal(j); err != nil {
		_ = u.removeRecord(releaseFile(j.ID))
		return refuse(err)
	}
	u.cfg.Logger.Info("upgrade started", "from", st.ReleaseVersion, "to", rel.Version, "backup", backupID)
	return u.run(ctx, j, rel, res, OutcomeApplied)
}

// run executes the steps after the backup: migrate, switch the runtime, probe,
// commit. Each step is idempotent, so Recover reruns them all.
func (u *Upgrader) run(ctx context.Context, j Journal, rel *Release, res Result, ok Outcome) (Result, error) {
	u.describe(&res, j)
	step := func(phase string, fn func() error) error {
		j.Phase = phase
		if err := u.saveJournal(j); err != nil {
			return err
		}
		return fn()
	}
	err := step(PhaseMigrating, func() error {
		db, err := u.openDB(ctx, u.reg)
		if err != nil {
			return err
		}
		return db.Close()
	})
	if err == nil && j.SwitchRuntime && j.To.Runtime != nil && u.cfg.Runtime != nil {
		err = step(PhaseActivating, func() error { return u.cfg.Runtime.Activate(ctx, *j.To.Runtime) })
	}
	var observed *RuntimeSet
	if err == nil {
		err = step(PhaseProbing, func() error {
			var perr error
			observed, perr = u.probe(ctx, rel, j, &res)
			return perr
		})
	}
	if err != nil {
		if ctx.Err() != nil {
			// Interrupted (a signal), not failed: leave the journal for Recover.
			res.Outcome, res.Cause = OutcomeInterrupted, err
			return res, contracts.Wrap(contracts.ErrTimeout, err, "upgrade interrupted; run recover")
		}
		return u.rollback(ctx, j, res, err)
	}

	newState := j.To
	if observed != nil {
		newState.Runtime = observed
	}
	// The state first: until previous.json is replaced, a rollback still finds the
	// last successful upgrade's record and keeps its backup.
	if err := u.saveState(newState); err != nil {
		return u.rollback(ctx, j, res, err)
	}
	if err := u.writeJSON(previousFile, Previous{State: j.From, BackupID: j.BackupID, Quarantine: j.Quarantine}); err != nil {
		return u.rollback(ctx, j, res, err)
	}
	if err := u.removeRecord(journalFile); err != nil {
		return res, err
	}
	_ = u.removeRecord(releaseFile(j.ID))
	u.keepBackups(j.BackupID)
	res.Outcome = ok
	u.cfg.Logger.Info("upgrade finished", "to", j.To.ReleaseVersion, "outcome", string(ok))
	return res, nil
}

// probe is the health probe of the migrated installation: the store opens
// (migration checksums hold), damaged derived indexes are isolated and rebuilt,
// the whole database passes its integrity check, the caller's probe passes, and
// the runtime that is running is the one asked for and is qualified by the
// release (SEC-072). It returns the runtime set it observed.
func (u *Upgrader) probe(ctx context.Context, rel *Release, j Journal, res *Result) (*RuntimeSet, error) {
	db, err := u.openDB(ctx, u.reg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	idx, err := u.derived(ctx, db)
	if err != nil {
		return nil, err
	}
	rep, err := RepairDerived(ctx, u.quarantine, idx)
	res.Derived.merge(rep)
	if err != nil {
		return nil, err
	}
	if err := db.IntegrityCheck(ctx); err != nil {
		return nil, err
	}
	if u.cfg.Probe != nil {
		if err := u.cfg.Probe(ctx, db); err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the health probe failed")
		}
	}
	if u.cfg.Runtime == nil || j.To.Runtime == nil {
		return nil, nil
	}
	obs, err := u.cfg.Runtime.Observe(ctx)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the runtime cannot be observed after the upgrade")
	}
	if err := rel.Qualify(obs, u.cfg.Models...); err != nil {
		return nil, err
	}
	if !sameSet(obs, *j.To.Runtime) {
		return nil, contracts.Newf(contracts.ErrUnavailable, "the runtime runs %s %s, not the %s %s that was asked for", obs.Runtime, obs.Version, j.To.Runtime.Runtime, j.To.Runtime.Version)
	}
	return &obs, nil
}

func (u *Upgrader) derived(ctx context.Context, db *store.DB) ([]DerivedIndex, error) {
	idx, err := FTSIndexes(ctx, db)
	if err != nil {
		return nil, err
	}
	if u.cfg.ExtraDerived != nil {
		idx = append(idx, u.cfg.ExtraDerived(db)...)
	}
	return uniqueByName(idx), nil
}

// uniqueByName keeps one index per name, the last one listed: what the
// composition root adds replaces a built-in index of the same name.
func uniqueByName(idx []DerivedIndex) []DerivedIndex {
	last := map[string]int{}
	for i, ix := range idx {
		last[ix.Name()] = i
	}
	out := make([]DerivedIndex, 0, len(last))
	for i, ix := range idx {
		if last[ix.Name()] == i {
			out = append(out, ix)
		}
	}
	return out
}

// sameSet reports whether obs is the set want asks for: same runtime and version
// and every backend of want present at the same version or digest.
func sameSet(obs, want RuntimeSet) bool {
	if obs.Runtime != want.Runtime || !sameVersion(obs.Version, want.Version) {
		return false
	}
	for name, v := range want.Backends {
		if got, ok := obs.Backends[name]; !ok || !(sameVersion(got, v) || got == v) {
			return false
		}
	}
	return true
}

// preBackup takes the signed pre-upgrade backup. The database is opened without
// applying any migration (an empty registry), so the backup is of the state the
// upgrade started from. Damaged derived indexes are rebuilt first: a database
// that fails its integrity check cannot be backed up, and a derived index is
// rebuilt from its source without loss. Damage that cannot be rebuilt in place
// refuses the upgrade (the operator restores the last backup).
func (u *Upgrader) preBackup(ctx context.Context, out *Result) (dir, id string, err error) {
	db, err := u.openDB(ctx, store.NewRegistry())
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	idx, err := u.derived(ctx, db)
	if err != nil {
		return "", "", err
	}
	rep, err := RepairDerived(ctx, u.quarantine, idx)
	out.Derived.merge(rep)
	if err != nil {
		return "", "", err
	}
	res, err := backup.Create(ctx, db, backup.Options{
		Dir: filepath.Join(u.dir(), backupsDirName), DataDir: u.cfg.DataDir, Trees: u.cfg.BackupTrees,
		Signer: u.cfg.BackupSigner, Clock: u.cfg.Clock, Version: u.cfg.Version,
	})
	if err != nil {
		return "", "", err
	}
	return res.Dir, res.ID, nil
}

// rollback restores the database from the journal's backup and puts the runtime
// back. It is resumable: the journal says how far it got.
func (u *Upgrader) rollback(ctx context.Context, j Journal, res Result, cause error) (Result, error) {
	u.describe(&res, j)
	backupDir, err := u.backupPath(j.BackupID)
	if err != nil {
		return res, err
	}
	failed := func(err error) (Result, error) {
		res.Outcome, res.Cause = OutcomeRollbackFailed, err
		return res, contracts.Wrap(contracts.ErrInternal, fmt.Errorf("%w: %w (original failure: %v)", ErrRollbackFailed, err, cause),
			"the journal and the backup "+backupDir+" are kept; run recover")
	}
	if j.Phase != PhaseRestored {
		j.Phase = PhaseRollingBack
		if err := u.saveJournal(j); err != nil {
			return failed(err)
		}
		if _, err := backup.Restore(ctx, backup.RestoreOptions{
			BackupDir: backupDir, DataDir: u.cfg.DataDir, Keys: u.cfg.BackupKeys, Clock: u.cfg.Clock, Force: true,
		}); err != nil {
			return failed(err)
		}
		j.Phase = PhaseRestored
		if err := u.saveJournal(j); err != nil {
			return failed(err)
		}
	}
	if j.Quarantine != nil {
		// The quarantine lives outside the restored database: it goes back with it.
		if err := u.quarantine.reset(*j.Quarantine); err != nil {
			return failed(err)
		}
	}
	if u.cfg.Runtime != nil && (j.From.Runtime != nil || j.SwitchRuntime) {
		prev := RuntimeSet{} // the empty set means "no runtime recorded"
		if j.From.Runtime != nil {
			prev = *j.From.Runtime
		}
		if err := u.cfg.Runtime.Activate(ctx, prev); err != nil {
			return failed(contracts.Wrap(contracts.ErrUnavailable, err, "the previous runtime cannot be reactivated"))
		}
	}
	if j.From.ReleaseVersion != "" || j.From.Runtime != nil {
		if err := u.saveState(j.From); err != nil {
			return failed(err)
		}
	} else if err := u.removeRecord(stateFile); err != nil {
		return failed(err)
	}
	// Keep the backup the last successful upgrade can still be rolled back with;
	// a manual rollback consumes it.
	switch {
	case j.Manual:
		_ = u.removeRecord(previousFile)
		u.keepBackups()
	case j.Prior != nil:
		// previous.json may already name this upgrade (it crashed after writing
		// it): the record from before the upgrade is put back.
		if j.Prior.BackupID == "" {
			if err := u.removeRecord(previousFile); err != nil {
				return failed(err)
			}
			u.keepBackups()
		} else {
			if err := u.writeJSON(previousFile, *j.Prior); err != nil {
				return failed(err)
			}
			u.keepBackups(j.Prior.BackupID)
		}
	default:
		var prev Previous
		if found, _ := u.readJSON(previousFile, &prev); found {
			u.keepBackups(prev.BackupID)
		} else {
			u.keepBackups()
		}
	}
	if err := u.removeRecord(journalFile); err != nil {
		return failed(err)
	}
	_ = u.removeRecord(releaseFile(j.ID))
	res.Outcome, res.Cause = OutcomeRolledBack, cause
	u.cfg.Logger.Warn("upgrade rolled back", "to", j.From.ReleaseVersion, "backup", j.BackupID)
	if j.Manual {
		return res, nil
	}
	return res, wrapOutcome(ErrRolledBack, contracts.ErrUnavailable, cause)
}

// Recover resolves an unfinished upgrade after a crash or an interruption. An
// upgrade that stopped before its rollback began is rolled forward (the
// migrations are idempotent, each in its own transaction) and, if it cannot
// complete or fails its health probe, rolled back to the pre-upgrade backup. A
// rollback that was interrupted is finished. With no journal it rebuilds any
// derived index that was isolated and not rebuilt.
func (u *Upgrader) Recover(ctx context.Context) (Result, error) {
	var res Result
	j, err := u.loadJournal()
	if err != nil {
		return res, err
	}
	if j == nil {
		entries, err := u.quarantine.List()
		if err != nil || len(entries) == 0 {
			res.Outcome = OutcomeNothing
			return res, err
		}
		db, err := u.openDB(ctx, u.reg)
		if err != nil {
			return res, err
		}
		defer db.Close()
		idx, err := u.derived(ctx, db)
		if err != nil {
			return res, err
		}
		rep, err := RepairDerived(ctx, u.quarantine, idx)
		res.Derived.merge(rep)
		if err != nil {
			return res, err
		}
		res.Outcome = OutcomeRecovered
		return res, nil
	}
	// Both ways out replace or migrate the database, as Apply and Rollback do.
	if err := u.assertStopped(); err != nil {
		return res, err
	}
	u.describe(&res, *j)
	if j.Phase == PhaseRollingBack || j.Phase == PhaseRestored {
		return u.rollback(ctx, *j, res, errors.New("an interrupted rollback was resumed"))
	}
	raw, err := os.ReadFile(filepath.Join(u.dir(), releaseFile(j.ID)))
	if err != nil {
		return u.rollback(ctx, *j, res, contracts.Wrap(contracts.ErrUnavailable, err, "the release manifest of the unfinished upgrade is missing"))
	}
	rel, err := u.releaseVerifier().VerifyRelease(ctx, raw)
	if err != nil {
		return u.rollback(ctx, *j, res, err)
	}
	return u.run(ctx, *j, rel, res, OutcomeRecovered)
}

// Rollback returns to the state before the last successful upgrade at the
// operator's request: the pre-upgrade backup that upgrade recorded is verified
// and restored and the previous runtime set reactivated. Everything written since
// the upgrade stays in <data dir>/.pre-restore-*; it is not merged back.
func (u *Upgrader) Rollback(ctx context.Context) (Result, error) {
	var res Result
	if err := u.assertStopped(); err != nil {
		return res, err
	}
	if j, err := u.loadJournal(); err != nil {
		return res, err
	} else if j != nil {
		return res, contracts.Newf(contracts.ErrConflict, "an upgrade did not finish (phase %s); run recover first", j.Phase)
	}
	var prev Previous
	found, err := u.readJSON(previousFile, &prev)
	if err != nil {
		return res, err
	}
	if !found {
		return res, contracts.New(contracts.ErrConflict, "there is no earlier installed version to roll back to")
	}
	dir, err := u.backupPath(prev.BackupID)
	if err != nil {
		return res, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return res, contracts.Newf(contracts.ErrConflict, "the pre-upgrade backup %s is gone; restore a backup of your own with 'alixai backup restore'", prev.BackupID)
	}
	cur, err := u.LoadState()
	if err != nil {
		return res, err
	}
	// The runtime goes back as an automatic rollback puts it back: to the previous
	// set, or to the empty one when the upgrade recorded a set where none was before.
	j := Journal{ID: ids.NewGenerator(u.cfg.Clock).New("upg"), Phase: PhaseBackedUp, From: prev.State, To: cur, BackupID: prev.BackupID,
		SwitchRuntime: cur.Runtime != nil, Manual: true, StartedAt: u.now().Format(timeLayout), Quarantine: prev.Quarantine}
	if err := u.saveJournal(j); err != nil {
		return res, err
	}
	return u.rollback(ctx, j, res, errors.New("rollback requested by the operator"))
}

// assertStopped refuses to replace files under a process that has them open. A
// -shm file beside the database means some process has it open (or crashed with
// it open); on Unix a rename would not stop that process, which would keep writing
// to the moved file while new connections open the restored one.
func (u *Upgrader) assertStopped() error {
	if u.cfg.AllowInUse {
		return nil
	}
	shm := u.dbPath() + "-shm"
	if _, err := os.Lstat(shm); err == nil {
		return contracts.New(contracts.ErrConflict,
			"the database looks in use ("+store.DefaultFileName+"-shm exists): stop the service first; if it crashed, start it once and stop it again so SQLite cleans up, or force").
			WithDetails("path", shm)
	}
	return nil
}

// describe fills the identifying fields of a result from the journal.
func (u *Upgrader) describe(res *Result, j Journal) {
	res.From, res.To, res.BackupID = j.From.ReleaseVersion, j.To.ReleaseVersion, j.BackupID
	if dir, err := u.backupPath(j.BackupID); err == nil {
		res.BackupDir = dir
	}
}

// keepBackups removes the pre-upgrade backups other than the ones named: a
// backup holds all of the data, and the one the last upgrade can be rolled back
// with is enough.
func (u *Upgrader) keepBackups(keep ...string) {
	root := filepath.Join(u.dir(), backupsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		kept := false
		for _, k := range keep {
			if e.Name() == k {
				kept = true
			}
		}
		if !kept {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// Status describes the installation for an operator.
type Status struct {
	State       State
	Unfinished  *Journal
	Quarantined []QuarantineEntry
}

// Status reads the recorded state, any unfinished upgrade and the quarantine.
func (u *Upgrader) Status() (Status, error) {
	var s Status
	var err error
	if s.State, err = u.LoadState(); err != nil {
		return s, err
	}
	if s.Unfinished, err = u.loadJournal(); err != nil {
		return s, err
	}
	s.Quarantined, err = u.quarantine.List()
	return s, err
}

func wrapOutcome(sentinel error, code contracts.ErrCode, cause error) error {
	return contracts.Wrap(code, fmt.Errorf("%w: %w", sentinel, cause), "")
}
