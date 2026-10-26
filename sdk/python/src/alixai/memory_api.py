"""Long-term memory (ADR 0042, 1.2.0)."""

from __future__ import annotations

import json
from typing import Any, Mapping, Optional

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.validate import path_id

__all__ = ["AsyncMemoryApi", "MemoryApi"]


def _json(body: Mapping[str, Any]) -> bytes:
    return json.dumps(dict(body)).encode("utf-8")


class MemoryApi:
    def __init__(self, client: AlixAIClient) -> None:
        self._client = client

    def list(self, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(Call("listMemory", lambda d: d, timeout_ms=timeout_ms))

    def create(self, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call("createMemory", lambda d: d, body=_json(body), content_type="application/json", timeout_ms=timeout_ms)
        )

    def patch(self, memory_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(
            Call(
                "patchMemory",
                lambda d: d,
                params={"id": path_id("id", memory_id)},
                body=_json(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
            )
        )

    def delete(self, memory_id: str, *, timeout_ms: Optional[int] = None) -> None:
        self._client._execute(
            Call("deleteMemory", lambda d: d, params={"id": path_id("id", memory_id)}, empty=True, timeout_ms=timeout_ms)
        )

    def confirm(self, memory_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(Call("confirmMemory", lambda d: d, params={"id": path_id("id", memory_id)}, timeout_ms=timeout_ms))

    def reject(self, memory_id: str, *, timeout_ms: Optional[int] = None) -> Any:
        return self._client._execute(Call("rejectMemory", lambda d: d, params={"id": path_id("id", memory_id)}, timeout_ms=timeout_ms))


class AsyncMemoryApi:
    def __init__(self, client: AlixAIAsyncClient) -> None:
        self._client = client

    async def list(self, *, timeout_ms: Optional[int] = None) -> Any:
        return await self._client._execute(Call("listMemory", lambda d: d, timeout_ms=timeout_ms))
