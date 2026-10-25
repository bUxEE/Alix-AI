"""AG-UI for the Python SDK (master plan 15.6, ADR 0035, docs/integration/agui.md).

The AG-UI event types of the pinned release (``@ag-ui/core`` 1.0.1, protocol 1.0), an SSE encoder, and :class:`AguiProxy`:
an ASGI application and a FastAPI-compatible router that let a host backend serve a CopilotKit or AG-UI frontend from its own
origin. This module depends on httpx and the rest of this SDK only; it does not import CopilotKit, an AG-UI library or FastAPI
(``router()`` imports FastAPI when it is called, so an application that mounts the ASGI app needs neither).

    from alixai.agui import AguiProxy

    async def get_context_token(request, caller):
        # request.session_id is the thread; caller.headers holds the incoming request's headers. Authenticate the host's own
        # user, check they own the thread, and return a fresh single-use context token whose session is that thread.
        ...

    proxy = AguiProxy("http://127.0.0.1:8080", get_context_token)
    app.mount("/agui", proxy)                  # any ASGI framework: POST /agui/ runs
    app.include_router(proxy.router(), prefix="/agui")    # FastAPI

What the proxy does. The browser sends an AG-UI ``RunAgentInput`` to the host. The proxy validates the thread and run ids, asks the
host's callback for a fresh single-use context token (session = thread), forwards the body unchanged to ``POST /v1/agui`` and
streams the answer back frame by frame. It holds one frame at a time, keeps none after it was written and never logs a body, a
token or a secret.

The confirmation secret. A run that prepares an action ends in an interrupt, and the gateway sends the single-use confirmation
secret in the ``X-AlixAI-Confirmation-Token`` response header. The proxy keeps it server-side in a :class:`SecretStore` under
(thread, preparation id), stored before the ``RUN_FINISHED`` frame that carries the interrupt is written. It is attached, as the same
request header, only to a resume that explicitly approves that preparation (``{"approved": true}``); a reject or a cancelled resume
never carries it and discards it; an approval for which none is stored goes without one and gets the gateway's 428. The browser
never holds it: response headers of the upstream are copied by allowlist, request headers of the browser are never copied, and a
frame that contains the secret is not forwarded.
"""

import enum
import inspect
import json
import logging
import re
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, AsyncIterator, Awaitable, Callable, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple, Union
from urllib.parse import quote

import httpx

from alixai.client import ContextTokenRequest
from alixai.errors import AlixAIError, is_retryable_code
from alixai.sse import SseDecoder, SseMessage
from alixai.turn import CONFIRMATION_TOKEN_HEADER
from alixai.validate import context_token, is_confirmation_token, normalize_base_url

PROTOCOL_VERSION = "1.0"
PINNED_PACKAGE = "@ag-ui/core 1.0.1"

#: The header that carries the confirmation secret: in the gateway's response to the run that prepared an action, and in the request
#: of the resume that approves it. Never in an event or a body, and never sent to or accepted from the browser.
HEADER_CONFIRMATION_TOKEN = CONFIRMATION_TOKEN_HEADER

#: The body of a run the proxy reads: the gateway's own limit (1 MiB of input plus its framing).
MAX_RUN_REQUEST_BYTES = (1 << 20) + (4 << 10)
#: How long a stored secret is kept when the interrupt names no earlier expiry, and the most it can be set to.
DEFAULT_SECRET_TTL_S = 15 * 60
MAX_SECRET_TTL_S = 60 * 60

OP_RUN = "runAgui"
OP_EVENTS = "streamAguiRunEvents"
OP_CANCEL = "cancelAguiRun"
OP_CAPABILITIES = "getAguiCapabilities"

_log = logging.getLogger("alixai.agui")

_THREAD_ID = re.compile(r"[A-Za-z0-9_.-]{1,128}")
_CURSOR = re.compile(r"[0-9]{1,10}")
_CONFIRM_CALL_PREFIX = "alixai_confirm_"
_MAX_ERROR_BODY = 64 * 1024
_MAX_JSON_BODY = 1 << 20


# ---- the event types ----


