package actions

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// recoverBatch is how many rows one sweep reads at a time.
const recoverBatch = 100

// maxReportedErrors bounds the errors a report keeps.
const maxReportedErrors = 10

// RecoverOptions selects which committing rows Recover resolves.
type RecoverOptions struct {
	// Before resolves committing rows that were started at or before this
	// instant. Zero means now minus Config.CommitStaleAfter, which leaves
	// commits that are still in flight in this process alone. A process that has
	// just started has no commit in flight, so RecoverAtStartup passes now.
	Before time.Time
}

// RecoverReport says what a Recover or ExpireDue sweep did.
type RecoverReport struct {
	// Examined is how many rows were looked at.
	Examined int
	// Recorded is how many commits the host proved it recorded: a committed,
	// queued or partial receipt was written.
	Recorded int
	// Reopened is how many commits the host proved it never applied: the
	// preparation is back where it was and the user can commit it again.
	Reopened int
	// Uncertain is how many outcomes Status could not decide: an uncertain
	// receipt was written and the user is told so.
	Uncertain int
	// Expired is how many preparations ExpireDue moved to expired.
	Expired int
	// Failed is how many resolutions could not be recorded; those rows are
	// unchanged and the next sweep tries again.
	Failed int
	// Errors holds the first few failures.
	Errors []error
}

func (rp *RecoverReport) fail(err error) {
	rp.Failed++
	if len(rp.Errors) < maxReportedErrors {
		rp.Errors = append(rp.Errors, err)
	}
}

// RecoverAtStartup resolves every committing row. The composition root calls it
// before the service accepts requests: no commit of this process can be in
// flight yet, so every committing row belongs to a process that died.
func (c *Coordinator) RecoverAtStartup(ctx context.Context) (RecoverReport, error) {
	return c.Recover(ctx, RecoverOptions{Before: c.clk.Now()})
}

// Recover resolves committing rows: preparations whose commit was started (the
// committing state and its audit event were written first) and whose outcome was
// never recorded, because the process stopped between writing "committing" and
// recording the outcome (SEC-046, master plan 18 and 22.5).
//
// Each row is resolved by asking the host's Status for its idempotency key,
// never by committing again:
//
//   - a recorded outcome (ok, queued, partial) becomes the receipt;
//   - the no-record marker proves nothing was applied, and the preparation goes
//     back to the state it was committed from. Recovery does not send the
//     commit again: it holds only the scope the preparation was made under, a
//     token that may have expired and roles that may have changed. A commit is
//     sent by a caller with a live scope, which runs every check again;
//   - anything else, including a missing adapter or a denial of the Status call,
//     is an unknown outcome: the state becomes uncertain, the receipt says so,
//     and nothing is retried.
//
// Recover is a service operation across tenants, like jobs.Lease: it must not
// be reachable from a tenant request. Rows it cannot resolve are left as they
// are for the next sweep.
func (c *Coordinator) Recover(ctx context.Context, opts RecoverOptions) (RecoverReport, error) {
	var rep RecoverReport
	if ctx == nil {
		return rep, contracts.New(contracts.ErrInternal, "actions: no context")
	}
	before := opts.Before
	if before.IsZero() {
		before = c.clk.Now().Add(-c.cfg.CommitStaleAfter)
	}
	var cursor int64
	for {
		ids, last, err := c.pending(ctx,
			"SELECT seq, id FROM action_preparations WHERE state = 'committing' AND committing_at <= ? AND seq > ? ORDER BY seq LIMIT ?",
			toMS(before), cursor, recoverBatch)
		if err != nil {
			return rep, err
		}
		if len(ids) == 0 {
			break
		}
		cursor = last
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			rep.Examined++
			c.recoverOne(ctx, id, &rep)
		}
	}
	return rep, errors.Join(rep.Errors...)
}

// pending reads a page of preparation IDs and the last sequence number read.
func (c *Coordinator) pending(ctx context.Context, query string, args ...any) (ids []string, last int64, err error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the store could not be read")
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&last, &id); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	return ids, last, rows.Err()
}

