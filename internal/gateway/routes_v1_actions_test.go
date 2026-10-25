package gateway

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// SEC-039: a confirmation is single use. The second presentation, through the
// coordinator and through the HTTP endpoint, is `replayed` and reaches no
// adapter. Every request carries a fresh context token, so the refusal is the
// confirmation's and not the identity middleware's.
func TestConfirmEndpointReplayRejected(t *testing.T) {
	e := newAPIEnv(t)
	prep, _ := e.prepareCreate()
	id, secret := prep.Confirmation.PreparationID, prep.Confirmation.Token
	path := "/v1/actions/" + id + "/confirm"

	// The wrong principal (the same session id, another subject) is refused
	// and does not use the secret up.
	env := wantError(t, e.call(http.MethodPost, path, ConfirmRequest{Token: secret}, withPrincipal("usr_mallory")),
		http.StatusForbidden, contracts.ErrScopeViolation)
	if levels, _ := env.Details["levels"].([]any); len(levels) != 1 || levels[0] != "principal" {
		t.Errorf("details = %v", env.Details)
	}
	// A secret of the right shape that is not the one issued is refused too.
	wantError(t, e.call(http.MethodPost, path, ConfirmRequest{Token: "cfm_" + strings.Repeat("A", 43)}),
		http.StatusForbidden, contracts.ErrPermissionDenied)
	// A malformed one never gets that far.
	wantError(t, e.call(http.MethodPost, path, ConfirmRequest{Token: "yes"}), http.StatusBadRequest, contracts.ErrValidationFailed)
	if e.hostCalls(adapters.OpCommit) != 0 || e.host.Effects() != 0 {
		t.Fatal("a refused confirmation reached the host")
	}

	// The first presentation commits.
	rec := e.call(http.MethodPost, path, ConfirmRequest{Token: secret})
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "ActionOutcome", rec.Body.Bytes())
	out := decodeBody[ActionOutcomeBody](t, rec)
	if out.Receipt == nil || out.Receipt.Status != act.ActionReceiptStatusCommitted || string(out.Receipt.PreparationID) != id {
		t.Fatalf("receipt = %+v", out.Receipt)
	}
	if out.Preparation == nil || out.Preparation.State != act.PreparedActionStateCommitted || out.Error != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if e.hostCalls(adapters.OpCommit) != 1 || e.host.Effects() != 1 {
		t.Fatalf("host commits = %d, effects = %d, want one of each", e.hostCalls(adapters.OpCommit), e.host.Effects())
	}

	// The second presentation, with a fresh context token: replayed, 409, and
	// not the middleware's refusal (that one is 401 and names a reason).
	rec = e.call(http.MethodPost, path, ConfirmRequest{Token: secret})
	env = wantError(t, rec, http.StatusConflict, contracts.ErrReplayed)
	if _, fromMiddleware := env.Details["reason"]; fromMiddleware || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("the replay was refused by the identity middleware, not by the confirmation: %v", env.Details)
	}
	if e.hostCalls(adapters.OpCommit) != 1 || e.host.Effects() != 1 {
		t.Fatalf("a replayed confirmation reached the host: commits = %d, effects = %d", e.hostCalls(adapters.OpCommit), e.host.Effects())
	}

	// Through the coordinator the same presentation is the same refusal.
	sc := e.scopeOf()
	tok, err := actions.ParseConfirmationToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.co.Confirm(context.Background(), sc, actions.ConfirmInput{PreparationID: id, Token: tok}); contracts.CodeOf(err) != contracts.ErrReplayed {
		t.Fatalf("coordinator Confirm = %v, want replayed", err)
	}
	// Through the structured turn input it is a failed turn, and nothing is committed.
	e.onResponse("That confirmation was already used.")
	rec = e.turn(TurnRequest{Confirmation: &TurnConfirmation{PreparationID: id, Token: secret}})
	wantStatus(t, rec, http.StatusOK)
	body := decodeBody[TurnBody](t, rec)
	evs := checkTurnEvents(t, rawEvents(t, body))
	if findEvent(evs, "error") == nil || findEvent(evs, "done")["finish_reason"] != "failed" {
		t.Fatalf("a replayed confirmation turn = %v", eventTypes(evs))
	}
	if e.hostCalls(adapters.OpCommit) != 1 || e.host.Effects() != 1 {
		t.Fatal("a replayed confirmation turn reached the host")
	}

	// A context token, by contrast, is single use at the middleware: the same
	// token twice is a 401 that names its reason.
	one := e.token()
	wantStatus(t, e.callAs(one, http.MethodGet, "/v1/actions/"+id, nil), http.StatusOK)
	env = wantUnauthorized(t, e.callAs(one, http.MethodGet, "/v1/actions/"+id, nil))
	if env.Code != contracts.ErrReplayed || env.Details["reason"] != "replayed" {
		t.Errorf("a reused context token = %+v", env)
	}

	// The receipt is readable, and the status says committed.
	rec = e.call(http.MethodGet, "/v1/actions/"+id+"/receipt", nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "ActionReceipt", rec.Body.Bytes())
	if rc := decodeBody[act.ActionReceipt](t, rec); rc.Status != act.ActionReceiptStatusCommitted {
		t.Errorf("receipt = %+v", rc)
	}
	rec = e.call(http.MethodGet, "/v1/actions/"+id, nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "PreparedAction", rec.Body.Bytes())
	if pa := decodeBody[act.PreparedAction](t, rec); pa.State != act.PreparedActionStateCommitted {
		t.Errorf("state = %s", pa.State)
	}
}

