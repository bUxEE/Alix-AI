package actions

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Commit sends a prepared action to the host (master plan 3 rule 8, 11.4, 18).
// Nothing reaches the adapter's Commit unless, in this order:
//
//  1. the preparation exists in the caller's scope and the caller is the same
//     principal and session it was prepared under;
//  2. it is not expired;
//  3. the policy gateway still allows the capability, decided against the
//     stored application, tenant and workspace and the evidence obtained so far
//     (the zero Evidence is a denial), and has not raised the risk level;
//  4. the confirmation its level requires is present: confirmed, once, by this
//     principal in this session. A level 3 action can never be completed here
//     (step_up_required);
//  5. the host, asked to Validate the stored fields again, still reports the
//     data version the preparation was made against;
//  6. the "committing" state and its audit event have been written in one
//     transaction (SEC-087: if that write fails the commit does not happen).
//
// Commit is idempotent: once a preparation has an outcome, every further call
// returns the same receipt and sends nothing. It returns the receipt together
// with a non-nil typed error when the outcome is not a success (rejected,
// partial, uncertain), so a caller that only looks at the error cannot read it
// as one. The error for an uncertain outcome is uncertain_outcome.
//
// After the commit is sent the coordinator does not retry. A failure other
// than a definite one is resolved by asking the host's Status. Only the
// no-record answer proves nothing was applied; the preparation then goes back
// to where it was and the error is unavailable, and the caller may call Commit
// again with the same idempotency key. Any other answer, including a denial of
// the Status call itself, leaves the outcome unknown: the receipt is uncertain.
func (c *Coordinator) Commit(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error) {
	// A request that finds the preparation moved by another request (a concurrent
	// commit, a sweep) starts again from its new state, so the loser of a race
	// gets the winner's receipt or a typed conflict, not a refusal that was
	// true only a moment ago.
	for range 3 {
		rc, err := c.commitPass(ctx, scope, preparationID)
		if !errors.Is(err, errRetry) {
			return rc, err
		}
	}
	return contracts.ActionReceipt{}, contracts.New(contracts.ErrConflict, "the preparation keeps changing state; read it again")
}

// errRetry reports that the preparation was moved by another request while
// this one was checking it.
var errRetry = errors.New("actions: the preparation was moved by another request")

// voidFor ends a preparation for a reason found while checking a commit and
// returns the refusal that goes with it. If another request moved the
// preparation first, the whole commit starts again from the new state.
func (c *Coordinator) voidFor(ctx context.Context, scope contracts.Scope, r row, to act.PreparedActionState, ev audit.EventType, reason string, refusal error) error {
	if err := c.endPreparation(ctx, scope, r, to, ev, reason); errors.Is(err, errChanged) {
		return errRetry
	}
	return refusal
}

