package sdktest

import (
	"net/http"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

func (c *call) meCatalog() {
	c.json(http.StatusOK, pv.ProviderCatalog{Entries: []pv.ProviderCatalogEntry{}})
}

func (c *call) meGetLLM() {
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	c.json(http.StatusOK, pv.PrincipalLlm{
		PrincipalID:   common.Identifier(c.claims.PrincipalID),
		ApplicationID: common.Identifier(c.claims.ApplicationID),
		Bindings:      []pv.PrincipalLlmBinding{},
		UpdatedAt:     now,
	})
}

func (c *call) mePutRole() {
	role := c.param("role")
	switch role {
	case "conversational_controller", "language_realization", "stt", "tts":
	default:
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "role is not a principal LLM role")
		return
	}
	var body pv.PrincipalLlmBindingUpdate
	if !c.providersDecode(&body) {
		return
	}
	if body.ProfileID == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "profile_id is required")
		return
	}
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	model := pv.ModelID("gpt-test")
	if body.ModelID != nil && *body.ModelID != "" {
		model = *body.ModelID
	}
	b := pv.PrincipalLlmBinding{
		Role:               pv.PrincipalLlmRole(role),
		ProfileID:          body.ProfileID,
		PrivateEndpointID:  body.PrivateEndpointID,
		ProviderID:         "openai",
		ModelID:            model,
		AccountStatus:      pv.PrincipalLlmBindingAccountStatusMissing,
		BillingNoticeAcked: false,
		UpdatedAt:          now,
	}
	if body.PrivateEndpointID != nil && *body.PrivateEndpointID != "" {
		b.ProviderID = "self_hosted"
		src := pv.PrincipalLlmBindingSourcePrincipalPrivate
		b.Source = &src
		b.BillingNoticeAcked = true
	}
	c.json(http.StatusOK, pv.PrincipalLlm{
		PrincipalID:   common.Identifier(c.claims.PrincipalID),
		ApplicationID: common.Identifier(c.claims.ApplicationID),
		Bindings:      []pv.PrincipalLlmBinding{b},
		UpdatedAt:     now,
	})
}

func (c *call) meDeleteRole() {
	c.meGetLLM()
}

func (c *call) mePutCredential() {
	provider := c.param("provider")
	var body struct {
		Key string `json:"key"`
	}
	if !c.providersDecode(&body) {
		return
	}
	if body.Key == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "key is required")
		return
	}
	c.json(http.StatusOK, sdkgo.PrincipalCredentialStatus{ProviderID: provider, Status: "active"})
}

func (c *call) meDeleteCredential() {
	c.json(http.StatusOK, sdkgo.PrincipalCredentialStatus{ProviderID: c.param("provider"), Status: "revoked"})
}

func (c *call) meAckBilling() {
	provider := c.param("provider")
	var body sdkgo.PrincipalBillingAck
	if !c.providersDecode(&body) {
		return
	}
	c.json(http.StatusOK, sdkgo.PrincipalBillingAckResult{
		ProviderID: provider, NoticeVersion: body.NoticeVersion, AcknowledgedAt: time.Now().UTC(),
	})
}

func (c *call) meOAuthStart() {
	c.json(http.StatusOK, sdkgo.PrincipalOAuthStarted{
		AuthorizationURL: "https://example.test/oauth",
		RedirectURI:      "https://host.example/callback",
		ExpiresAt:        time.Now().UTC().Add(time.Minute),
		FlowID:           "oaf_01J0000000000000000000000A",
	})
}

func (c *call) meOAuthCallback() {
	c.json(http.StatusOK, sdkgo.PrincipalCredentialStatus{ProviderID: "openai", Status: "active"})
}

func (c *call) meOAuthFlowStatus() {
	c.json(http.StatusOK, sdkgo.PrincipalOAuthFlowStatus{
		FlowID: c.param("flow_id"), Provider: "openai", Status: "pending", UpdatedAt: time.Now().UTC(),
	})
}

func (c *call) meOAuthFlowCancel() {
	c.w.WriteHeader(http.StatusNoContent)
}

func (c *call) opsStartErasure() {
	var body sdkgo.ErasureRequest
	if !c.providersDecode(&body) {
		return
	}
	if body.Level == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "level is required")
		return
	}
	c.json(http.StatusAccepted, sdkgo.ErasureJobAccepted{JobID: "erj_01J0000000000000000000000A", Status: "queued"})
}

func (c *call) opsGetErasure() {
	now := time.Now().UTC()
	c.json(http.StatusOK, sdkgo.ErasureJobStatus{
		JobID: c.param("job_id"), Status: "completed", Level: "principal",
		TenantID: c.claims.TenantID, PrincipalID: "usr_alice",
		Counts: map[string]any{"audit_events_erased": 1, "sessions_removed": 1}, FinishedAt: &now,
	})
}

func (c *call) meUsage() {
	c.json(http.StatusOK, pv.MeUsage{Groups: []pv.MeUsageGroup{}})
}

