"""A turn: its input, its result, and the stream of its events.

``send_turn`` returns a :class:`TurnStream` (:class:`AsyncTurnStream` for the async client), an iterator of validated
events plus what is not an event: the single-use confirmation secret. Events are validated as they arrive and the
sequence is enforced (one ``turn_id``, ``seq`` from 0 without gaps, ``done`` last): a stream that ends before
``done``, a gap and an event of an unknown type each raise a typed :class:`AlixAIError` rather than ending the turn
quietly. Leaving the ``with`` block (or calling ``close``) releases the connection.
"""

from __future__ import annotations

import json
from collections import deque
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Deque, Iterator, Mapping, Optional

import httpx
from pydantic import ValidationError

from alixai.capabilities import DEFAULT_CLIENT_CAPABILITIES
from alixai.errors import AlixAIError, sdk_error
from alixai.events import TurnSequence, done_event, parse_turn_event, turn_text
from alixai.gen.api import TurnResponse
from alixai.gen.conversation.turn_event import DoneEvent, TurnEvent
from alixai.sse import SseDecoder, SseMessage
from alixai.validate import is_confirmation_token, is_file_id, path_id

#: The most files one text turn may name.
MAX_TURN_ATTACHMENTS = 4

#: The response header that carries the single-use confirmation secret of an event-stream turn.
CONFIRMATION_TOKEN_HEADER = "X-AlixAI-Confirmation-Token"

#: What a user can send in one turn: exactly one of ``text``, ``choice_id``, ``confirmation`` (``{preparation_id,
#: token}``) or ``reject`` (``{preparation_id}``), plus an optional ``language`` for the first two. A confirmation is
#: a structured input from the client UI: no text can ever confirm anything.
#:
#: A ``text`` turn may also name the files the user uploaded for it: ``attachments`` is a list of at most
#: ``MAX_TURN_ATTACHMENTS`` file ids (the ids ``POST /v1/files`` returned). The core resolves them together under the
#: turn's scope before anything runs: a file that is another tenant's, principal's or session's, one deleted, one expired
#: and one that never existed all fail the whole turn with the same ``scope_violation``; no model is called and no turn is
#: recorded.
TurnInput = Mapping[str, Any]

# Sentinel: client_capabilities key absent → fill DEFAULT_CLIENT_CAPABILITIES.
_CAPS_DEFAULT = object()


@dataclass(frozen=True)
class ConfirmationGrant:
    """The single-use secret that lets the user's client confirm one prepared action.

    It is not a TurnEvent: it arrives in the ``X-AlixAI-Confirmation-Token`` header of an event stream, or in the
    ``confirmation`` member of a JSON turn. Show it to the user's client only, present it once. Its repr hides it.
    """

    preparation_id: str
    token: str = field(repr=False)


@dataclass(frozen=True)
class TurnResult:
    """A finished turn: its events in order (the last is ``done``) and, when it prepared an action that needs the
    user, the confirmation grant."""

    turn_id: str
    events: list[TurnEvent]
    confirmation: Optional[ConfirmationGrant] = None

    @property
    def text(self) -> str:
        return turn_text(self.events)

    @property
    def done(self) -> Optional[DoneEvent]:
        return done_event(self.events)


def _malformed(reason: str) -> AlixAIError:
    return sdk_error("internal", f"the server's turn response is malformed: {reason}", details={"reason": "malformed_turn"})


