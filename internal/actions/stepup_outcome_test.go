package actions

import (
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func TestStepUpOutcomeCompletedWhenStatusConfirms(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")
	e.app.ApplyOutside(tenantA, ws1, string(p.Action.IdempotencyKey), "widget")

	rc, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeCompleted,
	})
	noErr(t, err)
	if rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("receipt = %+v", rc)
	}
	if e.state(id(p)) != act.PreparedActionStateCommitted {
		t.Fatalf("state = %s", e.state(id(p)))
	}
	// Replay is idempotent.
	rc2, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeCompleted,
	})
	noErr(t, err)
	if rc2.ReceiptID != rc.ReceiptID {
		t.Fatalf("replay receipt %s != %s", rc2.ReceiptID, rc.ReceiptID)
	}
}

func TestStepUpOutcomeCompletedRefusedWhenStatusShowsNoEffect(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")

	_, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeCompleted,
	})
	wantCode(t, err, contracts.ErrValidationFailed)
	if ce, ok := err.(*contracts.Error); !ok || ce.Details["reason"] != "step_up_effect_missing" {
		t.Fatalf("err = %v", err)
	}
	if e.state(id(p)) != act.PreparedActionStatePrepared {
		t.Fatalf("state moved to %s", e.state(id(p)))
	}
}

func TestStepUpOutcomeDeclinedWritesRejectedReceipt(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")

	rc, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeDeclined,
	})
	if contracts.CodeOf(err) == "" {
		t.Fatal("declined should return a typed error with the receipt")
	}
	if rc.Status != act.ActionReceiptStatusRejected || rc.ReceiptID == "" {
		t.Fatalf("receipt = %+v", rc)
	}
	if e.state(id(p)) != act.PreparedActionStateRejected {
		t.Fatalf("state = %s", e.state(id(p)))
	}
}

func TestStepUpOutcomeDeclinedRefusedWhenStatusShowsEffect(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")
	e.app.ApplyOutside(tenantA, ws1, string(p.Action.IdempotencyKey), "widget")

	rc, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeDeclined,
	})
	wantCode(t, err, contracts.ErrConflict)
	if ce, ok := err.(*contracts.Error); !ok || ce.Details["reason"] != "step_up_effect_present" {
		t.Fatalf("err = %v", err)
	}
	if rc.ReceiptID != "" {
		t.Fatalf("must not record a receipt: %+v", rc)
	}
	if e.state(id(p)) != act.PreparedActionStatePrepared {
		t.Fatalf("state moved to %s", e.state(id(p)))
	}
}

func TestStepUpOutcomeFailedAllowedWhenStatusUndecided(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")
	// No ApplyOutside → Status has no record; failed may record a rejected receipt.

	rc, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeFailed,
	})
	if contracts.CodeOf(err) == "" {
		t.Fatal("failed should return a typed error with the receipt")
	}
	if rc.Status != act.ActionReceiptStatusRejected || rc.ReceiptID == "" {
		t.Fatalf("receipt = %+v", rc)
	}
}

func TestStepUpOutcomeOtherPrincipalRefused(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")
	other := scopeOf("user-2", "ses-1")

	_, err := e.co.RecordStepUpOutcome(ctxb(), other, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeDeclined,
	})
	wantCode(t, err, contracts.ErrScopeViolation)
}

func TestStepUpOutcomeNonStepUpRefused(t *testing.T) {
	e := newEnv(t) // level 2 → explicit
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")

	_, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeDeclined,
	})
	wantCode(t, err, contracts.ErrValidationFailed)
}

func TestStepUpOutcomePastGraceExpired(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "widget")
	e.clk.Set(e.clk.Now().Add(stepUpOutcomeGrace + 2*time.Hour))

	_, err := e.co.RecordStepUpOutcome(ctxb(), sc, StepUpOutcomeInput{
		PreparationID: id(p), Outcome: StepUpOutcomeDeclined,
	})
	wantCode(t, err, contracts.ErrExpired)
}
