package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Per-principal LLM account (ADR 0041 / 0043). Contract: schemas/api/paths/me.yaml.
//
//	GET    /v1/providers/catalog
//	GET    /v1/me/llm
//	PUT    /v1/me/llm/{role}
//	DELETE /v1/me/llm/{role}
//	PUT    /v1/me/providers/{provider}/credential
//	DELETE /v1/me/providers/{provider}/credential
//	POST   /v1/me/providers/{provider}/billing-notice/ack
//	POST   /v1/me/oauth/{provider}/start
//	POST   /v1/me/oauth/callback
//	GET    /v1/me/oauth/flows/{flow_id}
//	DELETE /v1/me/oauth/flows/{flow_id}
//
// Self-service only: the verified token's principal. Needs ApplicationService; Wants
// PrincipalLLMService (unwired → unavailable). Never returns a secret.

func init() { RegisterArea("v1_me", registerV1Me) }

var (
	meProviderPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	meRoleOK          = map[string]bool{
		"conversational_controller": true,
		"language_realization":      true,
		"stt":                       true,
		"tts":                       true,
	}
)

// PrincipalLLMService is the self-service LLM account surface (ADR 0041).
type PrincipalLLMService interface {
	Catalog(ctx context.Context, scope contracts.Scope) (pv.ProviderCatalog, error)
	Get(ctx context.Context, scope contracts.Scope) (pv.PrincipalLlm, error)
	PutRole(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole, in PrincipalLLMBindingInput) (pv.PrincipalLlm, error)
	DeleteRole(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (pv.PrincipalLlm, error)
	SubmitCredential(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in PrincipalCredentialInput) (PrincipalCredentialStatus, error)
	DeleteCredential(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (PrincipalCredentialStatus, error)
	AcknowledgeBillingNotice(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in PrincipalBillingAckInput) (PrincipalBillingAckResult, error)
	OAuthStart(ctx context.Context, scope contracts.Scope, req PrincipalOAuthStart) (PrincipalOAuthStarted, error)
	// OAuthCallback completes a host-callback flow the caller started (1.3.0). Foreign scope → not_found.
	OAuthCallback(ctx context.Context, scope contracts.Scope, cb PrincipalOAuthCallback) (PrincipalCredentialStatus, error)
	// OAuthFlowStatus / OAuthFlowCancel are the caller's own popup flows only (1.3.0).
	OAuthFlowStatus(ctx context.Context, scope contracts.Scope, flowID string) (PrincipalOAuthFlowStatus, error)
	OAuthFlowCancel(ctx context.Context, scope contracts.Scope, flowID string) error
}

// PrincipalLLMBindingInput is the body of PUT /v1/me/llm/{role}.
type PrincipalLLMBindingInput struct {
	ProfileID pv.ProfileID        `json:"profile_id"`
	ModelID   pv.ModelID          `json:"model_id,omitzero"`
	Voice     *pv.ProviderVoiceID `json:"voice,omitzero"`
	Language  *pv.SpeechLanguage  `json:"language,omitzero"`
}

// PrincipalCredentialInput is the write-only credential body.
type PrincipalCredentialInput struct {
	Key   ProvidersSecret `json:"key"`
	Label string          `json:"label,omitempty"`
}

// PrincipalCredentialStatus is a safe status for the caller's own credential.
type PrincipalCredentialStatus struct {
	ProviderID string         `json:"provider_id"`
	Status     string         `json:"status"`
	Account    map[string]any `json:"account,omitempty"`
}

// PrincipalBillingAckInput is the body of the principal billing-notice ack.
type PrincipalBillingAckInput struct {
	NoticeVersion                      string `json:"notice_version"`
	ConsumerSubscriptionIsNotAPIAccess bool   `json:"consumer_subscription_is_not_api_access"`
	SeparateAPIBillingUnderstood       bool   `json:"separate_api_billing_understood"`
}

// PrincipalBillingAckResult is the ack response.
type PrincipalBillingAckResult struct {
	ProviderID     string    `json:"provider_id"`
	NoticeVersion  string    `json:"notice_version"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// PrincipalOAuthStart is the body of POST /v1/me/oauth/{provider}/start.
type PrincipalOAuthStart struct {
	Provider     pv.ProviderID
	Capabilities []string
	RedirectURI  string
	Presentation ProvidersOAuthPresentation
	Callback     ProvidersOAuthCallbackMode
	OpenerOrigin string
	Label        string
}

// PrincipalOAuthStarted is the start response.
type PrincipalOAuthStarted struct {
	AuthorizationURL string         `json:"authorization_url"`
	RedirectURI      string         `json:"redirect_uri"`
	ExpiresAt        time.Time      `json:"expires_at"`
	FlowID           string         `json:"flow_id,omitempty"`
	PollIntervalMS   int            `json:"poll_interval_ms,omitempty"`
	Consent          map[string]any `json:"consent,omitempty"`
}

// PrincipalOAuthCallback is the body of POST /v1/me/oauth/callback (1.3.0).
type PrincipalOAuthCallback struct {
	RedirectURI string
	Query       url.Values
}

// PrincipalOAuthFlowStatus is the body of GET /v1/me/oauth/flows/{flow_id} (1.3.0).
type PrincipalOAuthFlowStatus struct {
	FlowID    string    `json:"flow_id"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

const meCallbackQueryMax = 4096

var meFlowIDPattern = regexp.MustCompile(`^oaf_[0-9A-HJKMNP-TV-Z]{26}$`)

type meHandler struct {
	apps ApplicationService
	svc  PrincipalLLMService
}

func registerV1Me(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[PrincipalLLMService](n)
	if !n.Ready() {
		return
	}
	h := &meHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /providers/catalog", h.catalog)
	v1.HandleFunc("GET /me/llm", h.getLLM)
	v1.HandleFunc("PUT /me/llm/{role}", h.putRole)
	v1.HandleFunc("DELETE /me/llm/{role}", h.deleteRole)
	v1.HandleFunc("PUT /me/providers/{provider}/credential", h.putCredential)
	v1.HandleFunc("DELETE /me/providers/{provider}/credential", h.deleteCredential)
	v1.HandleFunc("POST /me/providers/{provider}/billing-notice/ack", h.ackBilling)
	v1.HandleFunc("POST /me/oauth/{provider}/start", h.oauthStart)
	v1.HandleFunc("POST /me/oauth/callback", h.oauthCallback)
	v1.HandleFunc("GET /me/oauth/flows/{flow_id}", h.oauthFlowStatus)
	v1.HandleFunc("DELETE /me/oauth/flows/{flow_id}", h.oauthFlowCancel)
}

func (h *meHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "principal LLM accounts are not available on this server").
			WithDetails("reason", "llm_not_configured"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *meHandler) refuseQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "this route takes no query: the scope is the verified token"))
		return false
	}
	return true
}

