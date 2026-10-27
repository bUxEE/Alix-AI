package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func TestStepUpOutcomeCompletedViaHostStatus(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	body := decodeBody[TurnBody](t, rec)
	evs := checkTurnEvents(t, rawEvents(t, body))
	pa := findEvent(evs, "prepared_action")["prepared_action"].(map[string]any)
	prepID := pa["preparation_id"].(string)
	key := pa["idempotency_key"].(string)
	e.host.ApplyOutside(apiTenant, apiWsp, key, "widget")

	rec = e.call(http.MethodPost, "/v1/sessions/"+apiSess+"/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "completed"})
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "ActionReceipt", rec.Body.Bytes())
	rc := decodeBody[act.ActionReceipt](t, rec)
	if rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("receipt = %+v", rc)
	}
	// Session goal completed with the receipt id (ADR 0048).
	got, err := e.sess.Open(context.Background(), e.scopeOf())
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Goal != nil {
		t.Fatalf("goal still open: %+v", got.State.Goal)
	}
	if len(got.State.Capsules) == 0 || got.State.Capsules[0].ReceiptID != string(rc.ReceiptID) ||
		got.State.Capsules[0].Outcome != sessions.OutcomeSuccess {
		t.Fatalf("capsules = %+v", got.State.Capsules)
	}
	if len(got.State.Results) == 0 || got.State.Results[0].Kind != sessions.ResultCommit {
		t.Fatalf("results = %+v", got.State.Results)
	}
	// Replay.
	rec = e.call(http.MethodPost, "/v1/sessions/"+apiSess+"/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "completed"})
	wantStatus(t, rec, http.StatusOK)

	// A following turn does not re-propose the completed preparation.
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "other", "quantity": 1}))
	rec = e.turn(TurnRequest{Text: "something else", Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	again, err := e.sess.Open(context.Background(), e.scopeOf())
	if err != nil {
		t.Fatal(err)
	}
	if again.State.Goal != nil && again.State.Goal.PreparedID == prepID {
		t.Fatalf("re-proposed the completed preparation: %+v", again.State.Goal)
	}
}

func TestStepUpOutcomeCompletedWithoutEffectRefused(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	prepID := findEvent(checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, rec))), "prepared_action")["prepared_action"].(map[string]any)["preparation_id"].(string)

	wantError(t, e.call(http.MethodPost, "/v1/sessions/"+apiSess+"/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "completed"}),
		http.StatusBadRequest, contracts.ErrValidationFailed)
}

func TestStepUpOutcomeDeclined(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	prepID := findEvent(checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, rec))), "prepared_action")["prepared_action"].(map[string]any)["preparation_id"].(string)

	rec = e.call(http.MethodPost, "/v1/sessions/"+apiSess+"/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "declined"})
	wantStatus(t, rec, http.StatusConflict)
	rc := decodeBody[act.ActionReceipt](t, rec)
	if rc.Status != act.ActionReceiptStatusRejected {
		t.Fatalf("receipt = %+v", rc)
	}
	got, err := e.sess.Open(context.Background(), e.scopeOf())
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Goal != nil {
		t.Fatalf("goal still open: %+v", got.State.Goal)
	}
	if len(got.State.Capsules) == 0 || got.State.Capsules[0].Outcome != sessions.OutcomeCancelled ||
		got.State.Capsules[0].ReceiptID != string(rc.ReceiptID) {
		t.Fatalf("capsules = %+v", got.State.Capsules)
	}
}

func TestStepUpOutcomeDeclinedWhenEffectPresent(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	pa := findEvent(checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, rec))), "prepared_action")["prepared_action"].(map[string]any)
	prepID := pa["preparation_id"].(string)
	e.host.ApplyOutside(apiTenant, apiWsp, pa["idempotency_key"].(string), "widget")

	wantError(t, e.call(http.MethodPost, "/v1/sessions/"+apiSess+"/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "declined"}),
		http.StatusConflict, contracts.ErrConflict)
	got, err := e.sess.Open(context.Background(), e.scopeOf())
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Goal == nil || got.State.Goal.PreparedID != prepID {
		t.Fatalf("goal should still wait on the preparation: %+v", got.State.Goal)
	}
}

func TestStepUpOutcomeOtherSessionForbidden(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	prepID := findEvent(checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, rec))), "prepared_action")["prepared_action"].(map[string]any)["preparation_id"].(string)

	wantError(t, e.call(http.MethodPost, "/v1/sessions/other-ses/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "declined"}),
		http.StatusForbidden, contracts.ErrScopeViolation)
}

func TestStepUpOutcomeSaveFailureStillReturnsReceipt(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	pa := findEvent(checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, rec))), "prepared_action")["prepared_action"].(map[string]any)
	prepID := pa["preparation_id"].(string)
	e.host.ApplyOutside(apiTenant, apiWsp, pa["idempotency_key"].(string), "widget")

	prev := applyStepUpReceiptFn
	t.Cleanup(func() { applyStepUpReceiptFn = prev })
	applyStepUpReceiptFn = func(context.Context, *sessions.Store, contracts.Scope, string, act.ActionReceipt, sessions.Outcome) error {
		return contracts.New(contracts.ErrConflict, "session version conflict")
	}

	rec = e.call(http.MethodPost, "/v1/sessions/"+apiSess+"/preparations/"+prepID+"/step-up-outcome",
		StepUpOutcomeRequest{Outcome: "completed"})
	wantStatus(t, rec, http.StatusOK)
	rc := decodeBody[act.ActionReceipt](t, rec)
	if rc.ReceiptID == "" || rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("receipt must still be returned: %+v", rc)
	}
}
