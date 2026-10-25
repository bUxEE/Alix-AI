package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// MigrationsDir is the directory inside a module's embedded FS that holds its
// NNNN_name.sql files, which is what //go:embed migrations/*.sql produces.
const MigrationsDir = "migrations"

var (
	moduleNameRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	migrationFileRE = regexp.MustCompile(`^(\d{4})_([a-z0-9][a-z0-9_]*)\.sql$`)
)

// migration is one embedded NNNN_name.sql file.
type migration struct {
	version  int
	file     string // base name, for messages
	sql      string
	checksum string // hex SHA-256 of the line-ending-normalised content
}

type moduleMigrations struct {
	name       string
	migrations []migration // ascending, contiguous from 1
}

// Registry holds the migrations of every module, in registration order. Most
// code uses RegisterMigrations, which adds to Default; tests build their own
// with NewRegistry so they never touch process-wide state.
type Registry struct {
	mu      sync.Mutex
	modules []*moduleMigrations
	byName  map[string]*moduleMigrations
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]*moduleMigrations{}}
}

// Default is the process-wide registry RegisterMigrations fills and Open uses
// when Options.Migrations is nil.
var Default = NewRegistry()

// RegisterMigrations adds a module's embedded migrations to Default. The module
// name is lower snake case; fsys holds migrations/NNNN_name.sql files, versions
// contiguous from 0001. It is meant for init() and panics on a programming
// error (bad name, duplicate module, malformed file name, a gap in the
// versions, an empty file), like config.Register.
func RegisterMigrations(module string, fsys fs.FS) {
	if err := Default.Register(module, fsys); err != nil {
		panic("store: RegisterMigrations: " + err.Error())
	}
}

// Register adds a module to the registry. Modules are applied in the order
// they were registered.
func (r *Registry) Register(module string, fsys fs.FS) error {
	if !moduleNameRE.MatchString(module) {
		return fmt.Errorf("invalid module name %q (want %s)", module, moduleNameRE)
	}
	mm, err := loadModule(module, fsys)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[module]; dup {
		return fmt.Errorf("module %q registered twice", module)
	}
	r.modules = append(r.modules, mm)
	r.byName[module] = mm
	return nil
}

// snapshot returns the modules in registration order. The migration slices are
// immutable after Register, so sharing them is safe.
func (r *Registry) snapshot() []*moduleMigrations {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*moduleMigrations(nil), r.modules...)
}

func loadModule(module string, fsys fs.FS) (*moduleMigrations, error) {
	if fsys == nil {
		return nil, fmt.Errorf("module %q: nil filesystem", module)
	}
	entries, err := fs.ReadDir(fsys, MigrationsDir)
	if err != nil {
		return nil, fmt.Errorf("module %q: reading %s/: %w", module, MigrationsDir, err)
	}
	mm := &moduleMigrations{name: module}
	for _, e := range entries {
		name := e.Name()
		m := migrationFileRE.FindStringSubmatch(name)
		if e.IsDir() || m == nil {
			return nil, fmt.Errorf("module %q: %s/%s is not a migration (want NNNN_name.sql, four digits, lower snake case)", module, MigrationsDir, name)
		}
		version, _ := strconv.Atoi(m[1])
		raw, err := fs.ReadFile(fsys, MigrationsDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("module %q: reading %s: %w", module, name, err)
		}
		if strings.TrimSpace(string(raw)) == "" {
			return nil, fmt.Errorf("module %q: migration %s is empty", module, name)
		}
		mm.migrations = append(mm.migrations, migration{
			version:  version,
			file:     name,
			sql:      string(raw),
			checksum: checksum(raw),
		})
	}
	if len(mm.migrations) == 0 {
		return nil, fmt.Errorf("module %q: no migrations found in %s/", module, MigrationsDir)
	}
	sort.Slice(mm.migrations, func(i, j int) bool { return mm.migrations[i].version < mm.migrations[j].version })
	for i, m := range mm.migrations {
		want := i + 1
		if m.version != want {
			return nil, fmt.Errorf("module %q: migration versions must be contiguous from 0001: expected %04d, found %s", module, want, m.file)
		}
	}
	return mm, nil
}

