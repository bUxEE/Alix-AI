//go:build !noexternal

package cli

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/oauth"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// principalLLM implements gateway.PrincipalLLMService (ADR 0041).
type principalLLM struct {
	admin   *externalAdmin
	reg     *providerreg.Registry
	creds   *credentials.Service
	oauth   *oauth.Engine
	clk     clock.Clock
	log     *slog.Logger
	audit   audit.Appender
	oauthOK map[string]bool // providers with official third-party OAuth enabled
}

func newPrincipalLLM(a *externalAdmin) *principalLLM {
	oauthOK := map[string]bool{}
	for id, p := range externalSection.OAuth.Providers {
		if p.Enabled && !p.Disabled {
			oauthOK[id] = true
		}
	}
	return &principalLLM{
		admin: a, reg: a.reg, creds: a.creds, oauth: a.oauth, clk: a.clk, log: a.log, audit: a.audit, oauthOK: oauthOK,
	}
}

var (
	principalLLMEventBinding = audit.MustRegister[principalLLMBindingRec](audit.TypeSpec{
		Name:        "providers.principal_llm_binding",
		Description: "a principal bound or cleared an LLM role (ADR 0041); codes only, never a secret",
		Levels:      audit.LevelTenant,
		Required:    []string{"action", "role", "principal_id"},
	})
	principalLLMEventCredential = audit.MustRegister[principalLLMCredRec](audit.TypeSpec{
		Name:        "providers.principal_credential",
		Description: "a principal submitted or revoked their own provider credential (ADR 0041); never the secret",
		Levels:      audit.LevelTenant,
		Required:    []string{"action", "provider_id", "principal_id"},
	})
)

type principalLLMBindingRec struct {
	Action      string `json:"action"`
	Role        string `json:"role"`
	PrincipalID string `json:"principal_id"`
	ProfileID   string `json:"profile_id,omitempty"`
	ProviderID  string `json:"provider_id,omitempty"`
}

type principalLLMCredRec struct {
	Action      string `json:"action"`
	ProviderID  string `json:"provider_id"`
	PrincipalID string `json:"principal_id"`
	Status      string `json:"status,omitempty"`
}

func (s *principalLLM) Catalog(ctx context.Context, scope contracts.Scope) (pv.ProviderCatalog, error) {
	return s.reg.Catalog(ctx, scope)
}

func (s *principalLLM) Get(ctx context.Context, scope contracts.Scope) (pv.PrincipalLlm, error) {
	return s.account(ctx, scope)
}

func (s *principalLLM) PutRole(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole, in gateway.PrincipalLLMBindingInput) (pv.PrincipalLlm, error) {
	if role == "" || !role.Valid() {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "role must be conversational_controller, language_realization, stt or tts")
	}
	if in.ProfileID == "" {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "profile_id is required")
	}
	if in.PrivateEndpointID != "" {
		return s.putPrivateRole(ctx, scope, role, in)
	}
	entry, found, err := s.reg.CatalogEntry(ctx, scope, in.ProfileID)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	if !found {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "the profile is not in the provider catalog").
			WithDetails("reason", "provider_not_in_catalog")
	}
	if !slices.Contains(entry.Roles, role) {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "the catalog entry does not offer this role")
	}
	model := entry.ModelID
	if in.ModelID != "" {
		allowed := entry.ModelIDs
		if len(allowed) == 0 {
			allowed = []pv.ModelID{entry.ModelID}
		}
		if !slices.Contains(allowed, in.ModelID) {
			return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "model_id is not offered by the catalog entry")
		}
		model = in.ModelID
	} else if len(entry.ModelIDs) > 1 {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "model_id is required when the catalog entry lists several models")
	}
	cred, ok, err := s.creds.ActivePrincipalRef(ctx, scope, entry.ProviderID)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	if !ok || cred.Status != pv.CredentialRefStatusActive {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "an active credential for this provider is required before binding")
	}
	acked, _, err := s.reg.PrincipalBillingAcked(ctx, scope, entry.ProviderID, entry.BillingNoticeVersion)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	if !acked {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "the billing notice must be acknowledged before binding")
	}
	bind := providerreg.PrincipalBinding{
		Role: role, ProfileID: entry.ProfileID, ProviderID: entry.ProviderID, ModelID: model,
	}
	if role == pv.PrincipalLlmRoleTTS && in.Voice != nil && *in.Voice != "" {
		bind.Voice = in.Voice
	}
	if (role == pv.PrincipalLlmRoleTTS || role == pv.PrincipalLlmRoleSTT) && in.Language != nil && *in.Language != "" {
		bind.Language = in.Language
	}
	if err := s.reg.PutPrincipalBinding(ctx, scope, bind); err != nil {
		return pv.PrincipalLlm{}, err
	}
	s.auditBinding(ctx, scope, "bound", role, entry.ProfileID, entry.ProviderID)
	return s.account(ctx, scope)
}

