package apppkg

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("apppkg", migrations) }

// State is where an installed package is in its life. A package is installed
// quarantined. Only the integration audit (M1-17) moves one to StateActive,
// from its own code: nothing in this package can, and the table refuses a
// change to it that carries no audit report. A rejected or superseded package
// never changes state again.
type State string

// The four states of an installed package.
const (
	// StateQuarantined: installed and inspectable, exposing nothing.
	StateQuarantined State = "quarantined"
	// StateActive: cleared by the integration audit. Read here, never written.
	StateActive State = "active"
	// StateRejected: refused by an operator or by the audit. Final.
	StateRejected State = "rejected"
	// StateSuperseded: replaced by a newer package. Final.
	StateSuperseded State = "superseded"
)

// Valid reports whether s is one of the four states.
func (s State) Valid() bool {
	switch s {
	case StateQuarantined, StateActive, StateRejected, StateSuperseded:
		return true
	}
	return false
}

// Record is one installed package as the database holds it.
type Record struct {
	ApplicationID  string
	ManifestHash   string
	PackageVersion string
	State          State
	// SecurityDomain is the manifest's optional security domain label.
	SecurityDomain string
	// SignerKeyID is the publisher key that authenticated the package.
	SignerKeyID string
	// Path is the install directory, relative to the data directory.
	Path     string
	Files    int
	Bytes    int64
	Warnings int
	// InstalledAt and StatusChangedAt come from the injected clock.
	InstalledAt     time.Time
	StatusChangedAt time.Time
	StatusReason    string
	// AuditReportID is the integration audit report that activated the
	// package, empty until then.
	AuditReportID string
}

// Installed is the result of Install.
type Installed struct {
	Record Record
	// Created is false when the very same content was already installed: the
	// stored record is returned exactly as it is, in whatever state it is in.
	Created bool
	// Findings are the warnings and information of the load. There is no
	// critical finding: a package with one is refused, not installed.
	Findings []contracts.Finding
}

// RefusedError is returned when a package cannot be installed or opened because
// of what the checks found. It carries the findings; its code is
// validation_failed.
type RefusedError struct {
	Reason   string
	Findings []contracts.Finding
}

func (e *RefusedError) Error() string {
	crit, _, _ := contracts.CountSeverity(e.Findings)
	return fmt.Sprintf("package refused: %s (%d critical finding(s))", e.Reason, crit)
}

// Unwrap gives the error its contracts code.
func (e *RefusedError) Unwrap() error { return contracts.New(contracts.ErrValidationFailed, e.Reason) }

// InstallerOptions configure an Installer.
type InstallerOptions struct {
	// DB is the store with the "apppkg" and "trust" migrations applied.
	DB *store.DB
	// Clock stamps the records. Nil means the wall clock.
	Clock clock.Clock
	// DataDir is the directory below which packages are written, as
	// <DataDir>/packages/<application>/<manifest hash>.
	DataDir string
	// Verifier authenticates the package against the trust store. Required: an
	// installer that cannot establish who signed a package installs nothing.
	Verifier *trust.Verifier
	// CoreVersion is the installed AlixAI core version. Required.
	CoreVersion string
	// Limits bound the package. Zero fields take DefaultLimits.
	Limits Limits
}

// Installer installs application packages into quarantine and reads them back.
//
// It has no method that activates a package, and none that could: the
// integration audit is the only writer of the active state, and the database
// refuses an activation that carries no audit report.
type Installer struct {
	db      *store.DB
	clk     clock.Clock
	dataDir string
	opts    Options
}