def turn_body(value: Any) -> bytes:
    """Checks a turn input before anything is sent and returns the request body, built from known members only.

    A malformed input must not cost a single-use context token.
    """

    def bad(reason: str) -> AlixAIError:
        return sdk_error("validation_failed", f"invalid turn input: {reason}", details={"argument": "input"})

    if not isinstance(value, Mapping):
        raise bad("it must be a mapping")
    for key in value:
        if key not in ("text", "choice_id", "confirmation", "reject", "language", "attachments", "client_capabilities"):
            raise bad(f'unknown member "{key}"')
    kinds = [key for key in ("text", "choice_id", "confirmation", "reject") if value.get(key) is not None]
    if len(kinds) != 1:
        raise bad("exactly one of text, choice_id, confirmation and reject is required")
    language = value.get("language")
    if language is not None and (not isinstance(language, str) or language == ""):
        raise bad("language must be a non-empty string")
    kind = kinds[0]
    attachments = value.get("attachments")
    if attachments is not None:
        # Files belong to a text message. The list is counted as given (the core counts a repeated id once, but limits the list);
        # a bare string is not a list of ids (it would be read one character at a time).
        if kind != "text":
            raise bad("attachments go with a text message")
        if not isinstance(attachments, (list, tuple)) or len(attachments) > MAX_TURN_ATTACHMENTS:
            raise bad(f"attachments must be a list of at most {MAX_TURN_ATTACHMENTS} file ids")
        if not all(is_file_id(file_id) for file_id in attachments):
            raise bad("an attachment is not a file id")
    caps = value.get("client_capabilities", _CAPS_DEFAULT)
    if caps is not _CAPS_DEFAULT and caps is not None and not isinstance(caps, Mapping):
        raise bad("client_capabilities must be a mapping")
    body: dict[str, Any]
    if kind in ("text", "choice_id"):
        content = value[kind]
        if not isinstance(content, str) or content == "":
            raise bad(f"{kind} must be a non-empty string")
        body = {kind: content}
        if language is not None:
            body["language"] = language
        if attachments:
            body["attachments"] = list(attachments)
    elif kind == "confirmation":
        if language is not None:
            raise bad("language does not apply to a confirmation")
        grant = value["confirmation"]
        if not isinstance(grant, Mapping):
            raise bad("confirmation must be a mapping")
        preparation_id = path_id("confirmation.preparation_id", grant.get("preparation_id"))
        if not is_confirmation_token(grant.get("token")):
            raise bad("confirmation.token is malformed")
        body = {"confirmation": {"preparation_id": preparation_id, "token": grant["token"]}}
    else:
        if language is not None:
            raise bad("language does not apply to a rejection")
        rejection = value["reject"]
        if not isinstance(rejection, Mapping):
            raise bad("reject must be a mapping")
        body = {"reject": {"preparation_id": path_id("reject.preparation_id", rejection.get("preparation_id"))}}
    # Absent key → announce what this SDK parses. Explicit None omits (server = 1.4).
    if caps is _CAPS_DEFAULT:
        body["client_capabilities"] = dict(DEFAULT_CLIENT_CAPABILITIES)
    elif caps is not None:
        body["client_capabilities"] = dict(caps)
    return json.dumps(body, separators=(",", ":")).encode("utf-8")


def _grant(raw: Any) -> ConfirmationGrant:
    if raw is None or not is_confirmation_token(raw.token):
        raise sdk_error("internal", "the server's turn response carries a malformed confirmation", details={"reason": "malformed_turn"})
    return ConfirmationGrant(raw.preparation_id, raw.token)


def turn_result_from_json(body: Any) -> TurnResult:
    """Decodes the JSON form of a turn (``TurnResponse``), with the same checks as a stream."""
    try:
        response = TurnResponse.model_validate(body)
    except ValidationError as exc:
        first = exc.errors(include_input=False, include_context=False, include_url=False)[0]
        raise _malformed(f"{first['type']} at {'.'.join(str(p) for p in first['loc'])}") from exc
    sequence = TurnSequence(response.turn_id)
    events = [sequence.accept(event) for event in response.events]
    sequence.finish()
    if response.confirmation is None:
        return TurnResult(response.turn_id, events)
    grant = _grant(response.confirmation)
    prepared = next((e for e in events if e.type == "prepared_action"), None)
    if prepared is not None and prepared.prepared_action.preparation_id != grant.preparation_id:
        raise _malformed("the confirmation names another preparation than the prepared_action event")
    return TurnResult(response.turn_id, events, grant)


