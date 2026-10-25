package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// outcome is what became of a dispatched commit, decided by the core: the
// receipt's status and the preparation's next state. The host's result supplies
// the facts and record IDs; it never supplies the status.
type outcome struct {
	status act.ActionReceiptStatus
	to     act.PreparedActionState
	event  audit.EventType
	// res is the host result the receipt's records, facts, errors and follow-up
	// come from; nil for an outcome the core decided on its own.
	res *contracts.DeterministicResult
	// issues are added by the core (uncertain_outcome, the code of a refusal
	// that happened before the commit left the process).
	issues []act.Issue
	reason string
}

// recordedOutcome is a host result of ok, queued or partial: the host recorded
// the commit. Queued and partial are terminal states of the preparation too
// (the state machine has no other place for them); the receipt carries the
// distinction.
func recordedOutcome(res contracts.DeterministicResult, reason string) outcome {
	o := outcome{to: act.PreparedActionStateCommitted, event: audit.EventActionCommitted, res: &res, reason: reason}
	switch res.Status {
	case act.DeterministicResultStatusQueued:
		o.status = act.ActionReceiptStatusQueued
	case act.DeterministicResultStatusPartial:
		o.status = act.ActionReceiptStatusPartial
	default:
		o.status = act.ActionReceiptStatusCommitted
	}
	return o
}

// rejectedOutcome is a host refusal of the commit: nothing was applied. A
// refusal with code stale means the data moved, so the preparation is
// invalidated rather than merely rejected.
func rejectedOutcome(res contracts.DeterministicResult) outcome {
	o := outcome{status: act.ActionReceiptStatusRejected, to: act.PreparedActionStateRejected,
		event: audit.EventActionRejected, res: &res, reason: "host_rejected"}
	for _, is := range res.Errors {
		if is.Code == common.ErrorCodeStale {
			o.to, o.event, o.reason = act.PreparedActionStateInvalidated, audit.EventActionInvalidated, "host_reported_stale"
			break
		}
	}
	return o
}

// refusedOutcome is a failure the core found before the commit left the
// process (a scope or validation refusal): nothing was applied.
func refusedOutcome(code contracts.ErrCode) outcome {
	return outcome{
		status: act.ActionReceiptStatusRejected, to: act.PreparedActionStateRejected,
		event: audit.EventActionRejected, reason: "commit_refused_before_send",
		issues: []act.Issue{{Code: common.ErrorCode(code), Message: "the commit was refused before it reached the application; nothing was applied"}},
	}
}

// uncertainOutcome is a commit whose result is unknown even after Status.
func uncertainOutcome(reason string) outcome {
	return outcome{
		status: act.ActionReceiptStatusUncertain, to: act.PreparedActionStateUncertain,
		event: audit.EventActionUncertain, reason: reason,
		issues: []act.Issue{{Code: common.ErrorCode(contracts.ErrUncertainOutcome),
			Message: "the commit was sent and its outcome could not be confirmed; it was not retried"}},
	}
}

func cloneSlice[T any](in []T) []T {
	if len(in) == 0 {
		return []T{}
	}
	return append([]T(nil), in...)
}

// buildReceipt assembles the receipt of an outcome. auditRef is the audit
// event that records the outcome, written in the same transaction as the
// receipt.
func buildReceipt(o outcome, receiptID string, r row, auditRef string, now time.Time) (contracts.ActionReceipt, error) {
	rc := contracts.ActionReceipt{
		ReceiptID:     act.ReceiptID(receiptID),
		PreparationID: act.PreparationID(r.id),
		CapabilityID:  common.CapabilityID(r.capabilityID),
		Status:        o.status,
		Records:       []act.RecordRef{},
		Facts:         []act.Fact{},
		Errors:        []act.Issue{},
		AuditRef:      act.AuditRef(auditRef),
		IssuedAt:      stamp(now),
	}
	if o.res != nil {
		switch o.status {
		case act.ActionReceiptStatusCommitted, act.ActionReceiptStatusQueued, act.ActionReceiptStatusPartial:
			rc.Records = cloneSlice(o.res.Identifiers)
			rc.Facts = cloneSlice(o.res.Values)
		case act.ActionReceiptStatusRejected:
			rc.Facts = cloneSlice(o.res.Values)
		}
		if o.status != act.ActionReceiptStatusCommitted && o.status != act.ActionReceiptStatusQueued {
			rc.Errors = cloneSlice(o.res.Errors)
		}
		if o.res.FollowUp != nil {
			fu := *o.res.FollowUp
			rc.FollowUp = &fu
		}
	}
	rc.Errors = append(rc.Errors, o.issues...)
	if err := checkReceipt(rc); err != nil {
		return contracts.ActionReceipt{}, err
	}
	return rc, nil
}