func (h *meHandler) catalog(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	got, err := h.svc.Catalog(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Entries == nil {
		got.Entries = []pv.ProviderCatalogEntry{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) getLLM(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	got, err := h.svc.Get(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Bindings == nil {
		got.Bindings = []pv.PrincipalLlmBinding{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) rolePath(w http.ResponseWriter, r *http.Request) (pv.PrincipalLlmRole, bool) {
	role := r.PathValue("role")
	if !meRoleOK[role] {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "role must be conversational_controller, language_realization or stt"))
		return "", false
	}
	return pv.PrincipalLlmRole(role), true
}

func (h *meHandler) putRole(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	role, ok := h.rolePath(w, r)
	if !ok {
		return
	}
	var in PrincipalLLMBindingInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.ProfileID == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "profile_id is required"))
		return
	}
	got, err := h.svc.PutRole(r.Context(), scope, role, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Bindings == nil {
		got.Bindings = []pv.PrincipalLlmBinding{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) deleteRole(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	role, ok := h.rolePath(w, r)
	if !ok {
		return
	}
	got, err := h.svc.DeleteRole(r.Context(), scope, role)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Bindings == nil {
		got.Bindings = []pv.PrincipalLlmBinding{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) providerPath(w http.ResponseWriter, r *http.Request) (pv.ProviderID, bool) {
	p := r.PathValue("provider")
	if !meProviderPattern.MatchString(p) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "provider id is not a provider slug"))
		return "", false
	}
	return pv.ProviderID(p), true
}

func (h *meHandler) putCredential(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	provider, ok := h.providerPath(w, r)
	if !ok {
		return
	}
	var in PrincipalCredentialInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Key.Empty() {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "key is required"))
		return
	}
	got, err := h.svc.SubmitCredential(r.Context(), scope, provider, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) deleteCredential(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	provider, ok := h.providerPath(w, r)
	if !ok {
		return
	}
	got, err := h.svc.DeleteCredential(r.Context(), scope, provider)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) ackBilling(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	provider, ok := h.providerPath(w, r)
	if !ok {
		return
	}
	var in PrincipalBillingAckInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.NoticeVersion == "" || !in.ConsumerSubscriptionIsNotAPIAccess || !in.SeparateAPIBillingUnderstood {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "billing notice acknowledgement is incomplete"))
		return
	}
	got, err := h.svc.AcknowledgeBillingNotice(r.Context(), scope, provider, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) oauthStart(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	provider, ok := h.providerPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Capabilities []string `json:"capabilities"`
		RedirectURI  string   `json:"redirect_uri"`
		Presentation string   `json:"presentation"`
		Callback     string   `json:"callback"`
		OpenerOrigin string   `json:"opener_origin"`
		Label        string   `json:"label"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Capabilities) == 0 || body.RedirectURI == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "capabilities and redirect_uri are required"))
		return
	}
	req := PrincipalOAuthStart{
		Provider:     provider,
		Capabilities: body.Capabilities,
		RedirectURI:  body.RedirectURI,
		Label:        body.Label,
		OpenerOrigin: body.OpenerOrigin,
	}
	if body.Presentation != "" {
		req.Presentation = ProvidersOAuthPresentation(body.Presentation)
	}
	if body.Callback != "" {
		req.Callback = ProvidersOAuthCallbackMode(body.Callback)
	}
	got, err := h.svc.OAuthStart(r.Context(), scope, req)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	var in struct {
		RedirectURI string `json:"redirect_uri"`
		Query       string `json:"query"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.RedirectURI == "" || len(in.RedirectURI) > 512 || in.Query == "" || len(in.Query) > meCallbackQueryMax {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "redirect_uri and query are required and bounded"))
		return
	}
	q, err := url.ParseQuery(strings.TrimPrefix(in.Query, "?"))
	if err != nil || q.Get("state") == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the callback query has no usable state"))
		return
	}
	got, err := h.svc.OAuthCallback(r.Context(), scope, PrincipalOAuthCallback{RedirectURI: in.RedirectURI, Query: q})
	if err != nil {
		meOAuthFlowError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) meFlowID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("flow_id")
	if !meFlowIDPattern.MatchString(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the flow id is malformed"))
		return "", false
	}
	return id, true
}

