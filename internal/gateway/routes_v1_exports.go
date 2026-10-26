package gateway

import (
	"context"
	"net/http"

	"github.com/buxee/alix-ai/internal/exports"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// POST /v1/sessions/{session_id}/exports generates a downloadable file from a
// turn's verified rich blocks (release 1.2.0). Contract: schemas/api/paths/exports.yaml.
//
// Needs ApplicationService; Wants ExportsService (unwired → unavailable). Capability: exports.create.

func init() { RegisterArea("v1_exports", registerV1Exports) }

// ExportsService is the tenant-facing export surface.
type ExportsService interface {
	Create(ctx context.Context, scope contracts.Scope, sessionID string, req exports.Request) (conversation.FileRef, error)
}

// TurnBlocksArchive stores verified rich blocks of a finished turn for durable
// export after the stream hub forgets the turn. history.Service implements it.
type TurnBlocksArchive interface {
	PutTurnBlocks(ctx context.Context, scope contracts.Scope, sessionID, turnID string, blocks []conversation.RichBlock) error
}

type exportsHandler struct {
	apps ApplicationService
	svc  ExportsService
}

func registerV1Exports(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[ExportsService](n)
	if !n.Ready() {
		return
	}
	h := &exportsHandler{apps: apps, svc: svc}
	rt.V1().HandleFunc("POST /sessions/{session_id}/exports", h.create)
}

func (h *exportsHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "exports are not available on this server"))
		return
	}
	var body conversation.ExportRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	ref, err := h.svc.Create(r.Context(), scope, scope.SessionID, exports.Request{
		TurnID: string(body.TurnID), BlockIDs: body.BlockIDs, Format: exports.Format(body.Format),
	})
	if err != nil {
		exportsError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/files/"+string(ref.FileID))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, ref)
}

func exportsError(w http.ResponseWriter, r *http.Request, err error) {
	if exports.IsNotFound(err) {
		WriteErrorStatus(w, r, http.StatusNotFound, err)
		return
	}
	if contracts.CodeOf(err) == contracts.ErrResourceExhausted {
		WriteErrorStatus(w, r, http.StatusInsufficientStorage, err)
		return
	}
	WriteError(w, r, err)
}
