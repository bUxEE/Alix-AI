package gateway

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Erasure over HTTP (1.3.0, ADR 0036). Contract: schemas/api/paths/ops.yaml.
//
//	POST /v1/ops/erasure              start (202 + job)
//	GET  /v1/ops/erasure/{job_id}     status
//
// Deployment operator or tenant admin of the caller's own tenant (ProvidersOperator).
// The audit chain survives (ADR 0036). Registered as area v1_ops_erasure so it can
// Want ErasureService independently of the base ops area.

func init() { RegisterArea("v1_ops_erasure", registerV1OpsErasure) }

// ErasureLevel is the target kind of POST /v1/ops/erasure.
type ErasureLevel string

// The accepted levels.
const (
	ErasureLevelPrincipal ErasureLevel = "principal"
	ErasureLevelTenant    ErasureLevel = "tenant"
	ErasureLevelWorkspace ErasureLevel = "workspace"
)

// ErasureScope separates tenant-local principal erasure from application-wide GDPR erasure.
type ErasureScope string

// Principal scopes (only when level is principal).
const (
	ErasureScopeTenant      ErasureScope = "tenant"
	ErasureScopeApplication ErasureScope = "application"
	// ErasureScopeWorkspace erases one principal inside one workspace only (1.5.0).
	ErasureScopeWorkspace ErasureScope = "workspace"
)

// ErasureRequest is the body of POST /v1/ops/erasure.
type ErasureRequest struct {
	Level       ErasureLevel `json:"level"`
	Scope       ErasureScope `json:"scope,omitempty"`
	TenantID    string       `json:"tenant_id,omitempty"`
	WorkspaceID string       `json:"workspace_id,omitempty"`
	PrincipalID string       `json:"principal_id,omitempty"`
	Reason      string       `json:"reason,omitempty"`
}

// ErasureJobAccepted is the 202 body.
type ErasureJobAccepted struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// ErasureCounts is per-kind counts on a finished or in-progress job.
type ErasureCounts struct {
	AuditEventsErased           int  `json:"audit_events_erased"`
	PreparationsRemoved         int  `json:"preparations_removed"`
	ReceiptsRemoved             int  `json:"receipts_removed"`
	TranscriptTurnsRemoved      int  `json:"transcript_turns_removed"`
	SessionTurnsRemoved         int  `json:"session_turns_removed"`
	SessionsRemoved             int  `json:"sessions_removed"`
	SessionsEnded               int  `json:"sessions_ended"`
	MemoryItemsRemoved          int  `json:"memory_items_removed"`
	FilesRemoved                int  `json:"files_removed,omitempty"`
	PrincipalCredentialsRevoked int  `json:"principal_credentials_revoked,omitempty"`
	PrincipalBindingsRemoved    int  `json:"principal_bindings_removed,omitempty"`
	More                        bool `json:"more,omitempty"`
}

// ErasureJobStatus is the body of GET /v1/ops/erasure/{job_id}.
type ErasureJobStatus struct {
	JobID       string         `json:"job_id"`
	Status      string         `json:"status"`
	Level       ErasureLevel   `json:"level"`
	Scope       ErasureScope   `json:"scope,omitempty"`
	TenantID    string         `json:"tenant_id"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	PrincipalID string         `json:"principal_id,omitempty"`
	Counts      *ErasureCounts `json:"counts,omitempty"`
	Error       string         `json:"error,omitempty"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
}

// ErasureService runs GDPR erasure jobs (Wanted; unwired → unavailable).
type ErasureService interface {
	// Authorize returns nil for a deployment operator, or a tenant admin of scope's tenant.
	Authorize(ctx context.Context, scope contracts.Scope) error
	Start(ctx context.Context, scope contracts.Scope, req ErasureRequest) (ErasureJobAccepted, error)
	Status(ctx context.Context, scope contracts.Scope, jobID string) (ErasureJobStatus, error)
}

var opsErasureJobIDPattern = regexp.MustCompile(`^erj_[0-9A-HJKMNP-TV-Z]{26}$`)

type opsErasureHandler struct {
	apps ApplicationService
	svc  ErasureService
}

func registerV1OpsErasure(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[ErasureService](n)
	if !n.Ready() {
		return
	}
	h := &opsErasureHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("POST /ops/erasure", h.start)
	v1.HandleFunc("GET /ops/erasure/{job_id}", h.status)
}

func (h *opsErasureHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "erasure is not available on this server"))
		return contracts.Scope{}, false
	}
	if err := h.svc.Authorize(r.Context(), scope); err != nil {
		WriteError(w, r, err)
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *opsErasureHandler) start(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in ErasureRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	got, err := h.svc.Start(r.Context(), scope, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, got)
}

func (h *opsErasureHandler) status(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("job_id")
	if !opsErasureJobIDPattern.MatchString(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the erasure job id is malformed"))
		return
	}
	got, err := h.svc.Status(r.Context(), scope, id)
	if err != nil {
		opsErasureError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func opsErasureError(w http.ResponseWriter, r *http.Request, err error) {
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