func (h *meHandler) oauthFlowStatus(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	id, ok := h.meFlowID(w, r)
	if !ok {
		return
	}
	got, err := h.svc.OAuthFlowStatus(r.Context(), scope, id)
	if err != nil {
		meOAuthFlowError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meHandler) oauthFlowCancel(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok || !h.refuseQuery(w, r) {
		return
	}
	id, ok := h.meFlowID(w, r)
	if !ok {
		return
	}
	if err := h.svc.OAuthFlowCancel(r.Context(), scope, id); err != nil {
		meOAuthFlowError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// meOAuthFlowError maps validation_failed with details.reason not_found to 404 so a
// foreign principal cannot distinguish unknown from out-of-scope.
func meOAuthFlowError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		reason, _ := ce.Details["reason"].(string)
		if ce.Code == contracts.ErrValidationFailed && reason == "not_found" {
			WriteErrorStatus(w, r, http.StatusNotFound, err)
			return
		}
		// Soften callback failures the way the operator oauth callback does: no code/state/URL.
		if ce.Code == contracts.ErrScopeViolation || reason == "scope_mismatch" || reason == "state_unknown" ||
			reason == "replayed" || reason == "state_expired" {
			code := ce.Code
			if code == "" {
				code = contracts.ErrInternal
			}
			WriteError(w, r, contracts.New(code, "the authorization could not be completed").WithDetails("reason", reason))
			return
		}
	}
	WriteError(w, r, err)
}
