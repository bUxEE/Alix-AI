package identity

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ModuleName is the module name this package registers its migrations under.
const ModuleName = "identity"

func init() { store.RegisterMigrations(ModuleName, migrations) }

// ReplayCache makes a context token single use. It is called only for a token
// whose signature, claims and scope all verified, so unauthenticated input can
// neither fill it nor burn a legitimate nonce.
type ReplayCache interface {
	// Consume records jti for scope until expiresAt. It returns a
	// contracts.ErrReplayed error when jti is already recorded and still live,
	// and any other error when it cannot decide; in both cases the token must
	// be refused.
	Consume(ctx context.Context, scope contracts.Scope, jti string, expiresAt time.Time) error
}

const (
	// pruneEvery is how often a Consume also sweeps expired rows.
	pruneEvery = time.Minute
	// pruneBatch bounds one sweep so it never holds the writer for long.
	pruneBatch = 1000
)

// SQLiteReplayCache is the ReplayCache backed by the identity_replay table. It
// survives a restart, because a captured token must stay useless across one.
type SQLiteReplayCache struct {
	db  *store.DB
	clk clock.Clock
	log *slog.Logger

	lastPrune atomic.Int64 // unix milliseconds of the last sweep
}

var _ ReplayCache = (*SQLiteReplayCache)(nil)

// NewSQLiteReplayCache returns a replay cache on db. A nil clk means the real
// clock and a nil log discards.
func NewSQLiteReplayCache(db *store.DB, clk clock.Clock, log *slog.Logger) *SQLiteReplayCache {
	if clk == nil {
		clk = clock.Real{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &SQLiteReplayCache{db: db, clk: clk, log: log}
	c.lastPrune.Store(clk.Now().UnixMilli())
	return c
}

// Consume implements ReplayCache. A row whose own expiry has passed does not
// count as a presentation: it is removed in the same transaction, so a nonce is
// refused exactly while its earlier use could still have been accepted.
func (c *SQLiteReplayCache) Consume(ctx context.Context, scope contracts.Scope, jti string, expiresAt time.Time) error {
	now := c.clk.Now()
	err := c.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "DELETE FROM identity_replay",
			Where:     "jti = ? AND expires_at <= ?",
			WhereArgs: []any{jti, now.UnixMilli()},
		}); err != nil {
			return err
		}
		_, err := store.InsertScoped(ctx, tx, scope, "identity_replay",
			[]string{"jti", "seen_at", "expires_at"},
			jti, now.UnixMilli(), expiresAt.UnixMilli())
		return err
	})
	switch {
	case err == nil:
		c.maybePrune(ctx, now)
		return nil
	case store.IsUniqueViolation(err):
		return reject(contracts.ErrReplayed, ReasonReplayed, "context token already used")
	default:
		return err
	}
}

// Prune deletes every expired row, in batches of pruneBatch so no single
// transaction holds the writer for long, and returns how many went. Consume
// calls it about once a minute; a deployment may also call it from a
// maintenance job. It sweeps every scope on purpose: expiry is the only
// condition, and an expired nonce is worthless in any scope.
func (c *SQLiteReplayCache) Prune(ctx context.Context) (int, error) {
	now := c.clk.Now()
	c.lastPrune.Store(now.UnixMilli())
	total := 0
	for {
		res, err := c.db.ExecContext(ctx,
			`DELETE FROM identity_replay WHERE (application_id, tenant_id, workspace_id, jti) IN (
			   SELECT application_id, tenant_id, workspace_id, jti FROM identity_replay
			   WHERE expires_at <= ? LIMIT ?)`,
			now.UnixMilli(), pruneBatch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(n)
		if n < pruneBatch {
			return total, nil
		}
	}
}

// Len returns the number of rows held, live or not yet pruned.
func (c *SQLiteReplayCache) Len(ctx context.Context) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_replay`).Scan(&n)
	return n, err
}

func (c *SQLiteReplayCache) maybePrune(ctx context.Context, now time.Time) {
	last := c.lastPrune.Load()
	if now.UnixMilli()-last < pruneEvery.Milliseconds() || !c.lastPrune.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	if _, err := c.Prune(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.log.Warn("replay cache sweep failed", "error", err)
	}
}