class EventType(str, enum.Enum):
    """The ``type`` member of an AG-UI event, for the pinned release. The gateway emits some of them (docs/integration/agui.md
    section 11); the rest are accepted when read."""

    RUN_STARTED = "RUN_STARTED"
    RUN_FINISHED = "RUN_FINISHED"
    RUN_ERROR = "RUN_ERROR"
    STEP_STARTED = "STEP_STARTED"
    STEP_FINISHED = "STEP_FINISHED"
    TEXT_MESSAGE_START = "TEXT_MESSAGE_START"
    TEXT_MESSAGE_CONTENT = "TEXT_MESSAGE_CONTENT"
    TEXT_MESSAGE_END = "TEXT_MESSAGE_END"
    TEXT_MESSAGE_CHUNK = "TEXT_MESSAGE_CHUNK"
    TOOL_CALL_START = "TOOL_CALL_START"
    TOOL_CALL_ARGS = "TOOL_CALL_ARGS"
    TOOL_CALL_END = "TOOL_CALL_END"
    TOOL_CALL_RESULT = "TOOL_CALL_RESULT"
    TOOL_CALL_CHUNK = "TOOL_CALL_CHUNK"
    STATE_SNAPSHOT = "STATE_SNAPSHOT"
    STATE_DELTA = "STATE_DELTA"
    MESSAGES_SNAPSHOT = "MESSAGES_SNAPSHOT"
    ACTIVITY_SNAPSHOT = "ACTIVITY_SNAPSHOT"
    ACTIVITY_DELTA = "ACTIVITY_DELTA"
    RAW = "RAW"
    CUSTOM = "CUSTOM"
    REASONING_START = "REASONING_START"
    REASONING_MESSAGE_START = "REASONING_MESSAGE_START"
    REASONING_MESSAGE_CONTENT = "REASONING_MESSAGE_CONTENT"
    REASONING_MESSAGE_END = "REASONING_MESSAGE_END"
    REASONING_MESSAGE_CHUNK = "REASONING_MESSAGE_CHUNK"
    REASONING_END = "REASONING_END"
    REASONING_ENCRYPTED_VALUE = "REASONING_ENCRYPTED_VALUE"
    SUBAGENT_STARTED = "SUBAGENT_STARTED"
    SUBAGENT_FINISHED = "SUBAGENT_FINISHED"
    SUBAGENT_ERROR = "SUBAGENT_ERROR"


def is_terminal(event_type: str) -> bool:
    """Whether the event type ends a run: nothing may follow it."""
    return event_type in (EventType.RUN_FINISHED.value, EventType.RUN_ERROR.value)


class NotAnEvent(ValueError):
    """Data that is not a JSON object with a non-empty string ``type``."""


@dataclass(frozen=True)
class Interrupt:
    """One reason a run paused for the user. ``id`` is the preparation id of the action awaiting confirmation."""

    id: str
    reason: str
    message: Optional[str] = None
    tool_call_id: Optional[str] = None
    expires_at: Optional[str] = None
    metadata: Any = None


@dataclass(frozen=True)
class Outcome:
    """The outcome of a finished run: ``success``, ``interrupt`` (with its interrupts) or ``cancelled``."""

    type: str
    interrupts: Tuple[Interrupt, ...] = ()


@dataclass(frozen=True)
class AguiEvent:
    """One AG-UI event as it was read: its ``type`` (a plain string, so an event of a newer protocol version is still an event),
    its decoded members and its source text."""

    type: str
    data: Mapping[str, Any]
    raw: str

    @property
    def known(self) -> bool:
        return self.type in _EVENT_TYPE_VALUES

    @property
    def outcome(self) -> Optional[Outcome]:
        """The outcome of a ``RUN_FINISHED`` event, None for anything else (or a finish without one)."""
        value = self.data.get("outcome") if self.type == EventType.RUN_FINISHED.value else None
        if not isinstance(value, dict) or not isinstance(value.get("type"), str):
            return None
        interrupts = []
        for item in value.get("interrupts") or []:
            if isinstance(item, dict) and isinstance(item.get("id"), str):
                interrupts.append(
                    Interrupt(
                        id=item["id"],
                        reason=str(item.get("reason", "")),
                        message=item.get("message"),
                        tool_call_id=item.get("toolCallId"),
                        expires_at=item.get("expiresAt"),
                        metadata=item.get("metadata"),
                    )
                )
        return Outcome(value["type"], tuple(interrupts))


_EVENT_TYPE_VALUES = frozenset(member.value for member in EventType)


def parse_event(data: Union[str, bytes]) -> AguiEvent:
    """Reads one AG-UI event. Every event type of the release, the chunk forms and types of a newer protocol version are accepted;
    data that is not an event raises :class:`NotAnEvent`."""
    text = data.decode("utf-8") if isinstance(data, (bytes, bytearray)) else data
    try:
        value = json.loads(text)
    except ValueError as exc:
        raise NotAnEvent("not JSON") from exc
    if not isinstance(value, dict) or not isinstance(value.get("type"), str) or not value["type"]:
        raise NotAnEvent("not an AG-UI event")
    return AguiEvent(value["type"], value, text)


def _event_type(text: str) -> Optional[str]:
    try:
        return parse_event(text).type
    except NotAnEvent:
        return None


def run_error_event(message: str, code: Optional[str] = None) -> Dict[str, Any]:
    """A ``RUN_ERROR`` event."""
    event: Dict[str, Any] = {"type": EventType.RUN_ERROR.value, "message": message}
    if code:
        event["code"] = code
    return event


# ---- the SSE encoder ----