func (c *Coordinator) commitPass(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error) {
	var none contracts.ActionReceipt
	r, err := c.loadOwned(ctx, scope, preparationID)
	if err != nil {
		return none, err
	}

	// Idempotency: an outcome already exists, so it is returned and nothing is sent.
	if r.receiptID != "" {
		rc, err := c.loadReceipt(ctx, c.db, scope, r)
		if err != nil {
			return none, err
		}
		return rc, receiptError(rc)
	}
	from := r.state
	switch r.state {
	case act.PreparedActionStateConfirmed, act.PreparedActionStatePrepared:
	default:
		return none, errForState(r)
	}

	if !c.clk.Now().Before(r.expiresAt) {
		return none, c.voidFor(ctx, scope, r, act.PreparedActionStateExpired, audit.EventActionExpired, "expired_at_commit", errExpired())
	}
	// A preparation that still needs its confirmation or a step-up is refused
	// before anything else is asked of anyone.
	if r.state == act.PreparedActionStatePrepared {
		switch r.mode {
		case act.ConfirmationRequirementModeStepUp:
			return none, contracts.New(contracts.ErrStepUpRequired, "this action must be completed in the application's own authenticated interface")
		case act.ConfirmationRequirementModeExplicit:
			return none, contracts.New(contracts.ErrConfirmationRequired, "the action has not been confirmed")
		}
	}

	// Policy decides again, now, with what has been obtained so far. StepUp is
	// never set: the AI path cannot complete a step-up.
	pkg, err := c.packages.ActivePackage(ctx, r.binding.ApplicationID)
	if err != nil {
		return none, err
	}
	d := c.policy.Decide(ctx, policy.Request{
		Scope: scope, Package: pkg, CapabilityID: r.capabilityID, Binding: r.binding,
		Phase: policy.PhaseCommit, Fields: r.fields,
		Evidence: policy.Evidence{Confirmed: r.state == act.PreparedActionStateConfirmed},
	})
	if !d.Allowed {
		return none, c.voidOnDenial(ctx, scope, r, d)
	}
	if d.EffectiveRisk > r.risk {
		return none, c.voidFor(ctx, scope, r, act.PreparedActionStateInvalidated, audit.EventActionInvalidated,
			string(act.PreparedActionInvalidationConditionsItemPermissionChanged),
			errStale().WithDetails("reason", "the policy now assigns a higher risk level"))
	}
	// The stored requirement binds too: the host may have raised the level at
	// prepare beyond what the policy computes from the declaration alone.
	switch r.mode {
	case act.ConfirmationRequirementModeStepUp:
		return none, contracts.New(contracts.ErrStepUpRequired, "this action must be completed in the application's own authenticated interface")
	case act.ConfirmationRequirementModeExplicit:
		if r.state != act.PreparedActionStateConfirmed {
			return none, contracts.New(contracts.ErrConfirmationRequired, "the action has not been confirmed")
		}
	}

	adapter, err := c.adapters.Adapter(ctx, r.binding.ApplicationID)
	if err != nil {
		return none, err
	}
	if err := c.recheck(ctx, scope, adapter, r); err != nil {
		return none, err
	}

	// Write-ahead (SEC-087, SEC-046): the committing state, the idempotency key
	// it is sent with and the audit event of it are written in ONE transaction,
	// and only when it has committed is the adapter called. The adapter is never
	// called inside the transaction: the store re-runs a transaction on a busy
	// database, which would send the commit twice.
	err = c.db.Tx(ctx, func(tx *store.Tx) error {
		loc, err := audit.ForScope(scope)
		if err != nil {
			return err
		}
		now := toMS(trunc(c.clk.Now()))
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "UPDATE action_preparations SET state = 'committing', committing_at = ?, committing_from = ?, " +
				"commit_attempts = commit_attempts + 1, updated_at = ?",
			Args:      []any{now, string(from), now},
			Where:     "id = ? AND state = ? AND expires_at > ?",
			WhereArgs: []any{r.id, string(from), now},
		})
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errChanged
		}
		_, err = c.audit.AppendTx(ctx, tx, loc, c.event(audit.EventActionCommitting, r, map[string]any{
			"from": string(from), "to": string(act.PreparedActionStateCommitting), "attempt": r.attempts + 1,
		}))
		return err
	})
	switch {
	case errors.Is(err, errChanged):
		return none, errRetry
	case err != nil:
		c.log.Warn("commit not sent: the write-ahead record could not be written", "preparation_id", r.id, "code", string(contracts.CodeOf(err)))
		return none, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the commit was not sent because its audit record could not be written")
	}
	r.state = act.PreparedActionStateCommitting
	r.committingFrom = from
	r.attempts++

	prepared := r.prepared
	prepared.State = act.PreparedActionStateCommitting
	res, callErr := adapter.Commit(ctx, scope, adapters.CommitRequest{
		CapabilityID: r.capabilityID, IdempotencyKey: act.IdempotencyKey(r.key), Prepared: prepared,
	})

	// From here the commit may have been applied. What is left to do must not
	// depend on the caller still being there.
	sctx, cancel := c.settleContext(ctx)
	defer cancel()
	return c.afterDispatch(sctx, scope, adapter, r, res, callErr)
}

// settleContext is the context of everything done after a commit was sent: it
// keeps the caller's values, drops its cancellation and has its own deadline.
func (c *Coordinator) settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), c.cfg.SettleTimeout)
}

