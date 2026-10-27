package sdkgo

import "net/http"

// Operation is one operation of the public API the SDK speaks, keyed by the operationId of
// schemas/api/openapi.yaml. The table below is the single place of this package that knows a method and
// a path: the client builds its requests from it and the fake server (sdktest) routes by it.
// contract_test.go reads the OpenAPI document and fails when a row here and the document disagree.
type Operation struct {
	// ID is the operationId of the OpenAPI document.
	ID string
	// Method is the HTTP method.
	Method string
	// Path is the OpenAPI path template, with {name} placeholders.
	Path string
	// Authenticated is false for the unauthenticated probes: no context token is requested and none is sent.
	Authenticated bool
	// Safe is true when sending the request again is always safe: a read, and the two RAG operations that
	// only read (search and answer). A transport failure of a safe request is `unavailable` or `timeout`;
	// of any other it is `uncertain_outcome`, because the request may have been carried out.
	Safe bool
}

var (
	opGetLiveness  = Operation{"getLiveness", http.MethodGet, "/healthz", false, true}
	opGetReadiness = Operation{"getReadiness", http.MethodGet, "/readyz", false, true}
	opGetHealth    = Operation{"getHealth", http.MethodGet, "/v1/health", true, true}

	opCreateSession   = Operation{"createSession", http.MethodPost, "/v1/sessions", true, false}
	opGetSession      = Operation{"getSession", http.MethodGet, "/v1/sessions/{session_id}", true, true}
	opEndSession      = Operation{"endSession", http.MethodDelete, "/v1/sessions/{session_id}", true, false}
	opGetPendingState = Operation{"getPendingState", http.MethodGet, "/v1/sessions/{session_id}/pending", true, true}
	opSendTurn        = Operation{"sendTurn", http.MethodPost, "/v1/sessions/{session_id}/turns", true, false}
	opStreamTurnEvent = Operation{"streamTurnEvents", http.MethodGet, "/v1/sessions/{session_id}/turns/{turn_id}/events", true, true}

	opGetAction        = Operation{"getAction", http.MethodGet, "/v1/actions/{preparation_id}", true, true}
	opConfirmAction    = Operation{"confirmAction", http.MethodPost, "/v1/actions/{preparation_id}/confirm", true, false}
	opRejectAction     = Operation{"rejectAction", http.MethodPost, "/v1/actions/{preparation_id}/reject", true, false}
	opGetActionReceipt = Operation{"getActionReceipt", http.MethodGet, "/v1/actions/{preparation_id}/receipt", true, true}

	opRegisterApplication = Operation{"registerApplication", http.MethodPost, "/v1/applications", true, false}
	opGetApplication      = Operation{"getApplication", http.MethodGet, "/v1/applications/{application_id}", true, true}
	opListCapabilities    = Operation{"listCapabilities", http.MethodGet, "/v1/capabilities", true, true}

	// Dictation (record-to-text, 1.1.0): not the voice WebSocket.
	opGetDictationCapabilities = Operation{"getDictationCapabilities", http.MethodGet, "/v1/dictations/capabilities", true, true}
	opCreateSessionDictation   = Operation{"createSessionDictation", http.MethodPost, "/v1/sessions/{session_id}/dictations", true, false}

	opRegisterRagSource       = Operation{"registerRagSource", http.MethodPost, "/v1/rag/sources", true, false}
	opListRagSources          = Operation{"listRagSources", http.MethodGet, "/v1/rag/sources", true, true}
	opReauditRagSources       = Operation{"reauditRagSources", http.MethodPost, "/v1/rag/sources/reaudit", true, false}
	opGetRagSource            = Operation{"getRagSource", http.MethodGet, "/v1/rag/sources/{source_id}", true, true}
	opRemoveRagSource         = Operation{"removeRagSource", http.MethodDelete, "/v1/rag/sources/{source_id}", true, false}
	opSyncRagSource           = Operation{"syncRagSource", http.MethodPost, "/v1/rag/sources/{source_id}/sync", true, false}
	opRebuildRagSource        = Operation{"rebuildRagSource", http.MethodPost, "/v1/rag/sources/{source_id}/rebuild", true, false}
	opListRagGenerations      = Operation{"listRagGenerations", http.MethodGet, "/v1/rag/sources/{source_id}/generations", true, true}
	opInvalidateRagGeneration = Operation{"invalidateRagGeneration", http.MethodPost, "/v1/rag/sources/{source_id}/generations/{generation_id}/invalidate", true, false}
	opSearchRag               = Operation{"searchRag", http.MethodPost, "/v1/rag/search", true, true}
	opAnswerRag               = Operation{"answerRag", http.MethodPost, "/v1/rag/answer", true, true}
	opResolveRagCitation      = Operation{"resolveRagCitation", http.MethodGet, "/v1/rag/citations/{file_id}", true, true}

	// The decision layer is read-only here: only the status (the operator-only configuration is not spoken).
	opGetDecisionStatus = Operation{"getDecisionStatus", http.MethodGet, "/v1/decision/status", true, true}

	// External Provider Mode (milestone X): the operator-facing providers area and the OAuth start and callback. Every
	// route answers `unavailable` when the deployment has no providers service (external mode is off by default).
	opListProviders           = Operation{"listProviders", http.MethodGet, "/v1/providers", true, true}
	opPutProviderCredential   = Operation{"putProviderCredential", http.MethodPut, "/v1/providers/{provider}/credential", true, false}
	opDeleteProviderConn      = Operation{"deleteProviderConnection", http.MethodDelete, "/v1/providers/{provider}/connection", true, false}
	opAckProviderBillingNote  = Operation{"ackProviderBillingNotice", http.MethodPost, "/v1/providers/{provider}/billing-notice/ack", true, false}
	opGetProviderDisclosure   = Operation{"getProviderDisclosure", http.MethodGet, "/v1/providers/{provider}/disclosure", true, true}
	opGetProviderMode         = Operation{"getProviderMode", http.MethodGet, "/v1/providers/mode", true, true}
	opPutProviderMode         = Operation{"putProviderMode", http.MethodPut, "/v1/providers/mode", true, false}
	opGetProviderEgressPolicy = Operation{"getProviderEgressPolicy", http.MethodGet, "/v1/providers/egress-policy", true, true}
	opPutProviderEgressPolicy = Operation{"putProviderEgressPolicy", http.MethodPut, "/v1/providers/egress-policy", true, false}
	opGetProviderBudgets      = Operation{"getProviderBudgets", http.MethodGet, "/v1/providers/budgets", true, true}
	opPutProviderBudget       = Operation{"putProviderBudget", http.MethodPut, "/v1/providers/budgets", true, false}
	opExportProviderUsage     = Operation{"exportProviderUsage", http.MethodGet, "/v1/providers/usage/export", true, true}
	opGetProviderKillSwitch   = Operation{"getProviderKillSwitch", http.MethodGet, "/v1/providers/kill-switch", true, true}
	opPutProviderKillSwitch   = Operation{"putProviderKillSwitch", http.MethodPut, "/v1/providers/kill-switch", true, false}
	opStartProviderOAuth      = Operation{"startProviderOAuth", http.MethodPost, "/v1/oauth/{provider}/start", true, false}
	opCompleteProviderOAuth   = Operation{"completeProviderOAuth", http.MethodPost, "/v1/oauth/callback", true, false}
	opGetProviderOAuthFlow    = Operation{"getProviderOAuthFlow", http.MethodGet, "/v1/oauth/flows/{flow_id}", true, true}
	opCancelProviderOAuthFlow = Operation{"cancelProviderOAuthFlow", http.MethodDelete, "/v1/oauth/flows/{flow_id}", true, false}

	// Per-principal LLM account and catalog (ADR 0041, 1.2.0).
	opGetProviderCatalog                = Operation{"getProviderCatalog", http.MethodGet, "/v1/providers/catalog", true, true}
	opGetPrincipalLlm                   = Operation{"getPrincipalLlm", http.MethodGet, "/v1/me/llm", true, true}
	opPutPrincipalLlmRole               = Operation{"putPrincipalLlmRole", http.MethodPut, "/v1/me/llm/{role}", true, false}
	opDeletePrincipalLlmRole            = Operation{"deletePrincipalLlmRole", http.MethodDelete, "/v1/me/llm/{role}", true, false}
	opPutPrincipalProviderCredential    = Operation{"putPrincipalProviderCredential", http.MethodPut, "/v1/me/providers/{provider}/credential", true, false}
	opDeletePrincipalProviderCredential = Operation{"deletePrincipalProviderCredential", http.MethodDelete, "/v1/me/providers/{provider}/credential", true, false}
	opAckPrincipalProviderBillingNotice = Operation{"ackPrincipalProviderBillingNotice", http.MethodPost, "/v1/me/providers/{provider}/billing-notice/ack", true, false}
	opStartPrincipalOAuth               = Operation{"startPrincipalOAuth", http.MethodPost, "/v1/me/oauth/{provider}/start", true, false}
	opCompletePrincipalOAuth            = Operation{"completePrincipalOAuth", http.MethodPost, "/v1/me/oauth/callback", true, false}
	opGetPrincipalOAuthFlow             = Operation{"getPrincipalOAuthFlow", http.MethodGet, "/v1/me/oauth/flows/{flow_id}", true, true}
	opCancelPrincipalOAuthFlow          = Operation{"cancelPrincipalOAuthFlow", http.MethodDelete, "/v1/me/oauth/flows/{flow_id}", true, false}
	opStartErasure                      = Operation{"startErasure", http.MethodPost, "/v1/ops/erasure", true, false}
	opGetErasureJob                     = Operation{"getErasureJob", http.MethodGet, "/v1/ops/erasure/{job_id}", true, true}
	opPutPrivateLlmEndpoint             = Operation{"putPrivateLlmEndpoint", http.MethodPut, "/v1/ops/private-llm/endpoints/{endpoint_id}", true, false}
	opGetPrivateLlmEndpoint             = Operation{"getPrivateLlmEndpoint", http.MethodGet, "/v1/ops/private-llm/endpoints/{endpoint_id}", true, true}
	opDeletePrivateLlmEndpoint          = Operation{"deletePrivateLlmEndpoint", http.MethodDelete, "/v1/ops/private-llm/endpoints/{endpoint_id}", true, false}
	opPutTenantPrivateLlm               = Operation{"putTenantPrivateLlm", http.MethodPut, "/v1/ops/tenants/{tenant_id}/private-llm", true, false}
	opGetTenantPrivateLlm               = Operation{"getTenantPrivateLlm", http.MethodGet, "/v1/ops/tenants/{tenant_id}/private-llm", true, true}
	opDeleteTenantPrivateLlm            = Operation{"deleteTenantPrivateLlm", http.MethodDelete, "/v1/ops/tenants/{tenant_id}/private-llm", true, false}
	opPutPrincipalPrivateLlm            = Operation{"putPrincipalPrivateLlm", http.MethodPut, "/v1/ops/principals/{principal_id}/private-llm", true, false}
	opGetPrincipalPrivateLlm            = Operation{"getPrincipalPrivateLlm", http.MethodGet, "/v1/ops/principals/{principal_id}/private-llm", true, true}
	opDeletePrincipalPrivateLlm         = Operation{"deletePrincipalPrivateLlm", http.MethodDelete, "/v1/ops/principals/{principal_id}/private-llm", true, false}
	opGetMeUsage                        = Operation{"getMeUsage", http.MethodGet, "/v1/me/usage", true, true}
	opGetMeBudget                       = Operation{"getMeBudget", http.MethodGet, "/v1/me/budget", true, true}

	// Long-term memory (ADR 0042, 1.2.0).
	opListMemory    = Operation{"listMemory", http.MethodGet, "/v1/memory", true, true}
	opCreateMemory  = Operation{"createMemory", http.MethodPost, "/v1/memory", true, false}
	opPatchMemory   = Operation{"patchMemory", http.MethodPatch, "/v1/memory/{id}", true, false}
	opDeleteMemory  = Operation{"deleteMemory", http.MethodDelete, "/v1/memory/{id}", true, false}
	opConfirmMemory = Operation{"confirmMemory", http.MethodPost, "/v1/memory/{id}/confirm", true, false}
	opRejectMemory  = Operation{"rejectMemory", http.MethodPost, "/v1/memory/{id}/reject", true, false}

	// Downloadable exports from verified turn blocks (1.2.0).
	opCreateExport = Operation{"createExport", http.MethodPost, "/v1/sessions/{session_id}/exports", true, false}
)