func (s *principalLLM) putPrivateRole(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole, in gateway.PrincipalLLMBindingInput) (pv.PrincipalLlm, error) {
	ep, ok, err := s.reg.GetPrivateEndpoint(ctx, scope, in.PrivateEndpointID)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	if !ok {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "the private endpoint does not exist")
	}
	if in.ProfileID != ep.ProfileID {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "profile_id must match the private endpoint's profile_id")
	}
	if err := providerreg.EndpointOwnerPermitsUse(scope, ep); err != nil {
		return pv.PrincipalLlm{}, err
	}
	if !slices.Contains(ep.Roles, role) {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "the endpoint does not offer this role")
	}
	model := ep.ModelIDs[0]
	if in.ModelID != "" {
		if !slices.Contains(ep.ModelIDs, in.ModelID) {
			return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "model_id is not offered by the endpoint")
		}
		model = in.ModelID
	}
	if err := s.reg.PutPrincipalPrivateBinding(ctx, scope, scope.PrincipalID, providerreg.PrincipalPrivateBinding{
		Role: role, EndpointID: in.PrivateEndpointID, ModelID: model,
	}); err != nil {
		return pv.PrincipalLlm{}, err
	}
	if err := s.reg.DeletePrincipalBinding(ctx, scope, role); err != nil {
		return pv.PrincipalLlm{}, err
	}
	s.auditBinding(ctx, scope, "bound_private", role, ep.ProfileID, providerreg.ProviderIDSelfHosted)
	return s.account(ctx, scope)
}

func (s *principalLLM) DeleteRole(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (pv.PrincipalLlm, error) {
	if role == "" || !role.Valid() {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "role must be conversational_controller, language_realization, stt or tts")
	}
	if err := s.reg.DeletePrincipalBinding(ctx, scope, role); err != nil {
		return pv.PrincipalLlm{}, err
	}
	_ = s.reg.DeletePrincipalPrivateBinding(ctx, scope, scope.PrincipalID, role)
	s.auditBinding(ctx, scope, "cleared", role, "", "")
	return s.account(ctx, scope)
}

func (s *principalLLM) SubmitCredential(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in gateway.PrincipalCredentialInput) (gateway.PrincipalCredentialStatus, error) {
	_, err := s.creds.AddPrincipalAPIKey(ctx, scope, credentials.AddAPIKeyRequest{
		ProviderID: provider, Label: in.Label, Key: credentials.Secret(in.Key.Reveal()),
	})
	if err != nil {
		return gateway.PrincipalCredentialStatus{}, err
	}
	st := s.credStatus(ctx, scope, provider)
	s.auditCred(ctx, scope, "submitted", provider, st.Status)
	return st, nil
}