// NewInstaller returns an Installer. It does not touch the file system or the
// database until it is used.
func NewInstaller(o InstallerOptions) (*Installer, error) {
	switch {
	case o.DB == nil:
		return nil, contracts.New(contracts.ErrInternal, "apppkg: installer needs a database")
	case strings.TrimSpace(o.DataDir) == "":
		return nil, contracts.New(contracts.ErrInternal, "apppkg: installer needs a data directory")
	case o.Verifier == nil:
		return nil, contracts.New(contracts.ErrInternal, "apppkg: installer needs a trust verifier; nothing is installed unsigned")
	}
	if _, ok := parseSemver(o.CoreVersion); !ok {
		return nil, contracts.New(contracts.ErrInternal, "apppkg: installer needs the core version as a semantic version")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return &Installer{
		db:      o.DB,
		clk:     clk,
		dataDir: o.DataDir,
		opts:    Options{Verifier: o.Verifier, CoreVersion: o.CoreVersion, Limits: o.Limits},
	}, nil
}

// appDirRE is the grammar an application ID must match to be a directory name.
// The schema's grammar is wider only in what it never produces here.
var appDirRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func relInstallPath(app, hash string) string { return path.Join("packages", app, hash) }

// Install loads the package from src, requires a verified signature and no
// critical finding, writes the verified bytes below the data directory and
// records the package as quarantined.
//
// It calls Load itself with the Installer's verifier and never accepts a
// result computed elsewhere, so what is written is exactly the snapshot that
// was verified. A package with a critical finding, an unsigned or tampered one,
// and one signed by an unknown, revoked, retired or expired key is refused with
// a *RefusedError and nothing is written. Installing content that is already
// installed returns the stored record unchanged: it never resets a state.
func (in *Installer) Install(ctx context.Context, src Source) (*Installed, error) {
	res, err := Load(ctx, src, in.opts)
	if err != nil {
		return nil, err
	}
	if res.Critical() {
		return nil, &RefusedError{Reason: "the checks found critical findings", Findings: res.Findings}
	}
	if res.Package == nil || res.Authenticity != AuthVerified || res.Signer == nil || !hashRE.MatchString(res.ManifestHash) {
		// Defensive: with a verifier no result reaches here unverified.
		return nil, &RefusedError{Reason: "the package signature was not verified", Findings: res.Findings}
	}
	app := res.ApplicationID
	if !appDirRE.MatchString(app) {
		return nil, &RefusedError{Reason: "the application ID is not usable as an install directory name", Findings: res.Findings}
	}
	hash := res.ManifestHash

	if rec, err := in.Get(ctx, app, hash); err == nil {
		return &Installed{Record: rec, Created: false, Findings: res.Findings}, nil
	} else if !errors.Is(err, ErrNotInstalled) {
		return nil, err
	}

	_, warn, _ := contracts.CountSeverity(res.Findings)
	now := in.clk.Now()
	rec := Record{
		ApplicationID:   app,
		ManifestHash:    hash,
		PackageVersion:  res.PackageVersion,
		State:           StateQuarantined,
		SecurityDomain:  res.Package.SecurityDomain(),
		SignerKeyID:     res.Signer.KeyID,
		Path:            relInstallPath(app, hash),
		Files:           res.snap.Len(),
		Bytes:           res.snap.Size(),
		Warnings:        warn,
		InstalledAt:     now,
		StatusChangedAt: now,
		StatusReason:    "installed",
	}

	appDir := filepath.Join(in.dataDir, "packages", app)
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "cannot create the package directory")
	}
	staging, err := os.MkdirTemp(appDir, ".staging-")
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "cannot create a staging directory")
	}
	defer func() { _ = os.RemoveAll(staging) }() // a no-op once renamed away

	if err := writeSnapshot(staging, res.snap); err != nil {
		return nil, err
	}
	final := filepath.Join(appDir, hash)

	// placed is set once the directory is in its final place. The transaction
	// function may run again after a busy error, so it must not move the
	// directory twice.
	placed := false
	err = in.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, insertRecordSQL,
			rec.ApplicationID, rec.ManifestHash, rec.PackageVersion, rec.SecurityDomain, rec.SignerKeyID,
			rec.Path, rec.Files, rec.Bytes, rec.Warnings, now.UnixMilli(), now.UnixMilli(), rec.StatusReason); err != nil {
			return err
		}
		if placed {
			return nil
		}
		// A directory already at the final path is not tied to any record
		// (the insert above succeeded, so there is none): an earlier install
		// stopped between the rename and the commit. It is never trusted;
		// replace it with the verified bytes.
		if err := os.RemoveAll(final); err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "cannot clear a stale install directory")
		}
		if err := os.Rename(staging, final); err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "cannot move the package into place")
		}
		placed = true
		return nil
	})
	if err != nil {
		if placed {
			// the package was moved into place but the record was not committed
			_ = os.RemoveAll(final)
		}
		if store.IsUniqueViolation(err) {
			// A concurrent install of the same content won; return its record.
			existing, gerr := in.Get(ctx, app, hash)
			if gerr != nil {
				return nil, gerr
			}
			return &Installed{Record: existing, Created: false, Findings: res.Findings}, nil
		}
		var ce *contracts.Error
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "cannot record the installed package")
	}
	return &Installed{Record: rec, Created: true, Findings: res.Findings}, nil
}