// checkReceipt enforces the cross-field rules of schemas/actions/action-receipt
// that the generated type cannot express, so a receipt that breaks them is
// never persisted or announced.
func checkReceipt(r contracts.ActionReceipt) error {
	bad := func(msg string) error { return contracts.New(contracts.ErrInternal, "actions: invalid receipt: "+msg) }
	switch {
	case !r.Status.Valid():
		return bad("status")
	case !ids.Valid(string(r.ReceiptID), receiptPrefix) || !ids.Valid(string(r.PreparationID), preparationPrefix) || !ids.Valid(string(r.AuditRef), audit.IDPrefix):
		return bad("identifiers")
	case len(r.Records) > 256 || len(r.Facts) > 256 || len(r.Errors) > 64:
		return bad("size")
	}
	success := r.Status == act.ActionReceiptStatusCommitted || r.Status == act.ActionReceiptStatusQueued
	switch {
	case success && len(r.Errors) != 0:
		return bad("a committed or queued receipt carries errors")
	case !success && len(r.Errors) == 0:
		return bad("a receipt that is not committed or queued carries no error")
	case r.Status == act.ActionReceiptStatusRejected && len(r.Records) != 0:
		return bad("a rejected receipt names records")
	case r.Status == act.ActionReceiptStatusQueued && r.FollowUp == nil:
		return bad("a queued receipt carries no follow-up")
	}
	return nil
}

// receiptError is the typed error that goes with a receipt that is not a
// success, so a caller that only checks the error cannot read a rejected,
// partial or uncertain outcome as success.
func receiptError(r contracts.ActionReceipt) error {
	var e *contracts.Error
	switch r.Status {
	case act.ActionReceiptStatusCommitted, act.ActionReceiptStatusQueued:
		return nil
	case act.ActionReceiptStatusPartial:
		e = contracts.New(contracts.ErrPartialFailure, "part of the action was applied; see the receipt")
	case act.ActionReceiptStatusUncertain:
		e = contracts.New(contracts.ErrUncertainOutcome, "the outcome of the action is unknown; it was not retried")
	default:
		code := contracts.ErrInternal
		if len(r.Errors) > 0 {
			code = hostIssueCode(r.Errors[0].Code)
		}
		e = contracts.New(code, "the action was not applied")
		if len(r.Errors) > 0 {
			e = e.WithDetails("issues", issueSummary(r.Errors))
		}
	}
	return e.WithDetails("receipt_id", string(r.ReceiptID))
}

