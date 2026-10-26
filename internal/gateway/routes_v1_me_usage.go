package gateway

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Self-service usage and budget (ADR 0041 / 1.3.0). Contract: schemas/api/paths/me.yaml.
//
//	GET /v1/me/usage
//	GET /v1/me/budget
//
// Scope is the verified token's principal. Needs ApplicationService; Wants MeUsageBudgetService
// (unwired → unavailable). Never another principal's rows.

func init() { RegisterArea("v1_me_usage", registerV1MeUsage) }

// MeUsageBudgetService is the self-service usage and principal-budget surface.
type MeUsageBudgetService interface {
	MeUsage(ctx context.Context, scope contracts.Scope, q MeUsageQuery) (pv.MeUsage, error)
	MeBudget(ctx context.Context, scope contracts.Scope) (pv.MeBudget, error)
}

// MeUsageQuery selects the caller's usage page.
type MeUsageQuery struct {
	From, To time.Time
	TenantID string
	Limit    int
	Cursor   string
}

type meUsageHandler struct {
	apps ApplicationService
	svc  MeUsageBudgetService
}

func registerV1MeUsage(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[MeUsageBudgetService](n)
	if !n.Ready() {
		return
	}
	h := &meUsageHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /me/usage", h.usage)
	v1.HandleFunc("GET /me/budget", h.budget)
}

func (h *meUsageHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "principal usage and budget are not available on this server").
			WithDetails("reason", "usage_not_configured"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *meUsageHandler) usage(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for name := range q {
		switch name {
		case "from", "to", "tenant_id", "cursor", "limit":
		default:
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "unknown query parameter: "+name))
			return
		}
	}
	var in MeUsageQuery
	for name, dst := range map[string]*time.Time{"from": &in.From, "to": &in.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				WriteError(w, r, contracts.New(contracts.ErrValidationFailed, name+" is not an RFC 3339 time"))
				return
			}
			*dst = t
		}
	}
	in.TenantID = q.Get("tenant_id")
	in.Cursor = q.Get("cursor")
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "limit must be an integer from 1 to 100"))
			return
		}
		in.Limit = n
	}
	got, err := h.svc.MeUsage(r.Context(), scope, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Groups == nil {
		got.Groups = []pv.MeUsageGroup{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *meUsageHandler) budget(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "this route takes no query: the scope is the verified token"))
		return
	}
	got, err := h.svc.MeBudget(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Budgets == nil {
		got.Budgets = []pv.Budget{}
	}
	if got.States == nil {
		got.States = []pv.BudgetState{}
	}
	writeJSON(w, http.StatusOK, got)
}
