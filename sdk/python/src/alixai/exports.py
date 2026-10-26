"""Downloadable exports from a turn's verified rich blocks (1.2.0).

``POST /v1/sessions/{session_id}/exports`` with ``{turn_id, block_ids?, format}`` returns a
``FileRef`` (``origin: generated``). Needs ``exports.create`` in the context token. The core never
accepts model-written file bytes.
"""

from __future__ import annotations

import json
import re
from typing import Any, Mapping, Optional, Sequence

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.errors import sdk_error
from alixai.gen.conversation.file_ref import FileRef
from alixai.validate import path_id

__all__ = ["AsyncExportsApi", "ExportsApi"]

_TURN_ID = re.compile(r"^trn_[0-9A-HJKMNP-TV-Z]{26}$")
_BLOCK_ID = re.compile(r"^[A-Za-z0-9._:-]{1,128}$")
_FORMATS = frozenset({"csv", "xlsx", "html", "md"})


def _body(body: Mapping[str, Any]) -> bytes:
    turn = body.get("turn_id", body.get("turnId"))
    if not isinstance(turn, str) or not _TURN_ID.match(turn):
        raise sdk_error("validation_failed", "turn_id is not a well-formed turn id", details={"argument": "turn_id"})
    fmt = body.get("format")
    if not isinstance(fmt, str) or fmt not in _FORMATS:
        raise sdk_error("validation_failed", "format must be csv, xlsx, html or md", details={"argument": "format"})
    wire: dict[str, Any] = {"turn_id": turn, "format": fmt}
    raw_ids = body.get("block_ids", body.get("blockIds"))
    if raw_ids is not None:
        if not isinstance(raw_ids, Sequence) or isinstance(raw_ids, (str, bytes)) or len(raw_ids) > 64:
            raise sdk_error("validation_failed", "block_ids must be a list of at most 64 ids", details={"argument": "block_ids"})
        seen: set[str] = set()
        ids: list[str] = []
        for item in raw_ids:
            if not isinstance(item, str) or not _BLOCK_ID.match(item):
                raise sdk_error("validation_failed", "a block id is not well formed", details={"argument": "block_ids"})
            if item in seen:
                raise sdk_error("validation_failed", "block ids must be unique", details={"argument": "block_ids"})
            seen.add(item)
            ids.append(item)
        if ids:
            wire["block_ids"] = ids
    return json.dumps(wire).encode("utf-8")


class ExportsApi:
    """Sync exports surface on an :class:`AlixAIClient`."""

    def __init__(self, client: AlixAIClient) -> None:
        self._client = client

    def create(self, session_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> FileRef:
        """``POST /v1/sessions/{session_id}/exports`` → ``FileRef`` (``origin: generated``)."""
        sid = path_id("session_id", session_id)
        return self._client._execute(
            Call(
                "createExport",
                FileRef.model_validate,
                params={"session_id": sid},
                body=_body(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
                what="export",
            )
        )


class AsyncExportsApi:
    """Async exports surface on an :class:`AlixAIAsyncClient`."""

    def __init__(self, client: AlixAIAsyncClient) -> None:
        self._client = client

    async def create(self, session_id: str, body: Mapping[str, Any], *, timeout_ms: Optional[int] = None) -> FileRef:
        sid = path_id("session_id", session_id)
        return await self._client._execute(
            Call(
                "createExport",
                FileRef.model_validate,
                params={"session_id": sid},
                body=_body(body),
                content_type="application/json",
                timeout_ms=timeout_ms,
                what="export",
            )
        )
