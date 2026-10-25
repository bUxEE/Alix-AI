package gateway

import (
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// DEV ONLY. This whole file is the M0 spike endpoint. The real turn API is
// POST /v1/sessions/{session_id}/turns (routes_v1_sessions.go, M1-16); this
// route stays only because the M0 end-to-end proof drives it, and only behind
// the dev flag. It has no authentication, which is why it is off unless
// dev.spike_endpoint is true (the default is false), why it is never served on
// a listener that is not loopback or a unix socket (ListenPolicy), why it lives
// under /v0/dev and not /v1, and why it is not on the OpenAPI document: it is
// not part of the public API, and TestRoutesMatchOpenAPISpec fails if a /v0
// route ever is.

func init() { RegisterArea("dev", registerDev) }

func registerDev(rt *Router, d *Deps) {
	if !d.Dev.SpikeEndpoint {
		return // not mounted: the path is an ordinary 404
	}
	d.Logger.Warn("dev.spike_endpoint is on: POST /v0/dev/propose is unauthenticated and dev-only")
	h := &spikeHandler{d: d, slot: make(chan struct{}, 1)}
	rt.HandleFunc("POST /v0/dev/propose", h.propose)
}

type spikeHandler struct {
	d *Deps
	// slot admits one model call at a time: the Lite profile runs one
	// interactive generation, and a stampede must not queue behind the model.
	slot chan struct{}
}

// ProposeResponse is the body of a successful POST /v0/dev/propose.
type ProposeResponse struct {
	DevOnly bool `json:"dev_only"`
	// SchemaValid is always true: a proposal that did not validate is an
	// error envelope and is never returned.
	SchemaValid bool                   `json:"schema_valid"`
	Proposal    contracts.TaskProposal `json:"proposal"`
	// Model is the model the runtime reports having used.
	Model    string          `json:"model,omitempty"`
	Usage    providers.Usage `json:"usage"`
	PromptID string          `json:"prompt_id"`
}

// devScope is the scope of a spike request. Real requests never get one like
// it: /v1 scopes come only from a verified context token (M1-06). It exists
// because providers refuse a request without every scope level, and it is
// confined to this file. The capability scopes are the request's own catalogue.
func devScope(gen *ids.Generator, capabilityIDs []string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dev",
		ApplicationID:    "dev-spike",
		TenantID:         "dev",
		WorkspaceID:      "dev",
		PrincipalID:      "dev-spike-caller",
		Roles:            []string{"dev"},
		CapabilityScopes: capabilityIDs,
		SessionID:        gen.New("ses"),
	}
}

func (h *spikeHandler) propose(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-AlixAI-Dev-Only", "true")
	ctx := r.Context()
	log := loggerFrom(ctx)

	var req ProposeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	today, err := req.validate(h.d.Clock.Now())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	chat, schema, err := proposeBuildChat(&req, today)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	compact, err := compileCompact(schema)
	if err != nil {
		WriteError(w, r, contracts.Wrap(contracts.ErrInternal, err, "gateway: cannot compile the output schema"))
		return
	}

	gen, err := h.d.Providers.TextGenerator(providers.RoleController)
	if err != nil {
		WriteError(w, r, err) // unavailable: no controller registered
		return
	}
	if !h.d.Providers.Usable(providers.RoleController) {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "the controller model is not available").
			WithDetails("effect", providers.RoleController.Effect()))
		return
	}
	budgeted, err := providers.WithBudget(gen, h.d.Budget)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	select {
	case h.slot <- struct{}{}:
		defer func() { <-h.slot }()
	default:
		WriteError(w, r, contracts.New(contracts.ErrResourceExhausted, "another dev request is running"))
		return
	}

	start := h.d.Clock.Now()
	res, err := budgeted.Chat(ctx, devScope(h.d.IDs, req.capabilityIDs()), chat)
	if err != nil {
		if contracts.CodeOf(err) == contracts.ErrSchemaInvalid {
			// The runtime answered, and what it answered is not usable: an
			// upstream fault, not the caller's.
			WriteErrorStatus(w, r, http.StatusBadGateway, err)
			return
		}
		WriteError(w, r, err)
		return
	}

	// The trace ID comes from the provider call; if the runtime gave none, or
	// one that is not a valid identifier, the core's request ID stands in. It
	// is core-minted either way, never model text.
	trace := res.TraceID
	if !validIdentifier(trace) {
		trace = RequestID(ctx)
	}
	proposal, err := proposeDecode(res, compact, trace)
	if err != nil {
		WriteErrorStatus(w, r, http.StatusBadGateway, err)
		return
	}

	log.Info("spike proposal validated",
		slog.Int("capabilities", len(req.Capabilities)),
		slog.Int("tok_in", res.Usage.InputTokens),
		slog.Int("tok_out", res.Usage.OutputTokens),
		slog.Duration("dur", h.d.Clock.Now().Sub(start)))
	writeJSON(w, http.StatusOK, ProposeResponse{
		DevOnly:     true,
		SchemaValid: true,
		Proposal:    proposal,
		Model:       res.Model,
		Usage:       res.Usage,
		PromptID:    ProposePromptID,
	})
}

// validIdentifier mirrors schemas/common/ids identifier closely enough to
// choose between a runtime's trace ID and the core's own: 1 to 128 characters,
// no whitespace or control characters, no '*'. proposeDecode still applies the
// real schema.
func validIdentifier(s string) bool {
	if s == "" || len(s) > 128 || strings.ContainsRune(s, '*') {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