// voidOnDenial ends a preparation when the policy denies it for a reason that
// will not pass by itself: the principal lost the permission, or the
// application was quarantined. Other denials (a missing step-up or
// confirmation, a token that does not name the capability, an audit sink that
// is down) leave it as it is.
func (c *Coordinator) voidOnDenial(ctx context.Context, scope contracts.Scope, r row, d policy.Decision) error {
	var reason act.PreparedActionInvalidationConditionsItem
	switch d.Code {
	case contracts.ErrPermissionDenied:
		reason = act.PreparedActionInvalidationConditionsItemPermissionChanged
	case contracts.ErrQuarantined:
		reason = act.PreparedActionInvalidationConditionsItemApplicationQuarantined
	default:
		return d.Err()
	}
	return c.voidFor(ctx, scope, r, act.PreparedActionStateInvalidated, audit.EventActionInvalidated, string(reason), d.Err())
}

// recheck asks the host to validate the stored fields again and compares the
// data version it reports with the one the preparation was made against
// (SEC-042). A host that cannot answer is not a stale preparation: the error is
// returned and the preparation stays as it is, so the commit can be tried again
// when the host is back. The comparison uses the version stored at prepare,
// never one taken from a later reply (SEC-050).
func (c *Coordinator) recheck(ctx context.Context, scope contracts.Scope, adapter adapters.AppAdapter, r row) error {
	res, err := adapter.Validate(ctx, scope, adapters.ValidateRequest{CapabilityID: r.capabilityID, Fields: r.fields})
	if err != nil {
		return err
	}
	if res, err = checkResult(res, adapters.OpValidate, r.capabilityID, ""); err != nil {
		return err
	}
	switch res.Status {
	case act.DeterministicResultStatusOk:
		if res.Freshness != nil && string(res.Freshness.Version) == r.snapshot {
			return nil
		}
		return c.voidFor(ctx, scope, r, act.PreparedActionStateInvalidated, audit.EventActionInvalidated,
			string(act.PreparedActionInvalidationConditionsItemDataVersionChanged), errStale())
	case act.DeterministicResultStatusRejected:
		code := contracts.ErrStale
		reason := act.PreparedActionInvalidationConditionsItemDataVersionChanged
		if len(res.Errors) > 0 {
			switch c0 := hostIssueCode(res.Errors[0].Code); c0 {
			case contracts.ErrPermissionDenied, contracts.ErrScopeViolation, contracts.ErrUnauthenticated:
				code, reason = c0, act.PreparedActionInvalidationConditionsItemPermissionChanged
			}
		}
		refusal := contracts.New(code, "the application no longer accepts the preparation; prepare the action again").
			WithDetails("issues", issueSummary(res.Errors))
		return c.voidFor(ctx, scope, r, act.PreparedActionStateInvalidated, audit.EventActionInvalidated, string(reason), refusal)
	default:
		return contracts.New(contracts.ErrUnavailable, "the application could not re-check the preparation; nothing was committed").
			WithDetails("host_status", string(res.Status))
	}
}

// verdict classifies what came back from a dispatched Commit.
type verdict int

const (
	// verdictRecorded: the host recorded the commit (ok, queued or partial) and
	// the result concerns this action.
	verdictRecorded verdict = iota
	// verdictRejected: the host refused; nothing was applied.
	verdictRejected
	// verdictNotSent: the failure proves the request never reached the host.
	verdictNotSent
	// verdictRefused: the request was refused before it left the process.
	verdictRefused
	// verdictUnknown: the commit may or may not have been applied.
	verdictUnknown
)

