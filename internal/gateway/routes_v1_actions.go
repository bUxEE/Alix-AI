package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The actions part of the public API (master plan 14.2 "Actions"):
//
//	GET  /v1/actions/{preparation_id}           inspect a preparation and its state
//	POST /v1/actions/{preparation_id}/confirm   present the confirmation; commit the action
//	POST /v1/actions/{preparation_id}/reject    decline the preparation
//	GET  /v1/actions/{preparation_id}/receipt   the receipt of the outcome
//
// Preparing an action is not an endpoint: an action is prepared by a turn
// (the dialogue engine proposes, the host validates, the coordinator stores),
// and the client learns of it from the turn's events. The confirmation secret
// that comes with it is presented here, or as a structured turn input.
//
// These routes reach the host only through actions.Coordinator, the one path
// to an adapter's Commit. The coordinator finds a preparation through the
// verified scope (a preparation of another tenant, workspace or application is
// indistinguishable from one that does not exist: scope_violation), requires
// the same principal and session the preparation was made under, uses a
// confirmation exactly once and never lets a level 3 action be confirmed here
// (step_up_required). The second presentation of a confirmation is "replayed",
// answered 409 (the context token that carried it was valid; the confirmation
// was spent) and reaches no adapter.
//
// Confirming and committing are two coordinator steps and one request. If the
// commit is then refused before anything was sent (the host is unavailable and
// proved it applied nothing) the preparation stays confirmed and the secret is
// spent; the user starts again from a new turn, or declines it.

type actionsHandler struct {
	actions ActionService
	apps    ApplicationService
}

// ActionService is the part of the action coordinator a tenant request may
// reach: the operations that take the verified scope. The service sweeps
// (Recover, RecoverAtStartup, ExpireDue) work across tenants and are not here,
// so no handler can call them; the composition root runs them as service jobs.
// *actions.Coordinator implements it.
type ActionService interface {
	Get(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.PreparedAction, error)
	Confirm(ctx context.Context, scope contracts.Scope, in actions.ConfirmInput) (contracts.PreparedAction, error)
	Commit(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error)
	Reject(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.PreparedAction, error)
	Receipt(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error)
	// Invalidate is what ending a session needs (sessions.Invalidator).
	Invalidate(ctx context.Context, scope contracts.Scope, preparationID string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error)
	// RecordStepUpOutcome is the host-backend report after a level-3 step-up (ADR 0048).
	RecordStepUpOutcome(ctx context.Context, scope contracts.Scope, in actions.StepUpOutcomeInput) (contracts.ActionReceipt, error)
}

var _ ActionService = (*actions.Coordinator)(nil)

func init() { RegisterArea("v1_actions", registerV1Actions) }

// registerV1Actions needs the action service (as ActionService) and the
// application service. Providing the coordinator under its concrete type does
// not satisfy it: only what is provided as ActionService is reachable from a
// request, which is how the cross-tenant sweeps stay out of it.
func registerV1Actions(rt *Router, d *Deps) {
	n := d.Needs()
	h := &actionsHandler{
		actions: Need[ActionService](n),
		apps:    Need[ApplicationService](n),
	}
	if !n.Ready() {
		return
	}
	v1 := rt.V1()
	v1.HandleFunc("GET /actions/{preparation_id}", h.status)
	v1.HandleFunc("POST /actions/{preparation_id}/confirm", h.confirm)
	v1.HandleFunc("POST /actions/{preparation_id}/reject", h.reject)
	v1.HandleFunc("GET /actions/{preparation_id}/receipt", h.receipt)
}

// begin is the common start of an action route: the verified scope, an active
// application and a well-formed preparation id.
func (h *actionsHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, string, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return scope, "", false
	}
	id := r.PathValue("preparation_id")
	if !validPathID(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the preparation id is malformed"))
		return scope, "", false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return scope, "", false
	}
	return scope, id, true
}

func (h *actionsHandler) status(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	pa, err := h.actions.Get(r.Context(), scope, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pa)
}

func (h *actionsHandler) reject(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	pa, err := h.actions.Reject(r.Context(), scope, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pa)
}

func (h *actionsHandler) receipt(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	rc, err := h.actions.Receipt(r.Context(), scope, id)
	if err != nil {
		WriteError(w, r, err) // conflict while no outcome exists yet
		return
	}
	writeJSON(w, http.StatusOK, rc)
}

// writeDeadline is how long a turn or a confirmation may take to answer. The
// gateway's write_timeout is sized for one model call; a turn makes two and may
// wait on the host, and a commit waits on the host and then settles. Hanging up
// on a commit that went through would lose its receipt.
const writeDeadline = 10 * time.Minute

// extendWriteDeadline gives the response of one request writeDeadline instead
// of the server-wide write timeout. A writer that cannot (a test recorder) keeps
// whatever it has.
func extendWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeDeadline))
}

// ConfirmRequest is the body of POST /v1/actions/{preparation_id}/confirm.
type ConfirmRequest struct {
	// Token is the single-use confirmation secret that came with the prepared
	// action. It is bound to that preparation, the principal, the session and
	// the data versions the preparation was made against.
	Token string `json:"token"`
}

// ActionOutcomeBody is the body of a confirmation that reached the host: the
// preparation as it stands and the receipt of the outcome. When the outcome is
// not a success (rejected, partial, uncertain) the HTTP status says so, Error
// says why, and the receipt is still here: it is the durable record of what
// happened.
type ActionOutcomeBody struct {
	Preparation *contracts.PreparedAction `json:"preparation,omitempty"`
	Receipt     *contracts.ActionReceipt  `json:"receipt,omitempty"`
	Error       *contracts.Error          `json:"error,omitempty"`
}

func (h *actionsHandler) confirm(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	var req ConfirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	extendWriteDeadline(w)
	tok, err := actions.ParseConfirmationToken(req.Token)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	if _, err := h.actions.Confirm(ctx, scope, actions.ConfirmInput{PreparationID: id, Token: tok}); err != nil {
		if contracts.CodeOf(err) == contracts.ErrReplayed {
			WriteErrorStatus(w, r, http.StatusConflict, err)
			return
		}
		WriteError(w, r, err)
		return
	}
	rc, cerr := h.actions.Commit(ctx, scope, id)
	if rc.ReceiptID == "" {
		// Refused before anything was sent: no receipt, an error like any other.
		if cerr == nil {
			cerr = contracts.New(contracts.ErrInternal, "the commit produced no receipt")
		}
		WriteError(w, r, cerr)
		return
	}
	body := ActionOutcomeBody{Receipt: &rc}
	if pa, err := h.actions.Get(ctx, scope, id); err == nil {
		body.Preparation = &pa
	}
	if cerr != nil {
		// The coordinator returns the receipt with a typed error when the
		// outcome is not a success, so that a caller that only looks at the
		// status cannot read it as one.
		p := problemFor(cerr)
		if id := RequestID(ctx); id != "" {
			if p.body.Details == nil {
				p.body.Details = map[string]any{}
			}
			p.body.Details["request_id"] = id
		}
		body.Error = &p.body
		// Two statuses only, whatever code the host's refusal carried: 409 when
		// the host refused the commit (nothing was applied), 500 when the
		// outcome is partial or unknown. The document declares exactly these.
		status := http.StatusInternalServerError
		if rc.Status == act.ActionReceiptStatusRejected {
			status = http.StatusConflict
		}
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}
