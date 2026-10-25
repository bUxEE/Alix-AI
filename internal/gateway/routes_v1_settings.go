package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/pkg/contracts"
	settingsgen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// The settings and client preferences part of the public API (master plan 14.2,
// "Settings and client preferences"). The contract is schemas/api/paths/settings.yaml
// and the schemas under schemas/settings; TestRoutesMatchOpenAPISpec fails on any
// difference between the routes registered here and the document.
//
//	GET  /v1/settings/user                 the caller's own user settings (version, schema, document)
//	PUT  /v1/settings/user                 replace them, against the version that was read
//	GET  /v1/settings/tenant               the caller's tenant settings
//	PUT  /v1/settings/tenant               replace them
//	GET  /v1/settings/deployment           the settings of the caller's application as a whole
//	PUT  /v1/settings/deployment           replace them
//	GET  /v1/settings/effective            what the three levels add up to for the caller
//	GET  /v1/settings/profiles             the approved model and resource profiles
//	GET  /v1/settings/capabilities         the capabilities enabled for the caller
//	GET  /v1/settings/integration-audit    the latest integration audit: findings and remediation status
//	GET  /v1/settings/providers            the health of the model roles
//
// # What the area does and does not decide
//
// The handlers are thin: the scope comes from the verified token and from nothing
// else (a tenant, workspace, principal or application in a path, a query or a body
// does not exist for them), the body of a write is exactly {expected_version,
// settings}, and everything else (the permission a token needs for a level and an
// operation, the schema, the rules a schema can not state, the optimistic
// version check, the audit event) is internal/settings. A client can not use any
// of these routes to enable an undeclared tool, enlarge a scope, bypass or lower a
// confirmation, expose LocalAI administration, enable external or hybrid mode,
// change data egress, hold a provider credential, raise a budget or change the
// decision layer: there is no member in any schema, and no route, for any of it
// (TestSettingsCannotEnableExternalModeOrChangeEgress and its neighbours in
// internal/settings, and the inventory test of this area).
//
// The area needs one service, SettingsService, which the composition root builds
// (internal/cli/cmd_settings.go). It resolves the active package of the caller's
// application itself, so a quarantined application answers quarantined here as it
// does everywhere else (the integration-audit and provider read-outs excepted: an
// application's findings matter most while it is quarantined).

func init() { RegisterArea("v1_settings", registerV1Settings) }

// SettingsService is what the settings area needs: internal/settings.Service is
// the implementation and tests stand in their own. Each method takes the verified
// Scope and decides permission from it.
type SettingsService interface {
	Get(ctx context.Context, scope contracts.Scope, level settings.Level) (settings.Record, error)
	Put(ctx context.Context, scope contracts.Scope, level settings.Level, req settings.PutRequest) (settings.Record, error)
	Effective(ctx context.Context, scope contracts.Scope) (settingsgen.EffectiveSettings, error)
	Profiles(ctx context.Context, scope contracts.Scope) (settings.ProfileList, error)
	Capabilities(ctx context.Context, scope contracts.Scope) (settings.CapabilityView, error)
	IntegrationAudit(ctx context.Context, scope contracts.Scope) (settings.AuditStatus, error)
	ProviderHealth(ctx context.Context, scope contracts.Scope) (settings.ProviderHealth, error)
}

var _ SettingsService = (*settings.Service)(nil)

type settingsHandler struct {
	svc SettingsService
}

// registerV1Settings needs the settings service; without it the area mounts
// nothing (Server.Unmounted names it) and `alixai serve` refuses to start.
func registerV1Settings(rt *Router, d *Deps) {
	n := d.Needs()
	h := &settingsHandler{svc: Need[SettingsService](n)}
	if !n.Ready() {
		return
	}
	v1 := rt.V1()
	for _, level := range settings.Levels() {
		v1.HandleFunc("GET /settings/"+string(level), h.get(level))
		v1.HandleFunc("PUT /settings/"+string(level), h.put(level))
	}
	v1.HandleFunc("GET /settings/effective", h.effective)
	v1.HandleFunc("GET /settings/profiles", h.profiles)
	v1.HandleFunc("GET /settings/capabilities", h.capabilities)
	v1.HandleFunc("GET /settings/integration-audit", h.integrationAudit)
	v1.HandleFunc("GET /settings/providers", h.providers)
}

// settingsPutBody is the body of every PUT: the version the caller read (0 to
// create) and the whole document. Nothing else is accepted.
type settingsPutBody struct {
	ExpectedVersion *int64          `json:"expected_version"`
	Settings        json.RawMessage `json:"settings"`
}

func (h *settingsHandler) get(level settings.Level) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := v1Scope(w, r)
		if !ok {
			return
		}
		rec, err := h.svc.Get(r.Context(), scope, level)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

func (h *settingsHandler) put(level settings.Level) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := v1Scope(w, r)
		if !ok {
			return
		}
		var body settingsPutBody
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.ExpectedVersion == nil {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "expected_version is required: the version that was read, or 0 to create the document"))
			return
		}
		if len(body.Settings) == 0 || string(body.Settings) == "null" {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "settings is required: the whole document"))
			return
		}
		rec, err := h.svc.Put(r.Context(), scope, level, settings.PutRequest{ExpectedVersion: *body.ExpectedVersion, Settings: body.Settings})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

func (h *settingsHandler) effective(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	eff, err := h.svc.Effective(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, eff)
}

func (h *settingsHandler) profiles(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	list, err := h.svc.Profiles(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *settingsHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	view, err := h.svc.Capabilities(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *settingsHandler) integrationAudit(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	st, err := h.svc.IntegrationAudit(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *settingsHandler) providers(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	health, err := h.svc.ProviderHealth(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}