// persistOutcome records what became of a dispatched commit in ONE transaction:
// the audit event, the receipt, and the preparation's new state. If any part
// fails nothing is recorded and the preparation stays as it was (committing, or
// uncertain for a reconciliation), so it is resolved again later and nothing is
// ever announced that was not persisted.
//
// from is the state the preparation must still be in.
func (c *Coordinator) persistOutcome(ctx context.Context, actor contracts.Scope, r row, from act.PreparedActionState, o outcome) (contracts.ActionReceipt, error) {
	receiptID := c.ids.New(receiptPrefix)
	var receipt contracts.ActionReceipt
	err := c.db.Tx(ctx, func(tx *store.Tx) error {
		loc, err := audit.ForScope(actor)
		if err != nil {
			return err
		}
		now := trunc(c.clk.Now())
		ev, err := c.audit.AppendTx(ctx, tx, loc, c.event(o.event, r, map[string]any{
			"from": string(from), "to": string(o.to), "receipt_id": receiptID,
			"receipt_status": string(o.status), "reason": o.reason,
		}))
		if err != nil {
			return err
		}
		rc, err := buildReceipt(o, receiptID, r, ev.ID, now)
		if err != nil {
			return err
		}
		body, err := json.Marshal(rc)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "actions: the receipt cannot be encoded")
		}
		if _, err := store.InsertScoped(ctx, tx, actor, "action_receipts",
			[]string{"id", "preparation_id", "status", "receipt_json", "issued_at"},
			receiptID, r.id, string(rc.Status), string(body), toMS(now)); err != nil {
			return err
		}
		res, err := store.ExecScoped(ctx, tx, actor, store.ScopedStmt{
			SQL:       "UPDATE action_preparations SET state = ?, receipt_id = ?, updated_at = ?",
			Args:      []any{string(o.to), receiptID, toMS(now)},
			Where:     "id = ? AND state = ?",
			WhereArgs: []any{r.id, string(from)},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errChanged
		}
		receipt = rc
		return nil
	})
	if err != nil {
		return contracts.ActionReceipt{}, err
	}
	c.log.Info("action outcome recorded", "preparation_id", r.id, "receipt_id", receiptID, "status", string(o.status), "state", string(o.to))
	return receipt, nil
}

// reopen puts a committing (or uncertain) preparation back in the state it was
// committed from, because the host proved the commit was not applied. The same
// idempotency key is used if the commit is sent again, so a request that was
// delayed rather than lost cannot cause a second effect.
func (c *Coordinator) reopen(ctx context.Context, actor contracts.Scope, r row, from act.PreparedActionState, reason string) error {
	to := r.committingFrom
	var ev audit.EventType
	switch to {
	case act.PreparedActionStateConfirmed:
		ev = audit.EventActionConfirmed
	case act.PreparedActionStatePrepared:
		ev = audit.EventActionPrepared
	default:
		return corrupt("a committing row has no state to return to")
	}
	err := c.db.Tx(ctx, func(tx *store.Tx) error {
		loc, err := audit.ForScope(actor)
		if err != nil {
			return err
		}
		res, err := store.ExecScoped(ctx, tx, actor, store.ScopedStmt{
			SQL:       "UPDATE action_preparations SET state = ?, committing_at = NULL, committing_from = NULL, receipt_id = NULL, updated_at = ?",
			Args:      []any{string(to), toMS(trunc(c.clk.Now()))},
			Where:     "id = ? AND state = ?",
			WhereArgs: []any{r.id, string(from)},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errChanged
		}
		_, err = c.audit.AppendTx(ctx, tx, loc, c.event(ev, r, map[string]any{
			"from": string(from), "to": string(to), "reopened": true, "reason": reason,
		}))
		return err
	})
	if err != nil {
		return err
	}
	c.log.Info("commit not applied; preparation reopened", "preparation_id", r.id, "state", string(to), "reason", reason)
	return nil
}

// ---- reading ----

// loadReceipt reads the preparation's current receipt.
func (c *Coordinator) loadReceipt(ctx context.Context, q store.Reader, scope contracts.Scope, r row) (contracts.ActionReceipt, error) {
	if r.receiptID == "" {
		return contracts.ActionReceipt{}, contracts.New(contracts.ErrConflict, "actions: the preparation has no receipt yet")
	}
	srow, err := store.QueryRowScoped(ctx, q, scope, store.ScopedStmt{
		SQL:       "SELECT receipt_json FROM action_receipts",
		Where:     "id = ? AND preparation_id = ?",
		WhereArgs: []any{r.receiptID, r.id},
	})
	if err != nil {
		return contracts.ActionReceipt{}, err
	}
	var body string
	if err := srow.Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.ActionReceipt{}, corrupt("the receipt of a preparation is missing")
		}
		return contracts.ActionReceipt{}, err
	}
	var rc contracts.ActionReceipt
	if err := json.Unmarshal([]byte(body), &rc); err != nil {
		return contracts.ActionReceipt{}, corrupt("the receipt does not decode")
	}
	if string(rc.PreparationID) != r.id || string(rc.ReceiptID) != r.receiptID {
		return contracts.ActionReceipt{}, corrupt("the receipt belongs to another preparation")
	}
	return rc, nil
}
