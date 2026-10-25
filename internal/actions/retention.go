package actions

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Retention and erasure of finished preparations and their receipts (master
// plan 19; ADR-0036, M5-26).
//
// A receipt is evidence, so nothing here touches one that an action still
// depends on. The states split in two:
//
//	pending    prepared, confirmed, committing, uncertain: the action is not over, or
//	           its outcome is not known. Its preparation, its receipts and the audit
//	           events that name it are kept and are never erased.
//	finished   committed, rejected, expired, invalidated: nothing further happens to
//	           the preparation. Its receipts and the preparation row (whose scope_json,
//	           fields_json and prepared_json name the principal and hold the field
//	           values) are removed together.
//
// A receipt is removed WITH its preparation, never alone: the coordinator reads a
// preparation's current receipt by id and treats a dangling one as corruption. A
// removed preparation is "no such preparation in this scope", so nothing a
// removed row once was can be committed, confirmed or reconciled again; the
// idempotency key it held is free, and a new Prepare mints a new one.

var (
	// pendingStates are the states of an action that is not over.
	pendingStates = []string{"prepared", "confirmed", "committing", "uncertain"}
	// finishedStates are the states nothing leaves.
	finishedStates = []string{"committed", "rejected", "expired", "invalidated"}
)

// MaxEraseBatch bounds the preparations one EraseFinished call removes; callers
// loop until it reports none.
const MaxEraseBatch = 2000

// inList renders the states as a SQL list. The values are this package's own
// constants, never input.
func inList(states []string) string { return "('" + strings.Join(states, "', '") + "')" }

// PendingGuard is the audit.Guard of this package: an audit event whose data
// names a pending preparation of the chain's tenant keeps its payload, because
// the action it records is not over and the audit trail is what Recover and
// Reconcile, and an operator, rely on to see what happened. An application chain
// holds no event that names a preparation, so it keeps nothing.
//
// It reads the pending preparations once, in the transaction that erases, so
// nothing changes between the check and the erasure.
func PendingGuard(ctx context.Context, q store.Reader, chain audit.ChainKey) (func(audit.Event) string, error) {
	if chain.IsApplication() {
		return func(audit.Event) string { return "" }, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT id FROM action_preparations WHERE application_id = ? AND tenant_id = ? AND state IN `+inList(pendingStates),
		chain.ApplicationID, chain.TenantID)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be read")
	}
	defer rows.Close()
	pending := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be read")
		}
		pending[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be read")
	}
	if len(pending) == 0 {
		return func(audit.Event) string { return "" }, nil
	}
	return func(e audit.Event) string {
		var d struct {
			PreparationID string `json:"preparation_id"`
		}
		if json.Unmarshal(e.Data, &d) == nil && pending[d.PreparationID] {
			return "a pending or uncertain action depends on this event"
		}
		return ""
	}, nil
}

// ErrErasureBlocked is wrapped by the error EraseFinished returns when a pending
// preparation matches the request: errors.Is(err, ErrErasureBlocked).
var ErrErasureBlocked = errors.New("actions: erasure refused because a pending or uncertain action matches")

// EraseSpec says which finished preparations are removed. ApplicationID and
// TenantID are required: the operator path never spans tenants.
type EraseSpec struct {
	ApplicationID string
	TenantID      string
	// PrincipalID, when set, narrows to the preparations made by that principal.
	PrincipalID string
	// Before, when not zero, narrows to preparations last changed before it
	// (retention).
	Before time.Time
	// Kind, Reason and Operator are recorded in the audit event (see
	// audit.EraseRequest); Kind is "tenant", "principal" or "retention".
	Kind, Reason, Operator string
	// SkipPending leaves the pending preparations that match alone and carries on
	// (retention). Without it one pending match refuses the whole request.
	SkipPending bool
}

// EraseOutcome says what EraseFinished did.
type EraseOutcome struct {
	// Preparations and Receipts are how many rows were removed; Pending how many
	// matching preparations were kept because they are not over.
	Preparations, Receipts, Pending int
	// RecordID is the audit event that records the removal, empty when nothing
	// was removed.
	RecordID string
}

// matches builds the WHERE clause (and its arguments) of the preparations a spec
// selects, without the state condition.
func (s EraseSpec) matches() (string, []any) {
	where := "application_id = ? AND tenant_id = ?"
	args := []any{s.ApplicationID, s.TenantID}
	if s.PrincipalID != "" {
		where += " AND principal_id = ?"
		args = append(args, s.PrincipalID)
	}
	if !s.Before.IsZero() {
		where += " AND updated_at < ?"
		args = append(args, toMS(s.Before))
	}
	return where, args
}

// eraseReceiptsSQL deletes the receipts of an application and tenant that belong
// to the preparations listed by marks (a list of placeholders).
func eraseReceiptsSQL(marks string) string {
	return "DELETE FROM action_receipts WHERE application_id = ? AND tenant_id = ? AND preparation_id IN (" + marks + ")"
}

// EraseFinished removes the finished preparations a spec selects, with their
// receipts, inside tx, and appends one deletion.receipts audit event that says
// how many and why (never whose). At most MaxEraseBatch preparations go per call;
// a caller that gets Preparations == MaxEraseBatch calls again.
//
// A pending preparation that matches is never removed. Unless spec.SkipPending
// is set, one such preparation refuses the whole request with a conflict and
// nothing is written; a request is all or nothing. The audit event is appended
// by ap in tx: if it cannot be written the removal does not happen.
func EraseFinished(ctx context.Context, tx *store.Tx, ap audit.Appender, spec EraseSpec) (EraseOutcome, error) {
	var out EraseOutcome
	switch {
	case tx == nil || ap == nil:
		return out, contracts.New(contracts.ErrInternal, "actions: EraseFinished needs a transaction and an audit appender")
	case spec.ApplicationID == "" || spec.TenantID == "":
		return out, contracts.New(contracts.ErrScopeMissing, "actions: an erasure names an application and a tenant")
	case !slices.Contains(audit.EraseKinds, spec.Kind) || strings.TrimSpace(spec.Reason) == "" || len(spec.Reason) > audit.MaxReasonBytes || strings.TrimSpace(spec.Operator) == "":
		return out, contracts.New(contracts.ErrValidationFailed, "actions: an erasure states its kind (tenant, principal or retention), a short reason and the operator")
	}
	where, args := spec.matches()

	var pending []string
	rows, err := tx.QueryContext(ctx,
		"SELECT id FROM action_preparations WHERE "+where+" AND state IN "+inList(pendingStates)+" ORDER BY seq LIMIT 11", args...)
	if err != nil {
		return out, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be read")
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be read")
		}
		pending = append(pending, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be read")
	}
	_ = rows.Close()
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM action_preparations WHERE "+where+" AND state IN "+inList(pendingStates), args...).Scan(&out.Pending); err != nil {
		return EraseOutcome{}, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the pending preparations cannot be counted")
	}
	if out.Pending > 0 && !spec.SkipPending {
		if len(pending) > 10 {
			pending = pending[:10]
		}
		return EraseOutcome{}, contracts.Wrap(contracts.ErrConflict, ErrErasureBlocked,
			"actions: erasure refused: a pending or uncertain action matches; nothing was erased").
			WithDetails("pending", out.Pending).WithDetails("preparation_ids", pending)
	}

	var ids []string
	rows, err = tx.QueryContext(ctx,
		"SELECT id FROM action_preparations WHERE "+where+" AND state IN "+inList(finishedStates)+" ORDER BY seq LIMIT ?",
		append(append([]any(nil), args...), MaxEraseBatch)...)
	if err != nil {
		return EraseOutcome{}, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the finished preparations cannot be read")
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return EraseOutcome{}, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the finished preparations cannot be read")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return EraseOutcome{}, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the finished preparations cannot be read")
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return out, nil
	}

	// The removal and its record are one transaction: if the record cannot be
	// written, nothing is removed.
	loc, err := audit.ForIDs(spec.ApplicationID, spec.TenantID, audit.OperatorWorkspace, spec.Operator, "")
	if err != nil {
		return EraseOutcome{}, err
	}
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		cargs := []any{spec.ApplicationID, spec.TenantID}
		for _, id := range chunk {
			cargs = append(cargs, id)
		}
		res, err := tx.ExecContext(ctx, eraseReceiptsSQL(marks), cargs...)
		if err != nil {
			return EraseOutcome{}, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the receipts cannot be removed")
		}
		n, _ := res.RowsAffected()
		out.Receipts += int(n)
		// The state is checked again in the statement that deletes.
		res, err = tx.ExecContext(ctx,
			"DELETE FROM action_preparations WHERE application_id = ? AND tenant_id = ? AND state IN "+inList(finishedStates)+" AND id IN ("+marks+")", cargs...)
		if err != nil {
			return EraseOutcome{}, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the preparations cannot be removed")
		}
		n, _ = res.RowsAffected()
		out.Preparations += int(n)
	}
	rec, err := ap.AppendTx(ctx, tx, loc, audit.NewEvent{Type: audit.EventDeletionReceipts, Data: map[string]any{
		"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator,
		"preparations": out.Preparations, "receipts": out.Receipts, "pending": out.Pending,
	}})
	if err != nil {
		return EraseOutcome{}, err
	}
	out.RecordID = rec.ID
	return out, nil
}
