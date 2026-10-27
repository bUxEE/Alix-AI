package gateway

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Private LLM ops (ADR 0045). Contract: schemas/api/paths/ops.yaml.

func init() { RegisterArea("v1_ops_private_llm", registerV1OpsPrivateLLM) }

const (
	PrivateLLMCapabilityRead  = "ops.private_llm.read"
	PrivateLLMCapabilityWrite = "ops.private_llm.write"
)

var privateLLMEndpointIDPattern = regexp.MustCompile(`^ple_[0-9A-HJKMNP-TV-Z]{26}$`)

// PrivateLLMService manages private self-hosted endpoints (Wanted; unwired → unavailable).
type PrivateLLMService interface {
	AuthorizeRead(ctx context.Context, scope contracts.Scope) error
	AuthorizeWrite(ctx context.Context, scope contracts.Scope, tenantID string) error
	PutEndpoint(ctx context.Context, scope contracts.Scope, id pv.PrivateEndpointID, in pv.PrivateLlmEndpointUpdate) (pv.PrivateLlmEndpoint, error)
	GetEndpoint(ctx context.Context, scope contracts.Scope, id pv.PrivateEndpointID) (pv.PrivateLlmEndpoint, error)
	DeleteEndpoint(ctx context.Context, scope contracts.Scope, id pv.PrivateEndpointID) error
	PutTenantPin(ctx context.Context, scope contracts.Scope, tenantID string, in pv.PrivateLlmTenantPinUpdate) (pv.PrivateLlmTenantPin, error)
	GetTenantPin(ctx context.Context, scope contracts.Scope, tenantID string) (pv.PrivateLlmTenantPin, error)
	DeleteTenantPin(ctx context.Context, scope contracts.Scope, tenantID string) error
	PutPrincipalBinding(ctx context.Context, scope contracts.Scope, principalID string, in pv.PrivateLlmPrincipalBindingUpdate) (pv.PrivateLlmPrincipalBinding, error)
	GetPrincipalBinding(ctx context.Context, scope contracts.Scope, principalID string) (pv.PrivateLlmPrincipalBinding, error)
	DeletePrincipalBinding(ctx context.Context, scope contracts.Scope, principalID string) error
}

type opsPrivateLLMHandler struct {
	apps ApplicationService
	svc  PrivateLLMService
}

func registerV1OpsPrivateLLM(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[PrivateLLMService](n)
	if !n.Ready() {
		return
	}
	h := &opsPrivateLLMHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("PUT /ops/private-llm/endpoints/{endpoint_id}", h.putEndpoint)
	v1.HandleFunc("GET /ops/private-llm/endpoints/{endpoint_id}", h.getEndpoint)
	v1.HandleFunc("DELETE /ops/private-llm/endpoints/{endpoint_id}", h.deleteEndpoint)
	v1.HandleFunc("PUT /ops/tenants/{tenant_id}/private-llm", h.putTenantPin)
	v1.HandleFunc("GET /ops/tenants/{tenant_id}/private-llm", h.getTenantPin)
	v1.HandleFunc("DELETE /ops/tenants/{tenant_id}/private-llm", h.deleteTenantPin)
	v1.HandleFunc("PUT /ops/principals/{principal_id}/private-llm", h.putPrincipalBinding)
	v1.HandleFunc("GET /ops/principals/{principal_id}/private-llm", h.getPrincipalBinding)
	v1.HandleFunc("DELETE /ops/principals/{principal_id}/private-llm", h.deletePrincipalBinding)
}

