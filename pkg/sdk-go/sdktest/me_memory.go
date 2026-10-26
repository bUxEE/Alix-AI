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
	c.json(http.StatusOK, pv.PrincipalLlm{
		PrincipalID:   common.Identifier(c.claims.PrincipalID),
		ApplicationID: common.Identifier(c.claims.ApplicationID),
		Bindings: []pv.PrincipalLlmBinding{{
			Role:               pv.PrincipalLlmRole(role),
			ProfileID:          body.ProfileID,
			ProviderID:         "openai",
			ModelID:            model,
			AccountStatus:      pv.PrincipalLlmBindingAccountStatusMissing,
			BillingNoticeAcked: false,
			UpdatedAt:          now,
		}},
		UpdatedAt: now,
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
	var body sdkgo.PrincipalBillingAck
	if !c.providersDecode(&body) {
		return
	}
	if body.NoticeVersion == "" || !body.ConsumerSubscriptionIsNotAPIAccess || !body.SeparateAPIBillingUnderstood {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "billing notice acknowledgement is incomplete")
		return
	}
	c.json(http.StatusOK, sdkgo.PrincipalBillingAckResult{
		ProviderID:     c.param("provider"),
		NoticeVersion:  body.NoticeVersion,
		AcknowledgedAt: time.Now().UTC(),
	})
}

func (c *call) meOAuthStart() {
	var body sdkgo.PrincipalOAuthStart
	if !c.providersDecode(&body) {
		return
	}
	if len(body.Capabilities) == 0 || body.RedirectURI == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "capabilities and redirect_uri are required")
		return
	}
	c.json(http.StatusOK, sdkgo.PrincipalOAuthStarted{
		AuthorizationURL: "https://example.test/oauth",
		RedirectURI:      body.RedirectURI,
		ExpiresAt:        time.Now().UTC().Add(time.Minute),
		FlowID:           "oaf_01J0000000000000000000000A",
		PollIntervalMS:   1500,
	})
}

func (c *call) meOAuthCallback() {
	var body sdkgo.PrincipalOAuthCallback
	if !c.providersDecode(&body) {
		return
	}
	if body.RedirectURI == "" || body.Query == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "redirect_uri and query are required")
		return
	}
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
