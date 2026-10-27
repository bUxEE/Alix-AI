package actions

import (
	"context"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// How long after expires_at a host may still report a step-up outcome (ADR 0048).
const stepUpOutcomeGrace = 24 * time.Hour

// StepUpOutcome is what the host backend reports after the user finished (or
// abandoned) a level-3 step-up in the application's own authenticated UI.
type StepUpOutcome string

// The accepted outcomes of POST …/step-up-outcome.
const (
	StepUpOutcomeCompleted StepUpOutcome = "completed"
	StepUpOutcomeDeclined  StepUpOutcome = "declined"
	StepUpOutcomeFailed    StepUpOutcome = "failed"
)

// Valid reports whether o is one of the accepted outcomes.
func (o StepUpOutcome) Valid() bool {
	switch o {
	case StepUpOutcomeCompleted, StepUpOutcomeDeclined, StepUpOutcomeFailed:
		return true
	}
	return false
}

// StepUpOutcomeInput is the body of RecordStepUpOutcome.
type StepUpOutcomeInput struct {
	PreparationID string
	Outcome       StepUpOutcome
	// Reference is an optional host-side correlation id (no secrets).
	Reference string
}

// RecordStepUpOutcome records what became of a level-3 preparation after the
// host's own authenticated interface finished the step-up (ADR 0048).
//
// The AI Confirm/Commit path still cannot complete mode step_up. Every outcome
// asks the adapter's Status for the preparation's idempotency key: completed is
// accepted only when Status shows the effect; declined / failed are refused
// with reason step_up_effect_present when Status shows the effect (the host
// must report completed), and otherwise write a rejected receipt. Replays of
// the same outcome return the existing receipt.
func (c *Coordinator) RecordStepUpOutcome(ctx context.Context, scope contracts.Scope, in StepUpOutcomeInput) (contracts.ActionReceipt, error) {
	var none contracts.ActionReceipt
	if !in.Outcome.Valid() {
		return none, contracts.New(contracts.ErrValidationFailed, "outcome must be completed, declined or failed")
	}
	if len(strings.TrimSpace(in.Reference)) > 128 {
		return none, contracts.New(contracts.ErrValidationFailed, "reference is too long")
	}
	r, err := c.loadOwned(ctx, scope, in.PreparationID)
	if err != nil {
		return none, err
	}
	if r.mode != act.ConfirmationRequirementModeStepUp {
		return none, contracts.New(contracts.ErrValidationFailed, "only a step_up preparation accepts a step-up outcome").
			WithDetails("reason", "not_step_up")
	}

	if r.receiptID != "" {
		rc, err := c.loadReceipt(ctx, c.db, scope, r)
		if err != nil {
			return none, err
		}
		if stepUpReceiptMatches(rc, in.Outcome) {
			return rc, receiptError(rc)
		}
		return none, contracts.New(contracts.ErrConflict, "the preparation already has a different outcome")
	}

	switch r.state {
	case act.PreparedActionStatePrepared:
	default:
		return none, errForState(r)
	}

	now := c.clk.Now()
	if now.After(r.expiresAt.Add(stepUpOutcomeGrace)) {
		_ = c.endPreparation(ctx, scope, r, act.PreparedActionStateExpired, audit.EventActionExpired, "expired_at_step_up_outcome")
		return none, errExpired()
	}

	switch in.Outcome {
	case StepUpOutcomeCompleted:
		return c.stepUpCompleted(ctx, scope, r)
	case StepUpOutcomeDeclined:
		return c.stepUpRejected(ctx, scope, r, "host_step_up_declined",
			"the user declined the step-up in the application's own authenticated interface")
	default:
		return c.stepUpRejected(ctx, scope, r, "host_step_up_failed",
			"the step-up failed in the application's own authenticated interface")
	}
}

func stepUpReceiptMatches(rc contracts.ActionReceipt, o StepUpOutcome) bool {
	switch o {
	case StepUpOutcomeCompleted:
		return rc.Status == act.ActionReceiptStatusCommitted ||
			rc.Status == act.ActionReceiptStatusQueued ||
			rc.Status == act.ActionReceiptStatusPartial
	case StepUpOutcomeDeclined, StepUpOutcomeFailed:
		return rc.Status == act.ActionReceiptStatusRejected
	}
	return false
}

func (c *Coordinator) stepUpCompleted(ctx context.Context, scope contracts.Scope, r row) (contracts.ActionReceipt, error) {
	var none contracts.ActionReceipt
	adapter, err := c.adapters.Adapter(ctx, r.binding.ApplicationID)
	if err != nil {
		return none, err
	}
	v, res := c.askStatus(ctx, scope, adapter, r)
	switch v {
	case statusRecorded:
		rc, err := c.persistOutcome(ctx, scope, r, act.PreparedActionStatePrepared,
			recordedOutcome(res, "host_step_up_completed"))
		if err != nil {
			return none, c.transitionError(err)
		}
		return rc, receiptError(rc)
	case statusNoRecord:
		return none, contracts.New(contracts.ErrValidationFailed,
			"Status shows no effect for this preparation; completed was refused").
			WithDetails("reason", "step_up_effect_missing")
	default:
		return none, contracts.New(contracts.ErrUnavailable,
			"Status could not confirm the step-up effect; completed was refused").
			WithDetails("reason", "step_up_status_undecided")
	}
}

func (c *Coordinator) stepUpRejected(ctx context.Context, scope contracts.Scope, r row, reason, message string) (contracts.ActionReceipt, error) {
	var none contracts.ActionReceipt
	adapter, err := c.adapters.Adapter(ctx, r.binding.ApplicationID)
	if err != nil {
		return none, err
	}
	if v, _ := c.askStatus(ctx, scope, adapter, r); v == statusRecorded {
		return none, contracts.New(contracts.ErrConflict,
			"Status shows the effect for this preparation; declined/failed was refused — report completed").
			WithDetails("reason", "step_up_effect_present")
	}
	o := outcome{
		status: act.ActionReceiptStatusRejected,
		to:     act.PreparedActionStateRejected,
		event:  audit.EventActionRejected,
		reason: reason,
		issues: []act.Issue{{
			Code:    common.ErrorCodeConflict,
			Message: message,
		}},
	}
	rc, err := c.persistOutcome(ctx, scope, r, act.PreparedActionStatePrepared, o)
	if err != nil {
		return none, c.transitionError(err)
	}
	return rc, receiptError(rc)
}
