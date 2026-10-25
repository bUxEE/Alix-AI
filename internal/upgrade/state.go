package upgrade

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Upgrade files live OUTSIDE the database, because the database is what a
// rollback replaces:
//
//	<data dir>/upgrade/state.json        what is installed now: release version and runtime set
//	<data dir>/upgrade/previous.json     what was installed before the last successful upgrade
//	<data dir>/upgrade/journal.json      present only while an upgrade is unfinished
//	<data dir>/upgrade/release-<id>.json the verified manifest the unfinished upgrade applies
//	<data dir>/upgrade/backups/<id>/     the pre-upgrade backup (internal/backup)
//	<data dir>/upgrade/quarantine/       derived indexes isolated and not yet rebuilt
const (
	dirName         = "upgrade"
	stateFile       = "state.json"
	previousFile    = "previous.json"
	journalFile     = "journal.json"
	backupsDirName  = "backups"
	quarantineDir   = "quarantine"
	timeLayout      = "2006-01-02T15:04:05Z"
	maxControlBytes = 1 << 20
)

// State is what is installed.
type State struct {
	ReleaseVersion string      `json:"release_version"`
	Runtime        *RuntimeSet `json:"runtime,omitempty"`
	UpdatedAt      string      `json:"updated_at,omitempty"`
}

// Phases of an unfinished upgrade, recorded in the journal before the step
// they name starts, so a crash leaves the journal naming the step to redo.
const (
	// PhaseBackedUp: the pre-upgrade backup is complete; nothing else changed.
	PhaseBackedUp = "backed_up"
	// PhaseMigrating: migrations are being applied (each is its own
	// transaction, so the database is at a migration boundary at any instant).
	PhaseMigrating = "migrating"
	// PhaseActivating: the runtime is being switched to the new set.
	PhaseActivating = "activating"
	// PhaseProbing: the health probe is running.
	PhaseProbing = "probing"
	// PhaseRollingBack: the database is being restored from the backup.
	PhaseRollingBack = "rolling_back"
	// PhaseRestored: the database is restored; the runtime is being put back.
	PhaseRestored = "restored"
)

// Previous is what the last successful upgrade replaced and the backup that
// holds it: Rollback restores exactly this backup, never "the newest one".
type Previous struct {
	State    State  `json:"state"`
	BackupID string `json:"backup_id"`
	// Quarantine is the journal's: what Rollback puts back with the backup.
	Quarantine *[]QuarantineEntry `json:"quarantine,omitempty"`
}

// Journal records an unfinished upgrade.
type Journal struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
	From  State  `json:"from"`
	To    State  `json:"to"`
	// BackupID names <data dir>/upgrade/backups/<id>; the directory is derived from
	// it (and the id validated), never read from the file.
	BackupID string `json:"backup_id"`
	// SwitchRuntime is true when the upgrade switches the runtime to To.Runtime
	// (otherwise the runtime stays and is only checked).
	SwitchRuntime bool `json:"switch_runtime,omitempty"`
	// Manual marks a rollback the operator asked for (it also rewrites state).
	Manual    bool   `json:"manual,omitempty"`
	StartedAt string `json:"started_at"`
	// Prior is what previous.json held when the upgrade started (an empty
	// BackupID: nothing). A rollback puts it back, so a crash after the upgrade
	// replaced previous.json cannot cost the backup of the upgrade before. Nil in
	// a journal written before it was recorded: the rollback reads previous.json.
	Prior *Previous `json:"prior,omitempty"`
	// Quarantine is the quarantine as it was when the backup was taken. It lives
	// outside the database, so a rollback puts it back with the backup: a record
	// added since is released, one released since is recorded again. Nil in a
	// journal written before it was recorded: the quarantine is left as it is.
	Quarantine *[]QuarantineEntry `json:"quarantine,omitempty"`
}

