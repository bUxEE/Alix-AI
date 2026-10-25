package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// driverName is the database/sql name modernc.org/sqlite registers.
const driverName = "sqlite"

// DB is the open store: one writer connection and a bounded pool of read-only
// connections on the same WAL database. It is safe for concurrent use.
type DB struct {
	path string
	cfg  Config

	w *sql.DB // exactly one connection, BEGIN IMMEDIATE
	r *sql.DB // bounded, query_only

	clk clock.Clock
	log *slog.Logger

	busyAttempts int
	busyBackoff  time.Duration
	busyRetries  atomic.Uint64

	closeOnce sync.Once
	closeErr  error
}

// Stats is a point-in-time view of the connection pools, for diagnostics and
// for tests that assert the pools stay bounded.
type Stats struct {
	// Writer and Readers are database/sql's own pool statistics.
	Writer  sql.DBStats
	Readers sql.DBStats
	// BusyRetries counts attempts DB.Tx and DB.View repeated after SQLITE_BUSY
	// since Open.
	BusyRetries uint64
}

// Open opens (creating if needed) the database at opts.Path, enables WAL and
// the standard pragmas, applies every pending migration in opts.Migrations
// (default: the registry RegisterMigrations fills) and only then opens the
// reader pool.
//
// Open fails closed: WAL not taking effect, a migration checksum that no
// longer matches its embedded file, a recorded migration the binary does not
// embed, or a failing migration all return an error and leave nothing open.
// Failures to reach or open the file are contracts.ErrUnavailable; migration
// problems are contracts.ErrInternal; bad options are
// contracts.ErrValidationFailed.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if err := opts.validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "store options invalid")
	}
	cfg := opts.Config.withDefaults()
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "store path invalid")
	}
	if err := ensureFile(path); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "store file cannot be created")
	}

	db := &DB{
		path:         path,
		cfg:          cfg,
		clk:          opts.Clock,
		log:          opts.Logger,
		busyAttempts: opts.BusyAttempts,
		busyBackoff:  opts.BusyBackoff,
	}
	if db.clk == nil {
		db.clk = clock.Real{}
	}
	if db.log == nil {
		db.log = slog.New(slog.DiscardHandler)
	}
	if db.busyAttempts == 0 {
		db.busyAttempts = defaultBusyAttempts
	}
	if db.busyBackoff == 0 {
		db.busyBackoff = defaultBusyBackoff
	}
	reg := opts.Migrations
	if reg == nil {
		reg = Default
	}

	fail := func(err error) (*DB, error) {
		_ = db.Close()
		return nil, err
	}

	db.w, err = openPool(cfg.dsn(path, roleWriter), 1, 0)
	if err != nil {
		return fail(contracts.Wrap(contracts.ErrUnavailable, err, "store writer cannot be opened"))
	}
	if err := db.w.PingContext(ctx); err != nil {
		return fail(contracts.Wrap(contracts.ErrUnavailable, err, "store writer cannot connect"))
	}
	var mode string
	if err := db.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fail(contracts.Wrap(contracts.ErrUnavailable, err, "store journal mode unreadable"))
	}
	if !strings.EqualFold(mode, "wal") {
		return fail(contracts.Newf(contracts.ErrUnavailable,
			"store requires WAL journaling but the database reports journal_mode=%q (network or unsupported filesystem?)", mode))
	}

	if err := db.migrate(ctx, reg); err != nil {
		return fail(err)
	}

	db.r, err = openPool(cfg.dsn(path, roleReader), cfg.ReaderConns, readerIdleTime)
	if err != nil {
		return fail(contracts.Wrap(contracts.ErrUnavailable, err, "store reader pool cannot be opened"))
	}
	if err := db.r.PingContext(ctx); err != nil {
		return fail(contracts.Wrap(contracts.ErrUnavailable, err, "store reader cannot connect"))
	}
	return db, nil
}

