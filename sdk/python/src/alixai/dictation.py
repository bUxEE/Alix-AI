"""Dictation (record-to-text, 1.1.0): capabilities and one-shot clip transcription.

Not the voice WebSocket. The transcript is data for the host to put in a chat input; AlixAI never
submits it as a turn. Paths use ``dictations`` (not ``transcriptions``).
"""

from __future__ import annotations

import re
from typing import Any, Literal, Optional, Sequence, Union

from pydantic import BaseModel, ConfigDict, Field

from alixai.client import AlixAIAsyncClient, AlixAIClient, Call
from alixai.errors import sdk_error
from alixai.validate import path_id

__all__ = [
    "AsyncDictationsApi",
    "DictationTranscript",
    "DictationsApi",
    "TranscriptionCapabilities",
]

_LANGUAGE = re.compile(r"^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$")


class TranscriptionCapabilities(BaseModel):
    """What a deployment offers for record-to-text dictation."""

    model_config = ConfigDict(extra="forbid")

    available: bool
    locality: Literal["local", "external", "none"]
    max_seconds: int = Field(ge=1)
    sample_rate_hz: Literal[16000] = 16000
    format: Literal["audio/wav"] = "audio/wav"
    languages: Sequence[str] = ()


class DictationTranscript(BaseModel):
    """Recognised text from ``POST /v1/sessions/{session_id}/dictations``."""

    model_config = ConfigDict(extra="forbid")

    text: str
    language: str
    duration_ms: int = Field(ge=0)
    locality: Literal["local", "external"]
    provider: Optional[str] = None
    segment_kind: Literal["user_recorded"] = "user_recorded"


def _language(value: Any) -> Optional[str]:
    if value is None:
        return None
    if not isinstance(value, str) or len(value) > 35 or not _LANGUAGE.match(value):
        raise sdk_error("validation_failed", "the language is not a BCP 47 language tag", details={"argument": "language"})
    return value


def _wav(audio: Any) -> bytes:
    data = bytes(audio) if isinstance(audio, (bytes, bytearray, memoryview)) else None
    if data is None or len(data) < 44:
        raise sdk_error("validation_failed", "audio must be WAV bytes (PCM s16le mono 16 kHz)", details={"argument": "wav"})
    return data


class DictationsApi:
    """Sync dictation surface on an :class:`AlixAIClient`."""

    def __init__(self, client: AlixAIClient) -> None:
        self._client = client

    def capabilities(self, *, timeout_ms: Optional[int] = None) -> TranscriptionCapabilities:
        """``GET /v1/dictations/capabilities``."""
        return self._client._execute(
            Call(
                "getDictationCapabilities",
                TranscriptionCapabilities.model_validate,
                timeout_ms=timeout_ms,
                what="dictation capabilities",
            )
        )

    def create(
        self,
        session_id: str,
        wav: Union[bytes, bytearray, memoryview],
        *,
        language: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> DictationTranscript:
        """``POST /v1/sessions/{session_id}/dictations`` with raw ``audio/wav`` bytes."""
        sid = path_id("session_id", session_id)
        hint = _language(language)
        query = [("language", hint)] if hint is not None else ()
        return self._client._execute(
            Call(
                "createSessionDictation",
                DictationTranscript.model_validate,
                params={"session_id": sid},
                body=_wav(wav),
                content_type="audio/wav",
                query=query,
                timeout_ms=timeout_ms,
                what="dictation transcript",
            )
        )


class AsyncDictationsApi:
    """Async dictation surface on an :class:`AlixAIAsyncClient`."""

    def __init__(self, client: AlixAIAsyncClient) -> None:
        self._client = client

    async def capabilities(self, *, timeout_ms: Optional[int] = None) -> TranscriptionCapabilities:
        return await self._client._execute(
            Call(
                "getDictationCapabilities",
                TranscriptionCapabilities.model_validate,
                timeout_ms=timeout_ms,
                what="dictation capabilities",
            )
        )

    async def create(
        self,
        session_id: str,
        wav: Union[bytes, bytearray, memoryview],
        *,
        language: Optional[str] = None,
        timeout_ms: Optional[int] = None,
    ) -> DictationTranscript:
        sid = path_id("session_id", session_id)
        hint = _language(language)
        query = [("language", hint)] if hint is not None else ()
        return await self._client._execute(
            Call(
                "createSessionDictation",
                DictationTranscript.model_validate,
                params={"session_id": sid},
                body=_wav(wav),
                content_type="audio/wav",
                query=query,
                timeout_ms=timeout_ms,
                what="dictation transcript",
            )
        )
