package gateway

import (
	"context"
	"net/http"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// Analyst abilities (ADR 0047). Contract: schemas/api/paths/ops.yaml and me.yaml.
//
//	GET|PUT /v1/ops/tenants/{tenant_id}/analyst
//	GET     /v1/me/analyst
//
// Needs ApplicationService; Wants AnalystService (unwired → unavailable).

func init() { RegisterArea("v1_analyst", registerV1Analyst) }

const (
	AnalystCapabilityRead  = "ops.analyst.read"
	AnalystCapabilityWrite = "ops.analyst.write"
)

// AnalystService is the analyst settings surface (ADR 0047).
type AnalystService interface {
	AuthorizeTenantRead(ctx context.Context, scope contracts.Scope, tenantID string) error
	AuthorizeTenantWrite(ctx context.Context, scope contracts.Scope, tenantID string) error
	GetTenant(ctx context.Context, scope contracts.Scope, tenantID string) (an.TenantSettings, error)
	PutTenant(ctx context.Context, scope contracts.Scope, tenantID string, in an.TenantSettings) (an.TenantSettings, error)
	Effective(ctx context.Context, scope contracts.Scope) (an.Abilities, error)
}

type analystHandler struct {
	apps ApplicationService
	svc  AnalystService
}

func registerV1Analyst(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[AnalystService](n)
	if !n.Ready() {
		return
	}
	h := &analystHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /ops/tenants/{tenant_id}/analyst", h.getTenant)
	v1.HandleFunc("PUT /ops/tenants/{tenant_id}/analyst", h.putTenant)
	v1.HandleFunc("GET /me/analyst", h.effective)
}

func (h *analystHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "analyst settings are not available on this server"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *analystHandler) getTenant(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	tenantID := r.PathValue("tenant_id")
	if tenantID == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "tenant_id is required"))
		return
	}
	if !scope.Permits(AnalystCapabilityRead) && !scope.Permits(AnalystCapabilityWrite) {
		WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "ops.analyst.read is required"))
		return
	}
	if err := h.svc.AuthorizeTenantRead(r.Context(), scope, tenantID); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := h.svc.GetTenant(r.Context(), scope, tenantID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *analystHandler) putTenant(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	tenantID := r.PathValue("tenant_id")
	if tenantID == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "tenant_id is required"))
		return
	}
	if !scope.Permits(AnalystCapabilityWrite) {
		WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "ops.analyst.write is required"))
		return
	}
	if err := h.svc.AuthorizeTenantWrite(r.Context(), scope, tenantID); err != nil {
		WriteError(w, r, err)
		return
	}
	var in an.TenantSettings
	if !decodeJSON(w, r, &in) {
		return
	}
	in.UpdatedAt = nil
	out, err := h.svc.PutTenant(r.Context(), scope, tenantID, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *analystHandler) effective(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Effective(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