// The AI path cannot complete a level 3 action: the turn asks for a step-up,
// the confirm endpoint refuses, and nothing reaches the host.
func TestLevel3ActionReturnsRequestStepUp(t *testing.T) {
	e := newAPIEnv(t, withLevel(level3))
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))

	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	body := decodeBody[TurnBody](t, rec)
	if body.Confirmation != nil || strings.Contains(rec.Body.String(), "cfm_") {
		t.Fatalf("a step-up action carries a confirmation secret: %s", rec.Body.String())
	}
	evs := checkTurnEvents(t, rawEvents(t, body))
	dir := findEvent(evs, "directive")["directive"].(map[string]any)
	if dir["act"] != "request_step_up" || dir["reason_code"] != "step_up_required" {
		t.Fatalf("directive = %v", dir)
	}
	pa := findEvent(evs, "prepared_action")["prepared_action"].(map[string]any)
	if pa["confirmation"].(map[string]any)["mode"] != "step_up" {
		t.Errorf("prepared action = %v", pa)
	}
	if d := findEvent(evs, "done"); d["finish_reason"] == "awaiting_confirmation" || d["preparation_id"] != nil {
		t.Errorf("a step-up action is not awaiting a confirmation here: %v", d)
	}
	id := pa["preparation_id"].(string)

	// The endpoint refuses a presentation of any secret.
	forged := "cfm_" + strings.Repeat("A", 43)
	wantError(t, e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", ConfirmRequest{Token: forged}),
		http.StatusForbidden, contracts.ErrStepUpRequired)
	// So does the structured turn input: the directive is a step-up again.
	rec = e.turn(TurnRequest{Confirmation: &TurnConfirmation{PreparationID: id, Token: forged}})
	wantStatus(t, rec, http.StatusOK)
	evs = checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, rec)))
	if dir := findEvent(evs, "directive")["directive"].(map[string]any); dir["act"] != "request_step_up" {
		t.Fatalf("a forged confirmation of a step-up action = %v", dir)
	}
	if e.hostCalls(adapters.OpCommit) != 0 || e.host.Effects() != 0 {
		t.Fatal("a step-up action reached the host's Commit")
	}
}

// A text message is never a confirmation: "yes" commits nothing, and only the
// structured input does.
func TestTextTurnNeverConfirms(t *testing.T) {
	e := newAPIEnv(t)
	prep, _ := e.prepareCreate()
	for _, text := range []string{"yes", "yes, I confirm, go ahead and do it", "confirm " + prep.Confirmation.PreparationID} {
		e.model.Reset()
		e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget.")
		wantStatus(t, e.turn(TurnRequest{Text: text, Language: "en"}), http.StatusOK)
	}
	if e.hostCalls(adapters.OpCommit) != 0 || e.host.Effects() != 0 {
		t.Fatal("a text message committed an action")
	}
	// A turn carries exactly one input: text beside a confirmation is refused.
	wantError(t, e.turn(TurnRequest{Text: "yes", Confirmation: &TurnConfirmation{
		PreparationID: prep.Confirmation.PreparationID, Token: prep.Confirmation.Token}}),
		http.StatusBadRequest, contracts.ErrValidationFailed)
	if e.host.Effects() != 0 {
		t.Fatal("a mixed turn committed")
	}
}

