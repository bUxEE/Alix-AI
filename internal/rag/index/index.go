package index

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Module is the name the migrations are registered under.
const Module = "ragindex"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the module's embedded migrations, for a test that builds
// its own store.Registry instead of using store.Default.
func Migrations() fs.FS { return migrationFiles }

func init() { store.RegisterMigrations(Module, migrationFiles) }

// Rowid layout: scope_no << seqBits | seq. Each (application, tenant,
// workspace) scope owns one contiguous rowid range, and a query constrains
// `rowid BETWEEN lo AND hi` inside the FTS5 scan (see doc.go).
const (
	seqBits    = 36
	maxSeq     = int64(1)<<seqBits - 1
	maxScopeNo = int64(1)<<(63-seqBits) - 1
)

// ridRange is the inclusive rowid range of a scope.
func ridRange(scopeNo int64) (lo, hi int64) {
	lo = scopeNo << seqBits
	return lo, lo | maxSeq
}

// tsLayout is a fixed-width UTC instant that sorts as text and satisfies the
// schema's timestamp grammar (nine fractional digits, literal Z).
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(tsLayout) }

// Config holds the tunables of the index. The zero value of a field means its
// default; DefaultConfig lists them.
type Config struct {
	// ExactIdentifierBoost is added to the score of a chunk once for every
	// distinct identifier of the query it holds exactly (a code, id or SKU).
	// BM25 scores are of the order of 1 to 50, so the default makes an exact
	// identifier match outrank any text-only match; within the matches that
	// hold identifiers, and among the rest, BM25 decides. Negative disables
	// the boost.
	ExactIdentifierBoost float64
	// DefaultLimit is the number of hits a query asks for when it names none.
	DefaultLimit int
	// MaxLimit caps the hits one query may ask for.
	MaxLimit int
	// ValidateProbes is how many chunks Validate looks up through the full-text
	// index to prove it holds them.
	ValidateProbes int
}

// Default values of Config.
const (
	DefaultExactIdentifierBoost = 1000.0
	DefaultLimit                = 10
	DefaultMaxLimit             = 200
	DefaultValidateProbes       = 16
)

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		ExactIdentifierBoost: DefaultExactIdentifierBoost,
		DefaultLimit:         DefaultLimit,
		MaxLimit:             DefaultMaxLimit,
		ValidateProbes:       DefaultValidateProbes,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.ExactIdentifierBoost == 0 {
		c.ExactIdentifierBoost = d.ExactIdentifierBoost
	}
	if c.ExactIdentifierBoost < 0 {
		c.ExactIdentifierBoost = 0
	}
	if c.DefaultLimit <= 0 {
		c.DefaultLimit = d.DefaultLimit
	}
	if c.MaxLimit <= 0 {
		c.MaxLimit = d.MaxLimit
	}
	if c.DefaultLimit > c.MaxLimit {
		c.DefaultLimit = c.MaxLimit
	}
	if c.ValidateProbes <= 0 {
		c.ValidateProbes = d.ValidateProbes
	}
	return c
}

// Index is the scoped lexical index. It is safe for concurrent use; writes
// serialise on the store's single writer connection and reads run on the
// reader pool, each query inside one snapshot.
type Index struct {
	db  *store.DB
	clk clock.Clock
	ids *ids.Generator
	cfg Config

	// Test seams, nil in production.
	trace        func(sql string, args []any) // sees every statement of the search path
	activateHook func(step string) error      // runs inside the activation transaction between its writes
}

// New returns an Index over db, which must have been opened with this module's
// migrations (they register themselves with store.Default; a test using its own
// registry registers Migrations()). A nil clock means clock.Real.
func New(db *store.DB, clk clock.Clock, cfg Config) (*Index, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrInternal, "index: nil store")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	return &Index{db: db, clk: clk, ids: ids.NewGenerator(clk), cfg: cfg.withDefaults()}, nil
}

func (ix *Index) now() string { return formatTime(ix.clk.Now()) }

// errNotFound is returned for an id that does not exist in the caller's scope.
// An id that exists in another scope returns the same error, so the response
// is never an oracle for other tenants' ids.
func errNotFound(what string) error {
	return contracts.Newf(contracts.ErrValidationFailed, "index: no such %s", what).WithDetails("reason", "not_found")
}