func (c *call) meBudget() {
	c.json(http.StatusOK, pv.MeBudget{Budgets: []pv.Budget{}, States: []pv.BudgetState{}})
}

func (c *call) memoryList() {
	c.json(http.StatusOK, mem.MemoryList{Items: []mem.MemoryItem{}})
}

func (c *call) memoryCreate() {
	var body mem.MemoryCreate
	if !c.providersDecode(&body) {
		return
	}
	if body.Text == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "text is required")
		return
	}
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	c.json(http.StatusCreated, mem.MemoryItem{
		ID:            "mem_01J0000000000000000000000A",
		ApplicationID: common.Identifier(c.claims.ApplicationID),
		TenantID:      common.Identifier(c.claims.TenantID),
		WorkspaceID:   common.Identifier(c.claims.WorkspaceID),
		PrincipalID:   common.Identifier(c.claims.PrincipalID),
		Text:          body.Text,
		Origin:        mem.MemoryOriginUserExplicit,
		Status:        mem.MemoryStatusActive,
		DataClass:     mem.MemoryItemDataClassInternal,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

func (c *call) memoryPatch() {
	id := c.param("id")
	var body mem.MemoryPatch
	if !c.providersDecode(&body) {
		return
	}
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	text := mem.MemoryText("updated")
	if body.Text != nil {
		text = *body.Text
	}
	c.json(http.StatusOK, mem.MemoryItem{
		ID:            mem.MemoryID(id),
		ApplicationID: common.Identifier(c.claims.ApplicationID),
		TenantID:      common.Identifier(c.claims.TenantID),
		WorkspaceID:   common.Identifier(c.claims.WorkspaceID),
		PrincipalID:   common.Identifier(c.claims.PrincipalID),
		Text:          text,
		Origin:        mem.MemoryOriginUserExplicit,
		Status:        mem.MemoryStatusActive,
		DataClass:     mem.MemoryItemDataClassInternal,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

func (c *call) memoryDelete() {
	c.w.WriteHeader(http.StatusNoContent)
}

func (c *call) memoryConfirm() { c.memoryDecide(mem.MemoryStatusActive) }
func (c *call) memoryReject()  { c.memoryDecide(mem.MemoryStatusRejected) }

func (c *call) memoryDecide(st mem.MemoryStatus) {
	id := c.param("id")
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	c.json(http.StatusOK, mem.MemoryItem{
		ID:            mem.MemoryID(id),
		ApplicationID: common.Identifier(c.claims.ApplicationID),
		TenantID:      common.Identifier(c.claims.TenantID),
		WorkspaceID:   common.Identifier(c.claims.WorkspaceID),
		PrincipalID:   common.Identifier(c.claims.PrincipalID),
		Text:          "proposed fact",
		Origin:        mem.MemoryOriginAssistantProposed,
		Status:        st,
		DataClass:     mem.MemoryItemDataClassInternal,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

func (c *call) opsPutPrivateLlmEndpoint() {
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	id := pv.PrivateEndpointID(c.param("endpoint_id"))
	c.json(http.StatusOK, pv.PrivateLlmEndpoint{
		EndpointID: id, Origin: "http://127.0.0.1:8080", APIDialect: "openai-compatible",
		ModelIDs: []pv.ModelID{"local-model"}, Roles: []pv.PrincipalLlmRole{pv.PrincipalLlmRoleConversationalController},
		OwnerKind: pv.PrivateEndpointOwnerKindShared, Lifecycle: pv.PrivateLlmLifecycleReady, UpdatedAt: now,
	})
}
func (c *call) opsGetPrivateLlmEndpoint()    { c.opsPutPrivateLlmEndpoint() }
func (c *call) opsDeletePrivateLlmEndpoint() { c.w.WriteHeader(http.StatusNoContent) }
func (c *call) opsPutTenantPrivateLlm() {
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	c.json(http.StatusOK, pv.PrivateLlmTenantPin{TenantID: common.Identifier(c.param("tenant_id")), EndpointID: "ple_01J0000000000000000000000A", UpdatedAt: now})
}
func (c *call) opsGetTenantPrivateLlm()    { c.opsPutTenantPrivateLlm() }
func (c *call) opsDeleteTenantPrivateLlm() { c.w.WriteHeader(http.StatusNoContent) }
func (c *call) opsPutPrincipalPrivateLlm() {
	now := common.Timestamp(time.Now().UTC().Format(time.RFC3339))
	c.json(http.StatusOK, pv.PrivateLlmPrincipalBinding{
		PrincipalID: common.Identifier(c.param("principal_id")), EndpointID: "ple_01J0000000000000000000000A",
		Roles: []pv.PrincipalLlmRole{pv.PrincipalLlmRoleConversationalController}, UpdatedAt: now,
	})
}
func (c *call) opsGetPrincipalPrivateLlm()    { c.opsPutPrincipalPrivateLlm() }
func (c *call) opsDeletePrincipalPrivateLlm() { c.w.WriteHeader(http.StatusNoContent) }