def encode_frame(data: Union[str, bytes, Mapping[str, Any], AguiEvent], *, id: Optional[str] = None, event: Optional[str] = None) -> bytes:
    """One server-sent event in the framing of the AG-UI stream: an optional ``id: <n>`` line (the resume cursor), then ``data: <event>``,
    then a blank line. An event given as a mapping is written compactly on one line; text and bytes are written as they are. Anything
    that cannot be framed without changing what it means (a line break in the id or the event name, a CR in the data) is a ValueError
    and nothing is returned."""
    if isinstance(data, AguiEvent):
        text = data.raw
    elif isinstance(data, Mapping):
        text = json.dumps(data, separators=(",", ":"), ensure_ascii=False)
    elif isinstance(data, (bytes, bytearray)):
        text = bytes(data).decode("utf-8")
    else:
        text = data
    if "\r" in text:
        raise ValueError("a CR in the data cannot be framed")
    if id is not None and any(c in id for c in "\r\n\x00"):
        raise ValueError("a line break in the id cannot be framed")
    if event is not None and any(c in event for c in "\r\n"):
        raise ValueError("a line break in the event name cannot be framed")
    out = []
    if event and event != "message":
        out.append(f"event: {event}\n")
    if id is not None:
        out.append(f"id: {id}\n")
    out.extend(f"data: {line}\n" for line in text.split("\n"))
    out.append("\n")
    return "".join(out).encode("utf-8")


def iter_frames(chunks: Iterable[bytes]) -> Iterable[SseMessage]:
    """The frames of an event stream given as chunks of bytes (the SDK's SSE decoder: ``id`` belongs to the frame that carried it)."""
    decoder = SseDecoder()
    for chunk in chunks:
        yield from decoder.feed(chunk)
    yield from decoder.finish()


# ---- the confirmation secret store ----


@dataclass(frozen=True)
class SecretKey:
    """The confirmation secret of one prepared action: the thread (the session of the context token) and the preparation id."""

    thread_id: str
    preparation_id: str


class SecretStore:
    """Keeps the single-use confirmation secrets of the actions a thread has prepared, on the host's server. A host that runs several
    replicas of its backend passes a store they share; the default is :class:`MemorySecretStore`. A store holds a secret only until it is
    used or expires, and never logs one."""

    async def put(self, key: SecretKey, secret: str, expires_at: float) -> None:
        raise NotImplementedError

    async def get(self, key: SecretKey) -> Optional[str]:
        raise NotImplementedError

    async def delete(self, key: SecretKey) -> None:
        raise NotImplementedError


class MemorySecretStore(SecretStore):
    """The in-process store: bounded, with expiry. ``clock`` returns epoch seconds."""

    def __init__(self, clock: Callable[[], float] = time.time, max_entries: int = 1024) -> None:
        self._clock = clock
        self._max = max_entries
        self._entries: Dict[SecretKey, Tuple[str, float, int]] = {}
        self._order = 0

    def __len__(self) -> int:
        return len(self._entries)

    async def put(self, key: SecretKey, secret: str, expires_at: float) -> None:
        now = self._clock()
        if expires_at <= now:
            self._entries.pop(key, None)
            return
        if key not in self._entries and len(self._entries) >= self._max:
            for stale in [k for k, (_, exp, _) in self._entries.items() if exp <= now]:
                del self._entries[stale]
            if len(self._entries) >= self._max:
                del self._entries[min(self._entries, key=lambda k: self._entries[k][2])]
        self._order += 1
        self._entries[key] = (secret, expires_at, self._order)

    async def get(self, key: SecretKey) -> Optional[str]:
        entry = self._entries.get(key)
        if entry is None:
            return None
        if entry[1] <= self._clock():
            del self._entries[key]
            return None
        return entry[0]

    async def delete(self, key: SecretKey) -> None:
        self._entries.pop(key, None)


# ---- the proxy ----


@dataclass(frozen=True)
class AguiCaller:
    """What the host's token callback may need to know about the incoming request: its headers (names in lower case) and the ASGI scope."""

    headers: Mapping[str, str] = field(default_factory=dict)
    scope: Mapping[str, Any] = field(default_factory=dict)


def _caller_from_scope(scope: Mapping[str, Any]) -> AguiCaller:
    headers = {key.decode("latin-1").lower(): value.decode("latin-1") for key, value in scope.get("headers", [])}
    return AguiCaller(headers, scope)


#: Called once for every request the proxy forwards, with the :class:`ContextTokenRequest` (operation and thread as ``session_id``) and,
#: if it takes a second positional parameter, the :class:`AguiCaller`; it may be a coroutine function. It must authenticate the user, check
#: that they own the thread, and return a fresh single-use token whose session is that thread. Raising :class:`AlixAIError` with code
#: ``permission_denied`` or ``scope_violation`` answers 403, ``unavailable`` 503, ``rate_limited`` 429; anything else raised answers 401.
AguiTokenProvider = Callable[..., Union[str, Awaitable[str]]]


def _wants_caller(function: Callable[..., Any]) -> bool:
    try:
        parameters = inspect.signature(function).parameters.values()
    except (TypeError, ValueError):
        return False
    positional = [p for p in parameters if p.kind in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)]
    return len(positional) >= 2


@dataclass
class ProxyResponse:
    """What the proxy answers, independent of the web framework: a status, headers, and either a body or an event stream."""

    status: int
    headers: Dict[str, str] = field(default_factory=dict)
    body: bytes = b""
    stream: Optional["EventStream"] = None

    async def aclose(self) -> None:
        if self.stream is not None:
            await self.stream.aclose()