// Operations lists every operation of the public API this SDK speaks: the root probes, sessions (including
// the resumable event stream), actions, applications, the RAG area and the decision-layer status.
func Operations() []Operation {
	return []Operation{
		opGetLiveness, opGetReadiness, opGetHealth,
		opCreateSession, opGetSession, opEndSession, opGetPendingState, opSendTurn, opStreamTurnEvent,
		opGetAction, opConfirmAction, opRejectAction, opGetActionReceipt,
		opRegisterApplication, opGetApplication, opListCapabilities,
		opGetDictationCapabilities, opCreateSessionDictation,
		opRegisterRagSource, opListRagSources, opReauditRagSources, opGetRagSource, opRemoveRagSource,
		opSyncRagSource, opRebuildRagSource, opListRagGenerations, opInvalidateRagGeneration,
		opSearchRag, opAnswerRag, opResolveRagCitation,
		opGetDecisionStatus,
		opListProviders, opPutProviderCredential, opDeleteProviderConn, opAckProviderBillingNote, opGetProviderDisclosure,
		opGetProviderMode, opPutProviderMode, opGetProviderEgressPolicy, opPutProviderEgressPolicy,
		opGetProviderBudgets, opPutProviderBudget, opExportProviderUsage, opGetProviderKillSwitch, opPutProviderKillSwitch,
		opStartProviderOAuth, opCompleteProviderOAuth, opGetProviderOAuthFlow, opCancelProviderOAuthFlow,
		opGetProviderCatalog, opGetPrincipalLlm, opPutPrincipalLlmRole, opDeletePrincipalLlmRole,
		opPutPrincipalProviderCredential, opDeletePrincipalProviderCredential, opAckPrincipalProviderBillingNotice, opStartPrincipalOAuth,
		opCompletePrincipalOAuth, opGetPrincipalOAuthFlow, opCancelPrincipalOAuthFlow,
		opStartErasure, opGetErasureJob,
		opPutPrivateLlmEndpoint, opGetPrivateLlmEndpoint, opDeletePrivateLlmEndpoint,
		opPutTenantPrivateLlm, opGetTenantPrivateLlm, opDeleteTenantPrivateLlm,
		opPutPrincipalPrivateLlm, opGetPrincipalPrivateLlm, opDeletePrincipalPrivateLlm,
		opGetMeUsage, opGetMeBudget,
		opListMemory, opCreateMemory, opPatchMemory, opDeleteMemory, opConfirmMemory, opRejectMemory,
		opCreateExport,
	}
}
