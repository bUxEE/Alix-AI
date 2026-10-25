package dialogue

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// This file holds the engine's side of the goal state machine (master plan 11.3).
// internal/sessions owns the state and its transitions (Suspend, Resume, Nest,
// Complete, Expire); they only change the state in memory and report an Impact. What
// they report that leaves the state (a preparation that lost its goal) is done here,
// through the action coordinator, before the turn is saved. Every transition is a
// deterministic function of the state and of the typed input; nothing a model wrote
// decides one (the model proposes an intent, the rules below decide what it means).

// Names of the deterministic transitions a turn took, recorded in Trace.Transitions.
const (
	TransitionExpired   = "expired"
	TransitionCancelled = "cancelled"
	TransitionRestarted = "restarted"
	TransitionSuspended = "suspended"
	TransitionNested    = "nested"
	TransitionResumed   = "resumed"
	TransitionReturned  = "returned_to_parent"
	TransitionCompleted = "completed"
	TransitionCorrected = "corrected"
	TransitionReference = "reference_resolved"
)

func (t *turn) transition(name string) { t.trace.Transitions = append(t.trace.Transitions, name) }

// lapsedPrep is a preparation whose recorded expiry passed before this turn: the
// session forgets it (State.Expire), and a confirmation for it is answered with the
// typed reason it is refused for instead of "conflict".
type lapsedPrep struct{ id, capID string }

// expire discards what has lapsed (State.Expire): an idle goal, suspended goals past
// their own expiry, a question past its timeout, a preparation past its own expiry
// and results past their lifetime. A preparation a discarded goal still referenced is
// invalidated through the coordinator first.
func (t *turn) expire(ctx context.Context) error {
	if g := t.sess.State.Goal; g != nil && g.PreparedID != "" && !g.PreparedExpiresAt.IsZero() && !t.now.Before(g.PreparedExpiresAt) {
		t.lapsed = lapsedPrep{id: g.PreparedID, capID: g.CapabilityID}
	}
	imp := t.sess.State.Expire(t.now, t.e.sessions.Config())
	if imp.Changed() {
		t.transition(TransitionExpired)
	}
	return t.applyImpact(ctx, imp)
}

// applyImpact invalidates, through the coordinator, every preparation a state change
// reported as voided. It stops at the first failure and returns it: the turn is then
// not saved, so a preparation is never left confirmable behind a state that no longer
// mentions it, and the same event is simply tried again.
func (t *turn) applyImpact(ctx context.Context, imp sessions.Impact) error {
	for _, v := range imp.Voided {
		if err := t.invalidate(ctx, v.PreparedID, v.Reason); err != nil {
			return err
		}
	}
	return nil
}

// invalidate voids one preparation. A preparation that already left the confirmable
// states is not an obstacle (that is the outcome wanted). A preparation that does not
// list the reason is voided as goal_changed, which every preparation lists.
func (t *turn) invalidate(ctx context.Context, preparationID string, reason act.PreparedActionInvalidationConditionsItem) error {
	reasons := []act.PreparedActionInvalidationConditionsItem{reason}
	if reason != act.PreparedActionInvalidationConditionsItemGoalChanged {
		reasons = append(reasons, act.PreparedActionInvalidationConditionsItemGoalChanged)
	}
	var err error
	for _, r := range reasons {
		_, err = t.e.actions.Invalidate(ctx, t.scope, preparationID, r)
		if err == nil {
			return nil
		}
		if isCanceled(err) {
			return providers.ContextError(err)
		}
		switch contracts.CodeOf(err) {
		case contracts.ErrConflict, contracts.ErrExpired, contracts.ErrStale:
			return nil
		case contracts.ErrScopeViolation:
			// The id comes from this session's own state under its own scope, so
			// "no such preparation" means retention erased it: it is final too.
			if preparationErased(err) {
				return nil
			}
		case contracts.ErrValidationFailed:
			continue
		}
		break
	}
	t.log("preparation not invalidated", "code", string(contracts.CodeOf(err)), "reason", string(reason))
	return contracts.Wrap(failureCode(err, contracts.ErrUnavailable), err, "dialogue: a preparation could not be voided")
}

// reject declines the preparation a goal references (the user said no, or cancelled)
// and clears the reference. If the coordinator cannot reject it, it is voided
// instead; if that fails too, the error is returned and the reference stays.
func (t *turn) rejectPrepared(ctx context.Context, g *sessions.Goal) error {
	if g == nil || g.PreparedID == "" {
		return nil
	}
	_, err := t.e.actions.Reject(ctx, t.scope, g.PreparedID)
	switch {
	case err == nil:
	case isCanceled(err):
		return providers.ContextError(err)
	default:
		switch contracts.CodeOf(err) {
		case contracts.ErrConflict, contracts.ErrExpired, contracts.ErrStale:
			// already final: dropping the reference is the right outcome
		default:
			if verr := t.invalidate(ctx, g.PreparedID, act.PreparedActionInvalidationConditionsItemGoalChanged); verr != nil {
				return verr
			}
		}
	}
	g.ClearPrepared()
	return nil
}