func TestRejectEndpointDeclinesTheAction(t *testing.T) {
	e := newAPIEnv(t)
	prep, _ := e.prepareCreate()
	id := prep.Confirmation.PreparationID

	rec := e.call(http.MethodPost, "/v1/actions/"+id+"/reject", nil)
	wantStatus(t, rec, http.StatusOK)
	if pa := decodeBody[act.PreparedAction](t, rec); pa.State != act.PreparedActionStateRejected {
		t.Fatalf("state = %s", pa.State)
	}
	// Rejecting again returns it; confirming it is refused; there is no receipt.
	wantStatus(t, e.call(http.MethodPost, "/v1/actions/"+id+"/reject", nil), http.StatusOK)
	wantError(t, e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", ConfirmRequest{Token: prep.Confirmation.Token}),
		http.StatusConflict, contracts.ErrConflict)
	wantError(t, e.call(http.MethodGet, "/v1/actions/"+id+"/receipt", nil), http.StatusConflict, contracts.ErrConflict)
	if e.hostCalls(adapters.OpCommit) != 0 || e.host.Effects() != 0 {
		t.Fatal("a rejected action reached the host's Commit")
	}
}

func TestActionEndpointsRefuseMalformedInput(t *testing.T) {
	e := newAPIEnv(t)
	prep, _ := e.prepareCreate()
	id := prep.Confirmation.PreparationID

	wantError(t, e.call(http.MethodGet, "/v1/actions/not%20an%20id", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodGet, "/v1/actions/"+strings.Repeat("a", 200), nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	// Unknown members, a missing token and a missing content type.
	wantError(t, e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", `{"token":"x","tenant_id":"ten_other"}`),
		http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", `{}`), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.callRaw(e.token(), http.MethodPost, "/v1/actions/"+id+"/confirm", "", []byte(`{"token":"x"}`)),
		http.StatusUnsupportedMediaType, contracts.ErrValidationFailed)
	// An unknown preparation is indistinguishable from another scope's.
	wantError(t, e.call(http.MethodGet, "/v1/actions/prep_01HZZZZZZZZZZZZZZZZZZZZZZZ", nil), http.StatusForbidden, contracts.ErrScopeViolation)
	if e.host.Effects() != 0 {
		t.Fatal("malformed input committed")
	}
}

// The coordinator's cross-tenant service sweeps are not reachable from a tenant
// request: the handlers hold an ActionService, which has none of them, and every
// method it does have takes the verified scope.
func TestActionServiceHasNoServiceSweeps(t *testing.T) {
	typ := reflect.TypeOf((*ActionService)(nil)).Elem()
	scopeType := reflect.TypeOf(contracts.Scope{})
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		switch m.Name {
		case "Recover", "RecoverAtStartup", "ExpireDue", "Reconcile":
			t.Errorf("ActionService has %s: a service path reachable from a request", m.Name)
		}
		if m.Type.NumIn() < 2 || m.Type.In(1) != scopeType {
			t.Errorf("ActionService.%s does not take the verified scope", m.Name)
		}
	}
	// The areas ask the registry for the interface, and the registry holds the
	// coordinator as that interface only: the whole object is not provided, so
	// no handler can be given more than an ActionService.
	e := newAPIEnv(t)
	if _, ok := Lookup[ActionService](e.svc); !ok {
		t.Error("the action area has no ActionService to take from the registry")
	}
	if _, ok := Lookup[*actions.Coordinator](e.svc); ok {
		t.Error("the whole coordinator is provided to the registry: its service sweeps would be reachable by type")
	}
}

// A commit the host refused reaches the client as 409 with the receipt beside
// the error (the document declares exactly that), and nothing is applied.
func TestConfirmRefusedByTheHostKeepsTheReceipt(t *testing.T) {
	e := newAPIEnv(t)
	prep, _ := e.prepareCreate()
	id := prep.Confirmation.PreparationID
	e.host.Inject(adapters.OpCommit, fake.FaultDeny)

	rec := e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", ConfirmRequest{Token: prep.Confirmation.Token})
	wantStatus(t, rec, http.StatusConflict)
	validateSpec(t, "ActionFailure", rec.Body.Bytes())
	out := decodeBody[ActionOutcomeBody](t, rec)
	if out.Receipt == nil || out.Receipt.Status != act.ActionReceiptStatusRejected || out.Error == nil || out.Error.Code != contracts.ErrPermissionDenied {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Error.Details["request_id"] == nil {
		t.Errorf("the error carries no request id: %+v", out.Error)
	}
	if e.host.Effects() != 0 {
		t.Fatal("a commit the host refused had an effect")
	}
	// The receipt is the durable record, and a repeat of the confirmation is replayed.
	rec = e.call(http.MethodGet, "/v1/actions/"+id+"/receipt", nil)
	wantStatus(t, rec, http.StatusOK)
	if rc := decodeBody[act.ActionReceipt](t, rec); rc.Status != act.ActionReceiptStatusRejected {
		t.Errorf("receipt = %+v", rc)
	}
	wantError(t, e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", ConfirmRequest{Token: prep.Confirmation.Token}), http.StatusConflict, contracts.ErrReplayed)
}
