"""Operator operations: GDPR erasure (1.3.0) and private LLM (1.4.0, ADR 0045). Server-side only."""

from __future__ import annotations

import json
from typing import Any, Mapping, Optional, Union

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.gen.providers import (
    PrincipalPrivateLLMBindingOperator,
    PrincipalPrivateLLMBindingUpdateOperator,
    PrivateLLMEndpoint,
    PrivateLLMEndpointUpdate,
    TenantPrivateLLMPin,
    TenantPrivateLLMPinUpdate,
)
from alixai.validate import path_id

__all__ = [
    "AsyncOpsApi",
    "OpsApi",
    "PrincipalPrivateLLMBindingOperator",
    "PrincipalPrivateLLMBindingUpdateOperator",
    "PrivateLLMEndpoint",
    "PrivateLLMEndpointUpdate",
    "TenantPrivateLLMPin",
    "TenantPrivateLLMPinUpdate",
]

_EndpointBody = Union[PrivateLLMEndpointUpdate, Mapping[str, Any]]
_TenantPinBody = Union[TenantPrivateLLMPinUpdate, Mapping[str, Any]]
_PrincipalBody = Union[PrincipalPrivateLLMBindingUpdateOperator, Mapping[str, Any]]


def _json(body: Mapping[str, Any]) -> bytes:
    return json.dumps(dict(body)).encode("utf-8")


def _dump(body: Any) -> bytes:
    if hasattr(body, "model_dump"):
        return _json(body.model_dump(mode="json", exclude_none=True))
    return _json(body)


class _MappingModel(dict):
    """Pydantic response that still supports 1.4 ``res["key"]`` access."""

    def __init__(self, model: Any) -> None:
        data = model.model_dump(mode="json")
        super().__init__(data)
        object.__setattr__(self, "_model", model)

    def __getattr__(self, name: str) -> Any:
        return getattr(object.__getattribute__(self, "_model"), name)


def _compat(model_cls: Any):
    def parse(d: Any) -> Any:
        return _MappingModel(model_cls.model_validate(d))

    return parse


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

    def put_private_llm_endpoint(self, endpoint_id: str, body: _EndpointBody, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putPrivateLlmEndpoint",
                _compat(PrivateLLMEndpoint),
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                body=_dump(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def get_private_llm_endpoint(self, endpoint_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "getPrivateLlmEndpoint",
                _compat(PrivateLLMEndpoint),
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                timeout_ms=timeout_ms,
            )
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

    def put_tenant_private_llm(self, tenant_id: str, body: _TenantPinBody, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putTenantPrivateLlm",
                _compat(TenantPrivateLLMPin),
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                body=_dump(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def get_tenant_private_llm(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "getTenantPrivateLlm",
                _compat(TenantPrivateLLMPin),
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                timeout_ms=timeout_ms,
            )
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

    def put_principal_private_llm(
        self, principal_id: str, body: _PrincipalBody, *, timeout_ms: Optional[int] = None
    ) -> Any:
        return self._client._execute(
            Call(
                "putPrincipalPrivateLlm",
                _compat(PrincipalPrivateLLMBindingOperator),
                params={"principal_id": path_id("principal_id", principal_id)},
                body=_dump(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def get_principal_private_llm(self, principal_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "getPrincipalPrivateLlm",
                _compat(PrincipalPrivateLLMBindingOperator),
                params={"principal_id": path_id("principal_id", principal_id)},
                timeout_ms=timeout_ms,
            )
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

    def get_tenant_analyst(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("getTenantAnalyst", lambda d: d, params={"tenant_id": path_id("tenant_id", tenant_id)}, timeout_ms=timeout_ms)
        )

    def put_tenant_analyst(self, tenant_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putTenantAnalyst",
                lambda d: d,
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                body=_json(body),
                content_type="application/json",
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

    async def put_private_llm_endpoint(self, endpoint_id: str, body: _EndpointBody, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putPrivateLlmEndpoint",
                _compat(PrivateLLMEndpoint),
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                body=_dump(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def get_private_llm_endpoint(self, endpoint_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "getPrivateLlmEndpoint",
                _compat(PrivateLLMEndpoint),
                params={"endpoint_id": path_id("endpoint_id", endpoint_id)},
                timeout_ms=timeout_ms,
            )
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

    async def put_tenant_private_llm(self, tenant_id: str, body: _TenantPinBody, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putTenantPrivateLlm",
                _compat(TenantPrivateLLMPin),
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                body=_dump(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def get_tenant_private_llm(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "getTenantPrivateLlm",
                _compat(TenantPrivateLLMPin),
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                timeout_ms=timeout_ms,
            )
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

    async def put_principal_private_llm(
        self, principal_id: str, body: _PrincipalBody, *, timeout_ms: Optional[int] = None
    ) -> Any:
        return await self._client._execute(
            Call(
                "putPrincipalPrivateLlm",
                _compat(PrincipalPrivateLLMBindingOperator),
                params={"principal_id": path_id("principal_id", principal_id)},
                body=_dump(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def get_principal_private_llm(self, principal_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "getPrincipalPrivateLlm",
                _compat(PrincipalPrivateLLMBindingOperator),
                params={"principal_id": path_id("principal_id", principal_id)},
                timeout_ms=timeout_ms,
            )
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

    async def get_tenant_analyst(self, tenant_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("getTenantAnalyst", lambda d: d, params={"tenant_id": path_id("tenant_id", tenant_id)}, timeout_ms=timeout_ms)
        )

    async def put_tenant_analyst(self, tenant_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putTenantAnalyst",
                lambda d: d,
                params={"tenant_id": path_id("tenant_id", tenant_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )
