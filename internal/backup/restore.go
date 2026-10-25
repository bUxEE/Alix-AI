package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Keys are the public keys the manifest signature is checked against.
	Keys trust.KeyLookup
	// Clock is read for signature expiry. Nil means the wall clock.
	Clock clock.Clock
	// TempDir is where a copy of the database is made for the integrity check
	// (the backup itself is never opened by SQLite). Empty means os.TempDir().
	TempDir string
}

// Verify checks a backup directory without changing it or any live data: the
// manifest's syntax and signature, that the directory holds exactly the listed
// files, that every file has the listed size and SHA-256, and that the
// database passes an integrity check. It returns the verified manifest.
func Verify(ctx context.Context, dir string, o VerifyOptions) (*Manifest, error) {
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	m, err := loadManifest(ctx, dir, o.Keys, clk)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(o.TempDir, "alixai-verify-")
	if err != nil {
		return nil, unavailable(err, "backup verification scratch directory cannot be created")
	}
	defer os.RemoveAll(tmp)
	if err := stage(ctx, dir, m, tmp, false); err != nil {
		return nil, err
	}
	return m, nil
}

// stage checks every listed file of the backup, size and SHA-256 of the bytes
// it read (so a file changed after the listing check is still caught), then
// runs an integrity check of the database. The database is always copied into
// stageDir first (SQLite never opens the backup itself); the trees are copied
// too when copyTrees is set (a restore places the copies) and are hashed where
// they are otherwise.
func stage(ctx context.Context, backupDir string, m *Manifest, stageDir string, copyTrees bool) error {
	for _, f := range m.Signed.Files {
		if err := ctx.Err(); err != nil {
			return contracts.Wrap(contracts.ErrTimeout, err, "backup verification cancelled")
		}
		rel := filepath.FromSlash(f.Path)
		var sum string
		var size int64
		var err error
		if copyTrees || f.Path == DatabaseFile {
			sum, size, err = copyHashed(filepath.Join(backupDir, rel), filepath.Join(stageDir, rel))
		} else {
			sum, size, err = hashRegular(filepath.Join(backupDir, rel))
		}
		if err != nil {
			return unavailable(err, "backup file "+f.Path+" cannot be read")
		}
		if sum != f.SHA256 || size != f.Size {
			return contracts.Newf(contracts.ErrValidationFailed,
				"backup file %s does not match the signed manifest (sha256 %.12s, %d bytes; listed %.12s, %d bytes)", f.Path, sum, size, f.SHA256, f.Size)
		}
	}
	db := filepath.Join(stageDir, DatabaseFile)
	if err := store.IntegrityCheckFile(ctx, db); err != nil {
		return err
	}
	// The check opens the file read-only; make sure it left no journal behind.
	_ = os.Remove(db + "-wal")
	_ = os.Remove(db + "-shm")
	return nil
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// BackupDir is the backup directory (<dir>/<backup id>).
	BackupDir string
	// DataDir is the data directory to restore into. Created owner only if missing.
	DataDir string
	// Keys are the public keys the manifest signature is checked against. A
	// restore after the data directory was lost needs them from somewhere else.
	Keys trust.KeyLookup
	// Clock is read for signature expiry. Nil means the wall clock.
	Clock clock.Clock
	// Force restores although the database looks in use (its -shm file exists:
	// a running service, or one that crashed). The files are moved aside, not
	// deleted, either way. Stop the service first.
	Force bool
	// SkipNewerRevocations restores the publisher and issuer keys exactly as the
	// backup holds them. By default a key that the database being replaced holds
	// as revoked is revoked in the restored one too, so an older backup cannot
	// bring back a key that was revoked after it was taken (L3). Set it only
	// when the database in place is known to be wrong, and revoke again by hand
	// what must stay revoked.
	SkipNewerRevocations bool
}

// RestoreResult describes a finished restore.
type RestoreResult struct {
	BackupID string
	// PreRestoreDir holds the database and trees that were in place before, so
	// audit records and receipts written after the backup are not lost. Empty
	// when nothing was in place. It is never deleted by this package.
	PreRestoreDir string
	// CarriedRevocations is how many keys the restored database holds as
	// revoked because the database it replaced did (RestoreOptions
	// .SkipNewerRevocations).
	CarriedRevocations int
}