// checksum is the hex SHA-256 of the migration text with CRLF folded to LF, so
// a checkout with different line endings does not look like an edited file.
func checksum(raw []byte) string {
	norm := strings.ReplaceAll(string(raw), "\r\n", "\n")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

const createSchemaMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
	module     TEXT    NOT NULL,
	version    INTEGER NOT NULL,
	checksum   TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	applied_at TEXT    NOT NULL,
	PRIMARY KEY (module, version)
) WITHOUT ROWID`

type appliedKey struct {
	module  string
	version int
}

// migrate brings the database to the registry's state. It first checks every
// recorded row against the registry and only then applies anything, so a
// checksum drift or downgrade leaves the database exactly as it was.
func (db *DB) migrate(ctx context.Context, reg *Registry) error {
	if err := db.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, createSchemaMigrations)
		return err
	}); err != nil {
		return wrapMigration(err, "creating schema_migrations")
	}

	modules := reg.snapshot()
	var applied map[appliedKey]string
	if err := db.Tx(ctx, func(tx *Tx) error {
		var err error
		applied, err = loadApplied(ctx, tx)
		return err
	}); err != nil {
		return wrapMigration(err, "reading schema_migrations")
	}

	// Verify first: no partial apply when anything has drifted.
	registered := map[appliedKey]migration{}
	for _, mod := range modules {
		for _, m := range mod.migrations {
			registered[appliedKey{mod.name, m.version}] = m
		}
	}
	for key, got := range applied {
		m, known := registered[key]
		if !known {
			// A recorded version of a module this binary registers but no
			// longer embeds: the database is newer than the code (downgrade)
			// or a file was deleted. Modules the binary does not register at
			// all are left alone: no code here touches their tables.
			if reg.has(key.module) {
				return contracts.Newf(contracts.ErrInternal,
					"migration %s version %04d is recorded in the database but not embedded in this binary (downgrade or deleted migration); refusing to start",
					key.module, key.version).
					WithDetails("module", key.module).WithDetails("version", key.version)
			}
			continue
		}
		if got != m.checksum {
			return mismatchError(key, m, got)
		}
	}

	for _, mod := range modules {
		for _, m := range mod.migrations {
			if _, done := applied[appliedKey{mod.name, m.version}]; done {
				continue
			}
			if err := db.applyOne(ctx, mod.name, m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Registry) has(module string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byName[module]
	return ok
}

func loadApplied(ctx context.Context, q Reader) (map[appliedKey]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT module, version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[appliedKey]string{}
	for rows.Next() {
		var k appliedKey
		var sum string
		if err := rows.Scan(&k.module, &k.version, &sum); err != nil {
			return nil, err
		}
		out[k] = sum
	}
	return out, rows.Err()
}

// applyOne runs one migration and records it in the same transaction. The row
// is re-checked inside the transaction so two processes starting together
// cannot both apply it.
func (db *DB) applyOne(ctx context.Context, module string, m migration) error {
	err := db.Tx(ctx, func(tx *Tx) error {
		var recorded string
		err := tx.QueryRowContext(ctx,
			`SELECT checksum FROM schema_migrations WHERE module = ? AND version = ?`, module, m.version).Scan(&recorded)
		switch {
		case err == nil:
			if recorded != m.checksum {
				return mismatchError(appliedKey{module, m.version}, m, recorded)
			}
			return nil // applied by someone else meanwhile
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return contracts.Wrap(contracts.ErrInternal, err,
				fmt.Sprintf("migration %s/%s failed and was rolled back", module, m.file)).
				WithDetails("module", module).WithDetails("version", m.version)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (module, version, checksum, name, applied_at) VALUES (?, ?, ?, ?, ?)`,
			module, m.version, m.checksum, m.file, db.clk.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return wrapMigration(err, fmt.Sprintf("applying %s/%s", module, m.file))
	}
	db.log.Info("migration applied", "module", module, "version", m.version, "file", m.file)
	return nil
}

func mismatchError(key appliedKey, m migration, recorded string) error {
	return contracts.Newf(contracts.ErrInternal,
		"migration checksum mismatch: %s/%s was edited after it was applied (recorded sha256 %.12s, embedded %.12s); refusing to start. Applied migrations are immutable: restore the file and add a new migration instead",
		key.module, m.file, recorded, m.checksum).
		WithDetails("module", key.module).WithDetails("version", key.version)
}

// wrapMigration turns an infrastructure failure during migration into
// ErrInternal unless it already carries a contracts code (a busy or timeout
// error keeps its retryable classification).
func wrapMigration(err error, what string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrInternal, err, "migration failed while "+what)
}
