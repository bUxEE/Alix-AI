"""The operations of the public API the SDK speaks, keyed by the operationId of schemas/api/openapi.yaml.

This table is the single place that knows a method and a path: the clients build their requests from it and the fake
server routes by it. tests/test_openapi_contract.py reads the OpenAPI document and fails when a row here and the
document disagree.
"""

from __future__ import annotations

import re
from typing import Mapping, NamedTuple, Optional
from urllib.parse import quote, unquote


class Operation(NamedTuple):
    method: str
    #: The OpenAPI path template, with ``{name}`` placeholders.
    path: str
    #: False for the unauthenticated probes: no context token is requested and none is sent.
    authenticated: bool


OPERATIONS: Mapping[str, Operation] = {
    "getLiveness": Operation("GET", "/healthz", False),
    "getReadiness": Operation("GET", "/readyz", False),
    "getHealth": Operation("GET", "/v1/health", True),
    "createSession": Operation("POST", "/v1/sessions", True),
    "getSession": Operation("GET", "/v1/sessions/{session_id}", True),
    "endSession": Operation("DELETE", "/v1/sessions/{session_id}", True),
    "getPendingState": Operation("GET", "/v1/sessions/{session_id}/pending", True),
    "sendTurn": Operation("POST", "/v1/sessions/{session_id}/turns", True),
    "reportStepUpOutcome": Operation(
        "POST", "/v1/sessions/{session_id}/preparations/{preparation_id}/step-up-outcome", True
    ),
    "getAction": Operation("GET", "/v1/actions/{preparation_id}", True),
    "confirmAction": Operation("POST", "/v1/actions/{preparation_id}/confirm", True),
    "rejectAction": Operation("POST", "/v1/actions/{preparation_id}/reject", True),
    "getActionReceipt": Operation("GET", "/v1/actions/{preparation_id}/receipt", True),
    "registerApplication": Operation("POST", "/v1/applications", True),
    "getApplication": Operation("GET", "/v1/applications/{application_id}", True),
    "listCapabilities": Operation("GET", "/v1/capabilities", True),
    # Dictation (record-to-text, 1.1.0): not the voice WebSocket; paths use "dictations".
    "getDictationCapabilities": Operation("GET", "/v1/dictations/capabilities", True),
    "createSessionDictation": Operation("POST", "/v1/sessions/{session_id}/dictations", True),
    # The decision layer is read-only here: only the status (the operator-only configuration is not spoken).
    "getDecisionStatus": Operation("GET", "/v1/decision/status", True),
    # External Provider Mode (milestone X): the operator-facing providers area and the OAuth start and callback. Every route
    # answers `unavailable` when the deployment has no providers service (external mode is off by default).
    "listProviders": Operation("GET", "/v1/providers", True),
    "putProviderCredential": Operation("PUT", "/v1/providers/{provider}/credential", True),
    "deleteProviderConnection": Operation("DELETE", "/v1/providers/{provider}/connection", True),
    "ackProviderBillingNotice": Operation("POST", "/v1/providers/{provider}/billing-notice/ack", True),
    "getProviderDisclosure": Operation("GET", "/v1/providers/{provider}/disclosure", True),
    "getProviderMode": Operation("GET", "/v1/providers/mode", True),
    "putProviderMode": Operation("PUT", "/v1/providers/mode", True),
    "getProviderEgressPolicy": Operation("GET", "/v1/providers/egress-policy", True),
    "putProviderEgressPolicy": Operation("PUT", "/v1/providers/egress-policy", True),
    "getProviderBudgets": Operation("GET", "/v1/providers/budgets", True),
    "putProviderBudget": Operation("PUT", "/v1/providers/budgets", True),
    "exportProviderUsage": Operation("GET", "/v1/providers/usage/export", True),
    "getProviderKillSwitch": Operation("GET", "/v1/providers/kill-switch", True),
    "putProviderKillSwitch": Operation("PUT", "/v1/providers/kill-switch", True),
    "startProviderOAuth": Operation("POST", "/v1/oauth/{provider}/start", True),
    "completeProviderOAuth": Operation("POST", "/v1/oauth/callback", True),
    "getProviderOAuthFlow": Operation("GET", "/v1/oauth/flows/{flow_id}", True),
    "cancelProviderOAuthFlow": Operation("DELETE", "/v1/oauth/flows/{flow_id}", True),
    # Per-principal LLM (ADR 0041) and long-term memory (ADR 0042).
    "getProviderCatalog": Operation("GET", "/v1/providers/catalog", True),
    "getPrincipalLlm": Operation("GET", "/v1/me/llm", True),
    "putPrincipalLlmRole": Operation("PUT", "/v1/me/llm/{role}", True),
    "deletePrincipalLlmRole": Operation("DELETE", "/v1/me/llm/{role}", True),
    "putPrincipalProviderCredential": Operation("PUT", "/v1/me/providers/{provider}/credential", True),
    "deletePrincipalProviderCredential": Operation("DELETE", "/v1/me/providers/{provider}/credential", True),
    "ackPrincipalProviderBillingNotice": Operation("POST", "/v1/me/providers/{provider}/billing-notice/ack", True),
    "startPrincipalOAuth": Operation("POST", "/v1/me/oauth/{provider}/start", True),
    "completePrincipalOAuth": Operation("POST", "/v1/me/oauth/callback", True),
    "getPrincipalOAuthFlow": Operation("GET", "/v1/me/oauth/flows/{flow_id}", True),
    "cancelPrincipalOAuthFlow": Operation("DELETE", "/v1/me/oauth/flows/{flow_id}", True),
    "startErasure": Operation("POST", "/v1/ops/erasure", True),
    "getErasureJob": Operation("GET", "/v1/ops/erasure/{job_id}", True),
    "putPrivateLlmEndpoint": Operation("PUT", "/v1/ops/private-llm/endpoints/{endpoint_id}", True),
    "getPrivateLlmEndpoint": Operation("GET", "/v1/ops/private-llm/endpoints/{endpoint_id}", True),
    "deletePrivateLlmEndpoint": Operation("DELETE", "/v1/ops/private-llm/endpoints/{endpoint_id}", True),
    "putTenantPrivateLlm": Operation("PUT", "/v1/ops/tenants/{tenant_id}/private-llm", True),
    "getTenantPrivateLlm": Operation("GET", "/v1/ops/tenants/{tenant_id}/private-llm", True),
    "deleteTenantPrivateLlm": Operation("DELETE", "/v1/ops/tenants/{tenant_id}/private-llm", True),
    "putPrincipalPrivateLlm": Operation("PUT", "/v1/ops/principals/{principal_id}/private-llm", True),
    "getPrincipalPrivateLlm": Operation("GET", "/v1/ops/principals/{principal_id}/private-llm", True),
    "deletePrincipalPrivateLlm": Operation("DELETE", "/v1/ops/principals/{principal_id}/private-llm", True),
    "getMeUsage": Operation("GET", "/v1/me/usage", True),
    "getMeBudget": Operation("GET", "/v1/me/budget", True),
    "getPrincipalAnalyst": Operation("GET", "/v1/me/analyst", True),
    "getTenantAnalyst": Operation("GET", "/v1/ops/tenants/{tenant_id}/analyst", True),
    "putTenantAnalyst": Operation("PUT", "/v1/ops/tenants/{tenant_id}/analyst", True),
    "listMemory": Operation("GET", "/v1/memory", True),
    "createMemory": Operation("POST", "/v1/memory", True),
    "patchMemory": Operation("PATCH", "/v1/memory/{id}", True),
    "deleteMemory": Operation("DELETE", "/v1/memory/{id}", True),
    "confirmMemory": Operation("POST", "/v1/memory/{id}/confirm", True),
    "rejectMemory": Operation("POST", "/v1/memory/{id}/reject", True),
    # Downloadable exports from verified turn blocks (1.2.0).
    "createExport": Operation("POST", "/v1/sessions/{session_id}/exports", True),
}

_PLACEHOLDER = re.compile(r"\{([a-z_]+)\}")


def fill_path(template: str, params: Mapping[str, str]) -> str:
    """Fills the placeholders of a path template. The values must already be validated identifiers."""
    return _PLACEHOLDER.sub(lambda m: quote(params.get(m.group(1), ""), safe=""), template)


def match_path(template: str, path: str) -> Optional[dict[str, str]]:
    """Matches a request path against a template; returns the placeholder values or None."""
    names: list[str] = []
    pattern = ""
    for index, part in enumerate(_PLACEHOLDER.split(template)):
        if index % 2 == 0:
            pattern += re.escape(part)
        else:
            names.append(part)
            pattern += "([^/]+)"
    found = re.fullmatch(pattern, path)
    if found is None:
        return None
    return {name: unquote(found.group(i + 1)) for i, name in enumerate(names)}
