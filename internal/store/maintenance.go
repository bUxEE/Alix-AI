package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxIntegrityProblems bounds how many integrity_check lines an error carries.
const maxIntegrityProblems = 10

// IntegrityCheck runs SQLite's full integrity_check and foreign_key_check on
// the live database. It returns nil only when both are clean; otherwise a
// contracts.ErrInternal whose Details["problems"] lists the first findings.
//
// It runs on a short-lived read-only connection of its own, so writers are not
// blocked and a pooled connection's caches are not disturbed. A fresh
// connection is also required for correctness: on a long-lived connection that
// has already used an FTS5 table, integrity_check can report false "corruption
// found reading blob" findings after another connection ran an FTS5 optimize
// or merge (it checks a stale cached index structure; ordinary MATCH queries
// are unaffected).
func (db *DB) IntegrityCheck(ctx context.Context) error {
	return checkFile(ctx, db.path, db.cfg)
}

// IntegrityCheckFile opens the database file at path read-only, runs the same
// checks and closes it, without migrating or modifying anything. Backup uses
// it to verify a copy before publishing it, and a restore flow can use it on a
// candidate file.
func IntegrityCheckFile(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return contracts.Wrap(contracts.ErrValidationFailed, err, "integrity check path invalid")
	}
	return checkFile(ctx, abs, DefaultConfig())
}

// ViewFile opens the database file at path read-only, without migrating or
// changing anything, runs fn on it and closes it. It is for a copy no store has
// open, such as a backup's snapshot: like IntegrityCheckFile it uses one
// short-lived connection, and it leaves no -wal or -shm file beside the copy.
func ViewFile(ctx context.Context, path string, fn func(Reader) error) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return contracts.Wrap(contracts.ErrValidationFailed, err, "database path invalid")
	}
	if _, err := os.Stat(abs); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "the database file cannot be read")
	}
	p, err := openPool(DefaultConfig().dsn(abs, roleReader), 1, 0)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "the database file cannot be opened")
	}
	ferr := p.PingContext(ctx)
	if ferr != nil {
		ferr = contracts.Wrap(contracts.ErrUnavailable, ferr, "the database file cannot be opened")
	} else {
		ferr = fn(p)
	}
	cerr := p.Close()
	_ = removeIfExists(abs + "-wal")
	_ = removeIfExists(abs + "-shm")
	if ferr != nil {
		return ferr
	}
	if cerr != nil {
		return contracts.Wrap(contracts.ErrUnavailable, cerr, "the database file did not close cleanly")
	}
	return nil
}

func checkFile(ctx context.Context, abs string, cfg Config) error {
	if _, err := os.Stat(abs); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "integrity check cannot read the file")
	}
	p, err := openPool(cfg.dsn(abs, roleReader), 1, 0)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "integrity check cannot open the file")
	}
	defer p.Close()
	return integrityCheck(ctx, p)
}

func integrityCheck(ctx context.Context, p *sql.DB) error {
	problems, err := collectLines(ctx, p, `PRAGMA integrity_check`)
	if err != nil {
		// A badly damaged file often fails the pragma itself.
		return contracts.Wrap(contracts.ErrInternal, err, "integrity check could not complete").
			WithDetails("problems", []string{err.Error()})
	}
	if len(problems) == 1 && problems[0] == "ok" {
		problems = nil
	}
	fkRows, err := p.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "foreign key check could not complete")
	}
	for fkRows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			fkRows.Close()
			return contracts.Wrap(contracts.ErrInternal, err, "foreign key check unreadable")
		}
		problems = append(problems, fmt.Sprintf("foreign key violation: %s row %d references missing %s", table, rowid.Int64, parent))
	}
	if err := fkRows.Err(); err != nil {
		fkRows.Close()
		return contracts.Wrap(contracts.ErrInternal, err, "foreign key check failed")
	}
	fkRows.Close()

	if len(problems) == 0 {
		return nil
	}
	n := len(problems)
	if n > maxIntegrityProblems {
		problems = problems[:maxIntegrityProblems]
	}
	return contracts.Newf(contracts.ErrInternal, "database integrity check failed (%d findings, first: %s)", n, problems[0]).
		WithDetails("problems", problems)
}

func collectLines(ctx context.Context, p *sql.DB, query string) ([]string, error) {
	rows, err := p.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Backup writes a consistent, self-contained copy of the live database to dest
// using VACUUM INTO, without blocking readers or writers (the copy reads a
// snapshot; WAL lets writes continue). The copy is written to dest+".partial",
// checked with IntegrityCheckFile, synced and only then renamed to dest, so a
// crash never leaves a plausible-looking partial file at dest. The copy is
// owner-readable only.
//
// An existing dest is never overwritten: Backup returns contracts.ErrConflict.
// dest's directory is created (0700) if missing.
func (db *DB) Backup(ctx context.Context, dest string) error {
	abs, err := filepath.Abs(dest)
	if err != nil || strings.TrimSpace(dest) == "" {
		return contracts.New(contracts.ErrValidationFailed, "backup destination invalid")
	}
	if abs == db.path {
		return contracts.New(contracts.ErrValidationFailed, "backup destination is the live database")
	}
	if _, err := os.Stat(abs); err == nil {
		return contracts.New(contracts.ErrConflict, "backup destination already exists; refusing to overwrite").
			WithDetails("path", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "backup directory cannot be created")
	}
	tmp := abs + ".partial"
	if err := removeIfExists(tmp); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "stale partial backup cannot be removed")
	}
	cleanup := func() { _ = removeIfExists(tmp) }

	// VACUUM INTO fails on the query_only reader pool, and running it on the
	// writer would hold the only write connection for the whole copy, so a
	// short-lived connection does it.
	p, err := openPool(db.cfg.dsn(db.path, roleBackup), 1, 0)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "backup connection cannot be opened")
	}
	_, execErr := p.ExecContext(ctx, `VACUUM INTO ?`, tmp)
	closeErr := p.Close()
	if execErr != nil {
		cleanup()
		return ctxError(ctx, contracts.Wrap(contracts.ErrUnavailable, execErr, "backup copy failed"))
	}
	if closeErr != nil {
		cleanup()
		return contracts.Wrap(contracts.ErrUnavailable, closeErr, "backup connection did not close cleanly")
	}
	if err := IntegrityCheckFile(ctx, tmp); err != nil {
		cleanup()
		return err
	}
	if err := syncFile(tmp); err != nil {
		cleanup()
		return contracts.Wrap(contracts.ErrUnavailable, err, "backup cannot be synced to disk")
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, abs); err != nil {
		cleanup()
		return contracts.Wrap(contracts.ErrUnavailable, err, "backup cannot be moved into place")
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