class _TurnCore:
    """The part of a turn stream that does no I/O: frames in, validated events and the confirmation grant out."""

    def __init__(self, response: httpx.Response) -> None:
        self.sequence = TurnSequence()
        self.events: list[TurnEvent] = []
        self.grant: Optional[ConfirmationGrant] = None
        self.complete = False
        self._preparation_id: Optional[str] = None
        self._queue: Deque[SseMessage] = deque()
        self._json: Deque[TurnEvent] = deque()
        self._json_grant: Optional[ConfirmationGrant] = None
        self._decoder: Optional[SseDecoder] = None
        self._header_token: Optional[str] = None
        self.is_sse = "text/event-stream" in response.headers.get("content-type", "").lower()
        if self.is_sse:
            header = response.headers.get(CONFIRMATION_TOKEN_HEADER)
            if header is not None and not is_confirmation_token(header):
                raise _malformed("the confirmation token header is malformed")
            self._header_token = header
            self._decoder = SseDecoder()
        elif "application/json" not in response.headers.get("content-type", "").lower():
            raise _malformed("the content type is neither text/event-stream nor application/json")

    # -- JSON form (the server answered the stream request with the whole turn)

    def load_json(self, raw: bytes) -> None:
        try:
            body = json.loads(raw)
        except ValueError as exc:
            raise _malformed("the body is not JSON") from exc
        result = turn_result_from_json(body)
        self._json.extend(result.events)
        self._json_grant = result.confirmation

    # -- SSE form

    def feed(self, chunk: bytes) -> None:
        assert self._decoder is not None
        self._queue.extend(self._decoder.feed(chunk))

    def end(self) -> None:
        assert self._decoder is not None
        self._queue.extend(self._decoder.finish())

    def next_event(self) -> Optional[TurnEvent]:
        """The next event already available, or None when more bytes are needed."""
        if not self.is_sse:
            if not self._json:
                return None
            return self._record(self.sequence.accept(self._json.popleft()))
        if not self._queue:
            return None
        message = self._queue.popleft()
        try:
            data = json.loads(message.data)
        except ValueError as exc:
            raise _malformed("an event is not valid JSON") from exc
        return self._record(self.sequence.accept(parse_turn_event(data), message.event, message.id, framed=True))

    def _record(self, event: TurnEvent) -> TurnEvent:
        self.events.append(event)
        if event.type == "prepared_action":
            self._preparation_id = event.prepared_action.preparation_id
        if event.type == "done":
            self._complete(event)
        return event

    def _complete(self, done: DoneEvent) -> None:
        """Pairs the secret with the preparation the turn prepared."""
        preparation_id = self._preparation_id if self._preparation_id is not None else done.preparation_id
        if self._preparation_id is not None and done.preparation_id is not None and self._preparation_id != done.preparation_id:
            raise _malformed("done names another preparation than the prepared_action event")
        if self.is_sse and self._header_token is not None:
            if preparation_id is None:
                raise _malformed("a confirmation token arrived without a prepared action")
            self.grant = ConfirmationGrant(preparation_id, self._header_token)
        elif not self.is_sse and self._json_grant is not None:
            if preparation_id is not None and preparation_id != self._json_grant.preparation_id:
                raise _malformed("the confirmation names another preparation than the prepared_action event")
            self.grant = self._json_grant
        self.complete = True

    def exhausted(self) -> AlixAIError:
        """The error for a source that ended without another event."""
        self.sequence.finish()  # raises uncertain_outcome when `done` never came
        return _malformed("the stream ended")

    def result(self) -> TurnResult:
        turn_id = self.sequence.turn_id
        if turn_id is None or not self.sequence.done:
            raise _malformed("the turn has no events")
        return TurnResult(turn_id, list(self.events), self.grant)