// classifyCommit applies the rule "after a commit is dispatched, any failure
// other than a known definite one is an uncertain outcome". The definite ones
// are listed here and not taken from the adapter wrapper, whose own list also
// holds uncertain_outcome (to avoid wrapping it twice) and partial_failure
// (which is not "nothing happened").
func classifyCommit(r row, res contracts.DeterministicResult, callErr error) (verdict, contracts.ErrCode, contracts.DeterministicResult) {
	var zero contracts.DeterministicResult
	if callErr != nil {
		if contracts.HasCode(callErr, contracts.ErrUncertainOutcome) {
			return verdictUnknown, contracts.ErrUncertainOutcome, zero
		}
		code := contracts.CodeOf(callErr)
		switch code {
		case contracts.ErrUnavailable, contracts.ErrResourceExhausted, contracts.ErrRateLimited:
			return verdictNotSent, code, zero
		case contracts.ErrScopeMissing, contracts.ErrScopeViolation, contracts.ErrUnauthenticated, contracts.ErrPermissionDenied,
			contracts.ErrUnknownCapability, contracts.ErrValidationFailed, contracts.ErrMissingFields, contracts.ErrAmbiguous,
			contracts.ErrQuarantined:
			return verdictRefused, code, zero
		}
		return verdictUnknown, code, zero
	}
	checked, err := checkResult(res, adapters.OpCommit, r.capabilityID, r.key)
	if err != nil {
		return verdictUnknown, contracts.CodeOf(err), zero
	}
	switch checked.Status {
	case act.DeterministicResultStatusOk, act.DeterministicResultStatusQueued, act.DeterministicResultStatusPartial:
		if bindCommit(r.prepared, checked) != nil {
			return verdictUnknown, contracts.ErrSchemaInvalid, zero
		}
		return verdictRecorded, "", checked
	case act.DeterministicResultStatusRejected:
		return verdictRejected, "", checked
	}
	return verdictUnknown, "", zero
}

// afterDispatch decides what a dispatched commit became.
func (c *Coordinator) afterDispatch(ctx context.Context, scope contracts.Scope, adapter adapters.AppAdapter, r row, res contracts.DeterministicResult, callErr error) (contracts.ActionReceipt, error) {
	var none contracts.ActionReceipt
	v, code, checked := classifyCommit(r, res, callErr)
	switch v {
	case verdictRecorded:
		return c.conclude(ctx, scope, r, act.PreparedActionStateCommitting, recordedOutcome(checked, "host_recorded"))
	case verdictRejected:
		return c.conclude(ctx, scope, r, act.PreparedActionStateCommitting, rejectedOutcome(checked))
	case verdictRefused:
		return c.conclude(ctx, scope, r, act.PreparedActionStateCommitting, refusedOutcome(code))
	case verdictNotSent:
		if err := c.reopen(ctx, scope, r, act.PreparedActionStateCommitting, "commit_not_sent"); err != nil {
			c.log.Warn("preparation not reopened; recovery will resolve it", "preparation_id", r.id, "code", string(contracts.CodeOf(err)))
		}
		return none, callErr
	}
	// The commit may have been applied. Ask the host before anything else.
	return c.settle(ctx, scope, adapter, r, act.PreparedActionStateCommitting)
}

// conclude persists an outcome and turns it into the Commit return values. If
// the outcome cannot be persisted nothing is announced: the preparation stays
// committing and is resolved from the host's Status later.
func (c *Coordinator) conclude(ctx context.Context, scope contracts.Scope, r row, from act.PreparedActionState, o outcome) (contracts.ActionReceipt, error) {
	rc, err := c.persistOutcome(ctx, scope, r, from, o)
	if err != nil {
		c.log.Warn("commit outcome not recorded", "preparation_id", r.id, "code", string(contracts.CodeOf(err)))
		return contracts.ActionReceipt{}, contracts.Wrap(contracts.ErrUncertainOutcome, err,
			"the commit was sent and its outcome could not be recorded; it will be resolved from the application's status")
	}
	return rc, receiptError(rc)
}

// errNotApplied is the error for a commit the host proved it never applied.
func errNotApplied() *contracts.Error {
	return contracts.New(contracts.ErrUnavailable, "the application has no record of the commit; nothing was applied and the commit can be sent again").
		WithDetails("outcome", "not_applied")
}