// Restore replaces the database and file trees of o.DataDir with the backup's,
// keeping the key revocations of the database it replaces (see
// RestoreOptions.SkipNewerRevocations).
// Nothing live is touched until the backup has been verified (see Verify) and
// copied to a staging directory inside the data directory; then the live files
// are moved aside and the staged ones renamed into place as a unit. If any
// step of that fails the previous state is put back. The swap is recorded in
// the data directory while it runs, so one cut by a crash is undone by the next
// Restore, and InterruptedRestore refuses to open the directory meanwhile.
//
// The service must not have the database open: on Windows the rename fails,
// on Unix it would not, which is why a leftover -shm file refuses the restore
// unless o.Force is set.
func Restore(ctx context.Context, o RestoreOptions) (RestoreResult, error) {
	if strings.TrimSpace(o.BackupDir) == "" || strings.TrimSpace(o.DataDir) == "" {
		return RestoreResult{}, contracts.New(contracts.ErrValidationFailed, "restore: backup and data directory are required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	backupDir, err := filepath.Abs(o.BackupDir)
	if err != nil {
		return RestoreResult{}, contracts.Wrap(contracts.ErrValidationFailed, err, "restore: backup directory invalid")
	}
	dataDir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return RestoreResult{}, contracts.Wrap(contracts.ErrValidationFailed, err, "restore: data directory invalid")
	}
	m, err := loadManifest(ctx, backupDir, o.Keys, clk)
	if err != nil {
		return RestoreResult{}, err
	}
	for _, t := range m.Signed.Trees {
		if within(filepath.Join(dataDir, t), backupDir) {
			return RestoreResult{}, contracts.Newf(contracts.ErrValidationFailed, "restore: the backup lies inside the %s tree it would replace", t)
		}
	}
	live := filepath.Join(dataDir, DatabaseFile)
	if !o.Force && exists(live+"-shm") {
		return RestoreResult{}, contracts.New(contracts.ErrConflict,
			"restore: the database looks in use ("+DatabaseFile+"-shm exists); stop the service, or pass force if it crashed").
			WithDetails("path", live+"-shm")
	}
	if _, err := fsperm.MkdirOwnerOnly(dataDir); err != nil {
		return RestoreResult{}, unavailable(err, "restore: data directory cannot be created")
	}
	if err := undoInterruptedRestore(dataDir); err != nil {
		return RestoreResult{}, err
	}

	stageDir := filepath.Join(dataDir, ".restore-"+m.Signed.BackupID)
	if err := os.RemoveAll(stageDir); err != nil { // scratch of an interrupted restore
		return RestoreResult{}, unavailable(err, "restore: stale staging directory cannot be removed")
	}
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return RestoreResult{}, unavailable(err, "restore: staging directory cannot be created")
	}
	defer os.RemoveAll(stageDir)
	// The staged database and trees are what the data directory becomes: owner
	// only before the first byte is copied (on Windows a directory would
	// otherwise inherit what the data directory's parent grants).
	if err := fsperm.Restrict(stageDir, true); err != nil {
		return RestoreResult{}, unavailable(err, "restore: staging directory cannot be made owner only")
	}
	if err := stage(ctx, backupDir, m, stageDir, true); err != nil {
		return RestoreResult{}, err
	}
	carried := 0
	if !o.SkipNewerRevocations {
		live, err := liveRevocations(ctx, filepath.Join(dataDir, DatabaseFile), filepath.Join(stageDir, ".live"))
		if err != nil {
			return RestoreResult{}, contracts.Wrap(contracts.ErrUnavailable, err,
				"restore: the key revocations of the database in place cannot be read, and restoring without them could make revoked keys valid again; "+
					"restore with skip-newer-revocations only if that database is known to be wrong")
		}
		if carried, err = applyRevocations(ctx, filepath.Join(stageDir, DatabaseFile), live); err != nil {
			return RestoreResult{}, contracts.Wrap(contracts.ErrUnavailable, err, "restore: the key revocations of the database in place cannot be carried into the restored one")
		}
	}

	// Swap. Everything before this point only read live data. The marker is
	// written first, so a crash at any point of the swap leaves a record of it.
	aside := append([]string{DatabaseFile, DatabaseFile + "-wal", DatabaseFile + "-shm"}, m.Signed.Trees...)
	mk := restoreMarker{BackupID: m.Signed.BackupID, PreRestore: ".pre-restore-" + ids.NewGenerator(clk).New("pre"), Names: aside, Existed: []string{}}
	for _, name := range aside {
		if exists(filepath.Join(dataDir, name)) {
			mk.Existed = append(mk.Existed, name)
		}
	}
	if err := writeRestoreMarker(dataDir, mk); err != nil {
		return RestoreResult{}, err
	}
	pre := filepath.Join(dataDir, mk.PreRestore)
	fail := func(err error, what string) (RestoreResult, error) {
		if uerr := undoSwap(dataDir, mk); uerr != nil {
			return RestoreResult{}, unavailable(err, "restore failed and the previous state could not be fully put back ("+uerr.Error()+
				"); "+restoreMarkerFile+" records it and the next restore finishes putting it back: "+what)
		}
		if rerr := removeRestoreMarker(dataDir); rerr != nil {
			return RestoreResult{}, unavailable(err, "restore failed and the previous state was put back, but "+rerr.Error()+": "+what)
		}
		return RestoreResult{}, unavailable(err, "restore failed and the previous state was put back: "+what)
	}
	for _, name := range mk.Existed {
		if _, err := fsperm.MkdirOwnerOnly(pre); err != nil {
			return fail(err, "the pre-restore directory cannot be created")
		}
		if err := swapRename(filepath.Join(dataDir, name), filepath.Join(pre, name)); err != nil {
			return fail(err, name+" cannot be moved aside (is the service still running?)")
		}
	}
	place := append([]string{DatabaseFile}, m.Signed.Trees...)
	for _, name := range place {
		src := filepath.Join(stageDir, name)
		if name != DatabaseFile {
			src = filepath.Join(stageDir, TreeRoot, name)
		}
		if !exists(src) {
			continue // a tree that was empty at backup time stays absent
		}
		if err := swapRename(src, filepath.Join(dataDir, name)); err != nil {
			return fail(err, name+" cannot be moved into place")
		}
	}
	if err := removeRestoreMarker(dataDir); err != nil {
		return fail(err, "the restore cannot be recorded as finished")
	}
	res := RestoreResult{BackupID: m.Signed.BackupID, CarriedRevocations: carried}
	if len(mk.Existed) > 0 {
		res.PreRestoreDir = pre
	}
	return res, nil
}

