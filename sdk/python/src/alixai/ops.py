"""Operator operations: GDPR erasure (1.3.0) and private LLM (1.4.0, ADR 0045). Server-side only."""

from __future__ import annotations

import json
from typing import Any, Mapping, Optional

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.validate import path_id

__all__ = ["AsyncOpsApi", "OpsApi"]


def _json(body: Mapping[str, Any]) -> bytes:
    return json.dumps(dict(body)).encode("utf-8")


class OpsApi:
    def __init__(self, client: AlixAIClient) -> None:
        self._client = client

    def start_erasure(self, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("startErasure", lambda d: d, body=_json(body), content_type="application/json", timeout_ms=timeout_ms)
        )

    def erasure_status(self, job_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("getErasureJob", lambda d: d, params={"job_id": path_id("job_id", job_id)}, timeout_ms=timeout_ms)
        )

    def put_private_llm_endpoint(self, endpoint_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putPrivateLlmEndpoint",
                lambda d: d,
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def get_private_llm_endpoint(self, endpoint_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("getPrivateLlmEndpoint", lambda d: d, params={"endpoint_id": path_id("endpoint_id", endpoint_id)}, timeout_ms=timeout_ms)
        )

    def delete_private_llm_endpoint(self, endpoint_id: str, *, timeout_ms: Optional[int] = None) -> None:
        self._client._execute(
            Call(
                "deletePrivateLlmEndpoint",
                lambda d: d,
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                empty=True,
                timeout_ms=timeout_ms,
            )
        )

    def put_tenant_private_llm(self, tenant_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putTenantPrivateLlm",
                lambda d: d,
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def get_tenant_private_llm(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("getTenantPrivateLlm", lambda d: d, params={"tenant_id": path_id("tenant_id", tenant_id)}, timeout_ms=timeout_ms)
        )

    def delete_tenant_private_llm(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> None:
        self._client._execute(
            Call(
                "deleteTenantPrivateLlm",
                lambda d: d,
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                empty=True,
                timeout_ms=timeout_ms,
            )
        )

    def put_principal_private_llm(self, principal_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putPrincipalPrivateLlm",
                lambda d: d,
                params={"principal_id": path_id("principal_id", principal_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def get_principal_private_llm(self, principal_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("getPrincipalPrivateLlm", lambda d: d, params={"principal_id": path_id("principal_id", principal_id)}, timeout_ms=timeout_ms)
        )

    def delete_principal_private_llm(self, principal_id: str, *, timeout_ms: Optional[int] = None) -> None:
        self._client._execute(
            Call(
                "deletePrincipalPrivateLlm",
                lambda d: d,
                params={"principal_id": path_id("principal_id", principal_id)},
                empty=True,
                timeout_ms=timeout_ms,
            )
        )


class AsyncOpsApi:
    def __init__(self, client: AlixAIAsyncClient) -> None:
        self._client = client

    async def start_erasure(self, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("startErasure", lambda d: d, body=_json(body), content_type="application/json", timeout_ms=timeout_ms)
        )

    async def erasure_status(self, job_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("getErasureJob", lambda d: d, params={"job_id": path_id("job_id", job_id)}, timeout_ms=timeout_ms)
        )

    async def put_private_llm_endpoint(self, endpoint_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putPrivateLlmEndpoint",
                lambda d: d,
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def get_private_llm_endpoint(self, endpoint_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("getPrivateLlmEndpoint", lambda d: d, params={"endpoint_id": path_id("endpoint_id", endpoint_id)}, timeout_ms=timeout_ms)
        )

    async def delete_private_llm_endpoint(self, endpoint_id: str, *, timeout_ms: Optional[int] = None) -> None:
        await self._client._execute(
            Call(
                "deletePrivateLlmEndpoint",
                lambda d: d,
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                empty=True,
                timeout_ms=timeout_ms,
            )
        )

    async def put_tenant_private_llm(self, tenant_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putTenantPrivateLlm",
                lambda d: d,
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def get_tenant_private_llm(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("getTenantPrivateLlm", lambda d: d, params={"tenant_id": path_id("tenant_id", tenant_id)}, timeout_ms=timeout_ms)
        )

    async def delete_tenant_private_llm(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> None:
        await self._client._execute(
            Call(
                "deleteTenantPrivateLlm",
                lambda d: d,
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                empty=True,
                timeout_ms=timeout_ms,
            )
        )

    async def put_principal_private_llm(self, principal_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putPrincipalPrivateLlm",
                lambda d: d,
                params={"principal_id": path_id("principal_id", principal_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def get_principal_private_llm(self, principal_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("getPrincipalPrivateLlm", lambda d: d, params={"principal_id": path_id("principal_id", principal_id)}, timeout_ms=timeout_ms)
        )

    async def delete_principal_private_llm(self, principal_id: str, *, timeout_ms: Optional[int] = None) -> None:
        await self._client._execute(
            Call(
                "deletePrincipalPrivateLlm",
                lambda d: d,
                params={"principal_id": path_id("principal_id", principal_id)},
                empty=True,
                timeout_ms=timeout_ms,
            )
        )