def _transport_failure(error: Exception) -> Exception:
    """A read that failed mid-stream: the turn may have run, so the honest answer is that its outcome is unknown."""
    if isinstance(error, httpx.HTTPError):
        return sdk_error(
            "uncertain_outcome",
            "the connection failed while the turn was streaming: read the session's pending state before sending it again",
            details={"operation": "sendTurn", "phase": "stream"},
        )
    return error


class TurnStream:
    """The events of one turn, as an iterator, plus the confirmation grant. Use it as a context manager."""

    def __init__(self, response: httpx.Response) -> None:
        self._response = response
        self._closed = False
        try:
            self._core = _TurnCore(response)
            if self._core.is_sse:
                self._chunks: Iterator[bytes] = response.iter_bytes()
            else:
                self._core.load_json(response.read())
        except BaseException:
            self.close()
            raise
        self._ended = False

    def __enter__(self) -> "TurnStream":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def __iter__(self) -> "TurnStream":
        return self

    def __next__(self) -> TurnEvent:
        if self._closed:
            raise StopIteration
        try:
            while True:
                event = self._core.next_event()
                if event is not None:
                    break
                if not self._core.is_sse or self._ended:
                    raise self._core.exhausted()
                chunk = next(self._chunks, None)
                if chunk is None:
                    self._ended = True
                    self._core.end()
                else:
                    self._core.feed(chunk)
        except Exception as exc:
            self.close()
            mapped = _transport_failure(exc)
            if mapped is exc:
                raise
            raise mapped from exc
        if event.type == "done":
            self.close()
        return event

    @property
    def confirmation(self) -> Optional[ConfirmationGrant]:
        """The grant of the action the turn prepared, or None. Valid once the ``done`` event has been read."""
        if not self._core.complete:
            raise RuntimeError("read the stream to its done event before asking for the confirmation grant")
        return self._core.grant

    def result(self) -> TurnResult:
        """Reads the stream to its end and returns every event with the confirmation grant."""
        for _ in self:
            pass
        return self._core.result()

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            self._response.close()


class AsyncTurnStream:
    """The events of one turn, as an async iterator, plus the confirmation grant. Use it as an async context manager."""

    def __init__(self, response: httpx.Response, core: _TurnCore, chunks: Optional[AsyncIterator[bytes]]) -> None:
        self._response = response
        self._core = core
        self._chunks = chunks
        self._closed = False
        self._ended = False

    @classmethod
    async def open(cls, response: httpx.Response) -> "AsyncTurnStream":
        try:
            core = _TurnCore(response)
            if core.is_sse:
                return cls(response, core, response.aiter_bytes())
            core.load_json(await response.aread())
            return cls(response, core, None)
        except BaseException:
            await response.aclose()
            raise

    async def __aenter__(self) -> "AsyncTurnStream":
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    def __aiter__(self) -> "AsyncTurnStream":
        return self

    async def __anext__(self) -> TurnEvent:
        if self._closed:
            raise StopAsyncIteration
        try:
            while True:
                event = self._core.next_event()
                if event is not None:
                    break
                if self._chunks is None or self._ended:
                    raise self._core.exhausted()
                try:
                    chunk: Optional[bytes] = await self._chunks.__anext__()
                except StopAsyncIteration:
                    chunk = None
                if chunk is None:
                    self._ended = True
                    self._core.end()
                else:
                    self._core.feed(chunk)
        except Exception as exc:
            await self.aclose()
            mapped = _transport_failure(exc)
            if mapped is exc:
                raise
            raise mapped from exc
        if event.type == "done":
            await self.aclose()
        return event

    @property
    def confirmation(self) -> Optional[ConfirmationGrant]:
        if not self._core.complete:
            raise RuntimeError("read the stream to its done event before asking for the confirmation grant")
        return self._core.grant

    async def result(self) -> TurnResult:
        async for _ in self:
            pass
        return self._core.result()

    async def aclose(self) -> None:
        if not self._closed:
            self._closed = True
            await self._response.aclose()