// swapStep, when set, runs before each rename of the swap: a test fails or
// stops the swap there.
var swapStep func(from, to string) error

func swapRename(from, to string) error {
	if swapStep != nil {
		if err := swapStep(from, to); err != nil {
			return err
		}
	}
	return os.Rename(from, to)
}

// restoreMarkerFile is in the data directory while a restore swaps files: from
// before the first file is moved until the last one is in place, or until a
// failed swap was undone. A data directory that holds it may be part backup and
// part what was there before, and is not opened (InterruptedRestore); the next
// Restore puts the previous state back first.
const restoreMarkerFile = ".restore-in-progress.json"

// restoreMarker records a swap. It names only what the swap moves; the paths
// are derived from the names, which are checked, never read from the file.
type restoreMarker struct {
	BackupID string `json:"backup_id"`
	// PreRestore is the directory (inside the data directory) the live files
	// are moved into.
	PreRestore string `json:"pre_restore"`
	// Names are what the swap moves aside and places: the database, its -wal and
	// -shm, and the trees.
	Names []string `json:"names"`
	// Existed are those of Names that were in place before the swap.
	Existed []string `json:"existed"`
}

var preRestoreRE = regexp.MustCompile(`^\.pre-restore-pre_[0-9A-Z]{26}$`)

func (mk restoreMarker) validate() error {
	if !backupIDRE.MatchString(mk.BackupID) || !preRestoreRE.MatchString(mk.PreRestore) {
		return contracts.New(contracts.ErrInternal, "restore: "+restoreMarkerFile+" is malformed; it is not acted on")
	}
	names := map[string]bool{}
	for _, n := range mk.Names {
		if n != DatabaseFile && n != DatabaseFile+"-wal" && n != DatabaseFile+"-shm" && !restorableTrees[n] {
			return contracts.Newf(contracts.ErrInternal, "restore: %s names %q; it is not acted on", restoreMarkerFile, n)
		}
		names[n] = true
	}
	for _, n := range mk.Existed {
		if !names[n] {
			return contracts.Newf(contracts.ErrInternal, "restore: %s names %q; it is not acted on", restoreMarkerFile, n)
		}
	}
	return nil
}