// settle resolves an unknown outcome and turns the resolution into the return
// values of Commit.
func (c *Coordinator) settle(ctx context.Context, scope contracts.Scope, adapter adapters.AppAdapter, r row, from act.PreparedActionState) (contracts.ActionReceipt, error) {
	res, rc, err := c.resolve(ctx, scope, adapter, r, from)
	switch res {
	case resolvedRecorded, resolvedUncertain:
		if err != nil {
			c.log.Warn("commit outcome not recorded", "preparation_id", r.id, "code", string(contracts.CodeOf(err)))
			return contracts.ActionReceipt{}, contracts.Wrap(contracts.ErrUncertainOutcome, err,
				"the commit was sent and its outcome could not be recorded; it will be resolved from the application's status")
		}
		return rc, receiptError(rc)
	case resolvedReopened:
		if err != nil {
			c.log.Warn("preparation not reopened; recovery will resolve it", "preparation_id", r.id, "code", string(contracts.CodeOf(err)))
		}
		return contracts.ActionReceipt{}, errNotApplied()
	}
	return contracts.ActionReceipt{}, contracts.New(contracts.ErrUncertainOutcome, "the outcome of the action is unknown; it was not retried")
}

// resolution is what asking the host's Status led to.
type resolution int

const (
	// resolvedRecorded: the host recorded the commit; a committed, queued or
	// partial receipt was written.
	resolvedRecorded resolution = iota
	// resolvedReopened: the host has no record; the preparation is back in the
	// state it was committed from.
	resolvedReopened
	// resolvedUncertain: the host could not say; an uncertain receipt was written.
	resolvedUncertain
	// resolvedStillUnknown: a reconciliation of an uncertain outcome could not
	// decide either; nothing was written.
	resolvedStillUnknown
)

type statusVerdict int

const (
	statusUnknown statusVerdict = iota
	statusRecorded
	statusNoRecord
)

// askStatus asks the host what it recorded for the preparation's idempotency
// key. Only the no-record marker proves nothing was applied. Anything else
// that is not a recorded outcome for this very action (an error, a denial of
// the Status call itself, an uncertain or unavailable answer, a result about
// other records) leaves the outcome unknown.
func (c *Coordinator) askStatus(ctx context.Context, scope contracts.Scope, adapter adapters.AppAdapter, r row) (statusVerdict, contracts.DeterministicResult) {
	res, err := adapter.Status(ctx, scope, adapters.StatusRequest{CapabilityID: r.capabilityID, IdempotencyKey: act.IdempotencyKey(r.key)})
	if err != nil {
		return statusUnknown, contracts.DeterministicResult{}
	}
	if res, err = checkResult(res, adapters.OpStatus, r.capabilityID, r.key); err != nil {
		return statusUnknown, contracts.DeterministicResult{}
	}
	if adapters.StatusShowsNoRecord(res) {
		return statusNoRecord, res
	}
	switch res.Status {
	case act.DeterministicResultStatusOk, act.DeterministicResultStatusQueued, act.DeterministicResultStatusPartial:
		if bindCommit(r.prepared, res) == nil {
			return statusRecorded, res
		}
	}
	return statusUnknown, contracts.DeterministicResult{}
}

// resolve resolves a preparation that is committing (a commit was sent) or
// uncertain (reconciliation) by asking the host's Status. It never commits.
// The error is non-nil only when the resolution could not be recorded; the
// preparation is then unchanged and resolve can be called again.
func (c *Coordinator) resolve(ctx context.Context, scope contracts.Scope, adapter adapters.AppAdapter, r row, from act.PreparedActionState) (resolution, contracts.ActionReceipt, error) {
	switch v, res := c.askStatus(ctx, scope, adapter, r); v {
	case statusRecorded:
		rc, err := c.persistOutcome(ctx, scope, r, from, recordedOutcome(res, "status_recorded"))
		return resolvedRecorded, rc, err
	case statusNoRecord:
		return resolvedReopened, contracts.ActionReceipt{}, c.reopen(ctx, scope, r, from, "status_no_record")
	}
	if from == act.PreparedActionStateUncertain {
		return resolvedStillUnknown, contracts.ActionReceipt{}, nil
	}
	rc, err := c.persistOutcome(ctx, scope, r, from, uncertainOutcome("status_undecided"))
	return resolvedUncertain, rc, err
}
