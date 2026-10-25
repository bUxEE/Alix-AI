package actions

import (
	"context"
	"crypto/hmac"
	"errors"
	"slices"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// loadOwned loads a preparation of the caller's scope and checks that the
// caller is the same authenticated context it was prepared under: a
// preparation belongs to one principal in one session (SEC-040). The
// continuity error names only the levels that changed.
func (c *Coordinator) loadOwned(ctx context.Context, scope contracts.Scope, id string) (row, error) {
	if err := c.checkRequest(ctx, scope); err != nil {
		return row{}, err
	}
	r, err := c.load(ctx, c.db, scope, id)
	if err != nil {
		return row{}, err
	}
	if err := identity.CheckContinuity(r.scope, scope); err != nil {
		return row{}, err
	}
	return r, nil
}

// errForState is the typed error for asking to move a preparation that is in
// a state that cannot take the request.
func errForState(r row) error {
	switch r.state {
	case act.PreparedActionStateExpired:
		return errExpired()
	case act.PreparedActionStateInvalidated:
		return errStale()
	case act.PreparedActionStateRejected:
		return contracts.New(contracts.ErrConflict, "the preparation was rejected")
	case act.PreparedActionStateCommitting:
		return contracts.New(contracts.ErrConflict, "the commit of this preparation is already in progress")
	default:
		return contracts.New(contracts.ErrConflict, "the preparation is "+string(r.state))
	}
}

// endPreparation moves a prepared or confirmed preparation to a state it
// never leaves (expired, invalidated, rejected) and records the transition. No
// adapter is involved. If the write fails nothing changed; the checks that led
// here run again on the next request. The state is read again in the
// transaction, so a Confirm that ran since r was read does not stop the
// transition and the record names the state the preparation really left.
func (c *Coordinator) endPreparation(ctx context.Context, actor contracts.Scope, r row, to act.PreparedActionState, ev audit.EventType, reason string) error {
	err := c.db.Tx(ctx, func(tx *store.Tx) error {
		loc, err := audit.ForScope(actor)
		if err != nil {
			return err
		}
		cur, err := c.load(ctx, tx, actor, r.id)
		if err != nil {
			return err
		}
		if cur.state != act.PreparedActionStatePrepared && cur.state != act.PreparedActionStateConfirmed {
			return errChanged
		}
		res, err := store.ExecScoped(ctx, tx, actor, store.ScopedStmt{
			SQL:       "UPDATE action_preparations SET state = ?, updated_at = ?",
			Args:      []any{string(to), toMS(trunc(c.clk.Now()))},
			Where:     "id = ? AND state = ?",
			WhereArgs: []any{r.id, string(cur.state)},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errChanged
		}
		_, err = c.audit.AppendTx(ctx, tx, loc, c.event(ev, r, map[string]any{
			"from": string(cur.state), "to": string(to), "reason": reason,
		}))
		return err
	})
	if err != nil {
		c.log.Warn("preparation transition not recorded", "preparation_id", r.id, "to", string(to), "reason", reason, "code", string(contracts.CodeOf(err)))
		return err
	}
	c.log.Info("preparation ended", "preparation_id", r.id, "state", string(to), "reason", reason)
	return nil
}

// Confirm consumes the single-use confirmation of an action that needs one
// and moves it from prepared to confirmed (SEC-039, SEC-040).
//
//   - The caller must be the principal and session the action was prepared
//     under (identity.CheckContinuity); another principal, another session or
//     another preparation fails and does not use up the confirmation.
//   - The token must be the one Prepare issued for this preparation, this
//     principal, this session and these data versions.
//   - A confirmation is used exactly once. A second presentation, at any later
//     time, is replayed and reaches no adapter.
//   - An action that needs a step-up (level 3) cannot be confirmed here at all:
//     the AI path cannot complete it (step_up_required).
//   - An expired preparation moves to expired and returns expired.
func (c *Coordinator) Confirm(ctx context.Context, scope contracts.Scope, in ConfirmInput) (contracts.PreparedAction, error) {
	var none contracts.PreparedAction
	r, err := c.loadOwned(ctx, scope, in.PreparationID)
	if err != nil {
		return none, err
	}
	// A confirmation is used once, whatever became of the preparation since.
	if !r.confirmedAt.IsZero() {
		c.log.Warn("confirmation presented again", "preparation_id", r.id, "state", string(r.state))
		return none, contracts.New(contracts.ErrReplayed, "the confirmation was already used")
	}
	switch r.state {
	case act.PreparedActionStatePrepared:
	default:
		return none, errForState(r)
	}
	if !c.clk.Now().Before(r.expiresAt) {
		_ = c.endPreparation(ctx, scope, r, act.PreparedActionStateExpired, audit.EventActionExpired, "expired_at_confirm")
		return none, errExpired()
	}
	switch r.mode {
	case act.ConfirmationRequirementModeNone:
		return none, contracts.New(contracts.ErrValidationFailed, "this action needs no confirmation")
	case act.ConfirmationRequirementModeStepUp:
		return none, contracts.New(contracts.ErrStepUpRequired, "this action must be completed in the application's own authenticated interface")
	}
	if in.Token.IsZero() {
		return none, contracts.New(contracts.ErrConfirmationRequired, "a confirmation is required")
	}
	want := confirmMAC(in.Token, r.id, scope.PrincipalID, scope.SessionID, dataDigest(r.prepared.DataVersions))
	if r.mac == "" || !hmac.Equal([]byte(want), []byte(r.mac)) {
		c.log.Warn("confirmation does not match", "preparation_id", r.id)
		return none, contracts.New(contracts.ErrPermissionDenied, "the confirmation does not belong to this preparation")
	}

	err = c.db.Tx(ctx, func(tx *store.Tx) error {
		loc, err := audit.ForScope(scope)
		if err != nil {
			return err
		}
		now := toMS(trunc(c.clk.Now()))
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "UPDATE action_preparations SET state = 'confirmed', confirmed_at = ?, updated_at = ?",
			Args:      []any{now, now},
			Where:     "id = ? AND state = 'prepared' AND confirmed_at IS NULL",
			WhereArgs: []any{r.id},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errChanged
		}
		_, err = c.audit.AppendTx(ctx, tx, loc, c.event(audit.EventActionConfirmed, r, map[string]any{
			"from": string(act.PreparedActionStatePrepared), "to": string(act.PreparedActionStateConfirmed),
		}))
		return err
	})
	switch {
	case errors.Is(err, errChanged):
		// Another request used the confirmation first.
		return none, contracts.New(contracts.ErrReplayed, "the confirmation was already used")
	case err != nil:
		// Not recorded: the confirmation is not used up and can be presented again.
		return none, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the confirmation could not be recorded")
	}
	r.state = act.PreparedActionStateConfirmed
	r.prepared.State = act.PreparedActionStateConfirmed
	return r.prepared, nil
}

// Reject records that the user declined a preparation. Nothing is sent to the
// host. Asking again for a preparation that is already rejected returns it.
func (c *Coordinator) Reject(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.PreparedAction, error) {
	var none contracts.PreparedAction
	r, err := c.loadOwned(ctx, scope, preparationID)
	if err != nil {
		return none, err
	}
	switch r.state {
	case act.PreparedActionStateRejected:
		return r.prepared, nil
	case act.PreparedActionStatePrepared, act.PreparedActionStateConfirmed:
	default:
		return none, errForState(r)
	}
	if err := c.endPreparation(ctx, scope, r, act.PreparedActionStateRejected, audit.EventActionRejected, "declined_by_user"); err != nil {
		return none, c.transitionError(err)
	}
	r.prepared.State = act.PreparedActionStateRejected
	return r.prepared, nil
}

// Invalidate voids a preparation because something the preparation depended on
// changed: the user corrected a field, the goal changed, the session ended, or
// the application reported an event. The session and event layers call it; it
// never commits anything. reason must be one of the preparation's own
// invalidation conditions. Invalidating a preparation that is already
// invalidated returns it.
func (c *Coordinator) Invalidate(ctx context.Context, scope contracts.Scope, preparationID string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	var none contracts.PreparedAction
	r, err := c.loadOwned(ctx, scope, preparationID)
	if err != nil {
		return none, err
	}
	if !reason.Valid() || !slices.Contains(r.prepared.InvalidationConditions, reason) {
		return none, contracts.New(contracts.ErrValidationFailed, "the reason is not an invalidation condition of this preparation")
	}
	switch r.state {
	case act.PreparedActionStateInvalidated:
		return r.prepared, nil
	case act.PreparedActionStatePrepared, act.PreparedActionStateConfirmed:
	default:
		return none, errForState(r)
	}
	if err := c.endPreparation(ctx, scope, r, act.PreparedActionStateInvalidated, audit.EventActionInvalidated, string(reason)); err != nil {
		return none, c.transitionError(err)
	}
	r.prepared.State = act.PreparedActionStateInvalidated
	return r.prepared, nil
}

// transitionError types the failure of a state transition.
func (c *Coordinator) transitionError(err error) error {
	if errors.Is(err, errChanged) {
		return contracts.New(contracts.ErrConflict, "the preparation changed state; read it again")
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "actions: the transition could not be recorded")
}

// Get returns a preparation of the caller's own session, with its current state.
func (c *Coordinator) Get(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.PreparedAction, error) {
	r, err := c.loadOwned(ctx, scope, preparationID)
	if err != nil {
		return contracts.PreparedAction{}, err
	}
	return r.prepared, nil
}

// Receipt returns the current receipt of a preparation of the caller's own
// session, or conflict when no outcome exists yet.
func (c *Coordinator) Receipt(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error) {
	r, err := c.loadOwned(ctx, scope, preparationID)
	if err != nil {
		return contracts.ActionReceipt{}, err
	}
	return c.loadReceipt(ctx, c.db, scope, r)
}