func writeRestoreMarker(dataDir string, mk restoreMarker) error {
	raw, err := json.MarshalIndent(mk, "", "  ")
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "restore: the restore marker cannot be encoded")
	}
	if err := writeFileAtomic(filepath.Join(dataDir, restoreMarkerFile), append(raw, '\n')); err != nil {
		return unavailable(err, "restore: the restore marker cannot be written")
	}
	return nil
}

func removeRestoreMarker(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, restoreMarkerFile)); err != nil && !os.IsNotExist(err) {
		return unavailable(err, "the restore marker cannot be removed")
	}
	return nil
}

// readRestoreMarker returns the marker of dataDir; found is false when there is none.
func readRestoreMarker(dataDir string) (mk restoreMarker, found bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, restoreMarkerFile))
	if os.IsNotExist(err) {
		return mk, false, nil
	}
	if err != nil {
		return mk, false, unavailable(err, "restore: the restore marker cannot be read")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mk); err != nil {
		return mk, false, contracts.Wrap(contracts.ErrInternal, err, "restore: "+restoreMarkerFile+" is malformed; it is not acted on")
	}
	return mk, true, mk.validate()
}

// undoSwap puts the data directory back as it was before the swap mk records,
// whatever point the swap reached: a name that was in place and was moved aside
// comes back (replacing what was placed instead), a name that was not in place
// is removed, and a name in place that was not moved yet was not replaced either
// (every move aside comes before the first placement). Running it again changes
// nothing. It returns every failure.
func undoSwap(dataDir string, mk restoreMarker) error {
	pre := filepath.Join(dataDir, mk.PreRestore)
	existed := map[string]bool{}
	for _, n := range mk.Existed {
		existed[n] = true
	}
	var errs []error
	for i := len(mk.Names) - 1; i >= 0; i-- {
		name := mk.Names[i]
		live, aside := filepath.Join(dataDir, name), filepath.Join(pre, name)
		switch {
		case exists(aside):
			if err := os.RemoveAll(live); err != nil {
				errs = append(errs, err)
			} else if err := os.Rename(aside, live); err != nil {
				errs = append(errs, err)
			}
		case !existed[name]:
			if err := os.RemoveAll(live); err != nil {
				errs = append(errs, err)
			}
		case !exists(live):
			errs = append(errs, fmt.Errorf("%s was in place before the restore and is neither in place nor in %s", name, mk.PreRestore))
		}
	}
	_ = os.Remove(pre) // only when empty: what is left in it is kept
	return errors.Join(errs...)
}

// undoInterruptedRestore puts back the state before a restore of dataDir that
// was interrupted, if there was one, and removes its marker and scratch.
func undoInterruptedRestore(dataDir string) error {
	mk, found, err := readRestoreMarker(dataDir)
	if err != nil || !found {
		return err
	}
	if err := undoSwap(dataDir, mk); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "restore: an interrupted restore of backup "+mk.BackupID+
			" cannot be undone; "+restoreMarkerFile+" is kept").WithDetails("pre_restore_dir", filepath.Join(dataDir, mk.PreRestore))
	}
	if err := removeRestoreMarker(dataDir); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(dataDir, ".restore-"+mk.BackupID))
	return nil
}

// InterruptedRestore returns an error when a restore into dataDir was
// interrupted while it swapped files: the data directory may be part backup and
// part what was there before, and must not be opened. Running the restore again
// puts the previous state back first and then restores.
func InterruptedRestore(dataDir string) error {
	mk, found, err := readRestoreMarker(dataDir)
	if err != nil || !found {
		return err
	}
	return contracts.Newf(contracts.ErrConflict,
		"a restore of backup %s into %s was interrupted and the data directory may be half restored: run the restore again (it puts the previous state back first), or move the contents of %s back",
		mk.BackupID, dataDir, mk.PreRestore).WithDetails("path", filepath.Join(dataDir, restoreMarkerFile))
}
