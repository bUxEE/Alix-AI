package store

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Tx is a transaction handed to the function given to DB.Tx or DB.View. It
// can run statements but not commit or roll back: the helper does that from
// fn's result.
type Tx struct {
	tx *sql.Tx
}

// ExecContext runs a statement (or a script) in the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryContext runs a query in the transaction.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row query in the transaction.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// PrepareContext prepares a statement that lives as long as the transaction.
// Use it for bulk inserts; the statement is closed when the transaction ends.
func (t *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return t.tx.PrepareContext(ctx, query)
}

var _ Querier = (*Tx)(nil)

// Tx runs fn in a write transaction (BEGIN IMMEDIATE on the single writer
// connection) and commits if fn returns nil; any error, or a panic, rolls back.
// fn's own error is returned unchanged, so a contracts.Error passes through.
//
// When SQLite reports SQLITE_BUSY (another process holds the write lock past
// the busy timeout) the whole attempt is rolled back and retried with
// exponential, jittered, context-aware backoff, up to Options.BusyAttempts
// times; exhaustion returns contracts.ErrUnavailable wrapping the busy error.
// fn can therefore run more than once: keep side effects inside the
// transaction.
func (db *DB) Tx(ctx context.Context, fn func(*Tx) error) error {
	return db.run(ctx, db.w, fn)
}

// View runs fn in a read-only transaction on the reader pool: every statement
// in fn sees the same snapshot, and writes fail. Retry behaviour is the same
// as Tx.
func (db *DB) View(ctx context.Context, fn func(*Tx) error) error {
	return db.run(ctx, db.r, fn)
}

func (db *DB) run(ctx context.Context, pool *sql.DB, fn func(*Tx) error) error {
	backoff := db.busyBackoff
	for attempt := 1; ; attempt++ {
		err := attemptTx(ctx, pool, fn)
		if err == nil || !IsBusy(err) {
			return ctxError(ctx, err)
		}
		if attempt >= db.busyAttempts {
			return contracts.Wrap(contracts.ErrUnavailable, err, "store is busy: another connection holds the database lock").
				WithDetails("attempts", attempt)
		}
		db.busyRetries.Add(1)
		if err := sleepCtx(ctx, jitter(backoff)); err != nil {
			return ctxError(ctx, err)
		}
		backoff = min(backoff*2, maxBusyBackoff)
	}
}

// attemptTx runs one begin/fn/commit cycle. fn's panics roll back and
// propagate.
func attemptTx(ctx context.Context, pool *sql.DB, fn func(*Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	done = true
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return nil
}

// jitter spreads d over [d/2, d) so competing retries do not move in step.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	return d/2 + rand.N(d/2)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ctxError reports a deadline that expired while the call was in flight as
// contracts.ErrTimeout so callers can classify it. Errors that already carry a
// contracts code, and every other error, pass through untouched.
func ctxError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return contracts.Wrap(contracts.ErrTimeout, err, "store call exceeded its deadline")
	}
	return err
}
