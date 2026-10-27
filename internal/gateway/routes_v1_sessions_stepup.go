package gateway

import (
	"context"
	"net/http"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// StepUpOutcomeRequest is the body of
// POST /v1/sessions/{session_id}/preparations/{preparation_id}/step-up-outcome (ADR 0048).
type StepUpOutcomeRequest struct {
	Outcome   string `json:"outcome"`
	Reference string `json:"reference,omitempty"`
}

func (h *sessionsHandler) stepUpOutcome(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	if !sessionPath(w, r, scope) {
		return
	}
	prepID := r.PathValue("preparation_id")
	if !validPathID(prepID) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the preparation id is malformed"))
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	var in StepUpOutcomeRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	extendWriteDeadline(w)
	hostOutcome := actions.StepUpOutcome(in.Outcome)
	rc, err := h.actions.RecordStepUpOutcome(r.Context(), scope, actions.StepUpOutcomeInput{
		PreparationID: prepID,
		Outcome:       hostOutcome,
		Reference:     in.Reference,
	})
	if err != nil && rc.ReceiptID == "" {
		if contracts.CodeOf(err) == contracts.ErrExpired {
			WriteErrorStatus(w, r, http.StatusGone, err)
			return
		}
		WriteError(w, r, err)
		return
	}
	h.applyStepUpGoal(r, scope, prepID, rc, hostOutcome)
	if err != nil || rc.Status == act.ActionReceiptStatusRejected {
		writeJSON(w, http.StatusConflict, rc)
		return
	}
	writeJSON(w, http.StatusOK, rc)
}

// applyStepUpReceiptFn applies a step-up receipt to the session goal. Tests may
// override it to simulate a save failure while the receipt still stands.
var applyStepUpReceiptFn = func(ctx context.Context, store *sessions.Store, scope contracts.Scope, prepID string, rc act.ActionReceipt, outcome sessions.Outcome) error {
	if store == nil {
		return nil
	}
	return store.ApplyStepUpReceipt(ctx, scope, prepID, rc, outcome)
}

// applyStepUpGoal completes (or cancels) the session goal that still references
// the preparation, the same way an in-turn commit does. When the goal no longer
// references it, the receipt still stands and state is left alone. Delivery to
// an open turn stream needs a turn id the hub Lookup requires; without one the
// next turn and GET …/receipt see the outcome (ADR 0048).
func (h *sessionsHandler) applyStepUpGoal(r *http.Request, scope contracts.Scope, prepID string, rc act.ActionReceipt, host actions.StepUpOutcome) {
	if rc.ReceiptID == "" {
		return
	}
	if err := applyStepUpReceiptFn(r.Context(), h.sessions, scope, prepID, rc, stepUpSessionOutcome(host, rc)); err != nil {
		loggerFrom(r.Context()).Warn("step-up outcome recorded but session goal update failed",
			"preparation_id", prepID,
			"receipt_id", string(rc.ReceiptID),
			"session_id", scope.SessionID,
			"err", err.Error(),
		)
	}
}

func stepUpSessionOutcome(host actions.StepUpOutcome, rc act.ActionReceipt) sessions.Outcome {
	switch host {
	case actions.StepUpOutcomeDeclined:
		return sessions.OutcomeCancelled
	case actions.StepUpOutcomeFailed:
		return sessions.OutcomeFailure
	default: // completed
		switch rc.Status {
		case act.ActionReceiptStatusCommitted, act.ActionReceiptStatusQueued:
			return sessions.OutcomeSuccess
		case act.ActionReceiptStatusPartial:
			return sessions.OutcomeFailure
		default:
			return sessions.OutcomeFailure
		}
	}
}