func (c *Coordinator) recoverOne(ctx context.Context, id string, rep *RecoverReport) {
	r, err := c.loadAny(ctx, c.db, id)
	if err != nil {
		rep.fail(err)
		return
	}
	if r.state != act.PreparedActionStateCommitting {
		return // resolved by someone else since the page was read
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.SettleTimeout)
	defer cancel()
	adapter, err := c.adapters.Adapter(rctx, r.binding.ApplicationID)
	if err != nil {
		// Status cannot even be asked: the outcome is unknown.
		if _, perr := c.persistOutcome(rctx, r.scope, r, act.PreparedActionStateCommitting, uncertainOutcome("adapter_unavailable_at_recovery")); perr != nil {
			rep.fail(perr)
			return
		}
		rep.Uncertain++
		return
	}
	res, _, err := c.resolve(rctx, r.scope, adapter, r, act.PreparedActionStateCommitting)
	if err != nil {
		rep.fail(err)
		return
	}
	switch res {
	case resolvedRecorded:
		rep.Recorded++
	case resolvedReopened:
		rep.Reopened++
	case resolvedUncertain:
		rep.Uncertain++
	}
}

// Reconcile asks the host again about a preparation whose outcome was
// uncertain, with the caller's live scope (master plan 18: an uncertain outcome
// is checked, never retried blindly). It never commits.
//
//   - If the host now reports a recorded outcome, a new receipt is written and
//     returned (committed, queued or partial) and the preparation is committed.
//   - If the host proves it never applied the commit, the preparation goes back
//     to where it was and Reconcile returns unavailable with details
//     outcome=not_applied: the commit can be sent again with the same key.
//   - Otherwise nothing changes and the error is uncertain_outcome, returned
//     with the existing uncertain receipt.
func (c *Coordinator) Reconcile(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error) {
	var none contracts.ActionReceipt
	r, err := c.loadOwned(ctx, scope, preparationID)
	if err != nil {
		return none, err
	}
	if r.state != act.PreparedActionStateUncertain {
		return none, contracts.New(contracts.ErrConflict, "only a preparation with an uncertain outcome is reconciled")
	}
	adapter, err := c.adapters.Adapter(ctx, r.binding.ApplicationID)
	if err != nil {
		return none, err
	}
	sctx, cancel := c.settleContext(ctx)
	defer cancel()
	res, rc, err := c.resolve(sctx, scope, adapter, r, act.PreparedActionStateUncertain)
	switch res {
	case resolvedRecorded:
		if err != nil {
			return none, contracts.Wrap(contracts.ErrUncertainOutcome, err, "the outcome was found but could not be recorded; ask again")
		}
		return rc, receiptError(rc)
	case resolvedReopened:
		return none, errNotApplied()
	}
	cur, lerr := c.loadReceipt(ctx, c.db, scope, r)
	if lerr != nil {
		return none, lerr
	}
	return cur, receiptError(cur)
}

// ExpireDue moves every prepared or confirmed preparation whose expiry has
// passed to expired and records it. Expiry is also enforced on every Confirm
// and Commit, so this is housekeeping that keeps the stored states and the
// audit log truthful. It is a service operation across tenants.
func (c *Coordinator) ExpireDue(ctx context.Context) (RecoverReport, error) {
	var rep RecoverReport
	if ctx == nil {
		return rep, contracts.New(contracts.ErrInternal, "actions: no context")
	}
	now := toMS(c.clk.Now())
	var cursor int64
	for {
		ids, last, err := c.pending(ctx,
			"SELECT seq, id FROM action_preparations WHERE state IN ('prepared', 'confirmed') AND expires_at <= ? AND seq > ? ORDER BY seq LIMIT ?",
			now, cursor, recoverBatch)
		if err != nil {
			return rep, err
		}
		if len(ids) == 0 {
			break
		}
		cursor = last
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			rep.Examined++
			r, err := c.loadAny(ctx, c.db, id)
			if err != nil {
				rep.fail(err)
				continue
			}
			if r.state != act.PreparedActionStatePrepared && r.state != act.PreparedActionStateConfirmed {
				continue
			}
			if err := c.endPreparation(ctx, r.scope, r, act.PreparedActionStateExpired, audit.EventActionExpired, "expired_by_sweep"); err != nil {
				if !errors.Is(err, errChanged) {
					rep.fail(err)
				}
				continue
			}
			rep.Expired++
		}
	}
	return rep, errors.Join(rep.Errors...)
}