func (h *opsPrivateLLMHandler) beginWrite(w http.ResponseWriter, r *http.Request, tenantID string) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "private LLM ops are not available on this server"))
		return contracts.Scope{}, false
	}
	if !scope.Permits(PrivateLLMCapabilityWrite) {
		WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "ops.private_llm.write is required"))
		return contracts.Scope{}, false
	}
	if err := h.svc.AuthorizeWrite(r.Context(), scope, tenantID); err != nil {
		WriteError(w, r, err)
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *opsPrivateLLMHandler) beginRead(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "private LLM ops are not available on this server"))
		return contracts.Scope{}, false
	}
	if !scope.Permits(PrivateLLMCapabilityRead) && !scope.Permits(PrivateLLMCapabilityWrite) {
		WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "ops.private_llm.read is required"))
		return contracts.Scope{}, false
	}
	if err := h.svc.AuthorizeRead(r.Context(), scope); err != nil {
		WriteError(w, r, err)
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *opsPrivateLLMHandler) putEndpoint(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginWrite(w, r, "")
	if !ok {
		return
	}
	id := pv.PrivateEndpointID(r.PathValue("endpoint_id"))
	if !privateLLMEndpointIDPattern.MatchString(string(id)) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the endpoint id is malformed"))
		return
	}
	var in pv.PrivateLlmEndpointUpdate
	if !decodeJSON(w, r, &in) {
		return
	}
	got, err := h.svc.PutEndpoint(r.Context(), scope, id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *opsPrivateLLMHandler) getEndpoint(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginRead(w, r)
	if !ok {
		return
	}
	id := pv.PrivateEndpointID(r.PathValue("endpoint_id"))
	if !privateLLMEndpointIDPattern.MatchString(string(id)) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the endpoint id is malformed"))
		return
	}
	got, err := h.svc.GetEndpoint(r.Context(), scope, id)
	if err != nil {
		opsPrivateLLMError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *opsPrivateLLMHandler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginWrite(w, r, "")
	if !ok {
		return
	}
	id := pv.PrivateEndpointID(r.PathValue("endpoint_id"))
	if !privateLLMEndpointIDPattern.MatchString(string(id)) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the endpoint id is malformed"))
		return
	}
	if err := h.svc.DeleteEndpoint(r.Context(), scope, id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *opsPrivateLLMHandler) putTenantPin(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	scope, ok := h.beginWrite(w, r, tenantID)
	if !ok {
		return
	}
	var in pv.PrivateLlmTenantPinUpdate
	if !decodeJSON(w, r, &in) {
		return
	}
	got, err := h.svc.PutTenantPin(r.Context(), scope, tenantID, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *opsPrivateLLMHandler) getTenantPin(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	scope, ok := h.beginRead(w, r)
	if !ok {
		return
	}
	got, err := h.svc.GetTenantPin(r.Context(), scope, tenantID)
	if err != nil {
		opsPrivateLLMError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *opsPrivateLLMHandler) deleteTenantPin(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	scope, ok := h.beginWrite(w, r, tenantID)
	if !ok {
		return
	}
	if err := h.svc.DeleteTenantPin(r.Context(), scope, tenantID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *opsPrivateLLMHandler) putPrincipalBinding(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginWrite(w, r, "")
	if !ok {
		return
	}
	principalID := r.PathValue("principal_id")
	var in pv.PrivateLlmPrincipalBindingUpdate
	if !decodeJSON(w, r, &in) {
		return
	}
	got, err := h.svc.PutPrincipalBinding(r.Context(), scope, principalID, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *opsPrivateLLMHandler) getPrincipalBinding(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginRead(w, r)
	if !ok {
		return
	}
	principalID := r.PathValue("principal_id")
	got, err := h.svc.GetPrincipalBinding(r.Context(), scope, principalID)
	if err != nil {
		opsPrivateLLMError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *opsPrivateLLMHandler) deletePrincipalBinding(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginWrite(w, r, "")
	if !ok {
		return
	}
	principalID := r.PathValue("principal_id")
	if err := h.svc.DeletePrincipalBinding(r.Context(), scope, principalID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func opsPrivateLLMError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		reason, _ := ce.Details["reason"].(string)
		if ce.Code == contracts.ErrValidationFailed && reason == "not_found" {
			WriteErrorStatus(w, r, http.StatusNotFound, err)
			return
		}
	}
	WriteError(w, r, err)
}
