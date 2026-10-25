package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/diag"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The operations part of the public API (master plan 14.2 "Operations": queue
// status, provider/model status, audit export under authorization). The contract
// is schemas/api/paths/ops.yaml; TestRoutesMatchOpenAPISpec fails on any
// difference in either direction.
//
//	GET  /v1/ops/queue          the queue counts of the caller's own application, tenant and workspace
//	GET  /v1/ops/providers      the status of the model roles
//	POST /v1/ops/audit/export   the caller's own audit events as JSON lines
//
// # What is not here
//
// The deployment-wide view (global queue counts, resource usage, the Prometheus
// text, the diagnostics bundle with the configuration and the recent errors) is
// not on this API. A /v1 route is called with a token a host application minted,
// and a host must not read the counts of other applications or the deployment's
// configuration. That view is internal/diag's OperatorHandler on the loopback
// metrics listener (internal/metrics), behind the operator credential.
//
// # Identity and authorisation
//
// Every handler starts from identity.RequireScope: the scope is the verified
// context token and nothing a path, a query or a body says. The host authorises
// each operation by putting its capability in the token (ops.queue.read,
// ops.providers.read, audit.export), the match is exact, and a missing capability
// is 403 permission_denied before anything is read. The checks are
// internal/diag's and internal/audit's, not this file's. A quarantined
// application serves no operations either.
//
// # The export
//
// The audit export is one JSON object per event, oldest first, then a last line
// {"complete": true, "events": N}. The status is sent with the first byte, so a
// refusal (no capability, a malformed filter, an export that could not be
// recorded) is a normal error response and nothing else is sent; a failure half
// way is told by the missing last line.

func init() { RegisterArea("v1_ops", registerV1Ops) }

// OpsService is what the operations area needs: *diag.Service is the
// implementation and tests stand in their own. Each method takes the verified
// Scope and decides permission from it.
//
// The service is optional. The area is mounted wherever applications are served,
// and answers 503 unavailable, never an empty success, while no OpsService is
// provided: an operator who has not wired monitoring is told so rather than
// shown an empty queue.
type OpsService interface {
	Queue(ctx context.Context, scope contracts.Scope) (diag.QueueReport, error)
	Providers(ctx context.Context, scope contracts.Scope) (diag.ProviderReport, error)
	AuditExport(ctx context.Context, scope contracts.Scope, w io.Writer, opts audit.ExportOptions) (audit.ExportResult, error)
}

var _ OpsService = (*diag.Service)(nil)

type opsHandler struct {
	ops  OpsService // nil while none is provided
	apps ApplicationService
}

// registerV1Ops needs the application service (a quarantined application
// serves no traffic) and uses the operations service when there is one.
func registerV1Ops(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	ops, _ := Want[OpsService](n)
	if !n.Ready() {
		return
	}
	h := &opsHandler{ops: ops, apps: apps}
	v1 := rt.V1()
	v1.HandleFunc("GET /ops/queue", h.queue)
	v1.HandleFunc("GET /ops/providers", h.providers)
	v1.HandleFunc("POST /ops/audit/export", h.auditExport)
}

// OpsAuditExportRequest is the optional body of POST /v1/ops/audit/export. Every
// member narrows the export; an empty body exports every event of the caller's
// own workspace.
type OpsAuditExportRequest struct {
	// Types keeps only these event types (for example "action.committed").
	Types []string `json:"types,omitempty"`
	// Since is inclusive and Until exclusive; both bound the time of the event.
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`
}

// OpsAuditExportTrailer is the last line of a complete export.
type OpsAuditExportTrailer struct {
	Complete bool `json:"complete"`
	Events   int  `json:"events"`
}

// opsMaxExportTypes bounds the type filter of one export.
const opsMaxExportTypes = 64

// begin is the start of every handler: the verified scope, an active
// application and a provided service, or the refusal already written.
func (h *opsHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.ops == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "operations are not available on this server"))
		return contracts.Scope{}, false
	}
	return scope, true
}

func (h *opsHandler) queue(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	rep, err := h.ops.Queue(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h *opsHandler) providers(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	rep, err := h.ops.Providers(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// opsExportWriter starts the response on the first byte, so a refusal that
// comes before any event is a normal error response and not a half-written
// stream.
type opsExportWriter struct {
	w       http.ResponseWriter
	started bool
}

func (e *opsExportWriter) Write(p []byte) (int, error) {
	if !e.started {
		e.started = true
		h := e.w.Header()
		h.Set("Content-Type", "application/x-ndjson")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		e.w.WriteHeader(http.StatusOK)
	}
	return e.w.Write(p)
}

func (h *opsHandler) auditExport(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	var req OpsAuditExportRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	if len(req.Types) > opsMaxExportTypes {
		WriteError(w, r, contracts.Newf(contracts.ErrValidationFailed, "at most %d event types can be named", opsMaxExportTypes))
		return
	}
	if !req.Since.IsZero() && !req.Until.IsZero() && !req.Until.After(req.Since) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "until must be after since"))
		return
	}
	opts := audit.ExportOptions{Since: req.Since, Until: req.Until}
	for _, t := range req.Types {
		opts.Types = append(opts.Types, audit.EventType(t))
	}

	extendWriteDeadline(w)
	out := &opsExportWriter{w: w}
	res, err := h.ops.AuditExport(r.Context(), scope, out, opts)
	if err != nil {
		if !out.started {
			WriteError(w, r, err)
			return
		}
		// Events are on the wire: the status cannot change. The missing trailer is
		// how the client knows the export is not whole.
		loggerFrom(r.Context()).Error("the audit export failed after it started", "code", string(contracts.CodeOf(err)), "events", res.Events)
		return
	}
	// A whole export with no events still starts the response with its trailer.
	if err := json.NewEncoder(out).Encode(OpsAuditExportTrailer{Complete: true, Events: res.Events}); err != nil {
		loggerFrom(r.Context()).Error("the audit export trailer could not be written", "events", res.Events)
	}
}