func invalidArg(format string, args ...any) error {
	return contracts.Newf(contracts.ErrValidationFailed, "index: "+format, args...)
}

// validateScope fails closed on any missing or malformed level.
func validateScope(scope contracts.Scope) error { return scope.Validate() }

// checkID validates an opaque identifier handed in by a caller.
func checkID(what, v string, maxLen int) error {
	switch {
	case v == "":
		return invalidArg("%s is empty", what)
	case len(v) > maxLen:
		return invalidArg("%s is longer than %d bytes", what, maxLen)
	case strings.ContainsAny(v, "\x00\n\r"):
		return invalidArg("%s contains a control character", what)
	}
	return nil
}

// traced runs a read statement, reporting it to the trace seam first.
func (ix *Index) traced(ctx context.Context, r store.Reader, query string, args ...any) (*sql.Rows, error) {
	if ix.trace != nil {
		ix.trace(query, args)
	}
	return r.QueryContext(ctx, query, args...)
}

// scopeRow is a row of ragindex_scope.
type scopeRow struct {
	no    int64
	epoch int64
}

// lookupScope finds the scope's row; found is false when the scope never held
// index data, in which case every read returns nothing and no range exists.
func lookupScope(ctx context.Context, r store.Reader, scope contracts.Scope) (row scopeRow, found bool, err error) {
	rw, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{SQL: `SELECT scope_no, epoch FROM ragindex_scope`})
	if err != nil {
		return scopeRow{}, false, err
	}
	switch err := rw.Scan(&row.no, &row.epoch); {
	case errors.Is(err, sql.ErrNoRows):
		return scopeRow{}, false, nil
	case err != nil:
		return scopeRow{}, false, err
	}
	if row.no < 1 || row.no > maxScopeNo {
		return scopeRow{}, false, contracts.Newf(contracts.ErrInternal, "index: scope number %d is outside the rowid layout", row.no)
	}
	return row, true, nil
}

// ensureScope returns the scope's row, creating it on first use. It runs in a
// write transaction.
func (ix *Index) ensureScope(ctx context.Context, tx *store.Tx, scope contracts.Scope) (scopeRow, error) {
	if row, found, err := lookupScope(ctx, tx, scope); err != nil || found {
		return row, err
	}
	if _, err := store.InsertScoped(ctx, tx, scope, "ragindex_scope",
		[]string{"next_seq", "epoch", "created_at"}, 1, 0, ix.now()); err != nil {
		return scopeRow{}, err
	}
	row, found, err := lookupScope(ctx, tx, scope)
	if err != nil {
		return scopeRow{}, err
	}
	if !found {
		return scopeRow{}, contracts.New(contracts.ErrInternal, "index: scope row vanished after insert")
	}
	return row, nil
}

// allocRids reserves n consecutive rowids in the scope's range and returns the
// first. The range is never reused, so a stale index entry of a deleted chunk
// cannot collide with a new one.
func allocRids(ctx context.Context, tx *store.Tx, scope contracts.Scope, sc scopeRow, n int) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `UPDATE ragindex_scope SET next_seq = next_seq + ?`,
		Args:      []any{n},
		Where:     `scope_no = ?`,
		WhereArgs: []any{sc.no},
		Tail:      `RETURNING next_seq`,
	})
	if err != nil {
		return 0, err
	}
	var next int64
	if err := row.Scan(&next); err != nil {
		return 0, err
	}
	first := next - int64(n)
	if next-1 > maxSeq {
		return 0, contracts.Newf(contracts.ErrResourceExhausted, "index: the scope has used all %d row numbers of its range; compact and re-create the scope", maxSeq).
			WithDetails("scope_no", sc.no)
	}
	lo, _ := ridRange(sc.no)
	return lo + first, nil
}

// bumpEpoch records that something a query in the scope could see has changed.
// Every mutating method calls it inside its own transaction; cache keys embed
// the epoch.
func bumpEpoch(ctx context.Context, tx *store.Tx, scope contracts.Scope, sc scopeRow) error {
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `UPDATE ragindex_scope SET epoch = epoch + 1`,
		Where:     `scope_no = ?`,
		WhereArgs: []any{sc.no},
	})
	return err
}
