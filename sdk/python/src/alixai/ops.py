"""Operator operations: GDPR erasure over HTTP (1.3.0, ADR 0036). Server-side only."""

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