// Close closes the reader pool and then the writer, so the last connection to
// close can checkpoint and remove the WAL file. It is idempotent and safe to
// call after a failed operation.
func (db *DB) Close() error {
	db.closeOnce.Do(func() {
		var errs []error
		if db.r != nil {
			errs = append(errs, db.r.Close())
		}
		if db.w != nil {
			errs = append(errs, db.w.Close())
		}
		db.closeErr = errors.Join(errs...)
	})
	return db.closeErr
}

// Path returns the absolute database file path.
func (db *DB) Path() string { return db.path }

// Config returns the resolved configuration (defaults applied).
func (db *DB) Config() Config { return db.cfg }

// Stats returns the current pool statistics.
func (db *DB) Stats() Stats {
	s := Stats{Writer: db.w.Stats(), BusyRetries: db.busyRetries.Load()}
	if db.r != nil {
		s.Readers = db.r.Stats()
	}
	return s
}

// Querier runs statements. *Tx and *DB implement it; DB routes reads to the
// reader pool and writes to the writer connection.
type Querier interface {
	Reader
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Reader runs read-only statements.
type Reader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// QueryContext runs a read-only query on the reader pool. The result reflects
// the database at the moment the statement starts; use View for several
// statements against one snapshot.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.r.QueryContext(ctx, query, args...)
}

// QueryRowContext is QueryContext for a single row.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.r.QueryRowContext(ctx, query, args...)
}

// ExecContext runs one statement (or a script of several) in its own write
// transaction, with the same busy retry as Tx.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := db.Tx(ctx, func(tx *Tx) error {
		var err error
		res, err = tx.ExecContext(ctx, query, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

type role int

const (
	roleWriter role = iota
	roleReader
	roleBackup // read-write connection used only by Backup; reads the source, writes elsewhere
)

// dsn builds the connection string for a role. Every role carries the same
// hardening pragmas; they differ in cache size, lock mode and write access.
func (c Config) dsn(path string, r role) string {
	v := url.Values{}
	pragma := func(format string, a ...any) { v.Add("_pragma", fmt.Sprintf(format, a...)) }

	v.Set("_busy_timeout", fmt.Sprint(c.BusyTimeoutMs))
	v.Set("_foreign_keys", "1")
	v.Set("_synchronous", "NORMAL")
	v.Set("_dqs", "0")
	v.Set("_defensive", "1")
	v.Set("_time_format", "sqlite")
	v.Set("_timezone", "UTC")
	pragma("trusted_schema(OFF)")
	pragma("mmap_size(%d)", int64(c.MmapMiB)<<20)

	switch r {
	case roleWriter:
		v.Set("_txlock", "immediate")
		pragma("journal_mode(WAL)")
		pragma("journal_size_limit(%d)", journalSizeLimit)
		pragma("cache_size(-%d)", c.CacheKiB)
	case roleReader:
		v.Set("_query_only", "1")
		pragma("cache_size(-%d)", c.ReaderCacheKiB)
	case roleBackup:
		pragma("cache_size(-%d)", c.ReaderCacheKiB)
	}
	return path + "?" + v.Encode()
}

func openPool(dsn string, conns int, idle time.Duration) (*sql.DB, error) {
	p, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	p.SetMaxOpenConns(conns)
	p.SetMaxIdleConns(conns)
	p.SetConnMaxLifetime(0)
	p.SetConnMaxIdleTime(idle)
	return p, nil
}

// ensureFile creates the database file and its directory (the data
// directory) owner only when missing (internal/fsperm: mode 0600/0700, on
// Windows a protected ACL for the current user, which what is created in the
// directory inherits), so SQLite's -wal and -shm files are owner only too. An
// existing directory or file is left as it is. An empty file is a valid empty
// SQLite database.
func ensureFile(path string) error {
	if _, err := fsperm.MkdirOwnerOnly(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := fsperm.CreateFile(path)
	switch {
	case errors.Is(err, os.ErrExist):
		return nil
	case err != nil:
		return err
	}
	return f.Close()
}

var _ io.Closer = (*DB)(nil)