const insertRecordSQL = `INSERT INTO apppkg_packages
 (application_id, manifest_hash, package_version, status, security_domain, signer_key_id, install_path,
  file_count, byte_count, warning_count, installed_at, status_changed_at, status_reason)
 VALUES (?, ?, ?, 'quarantined', ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// writeSnapshot writes every file of the snapshot below dir (which exists),
// directories 0700 and files 0600, never following a link and never leaving
// the directory. The bytes come from the Snapshot, not from the source.
func writeSnapshot(dir string, snap *Snapshot) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "cannot open the staging directory")
	}
	defer root.Close()
	for _, p := range snap.Paths() {
		if d := path.Dir(p); d != "." {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return contracts.Wrap(contracts.ErrUnavailable, err, "cannot create a package directory")
			}
		}
		data, _ := snap.bytes(p)
		f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "cannot create a package file")
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return contracts.Wrap(contracts.ErrUnavailable, err, "cannot write a package file")
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return contracts.Wrap(contracts.ErrUnavailable, err, "cannot flush a package file")
		}
		if err := f.Close(); err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "cannot close a package file")
		}
	}
	return nil
}

const selectRecordSQL = `SELECT application_id, manifest_hash, package_version, status, security_domain, signer_key_id,
 install_path, file_count, byte_count, warning_count, installed_at, status_changed_at, status_reason, COALESCE(audit_report_id, '')
 FROM apppkg_packages`

type scanner interface{ Scan(dest ...any) error }

func scanRecord(r scanner) (Record, error) {
	var rec Record
	var state string
	var installed, changed int64
	if err := r.Scan(&rec.ApplicationID, &rec.ManifestHash, &rec.PackageVersion, &state, &rec.SecurityDomain, &rec.SignerKeyID,
		&rec.Path, &rec.Files, &rec.Bytes, &rec.Warnings, &installed, &changed, &rec.StatusReason, &rec.AuditReportID); err != nil {
		return Record{}, err
	}
	rec.State = State(state)
	rec.InstalledAt = time.UnixMilli(installed).UTC()
	rec.StatusChangedAt = time.UnixMilli(changed).UTC()
	return rec, nil
}

// ErrNotInstalled is the cause of the error Get and Open return for a package
// that is not installed (use errors.Is).
var ErrNotInstalled = errors.New("package is not installed")

// notInstalled builds the not-found error.
func notInstalled(app, hash string) error {
	return contracts.Wrap(contracts.ErrValidationFailed, ErrNotInstalled, "no such installed package").
		WithDetails("application_id", app).WithDetails("manifest_hash", hash)
}

// Get returns the record of an installed package. The error wraps
// ErrNotInstalled when there is none.
func (in *Installer) Get(ctx context.Context, applicationID, manifestHash string) (Record, error) {
	var rec Record
	err := in.db.View(ctx, func(tx *store.Tx) error {
		r, err := scanRecord(tx.QueryRowContext(ctx, selectRecordSQL+` WHERE application_id = ? AND manifest_hash = ?`, applicationID, manifestHash))
		if err != nil {
			return err
		}
		rec = r
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Record{}, notInstalled(applicationID, manifestHash)
	case err != nil:
		return Record{}, contracts.Wrap(contracts.ErrUnavailable, err, "cannot read the installed package")
	}
	return rec, nil
}

// List returns the installed packages of an application, oldest first.
func (in *Installer) List(ctx context.Context, applicationID string) ([]Record, error) {
	var out []Record
	err := in.db.View(ctx, func(tx *store.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, selectRecordSQL+` WHERE application_id = ? ORDER BY installed_at, manifest_hash`, applicationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rec, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "cannot list the installed packages")
	}
	return out, nil
}

// Reject moves a quarantined package to the final rejected state, recording
// why. It changes nothing for a package in any other state.
func (in *Installer) Reject(ctx context.Context, applicationID, manifestHash, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return contracts.New(contracts.ErrValidationFailed, "a rejection needs a reason of 1 to 500 characters")
	}
	now := in.clk.Now().UnixMilli()
	var n int64
	err := in.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE apppkg_packages SET status = 'rejected', status_changed_at = ?, status_reason = ?
			  WHERE application_id = ? AND manifest_hash = ? AND status = 'quarantined'`,
			now, reason, applicationID, manifestHash)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	switch {
	case err != nil:
		return contracts.Wrap(contracts.ErrUnavailable, err, "cannot reject the package")
	case n == 0:
		return contracts.New(contracts.ErrConflict, "no quarantined package with that application and hash")
	}
	return nil
}

// Open reads an installed package back from the data directory. It loads the
// stored bytes again with the full checks and the Installer's verifier and
// requires that they still hash to the recorded manifest hash, so a changed
// file, a revoked or expired signing key or a package replaced under the data
// directory never yields a Package. The returned Package reports the stored
// state through policy.ActivePackage; only a package the audit activated
// reports an active status.
func (in *Installer) Open(ctx context.Context, applicationID, manifestHash string) (*Package, Record, error) {
	rec, err := in.Get(ctx, applicationID, manifestHash)
	if err != nil {
		return nil, Record{}, err
	}
	dir := filepath.Join(in.dataDir, filepath.FromSlash(rec.Path))
	res, err := Load(ctx, FromInstalledDir(dir), in.opts)
	if err != nil {
		return nil, Record{}, err
	}
	if res.Critical() || res.Package == nil {
		return nil, Record{}, &RefusedError{Reason: "the installed package no longer passes its checks", Findings: res.Findings}
	}
	if res.ManifestHash != rec.ManifestHash || res.ApplicationID != rec.ApplicationID {
		return nil, Record{}, &RefusedError{
			Reason: "the installed package is not the package that was recorded",
			Findings: append(append([]contracts.Finding(nil), res.Findings...), contracts.Finding{
				CheckID:     chkSigTampered.id,
				Severity:    chkSigTampered.severity,
				Message:     "the files under the data directory hash to a different manifest than the recorded one",
				Remediation: chkSigTampered.remediation,
			}),
		}
	}
	return res.Package.withStatus(policy.PackageStatus(rec.State)), rec, nil
}

var _ fs.FS = (*Snapshot)(nil)
