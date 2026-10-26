package gateway

import (
	"context"
	"net/http"
	"regexp"
	"strconv"

	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// Long-term memory (ADR 0042, release 1.2.0). Contract: schemas/api/paths/memory.yaml.
//
//	GET    /v1/memory
//	POST   /v1/memory
//	PATCH  /v1/memory/{id}
//	DELETE /v1/memory/{id}
//	POST   /v1/memory/{id}/confirm
//	POST   /v1/memory/{id}/reject
//
// Scope is the verified token. Needs ApplicationService; Wants MemoryService (unwired → unavailable).

func init() { RegisterArea("v1_memory", registerV1Memory) }

var memoryIDPattern = regexp.MustCompile(`^mem_[0-9A-HJKMNP-TV-Z]{26}$`)

// MemoryListQuery filters GET /v1/memory.
type MemoryListQuery struct {
	Q      string
	Status string
	Cursor string
	Limit  int
}

// MemoryService is the long-term memory surface (ADR 0042).
type MemoryService interface {
	List(ctx context.Context, scope contracts.Scope, q MemoryListQuery) (mem.MemoryList, error)
	Create(ctx context.Context, scope contracts.Scope, in mem.MemoryCreate) (mem.MemoryItem, error)
	Patch(ctx context.Context, scope contracts.Scope, id mem.MemoryID, in mem.MemoryPatch) (mem.MemoryItem, error)
	Delete(ctx context.Context, scope contracts.Scope, id mem.MemoryID) error
	Confirm(ctx context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error)
	Reject(ctx context.Context, scope contracts.Scope, id mem.MemoryID) (mem.MemoryItem, error)
}

type memoryHandler struct {
	apps ApplicationService
	svc  MemoryService
}

func registerV1Memory(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[MemoryService](n)
	if !n.Ready() {
		return
	}
	h := &memoryHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /memory", h.list)
	v1.HandleFunc("POST /memory", h.create)
	v1.HandleFunc("PATCH /memory/{id}", h.patch)
	v1.HandleFunc("DELETE /memory/{id}", h.delete)
	v1.HandleFunc("POST /memory/{id}/confirm", h.confirm)
	v1.HandleFunc("POST /memory/{id}/reject", h.reject)
}

func (h *memoryHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "long-term memory is not available on this server"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *memoryHandler) idPath(w http.ResponseWriter, r *http.Request) (mem.MemoryID, bool) {
	id := r.PathValue("id")
	if !memoryIDPattern.MatchString(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "memory id is not a memory id"))
		return "", false
	}
	return mem.MemoryID(id), true
}

func (h *memoryHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := MemoryListQuery{Q: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status"), Cursor: r.URL.Query().Get("cursor")}
	if lim := r.URL.Query().Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil || n < 1 || n > 200 {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "limit must be an integer from 1 to 200"))
			return
		}
		q.Limit = n
	}
	for k := range r.URL.Query() {
		switch k {
		case "q", "status", "cursor", "limit":
		default:
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the query holds a parameter this route does not take"))
			return
		}
	}
	if q.Status != "" && q.Status != "proposed" && q.Status != "active" && q.Status != "rejected" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "status must be proposed, active or rejected"))
		return
	}
	got, err := h.svc.List(r.Context(), scope, q)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Items == nil {
		got.Items = []mem.MemoryItem{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *memoryHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "this route takes no query: the scope is the verified token"))
		return
	}
	var in mem.MemoryCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Text == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "text is required"))
		return
	}
	got, err := h.svc.Create(r.Context(), scope, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *memoryHandler) patch(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "this route takes no query: the scope is the verified token"))
		return
	}
	id, ok := h.idPath(w, r)
	if !ok {
		return
	}
	var in mem.MemoryPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	got, err := h.svc.Patch(r.Context(), scope, id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *memoryHandler) delete(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "this route takes no query: the scope is the verified token"))
		return
	}
	id, ok := h.idPath(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), scope, id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *memoryHandler) confirm(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, true)
}

func (h *memoryHandler) reject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, false)
}

func (h *memoryHandler) decide(w http.ResponseWriter, r *http.Request, confirm bool) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "this route takes no query: the scope is the verified token"))
		return
	}
	id, ok := h.idPath(w, r)
	if !ok {
		return
	}
	var (
		got mem.MemoryItem
		err error
	)
	if confirm {
		got, err = h.svc.Confirm(r.Context(), scope, id)
	} else {
		got, err = h.svc.Reject(r.Context(), scope, id)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}