// voidPrepared invalidates the preparation a goal references, because the goal changed
// under it, and clears the reference. The caller is about to change the goal, so a
// failure leaves it as it was.
func (t *turn) voidPrepared(ctx context.Context, g *sessions.Goal, reason act.PreparedActionInvalidationConditionsItem) error {
	if g == nil || g.PreparedID == "" {
		return nil
	}
	if err := t.invalidate(ctx, g.PreparedID, reason); err != nil {
		return err
	}
	g.ClearPrepared()
	return nil
}

// finished reports whether the goal's capability already produced its result (or
// failed): there is nothing left to continue, only a new request to start.
func finished(g *sessions.Goal) bool {
	return g != nil && (g.Phase == capabilities.PhaseSuccess || g.Phase == capabilities.PhaseFailure)
}

// keepFields are the fields a completed goal's capsule may carry the value of: dates
// and times the application did not mark sensitive. They are what a later "same time
// tomorrow" is resolved from. Every other value is dropped with the goal.
func (t *turn) keepFields(g *sessions.Goal) []string {
	if t.pkg == nil || g == nil {
		return nil
	}
	spec, ok := t.pkg.Spec(g.CapabilityID)
	if !ok {
		return nil
	}
	var out []string
	for _, f := range spec.Fields {
		if f.Sensitive != nil && *f.Sensitive {
			continue
		}
		if f.Type != ap.CapabilityFieldTypeDate && f.Type != ap.CapabilityFieldTypeDatetime {
			continue
		}
		if _, has := g.Slots[string(f.Name)]; has {
			out = append(out, string(f.Name))
		}
	}
	return out
}

// complete ends the active goal into its capsule. A goal that had finished keeps its
// outcome; one that had not needs the outcome the caller names. A nested goal returns
// control to its parent. The preparation the goal still referenced is voided.
func (t *turn) completeGoal(ctx context.Context, outcome sessions.Outcome, receiptID string) error {
	g := t.sess.State.Goal
	if g == nil {
		return nil
	}
	if finished(g) {
		outcome = "" // the phase says how it ended
	}
	_, imp, err := t.sess.State.Complete(t.now, sessions.CompleteOptions{
		Outcome: outcome, TurnID: t.id, ReceiptID: receiptID, KeepFields: t.keepFields(g),
	}, t.e.sessions.Config())
	if err != nil {
		return err
	}
	t.transition(TransitionCompleted)
	if imp.Resumed != "" {
		t.transition(TransitionReturned)
	}
	return t.applyImpact(ctx, imp)
}

// lapsedFor reports whether id is the preparation that lapsed before this turn.
func (t *turn) lapsedFor(id string) (lapsedPrep, bool) {
	return t.lapsed, id != "" && t.lapsed.id == id
}

// preparedExpiry is the preparation's own expiry, or the zero time when it cannot be
// read (the coordinator's expiry is authoritative either way).
func preparedExpiry(pa contracts.PreparedAction) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, string(pa.ExpiresAt))
	if err != nil {
		return time.Time{}
	}
	return ts.UTC()
}

// resultItems are the records a deterministic result presented, in the order it
// presented them, with the label the host gave each (labels pair with identifiers by
// position only when every identifier has one) and the data version of the result.
func resultItems(ids []act.RecordRef, facts []act.Fact, version string) []sessions.ResultItem {
	var labels []string
	for _, f := range facts {
		if string(f.Field) == labelField {
			labels = append(labels, canonicalText(f.Value))
		}
	}
	paired := len(labels) == len(ids)
	items := make([]sessions.ResultItem, 0, len(ids))
	for i, id := range ids {
		it := sessions.ResultItem{EntityType: string(id.EntityType), EntityID: string(id.EntityID), Version: version}
		if paired {
			it.Label = labels[i]
		}
		items = append(items, it)
	}
	return items
}

// recordRead keeps the records a read presented, so that "the second one" and "that
// item" can be resolved from state in a later turn. A result that presented no record
// is not kept: it must not push a list out of the bounded window.
func (t *turn) recordRead(capID string, res act.DeterministicResult) {
	if len(res.Identifiers) == 0 {
		return
	}
	version := ""
	if res.Freshness != nil {
		version = string(res.Freshness.Version)
	}
	t.sess.State.RecordResult(sessions.ResultSummary{
		TurnID: t.id, CapabilityID: capID, Kind: sessions.ResultRead, DataVersion: version,
		Items: resultItems(res.Identifiers, res.Values, version), At: t.now,
	}, t.e.sessions.Config())
}

// recordCommit keeps the records a committed action touched (a created item is "that
// item" afterwards).
func (t *turn) recordCommit(rc contracts.ActionReceipt) {
	if len(rc.Records) == 0 || (rc.Status != act.ActionReceiptStatusCommitted && rc.Status != act.ActionReceiptStatusQueued) {
		return
	}
	t.sess.State.RecordResult(sessions.ResultSummary{
		TurnID: t.id, CapabilityID: string(rc.CapabilityID), Kind: sessions.ResultCommit, ReceiptID: string(rc.ReceiptID),
		Items: resultItems(rc.Records, rc.Facts, ""), At: t.now,
	}, t.e.sessions.Config())
}

// preparationErased reports the coordinator's "no such preparation in this scope".
func preparationErased(err error) bool {
	var ce *contracts.Error
	return errors.As(err, &ce) && ce.Code == contracts.ErrScopeViolation && ce.Details["reason"] == "not_found"
}