class EventStream:
    """The frames of an AG-UI run as bytes, ready to write. Close it when done: that releases the upstream connection."""

    def __init__(self, frames: AsyncIterator[bytes], response: httpx.Response) -> None:
        self._frames = frames
        self._response = response

    def __aiter__(self) -> AsyncIterator[bytes]:
        return self._frames.__aiter__()

    async def aclose(self) -> None:
        close = getattr(self._frames, "aclose", None)
        try:
            if close is not None:
                await close()
        finally:
            await self._response.aclose()


class _Refusal(Exception):
    """A request the proxy answers itself, with an AlixAI error document. The message is a fixed text: nothing the request carried is echoed."""

    def __init__(self, status: int, code: str, message: str, reason: Optional[str] = None) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message
        self.reason = reason


@dataclass(frozen=True)
class _RunInput:
    thread_id: str
    run_id: str
    approve: Optional[SecretKey] = None
    discard: Optional[SecretKey] = None


def _decode_approval(payload: Any) -> Optional[bool]:
    """Exactly ``{"approved": true|false}``; anything else (no payload, a string, extra members, a token) is not an answer."""
    if isinstance(payload, dict) and len(payload) == 1 and isinstance(payload.get("approved"), bool):
        return payload["approved"]
    return None


def _valid_thread(value: Any) -> bool:
    return isinstance(value, str) and _THREAD_ID.fullmatch(value) is not None and value not in (".", "..")


def _valid_run(value: Any) -> bool:
    """The run ids the run request accepts (the OpenAPI's 1 to 128 characters, no NUL or line break), except a dot segment, which
    no URL path can carry. The id is percent-encoded into the gateway's path."""
    return isinstance(value, str) and 0 < len(value) <= 128 and not any(c in value for c in "\x00\r\n") and value not in (".", "..")


def _read_run_input(body: bytes) -> _RunInput:
    try:
        value = json.loads(body)
    except ValueError:
        value = None
    bad = _Refusal(400, "validation_failed", "the body is not a RunAgentInput with a valid threadId and runId")
    if not isinstance(value, dict):
        raise bad
    thread, run = value.get("threadId"), value.get("runId")
    if not _valid_thread(thread) or not isinstance(run, str) or not run or len(run) > 128 or any(c in run for c in "\x00\r\n"):
        raise bad
    resume, messages = value.get("resume"), value.get("messages")
    if resume is not None and not isinstance(resume, list) or messages is not None and not isinstance(messages, list):
        raise bad
    preparation: Optional[str] = None
    status: Optional[str] = None
    payload: Any = None
    if isinstance(resume, list) and len(resume) == 1 and isinstance(resume[0], dict):
        entry = resume[0]
        preparation, status, payload = entry.get("interruptId"), entry.get("status"), entry.get("payload")
    elif not resume and isinstance(messages, list) and messages and isinstance(messages[-1], dict):
        # The form of clients that answer the confirmation call as a tool message: its content is the same answer.
        last = messages[-1]
        call = last.get("toolCallId")
        if last.get("role") == "tool" and isinstance(call, str) and call.startswith(_CONFIRM_CALL_PREFIX) and isinstance(last.get("content"), str):
            try:
                payload = json.loads(last["content"])
            except ValueError:
                payload = None
            preparation, status = call[len(_CONFIRM_CALL_PREFIX) :], "resolved"
    if not isinstance(preparation, str) or not preparation or len(preparation) > 128:
        return _RunInput(thread, run)
    key = SecretKey(thread, preparation)
    approved = _decode_approval(payload)
    if status == "cancelled":
        return _RunInput(thread, run, discard=key)
    if status == "resolved" and approved is True:
        return _RunInput(thread, run, approve=key)
    if status == "resolved" and approved is False:
        return _RunInput(thread, run, discard=key)
    return _RunInput(thread, run)


def _status_for(code: str) -> int:
    return {
        "validation_failed": 400,
        "unauthenticated": 401,
        "permission_denied": 403,
        "scope_violation": 403,
        "rate_limited": 429,
        "timeout": 504,
        "unavailable": 502,
    }.get(code, 500)


def _media_type(value: Optional[str]) -> str:
    return (value or "").split(";", 1)[0].strip().lower()


_RELAYED_HEADERS = ("retry-after", "x-request-id")