func (s *principalLLM) DeleteCredential(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (gateway.PrincipalCredentialStatus, error) {
	refs, err := s.creds.ListPrincipal(ctx, scope, provider)
	if err != nil {
		return gateway.PrincipalCredentialStatus{}, err
	}
	store := scope
	store.TenantID = credentials.PrincipalCredentialTenant
	for _, r := range refs {
		if r.Status == pv.CredentialRefStatusRevoked {
			continue
		}
		var err error
		if r.Kind == pv.CredentialKindOauth && s.oauth != nil {
			_, err = s.oauth.Disconnect(ctx, store, r.RefID)
		} else {
			_, err = s.creds.Revoke(ctx, store, r.RefID)
		}
		if err != nil {
			return gateway.PrincipalCredentialStatus{}, err
		}
	}
	st := s.credStatus(ctx, scope, provider)
	s.auditCred(ctx, scope, "revoked", provider, st.Status)
	return st, nil
}

func (s *principalLLM) AcknowledgeBillingNotice(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in gateway.PrincipalBillingAckInput) (gateway.PrincipalBillingAckResult, error) {
	cat, err := s.reg.Catalog(ctx, scope)
	if err != nil {
		return gateway.PrincipalBillingAckResult{}, err
	}
	var notice string
	for _, e := range cat.Entries {
		if e.ProviderID == provider {
			notice = e.BillingNoticeVersion
			break
		}
	}
	if notice == "" {
		return gateway.PrincipalBillingAckResult{}, contracts.New(contracts.ErrValidationFailed, "no catalog entry for this provider").
			WithDetails("reason", "provider_not_in_catalog")
	}
	if in.NoticeVersion != notice {
		return gateway.PrincipalBillingAckResult{}, contracts.New(contracts.ErrConflict, "the notice version does not match the catalog").
			WithDetails("reason", "billing_notice_version_mismatch")
	}
	at := s.clk.Now().UTC()
	if err := s.reg.PutPrincipalBillingAck(ctx, scope, provider, in.NoticeVersion, at); err != nil {
		return gateway.PrincipalBillingAckResult{}, err
	}
	return gateway.PrincipalBillingAckResult{ProviderID: string(provider), NoticeVersion: in.NoticeVersion, AcknowledgedAt: at}, nil
}

func (s *principalLLM) OAuthStart(ctx context.Context, scope contracts.Scope, req gateway.PrincipalOAuthStart) (gateway.PrincipalOAuthStarted, error) {
	if s.oauth == nil || !s.oauthOK[string(req.Provider)] {
		return gateway.PrincipalOAuthStarted{}, contracts.New(contracts.ErrValidationFailed, "this provider is connected with a manual credential, not OAuth").
			WithDetails("reason", "manual_fallback")
	}
	store := principalOAuthStoreScope(scope)
	started, err := s.admin.oauthStartWithOptions(ctx, store, gateway.ProvidersOAuthStart{
		Provider: req.Provider, Capabilities: req.Capabilities, RedirectURI: req.RedirectURI,
		Presentation: req.Presentation, Callback: req.Callback, OpenerOrigin: req.OpenerOrigin, Label: req.Label,
	})
	if err != nil {
		return gateway.PrincipalOAuthStarted{}, externalOAuthError(err)
	}
	out := gateway.PrincipalOAuthStarted{
		AuthorizationURL: started.AuthorizationURL, RedirectURI: started.RedirectURI, ExpiresAt: started.ExpiresAt,
		FlowID: started.FlowID, PollIntervalMS: started.PollIntervalMS,
	}
	if c := started.Consent; c.ProviderName != "" || len(c.Scopes) > 0 || len(c.DataAccess) > 0 {
		out.Consent = map[string]any{
			"provider_name": c.ProviderName, "scopes": c.Scopes, "data_access": c.DataAccess,
			"reauthorization": c.Reauthorization, "switch_account": c.SwitchAccount,
		}
	}
	return out, nil
}

// principalOAuthStoreScope remaps the caller's tenant to the reserved principal-credential
// bucket so start/callback/flow status share the same owner (ADR 0041).
func principalOAuthStoreScope(scope contracts.Scope) contracts.Scope {
	out := scope
	out.TenantID = credentials.PrincipalCredentialTenant
	return out
}

func (s *principalLLM) OAuthCallback(ctx context.Context, scope contracts.Scope, cb gateway.PrincipalOAuthCallback) (gateway.PrincipalCredentialStatus, error) {
	if s.oauth == nil {
		return gateway.PrincipalCredentialStatus{}, contracts.New(contracts.ErrUnavailable, "principal LLM accounts are not available on this server").
			WithDetails("reason", "llm_not_configured")
	}
	store := principalOAuthStoreScope(scope)
	conn, err := s.oauth.Complete(ctx, store, oauth.CallbackFromQuery(cb.RedirectURI, cb.Query))
	if err != nil {
		return gateway.PrincipalCredentialStatus{}, externalOAuthError(err)
	}
	provider := conn.Ref.Binding.ProviderID
	s.auditCred(ctx, scope, "oauth_complete", provider, string(conn.Ref.Status))
	return s.credStatus(ctx, scope, provider), nil
}

func (s *principalLLM) OAuthFlowStatus(ctx context.Context, scope contracts.Scope, flowID string) (gateway.PrincipalOAuthFlowStatus, error) {
	if s.oauth == nil {
		return gateway.PrincipalOAuthFlowStatus{}, contracts.New(contracts.ErrUnavailable, "principal LLM accounts are not available on this server").
			WithDetails("reason", "llm_not_configured")
	}
	v, err := s.oauth.FlowGet(ctx, principalOAuthStoreScope(scope), flowID)
	if err != nil {
		return gateway.PrincipalOAuthFlowStatus{}, err
	}
	out := gateway.PrincipalOAuthFlowStatus{
		FlowID: v.FlowID, Provider: string(v.Provider), Status: string(v.Status), UpdatedAt: v.UpdatedAt,
	}
	if v.Reason != "" {
		out.Reason = string(v.Reason)
	}
	return out, nil
}

func (s *principalLLM) OAuthFlowCancel(ctx context.Context, scope contracts.Scope, flowID string) error {
	if s.oauth == nil {
		return contracts.New(contracts.ErrUnavailable, "principal LLM accounts are not available on this server").
			WithDetails("reason", "llm_not_configured")
	}
	return s.oauth.FlowCancel(ctx, principalOAuthStoreScope(scope), flowID)
}

func (s *principalLLM) account(ctx context.Context, scope contracts.Scope) (pv.PrincipalLlm, error) {
	bindings, err := s.reg.ListPrincipalBindings(ctx, scope)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	private, err := s.reg.ListPrincipalPrivateBindings(ctx, scope, scope.PrincipalID)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	privByRole := map[pv.PrincipalLlmRole]providerreg.PrincipalPrivateBinding{}
	for _, b := range private {
		privByRole[b.Role] = b
	}
	cat, err := s.reg.Catalog(ctx, scope)
	if err != nil {
		return pv.PrincipalLlm{}, err
	}
	noticeByProvider := map[pv.ProviderID]string{}
	for _, e := range cat.Entries {
		noticeByProvider[e.ProviderID] = e.BillingNoticeVersion
	}
	out := pv.PrincipalLlm{
		PrincipalID:   common.Identifier(scope.PrincipalID),
		ApplicationID: common.Identifier(scope.ApplicationID),
		Bindings:      []pv.PrincipalLlmBinding{},
		UpdatedAt:     common.Timestamp(s.clk.Now().UTC().Format("2006-01-02T15:04:05.000Z")),
	}
	var latest time.Time
	seenRole := map[pv.PrincipalLlmRole]bool{}
	for _, b := range bindings {
		seenRole[b.Role] = true
		bind := s.bindingView(ctx, scope, b, noticeByProvider)
		bind.Source = ptrSource(pv.PrincipalLlmBindingSourcePrincipalExternal)
		out.Bindings = append(out.Bindings, bind)
		if b.UpdatedAt.After(latest) {
			latest = b.UpdatedAt
		}
	}
	for _, b := range private {
		if seenRole[b.Role] {
			continue
		}
		ep, ok, err := s.reg.GetPrivateEndpoint(ctx, scope, b.EndpointID)
		if err != nil || !ok {
			continue
		}
		st := pv.PrincipalLlmBindingAccountStatusActive
		if ep.CredentialRef != nil {
			st = pv.PrincipalLlmBindingAccountStatus(s.credStatus(ctx, scope, providerreg.ProviderIDSelfHosted).Status)
		}
		src := pv.PrincipalLlmBindingSourcePrincipalPrivate
		if pin, ok, _ := s.reg.GetTenantPrivateLLMPin(ctx, scope, scope.TenantID); ok && pin.EndpointID == b.EndpointID {
			src = pv.PrincipalLlmBindingSourceTenantPin
		}
		lc := ep.Lifecycle
		eid := b.EndpointID
		bind := pv.PrincipalLlmBinding{
			Role: b.Role, ProfileID: ep.ProfileID, PrivateEndpointID: &eid, ProviderID: providerreg.ProviderIDSelfHosted, ModelID: b.ModelID,
			AccountStatus: st, BillingNoticeAcked: true, Source: ptrSource(src), Lifecycle: &lc,
			UpdatedAt: common.Timestamp(b.UpdatedAt.Format("2006-01-02T15:04:05.000Z")),
		}
		out.Bindings = append(out.Bindings, bind)
		if b.UpdatedAt.After(latest) {
			latest = b.UpdatedAt
		}
	}
	if !latest.IsZero() {
		out.UpdatedAt = common.Timestamp(latest.Format("2006-01-02T15:04:05.000Z"))
	}
	return out, nil
}

func (s *principalLLM) bindingView(ctx context.Context, scope contracts.Scope, b providerreg.PrincipalBinding, noticeByProvider map[pv.ProviderID]string) pv.PrincipalLlmBinding {
	st := s.credStatus(ctx, scope, b.ProviderID)
	acked := false
	if notice, ok := noticeByProvider[b.ProviderID]; ok {
		acked, _, _ = s.reg.PrincipalBillingAcked(ctx, scope, b.ProviderID, notice)
	}
	return pv.PrincipalLlmBinding{
		Role: b.Role, ProfileID: b.ProfileID, ProviderID: b.ProviderID, ModelID: b.ModelID,
		Voice: b.Voice, Language: b.Language,
		AccountStatus: pv.PrincipalLlmBindingAccountStatus(st.Status), BillingNoticeAcked: acked,
		UpdatedAt: common.Timestamp(b.UpdatedAt.Format("2006-01-02T15:04:05.000Z")),
	}
}

func ptrSource(s pv.PrincipalLlmBindingSource) *pv.PrincipalLlmBindingSource { return &s }

func (s *principalLLM) credStatus(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) gateway.PrincipalCredentialStatus {
	refs, err := s.creds.ListPrincipal(ctx, scope, provider)
	if err != nil || len(refs) == 0 {
		return gateway.PrincipalCredentialStatus{ProviderID: string(provider), Status: "missing"}
	}
	best, ok, _ := s.creds.ActivePrincipalRef(ctx, scope, provider)
	if ok {
		return gateway.PrincipalCredentialStatus{ProviderID: string(provider), Status: string(best.Status), Account: safeAccountMap(best.Account)}
	}
	// Prefer the most recent non-missing status.
	st := refs[len(refs)-1]
	return gateway.PrincipalCredentialStatus{ProviderID: string(provider), Status: string(st.Status), Account: safeAccountMap(st.Account)}
}

func safeAccountMap(a pv.SafeAccount) map[string]any {
	out := map[string]any{}
	if a.AccountID != nil {
		out["account_id"] = *a.AccountID
	}
	if a.OrganizationID != nil {
		out["organization_id"] = *a.OrganizationID
	}
	if a.ProjectID != nil {
		out["project_id"] = *a.ProjectID
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *principalLLM) auditBinding(ctx context.Context, scope contracts.Scope, action string, role pv.PrincipalLlmRole, profile pv.ProfileID, provider pv.ProviderID) {
	loc, err := audit.ForScope(scope)
	if err != nil || s.audit == nil {
		return
	}
	_, _ = principalLLMEventBinding.Append(ctx, s.audit, loc, principalLLMBindingRec{
		Action: action, Role: string(role), PrincipalID: scope.PrincipalID,
		ProfileID: string(profile), ProviderID: string(provider),
	})
}

func (s *principalLLM) auditCred(ctx context.Context, scope contracts.Scope, action string, provider pv.ProviderID, status string) {
	loc, err := audit.ForScope(scope)
	if err != nil || s.audit == nil {
		return
	}
	_, _ = principalLLMEventCredential.Append(ctx, s.audit, loc, principalLLMCredRec{
		Action: action, ProviderID: string(provider), PrincipalID: scope.PrincipalID, Status: status,
	})
}

// Adapters for modes.PrincipalPolicy / PrincipalProfiles.

func (s *principalLLM) PrincipalBindingFor(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (pv.ProfileID, bool, error) {
	b, ok, err := s.reg.PrincipalBindingFor(ctx, scope, role)
	if err != nil || !ok {
		return "", ok, err
	}
	return b.ProfileID, true, nil
}

func (s *principalLLM) BindingLookup(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrincipalBinding, bool, error) {
	return s.reg.PrincipalBindingFor(ctx, scope, role)
}

func (s *principalLLM) ActivePrincipalCred(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (pv.CredentialRefID, bool, error) {
	ref, ok, err := s.creds.ActivePrincipalRef(ctx, scope, provider)
	if err != nil || !ok {
		return "", ok, err
	}
	return ref.RefID, true, nil
}

// principalBindingAdapter adapts *principalLLM to modes.PrincipalBindingLookup.
type principalBindingAdapter struct{ s *principalLLM }

func (a principalBindingAdapter) PrincipalBindingFor(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrincipalBinding, bool, error) {
	return a.s.reg.PrincipalBindingFor(ctx, scope, role)
}

// principalPolicyBinding adapts *principalLLM to modes.PrincipalBindingSource.
type principalPolicyBinding struct{ s *principalLLM }

func (a principalPolicyBinding) PrincipalBindingFor(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (pv.ProfileID, bool, error) {
	return a.s.PrincipalBindingFor(ctx, scope, role)
}

// ErasePrincipalLLM is the retention hook (ADR 0041 / ADR 0036): revoke principal credentials and
// delete bindings. Wired from `retention erase -principal`.
func ErasePrincipalLLM(ctx context.Context, s *principalLLM, scope contracts.Scope, principal string) (creds, bindings int, err error) {
	if s == nil {
		return 0, 0, nil
	}
	n, err := s.creds.ErasePrincipalCredentials(ctx, scope, principal)
	if err != nil {
		return n, 0, err
	}
	b, err := s.reg.ErasePrincipalBindings(ctx, scope, principal)
	if err != nil {
		return n, b, err
	}
	pb, err := s.reg.ErasePrincipalPrivateBindings(ctx, scope, principal)
	return n, b + pb, err
}

// Ensure *principalLLM implements the gateway interface.
var _ gateway.PrincipalLLMService = (*principalLLM)(nil)
var _ modes.PrincipalCredSource = (*principalLLM)(nil)
