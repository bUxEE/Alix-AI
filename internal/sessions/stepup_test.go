package sessions

import (
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

func TestApplyStepUpReceiptCompletesGoalWithReceipt(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st := State{Goal: &Goal{
		ID: "goal_1", CapabilityID: "items.create", Phase: capabilities.PhasePrepared,
		PreparedID: "prep_1", Slots: map[string]Slot{}, StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	rc := contracts.ActionReceipt{
		ReceiptID: "rcpt_1", PreparationID: "prep_1", CapabilityID: "items.create",
		Status:  act.ActionReceiptStatusCommitted,
		Records: []act.RecordRef{{EntityType: "item", EntityID: "item-1"}},
	}
	changed, err := st.ApplyStepUpReceipt(now, "prep_1", rc, OutcomeSuccess, Config{})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if st.Goal != nil {
		t.Fatalf("goal still active: %+v", st.Goal)
	}
	if len(st.Capsules) != 1 || st.Capsules[0].Outcome != OutcomeSuccess || st.Capsules[0].ReceiptID != "rcpt_1" {
		t.Fatalf("capsule = %+v", st.Capsules)
	}
	if len(st.Results) != 1 || st.Results[0].Kind != ResultCommit || st.Results[0].ReceiptID != "rcpt_1" {
		t.Fatalf("results = %+v", st.Results)
	}
}

func TestApplyStepUpReceiptDeclinedCancelsGoal(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st := State{Goal: &Goal{
		ID: "goal_1", CapabilityID: "items.create", Phase: capabilities.PhasePrepared,
		PreparedID: "prep_1", Slots: map[string]Slot{}, StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	rc := contracts.ActionReceipt{
		ReceiptID: "rcpt_2", PreparationID: "prep_1", CapabilityID: "items.create",
		Status: act.ActionReceiptStatusRejected,
		Errors: []act.Issue{{Code: common.ErrorCodeConflict, Message: "declined"}},
	}
	changed, err := st.ApplyStepUpReceipt(now, "prep_1", rc, OutcomeCancelled, Config{})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if st.Goal != nil || len(st.Results) != 0 {
		t.Fatalf("goal=%+v results=%+v", st.Goal, st.Results)
	}
	if len(st.Capsules) != 1 || st.Capsules[0].Outcome != OutcomeCancelled || st.Capsules[0].ReceiptID != "rcpt_2" {
		t.Fatalf("capsule = %+v", st.Capsules)
	}
}

func TestApplyStepUpReceiptIgnoresUnrelatedGoal(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st := State{Goal: &Goal{
		ID: "goal_1", CapabilityID: "items.create", Phase: capabilities.PhasePrepared,
		PreparedID: "prep_other", Slots: map[string]Slot{}, StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	rc := contracts.ActionReceipt{ReceiptID: "rcpt_1", Status: act.ActionReceiptStatusCommitted}
	changed, err := st.ApplyStepUpReceipt(now, "prep_1", rc, OutcomeSuccess, Config{})
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if st.Goal == nil || st.Goal.PreparedID != "prep_other" {
		t.Fatalf("goal = %+v", st.Goal)
	}
}