class AguiProxy:
    """Forwards AG-UI runs from a host's frontend to the AlixAI gateway. One proxy serves every thread; it is an ASGI application
    (``await proxy(scope, receive, send)``, mount it under any prefix) and :meth:`router` returns a FastAPI router of the same routes.

    * ``POST /``                                        a run: ``RunAgentInput`` in, AG-UI events out as server-sent events
    * ``GET  /capabilities``                            what the endpoint claims
    * ``GET  /threads/{thread_id}/runs/{run_id}/events`` the events of a run again, from the cursor in ``Last-Event-ID`` or ``?cursor=``
    * ``POST /threads/{thread_id}/runs/{run_id}/cancel`` stop a run that is still in flight

    ``http_client`` is an ``httpx.AsyncClient`` the host owns (its transport is used as it is; redirects are never followed, whatever its own setting); without
    one the proxy makes its own, with no read timeout (a run is a stream), and closes it in :meth:`aclose`.
    """

    def __init__(
        self,
        base_url: str,
        get_context_token: AguiTokenProvider,
        *,
        secrets: Optional[SecretStore] = None,
        secret_ttl_s: float = DEFAULT_SECRET_TTL_S,
        http_client: Optional[httpx.AsyncClient] = None,
        allow_insecure_http: bool = False,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.base_url = normalize_base_url(base_url, allow_insecure_http)
        if not callable(get_context_token):
            raise ValueError("get_context_token is required: the host supplies a fresh single-use token per request")
        self._token = get_context_token
        self._token_wants_caller = _wants_caller(get_context_token)
        self._clock = clock
        self._secrets = secrets if secrets is not None else MemorySecretStore(clock)
        self._ttl = secret_ttl_s if 0 < secret_ttl_s <= MAX_SECRET_TTL_S else DEFAULT_SECRET_TTL_S
        self._owned = http_client is None
        self._http = http_client if http_client is not None else httpx.AsyncClient(
            follow_redirects=False, timeout=httpx.Timeout(connect=10.0, read=None, write=30.0, pool=10.0)
        )

    async def __aenter__(self) -> "AguiProxy":
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        if self._owned:
            await self._http.aclose()

    # -- the framework-neutral core

    async def run(self, body: bytes, *, content_type: Optional[str] = "application/json", caller: Optional[AguiCaller] = None) -> ProxyResponse:
        """Forwards a run and streams its events back."""
        try:
            return await self._run(body, content_type, caller or AguiCaller())
        except _Refusal as refusal:
            return self._refuse(OP_RUN, refusal)

    async def events(
        self,
        thread_id: str,
        run_id: str,
        *,
        cursor: Optional[str] = None,
        last_event_id: Optional[str] = None,
        caller: Optional[AguiCaller] = None,
    ) -> ProxyResponse:
        """Forwards the reconnect of a run: its events again, from ``cursor`` or ``last_event_id`` (the later wins at the gateway)."""
        try:
            for value in (cursor, last_event_id):
                if value is not None and _CURSOR.fullmatch(value) is None:
                    raise _Refusal(400, "validation_failed", "the cursor is not valid")
            self._check_ids(thread_id, run_id)
            url = f"{self.base_url}/v1/agui/threads/{thread_id}/runs/{quote(run_id, safe='')}/events"
            if cursor:
                url += f"?cursor={cursor}"
            token = await self._mint(OP_EVENTS, thread_id, caller or AguiCaller())
            response = await self._send("GET", url, OP_EVENTS, token=token, last_event_id=last_event_id)
            return await self._relay(response, OP_EVENTS, thread_id, None)
        except _Refusal as refusal:
            return self._refuse(OP_EVENTS, refusal)

    async def cancel(self, thread_id: str, run_id: str, *, caller: Optional[AguiCaller] = None) -> ProxyResponse:
        """Forwards the cancel of a run in flight and relays the JSON answer."""
        try:
            self._check_ids(thread_id, run_id)
            token = await self._mint(OP_CANCEL, thread_id, caller or AguiCaller())
            url = f"{self.base_url}/v1/agui/threads/{thread_id}/runs/{quote(run_id, safe='')}/cancel"
            response = await self._send("POST", url, OP_CANCEL, token=token)
            return await self._relay(response, OP_CANCEL, thread_id, None)
        except _Refusal as refusal:
            return self._refuse(OP_CANCEL, refusal)

    async def capabilities(self, *, caller: Optional[AguiCaller] = None) -> ProxyResponse:
        """Forwards the capabilities document. It needs a token like every route; the callback is told no thread."""
        try:
            token = await self._mint(OP_CAPABILITIES, None, caller or AguiCaller())
            response = await self._send("GET", f"{self.base_url}/v1/agui/capabilities", OP_CAPABILITIES, token=token)
            return await self._relay(response, OP_CAPABILITIES, None, None)
        except _Refusal as refusal:
            return self._refuse(OP_CAPABILITIES, refusal)

    # -- one run

    async def _run(self, body: bytes, content_type: Optional[str], caller: AguiCaller) -> ProxyResponse:
        if _media_type(content_type) != "application/json":
            raise _Refusal(415, "validation_failed", "Content-Type must be application/json")
        if len(body) > MAX_RUN_REQUEST_BYTES:
            raise _Refusal(413, "validation_failed", "the request body is too large")
        run = _read_run_input(body)
        token = await self._mint(OP_RUN, run.thread_id, caller)
        # The secret is looked up only now: a request that was refused earlier leaves it untouched.
        secret = await self._secrets.get(run.approve) if run.approve is not None else None
        response = await self._send("POST", f"{self.base_url}/v1/agui", OP_RUN, token=token, content=body, secret=secret)
        try:
            await self._settle(run, response.status_code)
        except BaseException:
            await response.aclose()
            raise
        return await self._relay(response, OP_RUN, run.thread_id, secret)

    @staticmethod
    def _check_ids(thread_id: str, run_id: str) -> None:
        if not _valid_thread(thread_id) or not _valid_run(run_id):
            raise _Refusal(400, "validation_failed", "the thread and run ids are not valid")

    def _refuse(self, operation: str, refusal: _Refusal) -> ProxyResponse:
        document: Dict[str, Any] = {"code": refusal.code, "message": refusal.message, "retryable": is_retryable_code(refusal.code)}
        if refusal.reason:
            document["details"] = {"reason": refusal.reason}
        _log.debug("agui proxy op=%s status=%s code=%s reason=%s", operation, refusal.status, refusal.code, refusal.reason)
        return ProxyResponse(
            refusal.status,
            {"content-type": "application/json", "cache-control": "no-store"},
            json.dumps(document, separators=(",", ":")).encode("utf-8"),
        )

    async def _mint(self, operation: str, thread_id: Optional[str], caller: AguiCaller) -> str:
        request = ContextTokenRequest(operation, session_id=thread_id)
        try:
            value = self._token(request, caller) if self._token_wants_caller else self._token(request)
            if inspect.isawaitable(value):
                value = await value
        except AlixAIError as exc:
            code = exc.code if exc.code in ("permission_denied", "scope_violation", "unavailable", "rate_limited") else "unauthenticated"
            raise _Refusal(_status_for(code), code, "the token provider refused the request, so it was not sent", "token_provider_failed") from None
        except Exception:
            raise _Refusal(401, "unauthenticated", "the token provider refused the request, so it was not sent", "token_provider_failed") from None
        try:
            return context_token(value)
        except AlixAIError:
            raise _Refusal(401, "unauthenticated", "the token provider must return the bare context token", "bad_token_shape") from None

    async def _send(
        self,
        method: str,
        url: str,
        operation: str,
        *,
        token: str,
        content: Optional[bytes] = None,
        secret: Optional[str] = None,
        last_event_id: Optional[str] = None,
    ) -> httpx.Response:
        """Builds the upstream request from scratch: nothing of the browser's request is copied but the body (and the validated cursor)."""
        accept = "text/event-stream, application/json" if operation in (OP_RUN, OP_EVENTS) else "application/json"
        headers = {"Authorization": f"Bearer {token}", "Accept": accept, "Accept-Encoding": "identity"}
        if content is not None:
            headers["Content-Type"] = "application/json"
        if secret:
            headers[HEADER_CONFIRMATION_TOKEN] = secret
        if last_event_id:
            headers["Last-Event-ID"] = last_event_id
        request = self._http.build_request(method, url, headers=headers, content=content)
        try:
            # Never followed, whatever the host's client says: a redirect would carry the token, the confirmation secret and the
            # body to another origin.
            response = await self._http.send(request, stream=True, follow_redirects=False)
        except httpx.RequestError:
            # A run may have reached the gateway: say so rather than that it failed. Nothing is retried here.
            if method == "POST":
                raise _Refusal(502, "uncertain_outcome", "the AlixAI service could not be reached", "upstream_uncertain") from None
            raise _Refusal(502, "unavailable", "the AlixAI service could not be reached", "upstream_unreachable") from None
        if 300 <= response.status_code < 400:
            # The gateway never redirects: whatever answered is not it, and nothing it said is relayed.
            await response.aclose()
            raise _Refusal(502, "unavailable", "the AlixAI service answered with a redirect, which is never followed", "upstream_redirect")
        return response

    async def _settle(self, run: _RunInput, status: int) -> None:
        """Forgets the stored secret of a preparation once it was presented (an approval whose answer used it up: accepted, 409 already
        decided or 410 expired, as the CopilotKit adapter does; any other refusal decided nothing and leaves it for a retry) or declined
        (a reject or cancelled resume that was accepted)."""
        key = None
        if run.approve is not None and (200 <= status < 300 or status in (409, 410)):
            key = run.approve
        elif run.discard is not None and 200 <= status < 300:
            key = run.discard
        if key is not None:
            await self._secrets.delete(key)

    async def _relay(self, response: httpx.Response, operation: str, thread: Optional[str], sent_secret: Optional[str]) -> ProxyResponse:
        """Answers from the upstream response. Headers are copied by allowlist: the upstream's X-AlixAI-Confirmation-Token, cookies and
        everything else stay here."""
        status = response.status_code
        headers = {"cache-control": "no-store"}
        for name in _RELAYED_HEADERS:
            if name in response.headers:
                headers[name] = response.headers[name]
        is_stream_operation = operation in (OP_RUN, OP_EVENTS)
        if status == 204:
            await response.aclose()
            return ProxyResponse(204, headers)
        if 200 <= status < 300 and is_stream_operation and _media_type(response.headers.get("content-type")) == "text/event-stream":
            headers["content-type"] = "text/event-stream"
            headers["x-accel-buffering"] = "no"
            frames = self._frames(response, thread, sent_secret)
            _log.debug("agui proxy op=%s status=%s", operation, status)
            return ProxyResponse(200, headers, stream=EventStream(frames, response))
        limit = _MAX_JSON_BODY if 200 <= status < 300 else _MAX_ERROR_BODY
        try:
            collected = bytearray()
            async for chunk in response.aiter_bytes():
                collected += chunk
                if len(collected) > limit:
                    break
        finally:
            await response.aclose()
        body = bytes(collected)
        valid = len(body) <= limit and _media_type(response.headers.get("content-type")) == "application/json"
        if valid:
            try:
                json.loads(body)
            except ValueError:
                valid = False
        if 200 <= status < 300 and is_stream_operation:
            return self._refuse(operation, _Refusal(502, "internal", "the AlixAI service answered with something other than an event stream", "upstream_malformed"))
        if not valid:
            return self._refuse(operation, _Refusal(502, "unavailable", "the AlixAI service answered without a JSON document", "upstream_malformed"))
        headers["content-type"] = "application/json"
        _log.debug("agui proxy op=%s status=%s", operation, status)
        return ProxyResponse(status, headers, body)

    async def _frames(self, response: httpx.Response, thread: Optional[str], sent_secret: Optional[str]) -> AsyncIterator[bytes]:
        """Copies an event stream frame by frame, one frame at a time. The secret of the response (when there is one) is stored before the
        RUN_FINISHED frame that asks for the confirmation is written, and stays until used, declined or expired. Nothing follows the end of
        a run, and a stream that ends without one gets a RUN_ERROR."""
        upstream_secret = response.headers.get(HEADER_CONFIRMATION_TOKEN) or ""
        guard = [value for value in (upstream_secret, sent_secret or "") if value]
        decoder = SseDecoder()
        ended = False
        try:
            async for chunk in response.aiter_bytes():
                try:
                    messages = decoder.feed(chunk)
                except AlixAIError:
                    yield encode_frame(run_error_event("the event stream could not be relayed", "upstream_malformed"))
                    return
                for message in messages:
                    if not message.data:
                        continue
                    kind = _event_type(message.data)
                    if kind is None or any(secret in message.data for secret in guard):
                        # Not an event, or a frame that carries a secret: it is not forwarded and the run ends.
                        _log.warning("agui proxy: a frame was refused")
                        yield encode_frame(run_error_event("the event stream could not be relayed", "upstream_malformed"))
                        return
                    if kind == EventType.RUN_FINISHED.value and upstream_secret and thread is not None:
                        await self._remember(message.data, upstream_secret, thread)
                    yield encode_frame(message.data, id=message.id, event=message.event)
                    if is_terminal(kind):
                        ended = True
                        break
                if ended:
                    break
            if not ended:
                yield encode_frame(run_error_event("the event stream ended before the run did", "upstream_ended"))
        finally:
            await response.aclose()

    async def _remember(self, data: str, secret: str, thread: str) -> None:
        """Stores the secret under (thread, preparation) for each confirmation interrupt of a RUN_FINISHED frame. Nothing is stored for a
        secret of the wrong shape, an outcome that is not an interrupt, another thread, or an interrupt that has already expired."""
        if not is_confirmation_token(secret):
            return
        event = parse_event(data)
        outcome = event.outcome
        if outcome is None or outcome.type != "interrupt" or event.data.get("threadId") != thread:
            return
        now = self._clock()
        for interrupt in outcome.interrupts:
            if not interrupt.id or len(interrupt.id) > 128 or interrupt.reason != "confirmation_required":
                continue
            expires = now + self._ttl
            if interrupt.expires_at:
                try:
                    at = datetime.fromisoformat(interrupt.expires_at)
                    expires = min(expires, (at if at.tzinfo else at.replace(tzinfo=timezone.utc)).timestamp())
                except ValueError:
                    pass
            if expires > now:
                await self._secrets.put(SecretKey(thread, interrupt.id), secret, expires)

    # -- ASGI

    def _route(self, scope: Mapping[str, Any]) -> Tuple[Optional[str], Optional[Tuple[str, ...]]]:
        """Returns (route name, arguments) for the request, or (None, allowed methods) for a path that exists with other methods."""
        path = scope.get("path", "/")
        root = scope.get("root_path", "")
        if root and path.startswith(root):
            path = path[len(root) :]
        path = path or "/"
        method = scope.get("method", "GET")
        if path == "/":
            return ("run", ()) if method == "POST" else (None, ("POST",))
        if path == "/capabilities":
            return ("capabilities", ()) if method == "GET" else (None, ("GET",))
        match = re.fullmatch(r"/threads/([^/]+)/runs/([^/]+)/(events|cancel)", path)
        if match:
            thread, run, action = match.groups()
            wanted = "GET" if action == "events" else "POST"
            return (action, (thread, run)) if method == wanted else (None, (wanted,))
        return None, None

    async def __call__(self, scope: Mapping[str, Any], receive: Callable[[], Awaitable[Any]], send: Callable[[Any], Awaitable[None]]) -> None:
        if scope["type"] == "lifespan":
            while True:
                message = await receive()
                if message["type"] == "lifespan.startup":
                    await send({"type": "lifespan.startup.complete"})
                elif message["type"] == "lifespan.shutdown":
                    await send({"type": "lifespan.shutdown.complete"})
                    return
        if scope["type"] != "http":
            if scope["type"] == "websocket":
                await send({"type": "websocket.close", "code": 1008})
            return
        name, args = self._route(scope)
        caller = _caller_from_scope(scope)
        if name is None:
            refusal = _Refusal(405 if args else 404, "validation_failed", "no such route")
            result = self._refuse("route", refusal)
            if args:
                result.headers["allow"] = ", ".join(args)
        elif name == "run":
            body = await _read_body(receive)
            if body is None:
                result = self._refuse(OP_RUN, _Refusal(413, "validation_failed", "the request body is too large"))
            else:
                result = await self.run(body, content_type=_header(scope, "content-type"), caller=caller)
        elif name == "capabilities":
            result = await self.capabilities(caller=caller)
        elif name == "events":
            query = dict(_query_pairs(scope.get("query_string", b"")))
            result = await self.events(
                args[0], args[1], cursor=query.get("cursor") or None, last_event_id=_header(scope, "last-event-id"), caller=caller
            )
        else:
            result = await self.cancel(args[0], args[1], caller=caller)
        await _write(send, result)

    @property
    def asgi_app(self) -> "AguiProxy":
        """The proxy itself is the ASGI application; this is for readability at the mount site."""
        return self

    # -- FastAPI

    def router(self, *, dependencies: Optional[Sequence[Any]] = None, tags: Optional[List[str]] = None) -> Any:
        """A FastAPI ``APIRouter`` of the four routes (``include_router(proxy.router(), prefix="/agui")``). FastAPI is imported here, so it
        is needed only by the application that calls this."""
        try:
            from fastapi import APIRouter, Request
            from starlette.responses import Response, StreamingResponse
        except ImportError as exc:  # pragma: no cover - exercised only without FastAPI
            raise ImportError("AguiProxy.router() needs FastAPI (pip install fastapi); the ASGI application needs nothing but the SDK") from exc

        router = APIRouter(dependencies=list(dependencies) if dependencies else None, tags=tags)

        def respond(result: ProxyResponse) -> Any:
            if result.stream is not None:
                return StreamingResponse(result.stream, status_code=result.status, headers=result.headers)
            return Response(content=result.body, status_code=result.status, headers=result.headers)

        async def run(request: Request):
            body = bytearray()
            async for chunk in request.stream():
                body += chunk
                if len(body) > MAX_RUN_REQUEST_BYTES:
                    return respond(self._refuse(OP_RUN, _Refusal(413, "validation_failed", "the request body is too large")))
            return respond(await self.run(bytes(body), content_type=request.headers.get("content-type"), caller=_caller_from_scope(request.scope)))

        async def capabilities(request: Request):
            return respond(await self.capabilities(caller=_caller_from_scope(request.scope)))

        async def events(thread_id: str, run_id: str, request: Request):
            return respond(
                await self.events(
                    thread_id,
                    run_id,
                    cursor=request.query_params.get("cursor") or None,
                    last_event_id=request.headers.get("last-event-id"),
                    caller=_caller_from_scope(request.scope),
                )
            )

        async def cancel(thread_id: str, run_id: str, request: Request):
            return respond(await self.cancel(thread_id, run_id, caller=_caller_from_scope(request.scope)))

        for path, endpoint, methods in (
            ("/", run, ["POST"]),
            ("/capabilities", capabilities, ["GET"]),
            ("/threads/{thread_id}/runs/{run_id}/events", events, ["GET"]),
            ("/threads/{thread_id}/runs/{run_id}/cancel", cancel, ["POST"]),
        ):
            router.add_api_route(path, endpoint, methods=methods, response_model=None, include_in_schema=False)
        return router


# ---- ASGI helpers ----


def _header(scope: Mapping[str, Any], name: str) -> Optional[str]:
    wanted = name.encode("latin-1")
    for key, value in scope.get("headers", []):
        if key.lower() == wanted:
            return value.decode("latin-1")
    return None


def _query_pairs(query: bytes) -> List[Tuple[str, str]]:
    from urllib.parse import parse_qsl

    return parse_qsl(query.decode("latin-1"), keep_blank_values=True)


async def _read_body(receive: Callable[[], Awaitable[Any]]) -> Optional[bytes]:
    """The request body, or None when it is over the limit (reading stops there)."""
    body = bytearray()
    while True:
        message = await receive()
        if message["type"] == "http.disconnect":
            return bytes(body)
        body += message.get("body", b"")
        if len(body) > MAX_RUN_REQUEST_BYTES:
            return None
        if not message.get("more_body"):
            return bytes(body)


async def _write(send: Callable[[Any], Awaitable[None]], result: ProxyResponse) -> None:
    headers = [(name.lower().encode("latin-1"), value.encode("latin-1")) for name, value in result.headers.items()]
    try:
        await send({"type": "http.response.start", "status": result.status, "headers": headers})
        if result.stream is None:
            await send({"type": "http.response.body", "body": result.body})
            return
        async for chunk in result.stream:
            await send({"type": "http.response.body", "body": chunk, "more_body": True})
        await send({"type": "http.response.body", "body": b""})
    finally:
        await result.aclose()