var (
	backupIDRE = regexp.MustCompile(`^bkp_[0-9A-Z]{26}$`)
	// journalIDRE is the id Apply and Rollback generate; it names release-<id>.json.
	journalIDRE = regexp.MustCompile(`^upg_[0-9A-Z]{26}$`)
)

// backupPath is the directory of backup id, which must be a well-formed id: a
// journal that names anything else is refused rather than followed.
func (u *Upgrader) backupPath(id string) (string, error) {
	if !backupIDRE.MatchString(id) {
		return "", contracts.Newf(contracts.ErrInternal, "upgrade record names a malformed backup id %q", id)
	}
	return filepath.Join(u.dir(), backupsDirName, id), nil
}

func (u *Upgrader) dir() string { return filepath.Join(u.cfg.DataDir, dirName) }

func (u *Upgrader) ensureDir() error {
	if err := os.MkdirAll(u.dir(), 0o700); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "upgrade directory cannot be created")
	}
	return nil
}

// writeJSON writes v to <dir>/name atomically (temporary file, fsync, rename)
// with mode 0600.
func (u *Upgrader) writeJSON(name string, v any) error {
	if err := u.ensureDir(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "upgrade record cannot be encoded")
	}
	path := filepath.Join(u.dir(), name)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "upgrade record cannot be written")
	}
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return contracts.Wrap(contracts.ErrUnavailable, err, "upgrade record cannot be written")
	}
	return nil
}

// readJSON reads <dir>/name strictly into v. found is false when the file does
// not exist. A file that exists but cannot be read or parsed is an error: a
// journal nobody can read is never guessed at.
func (u *Upgrader) readJSON(name string, v any) (found bool, err error) {
	path := filepath.Join(u.dir(), name)
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxControlBytes {
		return false, contracts.Newf(contracts.ErrInternal, "upgrade record %s is unreadable", name)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, contracts.Wrap(contracts.ErrInternal, err, "upgrade record "+name+" is unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false, contracts.Wrap(contracts.ErrInternal, err, "upgrade record "+name+" is malformed; it is not guessed at")
	}
	return true, nil
}

// writeRaw writes bytes to <dir>/name atomically with mode 0600.
func (u *Upgrader) writeRaw(name string, data []byte) error {
	if err := u.ensureDir(); err != nil {
		return err
	}
	path := filepath.Join(u.dir(), name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "upgrade record cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return contracts.Wrap(contracts.ErrUnavailable, err, "upgrade record cannot be written")
	}
	return nil
}

func (u *Upgrader) removeRecord(name string) error {
	if err := os.Remove(filepath.Join(u.dir(), name)); err != nil && !os.IsNotExist(err) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "upgrade record "+name+" cannot be removed")
	}
	return nil
}

// LoadState returns what is installed; the zero State when nothing is recorded.
func (u *Upgrader) LoadState() (State, error) {
	var s State
	_, err := u.readJSON(stateFile, &s)
	return s, err
}

func (u *Upgrader) saveState(s State) error {
	s.UpdatedAt = u.now().Format(timeLayout)
	return u.writeJSON(stateFile, s)
}

func (u *Upgrader) loadJournal() (*Journal, error) {
	var j Journal
	found, err := u.readJSON(journalFile, &j)
	if err != nil || !found {
		return nil, err
	}
	if !journalIDRE.MatchString(j.ID) {
		return nil, contracts.Newf(contracts.ErrInternal, "upgrade record names a malformed journal id %q", j.ID)
	}
	if _, err := u.backupPath(j.BackupID); err != nil {
		return nil, err
	}
	if j.Prior != nil && j.Prior.BackupID != "" {
		if _, err := u.backupPath(j.Prior.BackupID); err != nil {
			return nil, err
		}
	}
	return &j, nil
}

func (u *Upgrader) saveJournal(j Journal) error { return u.writeJSON(journalFile, j) }

func (u *Upgrader) now() time.Time { return u.cfg.Clock.Now().UTC().Truncate(time.Second) }
