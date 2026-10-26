"""Per-principal LLM account (ADR 0041, 1.2.0): catalog, bindings, credential, billing ack, OAuth start.

``submit_credential`` is host-server only — never call it from browser code. The key must never sit in
the page, in storage, or in a postMessage payload.
"""

from __future__ import annotations

import json
from typing import Any, Mapping, Optional, Sequence

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.validate import path_id

__all__ = ["AsyncMeApi", "MeApi"]


def _json(body: Mapping[str, Any]) -> bytes:
    return json.dumps(dict(body)).encode("utf-8")


class MeApi:
    def __init__(self, client: AlixAIClient) -> None:
        self._client = client

    def catalog(self, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(Call("getProviderCatalog", lambda d: d, timeout_ms=timeout_ms))

    def llm(self, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(Call("getPrincipalLlm", lambda d: d, timeout_ms=timeout_ms))

    def put_role(self, role: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "putPrincipalLlmRole",
                lambda d: d,
                params={"role": path_id("role", role)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def delete_role(self, role: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(Call("deletePrincipalLlmRole", lambda d: d, params={"role": path_id("role", role)}, timeout_ms=timeout_ms))

    def submit_credential(self, provider: str, key: str, *, label: Optional[str] = None, timeout_ms: Optional[int] = None) -> Any:
        payload: dict[str, Any] = {"key": key}
        if label is not None:
            payload["label"] = label
        return self._client._execute(
            Call(
                "putPrincipalProviderCredential",
                lambda d: d,
                params={"provider": path_id("provider", provider)},
                body=_json(payload),
                content_type="application/json",
                forbid=key,
                timeout_ms=timeout_ms,
            )
        )

    def delete_credential(self, provider: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("deletePrincipalProviderCredential", lambda d: d, params={"provider": path_id("provider", provider)}, timeout_ms=timeout_ms)
        )

    def acknowledge_billing_notice(self, provider: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "ackPrincipalProviderBillingNotice",
                lambda d: d,
                params={"provider": path_id("provider", provider)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def start_oauth(
        self,
        provider: str,
        *,
        capabilities: Sequence[str],
        redirect_uri: str,
        presentation: Optional[str] = None,
        callback: Optional[str] = None,
        opener_origin: Optional[str] = None,
        label: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> Any:
        payload: dict[str, Any] = {"capabilities": list(capabilities), "redirect_uri": redirect_uri}
        if presentation is not None:
            payload["presentation"] = presentation
        if callback is not None:
            payload["callback"] = callback
        if opener_origin is not None:
            payload["opener_origin"] = opener_origin
        if label is not None:
            payload["label"] = label
        return self._client._execute(
            Call(
                "startPrincipalOAuth",
                lambda d: d,
                params={"provider": path_id("provider", provider)},
                body=_json(payload),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )


class AsyncMeApi:
    def __init__(self, client: AlixAIAsyncClient) -> None:
        self._client = client

    async def catalog(self, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(Call("getProviderCatalog", lambda d: d, timeout_ms=timeout_ms))

    async def llm(self, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(Call("getPrincipalLlm", lambda d: d, timeout_ms=timeout_ms))

    async def put_role(self, role: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "putPrincipalLlmRole",
                lambda d: d,
                params={"role": path_id("role", role)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def delete_role(self, role: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("deletePrincipalLlmRole", lambda d: d, params={"role": path_id("role", role)}, timeout_ms=timeout_ms)
        )

    async def submit_credential(self, provider: str, key: str, *, label: Optional[str] = None, timeout_ms: Optional[int] = None) -> Any:
        payload: dict[str, Any] = {"key": key}
        if label is not None:
            payload["label"] = label
        return await self._client._execute(
            Call(
                "putPrincipalProviderCredential",
                lambda d: d,
                params={"provider": path_id("provider", provider)},
                body=_json(payload),
                content_type="application/json",
                forbid=key,
                timeout_ms=timeout_ms,
            )
        )

    async def delete_credential(self, provider: str, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call("deletePrincipalProviderCredential", lambda d: d, params={"provider": path_id("provider", provider)}, timeout_ms=timeout_ms)
        )

    async def acknowledge_billing_notice(self, provider: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(
            Call(
                "ackPrincipalProviderBillingNotice",
                lambda d: d,
                params={"provider": path_id("provider", provider)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    async def start_oauth(
        self,
        provider: str,
        *,
        capabilities: Sequence[str],
        redirect_uri: str,
        presentation: Optional[str] = None,
        callback: Optional[str] = None,
        opener_origin: Optional[str] = None,
        label: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> Any:
        payload: dict[str, Any] = {"capabilities": list(capabilities), "redirect_uri": redirect_uri}
        if presentation is not None:
            payload["presentation"] = presentation
        if callback is not None:
            payload["callback"] = callback
        if opener_origin is not None:
            payload["opener_origin"] = opener_origin
        if label is not None:
            payload["label"] = label
        return await self._client._execute(
            Call(
                "startPrincipalOAuth",
                lambda d: d,
                params={"provider": path_id("provider", provider)},
                body=_json(payload),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )
