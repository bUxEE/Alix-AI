"""The AlixAI clients: :class:`AlixAIClient` (sync) and :class:`AlixAIAsyncClient` (async), both over httpx.

The rules of the HTTP layer, all of them about the single-use context token and the fail-closed stance of the API:

* ``get_context_token`` is called once per authenticated request or stream open, after the arguments have been
  validated and before anything is sent, and the token lives only in a local variable for that call. It is never
  cached, retried with, logged or put in an error.
* The token is sent only as ``Authorization: Bearer``; redirects are never followed with the credential.
* Nothing is retried here. A request that may have reached the server and whose outcome is unknown is
  ``uncertain_outcome``; the host decides what to do with ``retryable``.
"""

from __future__ import annotations

import inspect
import json
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Awaitable, Callable, Generic, Mapping, Optional, Sequence, TypeVar, Union

import httpx

from alixai.errors import MAX_ERROR_BODY_BYTES, AlixAIError, error_from_parsed, sdk_error
from alixai.gen.actions.action_receipt import ActionReceipt
from alixai.gen.actions.prepared_action import PreparedAction
from alixai.gen.api import (
    ActionOutcome,
    Application,
    CapabilityList,
    Health,
    PendingResponse,
    PendingState,
    ReadyReport,
    Registration,
    Session,
    SessionEnded,
)
from alixai.gen.decision.decision_status import DecisionStatus
from alixai.operations import OPERATIONS, fill_path
from alixai.turn import AsyncTurnStream, TurnInput, TurnResult, TurnStream, turn_body, turn_result_from_json
from alixai.types import Liveness
from alixai.validate import confirmation_token, context_token, normalize_base_url, path_id

if TYPE_CHECKING:
    from alixai.dictation import AsyncDictationsApi, DictationsApi
    from alixai.providers import AsyncProvidersApi, ProvidersApi

T = TypeVar("T")

DEFAULT_TIMEOUT_MS = 30_000
DEFAULT_TURN_TIMEOUT_MS = 600_000


@dataclass(frozen=True)
class ContextTokenRequest:
    """What the host's token provider is told about the request it is minting a token for."""

    #: The OpenAPI operationId of the request.
    operation: str
    #: The session the request concerns, when the path names one.
    session_id: Optional[str] = None
    #: The preparation the request concerns, when the path names one.
    preparation_id: Optional[str] = None
    #: The application the request concerns, when the path names one.
    application_id: Optional[str] = None
    #: The provider id the request concerns, when the path names one (the providers and OAuth areas).
    provider: Optional[str] = None


#: Supplies a fresh, single-use context token for one request: the bare token, no ``Bearer`` prefix. Called for every
#: authenticated request and every event-stream open; never cached by the SDK.
ContextTokenProvider = Callable[[ContextTokenRequest], str]
#: The async client also accepts a coroutine function.
AsyncContextTokenProvider = Callable[[ContextTokenRequest], Union[str, Awaitable[str]]]


@dataclass(frozen=True)
class Call(Generic[T]):
    """One API call, described without I/O: which operation, with which validated arguments, parsed how."""

    operation: str
    parse: Callable[[Any], T]
    params: Mapping[str, str] = field(default_factory=dict)
    body: Optional[bytes] = None
    content_type: Optional[str] = None
    accept: str = "application/json"
    #: A 503 carries the report (readiness): it is information, not a failure.
    report_503: bool = False
    #: The call waits on the model or on the host's commit: the turn timeout applies.
    long: bool = False
    #: True when success is an empty 204 (no JSON body).
    empty: bool = False
    timeout_ms: Optional[int] = None
    what: str = "response"
    #: The query string, as pairs (a name may repeat).
    query: Sequence[tuple[str, str]] = ()
    #: A value that must appear nowhere in the answer or in an error (a credential, an authorization code).
    forbid: Optional[str] = None


def _timeout_ms(value: Any, name: str) -> Optional[int]:
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
        raise sdk_error("validation_failed", f"{name} must be a non-negative number of milliseconds", details={"argument": name})
    return int(value)


def _gzip(archive: Any) -> bytes:
    data = bytes(archive) if isinstance(archive, (bytes, bytearray, memoryview)) else None
    if data is None or len(data) < 2 or data[0] != 0x1F or data[1] != 0x8B:
        raise sdk_error("validation_failed", "an application package is a gzip-compressed tar archive", details={"argument": "archive"})
    return data


def _decision_status(body: Any) -> DecisionStatus:
    """Parses a decision status and refuses one that contradicts itself: ``rejected_count`` is always the sum of ``fallback_counts``."""
    status = DecisionStatus.model_validate(body)
    reasons = [entry.reason for entry in status.fallback_counts]
    if len(set(reasons)) != len(reasons):
        raise ValueError("a fallback reason is listed twice")
    if sum(entry.count for entry in status.fallback_counts) != status.rejected_count:
        raise ValueError("rejected_count is not the sum of fallback_counts")
    return status


def _outcome(body: Any) -> ActionOutcome:
    outcome = ActionOutcome.model_validate(body)
    if outcome.receipt is None:  # a 200 without a receipt is not a success: the receipt is the durable record
        raise ValueError("a confirmation answered without a receipt")
    return outcome


def _scrub(error: AlixAIError, secret: str) -> AlixAIError:
    """The error itself, unless it quotes the secret: then a fixed error of the same code."""
    if len(secret) >= 4 and (secret in error.message or secret in json.dumps(error.details or {}, default=str)):
        return AlixAIError(error.code, "the request was not accepted", retryable=error.retryable, status=error.status, request_id=error.request_id)
    return error


class _Calls:
    """Builds the Call of every operation. Arguments are validated here, before any token is requested."""

    @staticmethod
    def liveness(timeout_ms: Any) -> Call[Liveness]:
        return Call("getLiveness", Liveness.model_validate, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="liveness")

    @staticmethod
    def readiness(timeout_ms: Any) -> Call[ReadyReport]:
        return Call("getReadiness", ReadyReport.model_validate, report_503=True, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="readiness")

    @staticmethod
    def health(timeout_ms: Any) -> Call[Health]:
        return Call("getHealth", Health.model_validate, report_503=True, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="health")

    @staticmethod
    def create_session(timeout_ms: Any) -> Call[Session]:
        return Call("createSession", Session.model_validate, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="session")

    @staticmethod
    def get_session(session_id: Any, timeout_ms: Any) -> Call[Session]:
        params = {"session_id": path_id("session_id", session_id)}
        return Call("getSession", Session.model_validate, params, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="session")

    @staticmethod
    def end_session(session_id: Any, timeout_ms: Any) -> Call[SessionEnded]:
        params = {"session_id": path_id("session_id", session_id)}
        return Call("endSession", SessionEnded.model_validate, params, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="session")

    @staticmethod
    def pending_state(session_id: Any, timeout_ms: Any) -> Call[Optional[PendingState]]:
        params = {"session_id": path_id("session_id", session_id)}
        return Call(
            "getPendingState",
            lambda body: PendingResponse.model_validate(body).pending,
            params,
            timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"),
            what="pending state",
        )

    @staticmethod
    def turn(session_id: Any, value: TurnInput, timeout_ms: Any, *, stream: bool) -> Call[TurnResult]:
        params = {"session_id": path_id("session_id", session_id)}
        body = turn_body(value)
        return Call(
            "sendTurn",
            turn_result_from_json,
            params,
            body=body,
            content_type="application/json",
            accept="text/event-stream" if stream else "application/json",
            long=True,
            timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"),
            what="turn",
        )

    @staticmethod
    def get_action(preparation_id: Any, timeout_ms: Any) -> Call[PreparedAction]:
        params = {"preparation_id": path_id("preparation_id", preparation_id)}
        return Call("getAction", PreparedAction.model_validate, params, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="action")

    @staticmethod
    def confirm_action(preparation_id: Any, token: Any, timeout_ms: Any) -> Call[ActionOutcome]:
        params = {"preparation_id": path_id("preparation_id", preparation_id)}
        body = json.dumps({"token": confirmation_token(token)}).encode("utf-8")
        return Call(
            "confirmAction",
            _outcome,
            params,
            body=body,
            content_type="application/json",
            long=True,
            timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"),
            what="confirmation",
        )

    @staticmethod
    def reject_action(preparation_id: Any, timeout_ms: Any) -> Call[PreparedAction]:
        params = {"preparation_id": path_id("preparation_id", preparation_id)}
        return Call("rejectAction", PreparedAction.model_validate, params, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="action")

    @staticmethod
    def action_receipt(preparation_id: Any, timeout_ms: Any) -> Call[ActionReceipt]:
        params = {"preparation_id": path_id("preparation_id", preparation_id)}
        return Call("getActionReceipt", ActionReceipt.model_validate, params, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="receipt")

    @staticmethod
    def get_application(application_id: Any, timeout_ms: Any) -> Call[Application]:
        params = {"application_id": path_id("application_id", application_id)}
        return Call("getApplication", Application.model_validate, params, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="application")

    @staticmethod
    def list_capabilities(timeout_ms: Any) -> Call[CapabilityList]:
        return Call("listCapabilities", CapabilityList.model_validate, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="capabilities")

    @staticmethod
    def decision_status(timeout_ms: Any) -> Call[DecisionStatus]:
        return Call("getDecisionStatus", _decision_status, timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"), what="decision status")

    @staticmethod
    def register_application(archive: Any, timeout_ms: Any) -> Call[Registration]:
        return Call(
            "registerApplication",
            Registration.model_validate,
            body=_gzip(archive),
            content_type="application/gzip",
            timeout_ms=_timeout_ms(timeout_ms, "timeout_ms"),
            what="registration",
        )


# ---- shared plumbing ---------------------------------------------------------------------------------------------

#: A failure before the request left this machine: nothing was sent, so nothing can have happened.
_NEVER_SENT = (httpx.ConnectError, httpx.ConnectTimeout, httpx.PoolTimeout)


def transport_error(exc: httpx.RequestError, operation: str) -> AlixAIError:
    """A failure before a complete response. A read may be retried safely; for anything else the request may have
    been processed, so the honest answer is that the outcome is unknown."""
    method = OPERATIONS[operation].method
    safe = method == "GET" or isinstance(exc, _NEVER_SENT)
    timed_out = isinstance(exc, httpx.TimeoutException)
    details = {"operation": operation, "phase": "timeout" if timed_out else "transport"}
    if safe:
        code, message = ("timeout", "the request timed out") if timed_out else ("unavailable", "the request could not be completed (network error)")
        return sdk_error(code, message, details=details)
    what = "timed out" if timed_out else "failed (network error)"
    return sdk_error("uncertain_outcome", f"the request {what} and may have been processed: read the state before sending it again", details=details)


def error_from_response(response: httpx.Response) -> AlixAIError:
    """Turns a non-2xx response (body already read) into an AlixAIError; a body that is not JSON never escapes."""
    parsed: Any = None
    if len(response.content) <= MAX_ERROR_BODY_BYTES:
        try:
            parsed = json.loads(response.content)
        except ValueError:
            parsed = None
    return error_from_parsed(response.status_code, response.headers, parsed)


class _ClientBase:
    def __init__(
        self,
        base_url: str,
        get_context_token: Callable[..., Any],
        timeout_ms: int,
        turn_timeout_ms: int,
        allow_insecure_http: bool,
    ) -> None:
        self.base_url = normalize_base_url(base_url, allow_insecure_http)
        if not callable(get_context_token):
            raise sdk_error(
                "validation_failed",
                "get_context_token is required: the host supplies a fresh single-use token per request",
                details={"argument": "get_context_token"},
            )
        self._get_context_token = get_context_token
        self._timeout_ms = _timeout_ms(timeout_ms, "timeout_ms") or 0
        self._turn_timeout_ms = _timeout_ms(turn_timeout_ms, "turn_timeout_ms") or 0

    def _token_request(self, call: Call[Any]) -> Optional[ContextTokenRequest]:
        if not OPERATIONS[call.operation].authenticated:
            return None
        p = call.params
        return ContextTokenRequest(call.operation, p.get("session_id"), p.get("preparation_id"), p.get("application_id"), p.get("provider"))

    def _timeout(self, call: Call[Any]) -> Optional[float]:
        ms = call.timeout_ms if call.timeout_ms is not None else (self._turn_timeout_ms if call.long else self._timeout_ms)
        return ms / 1000 if ms > 0 else None

    @staticmethod
    def _redirect_error(call: Call[Any], response: httpx.Response) -> AlixAIError:
        """A redirect is never followed, whatever client the host supplied: it would carry the bearer token, a confirmation
        secret and the body to another place. The SDKs of every language answer it with an error."""
        return sdk_error(
            "internal",
            "the server answered with a redirect, which is never followed",
            status=response.status_code,
            request_id=response.headers.get("x-request-id"),
            details={"operation": call.operation, "reason": "redirect"},
        )

    def _token_failure(self, call: Call[Any], cause: BaseException) -> AlixAIError:
        return sdk_error(
            "unauthenticated",
            "get_context_token failed, so the request was not sent",
            details={"operation": call.operation, "reason": "token_provider_failed"},
        )

    def _build(self, http: Any, call: Call[Any], token: Optional[str]) -> httpx.Request:
        operation = OPERATIONS[call.operation]
        headers = {"Accept": call.accept}
        if call.content_type is not None:
            headers["Content-Type"] = call.content_type
        if token is not None:
            headers["Authorization"] = f"Bearer {context_token(token)}"
        return http.build_request(
            operation.method,
            f"{self.base_url}{fill_path(operation.path, call.params)}",
            headers=headers,
            content=call.body,
            params=list(call.query) if call.query else None,
            timeout=self._timeout(call),
        )

    @staticmethod
    def _finish(call: Call[T], response: httpx.Response) -> T:
        """Checks the status, parses the body, and maps every failure to a typed error.

        A call that carries a secret (``call.forbid``: a credential, an authorization code) is refused if the answer repeats
        it, and an error that quotes it is replaced by a fixed one of the same code: the value never reaches the host.
        """
        if call.forbid is None:
            return _ClientBase._decode(call, response)
        try:
            if len(call.forbid) >= 8 and call.forbid in response.text:
                raise sdk_error(
                    "internal",
                    "the server's answer repeats a value that must never be returned",
                    status=response.status_code,
                    request_id=response.headers.get("x-request-id"),
                    details={"operation": call.operation, "reason": "echoed_secret"},
                )
            return _ClientBase._decode(call, response)
        except AlixAIError as error:
            raise _scrub(error, call.forbid) from None

    @staticmethod
    def _decode(call: Call[T], response: httpx.Response) -> T:
        """Checks the status, parses the body, and maps every failure to a typed error."""
        status = response.status_code
        if not (response.is_success or (call.report_503 and status == 503)):
            raise error_from_response(response)
        if call.empty:
            if status != 204 and response.content:
                raise sdk_error(
                    "internal",
                    "the server answered with a body where none was expected",
                    status=status,
                    request_id=response.headers.get("x-request-id"),
                    details={"operation": call.operation, "reason": "unexpected_body"},
                )
            return call.parse(None)
        try:
            body = json.loads(response.content)
        except ValueError as exc:
            if not response.is_success:
                raise error_from_response(response) from None
            raise sdk_error(
                "internal",
                "the server answered with a body that is not JSON",
                status=status,
                request_id=response.headers.get("x-request-id"),
                details={"operation": call.operation, "reason": "not_json"},
            ) from exc
        try:
            return call.parse(body)
        except ValueError as exc:
            if not response.is_success:  # a 503 whose body is no report: an ordinary failure
                raise error_from_parsed(status, response.headers, body) from None
            raise sdk_error(
                "internal",
                f"the server's {call.what} response has an unexpected shape",
                status=status,
                request_id=response.headers.get("x-request-id"),
                details={"operation": call.operation, "reason": "unexpected_shape"},
            ) from exc


# ---- the synchronous client --------------------------------------------------------------------------------------


class AlixAIClient:
    """The synchronous AlixAI client.

    ``get_context_token`` receives a :class:`ContextTokenRequest` and returns a fresh single-use token. Pass
    ``http_client`` to supply your own ``httpx.Client`` (it is then not closed by :meth:`close`). Redirects are never followed,
    whatever the client's own setting: a 3xx answer is an error.
    """

    def __init__(
        self,
        base_url: str,
        get_context_token: ContextTokenProvider,
        *,
        timeout_ms: int = DEFAULT_TIMEOUT_MS,
        turn_timeout_ms: int = DEFAULT_TURN_TIMEOUT_MS,
        allow_insecure_http: bool = False,
        http_client: Optional[httpx.Client] = None,
    ) -> None:
        self._base = _ClientBase(base_url, get_context_token, timeout_ms, turn_timeout_ms, allow_insecure_http)
        self.base_url = self._base.base_url
        self._owned = http_client is None
        self._http = http_client if http_client is not None else httpx.Client(follow_redirects=False)

    def __enter__(self) -> "AlixAIClient":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def close(self) -> None:
        if self._owned:
            self._http.close()

    @property
    def dictations(self) -> "DictationsApi":
        """Record-to-text dictation (1.1.0): capabilities and clip transcription. Not the voice WebSocket."""
        from alixai.dictation import DictationsApi

        return DictationsApi(self)

    @property
    def providers(self) -> "ProvidersApi":
        """The operator-facing providers and OAuth area of External Provider Mode (off by default; see :class:`alixai.providers.ProvidersApi`)."""
        from alixai.providers import ProvidersApi

        return ProvidersApi(self)

    # -- plumbing

    def _request(self, call: Call[Any], *, stream: bool = False) -> httpx.Response:
        request_info = self._base._token_request(call)
        token: Optional[str] = None
        if request_info is not None:
            try:
                token = self._base._get_context_token(request_info)
            except Exception as exc:
                raise self._base._token_failure(call, exc) from exc
        request = self._base._build(self._http, call, token)
        try:
            response = self._http.send(request, stream=stream, follow_redirects=False)
        except httpx.RequestError as exc:
            raise transport_error(exc, call.operation) from exc
        if 300 <= response.status_code < 400:
            response.close()
            raise self._base._redirect_error(call, response)
        if stream and not response.is_success:
            # The error body is read here; the response is closed whatever happens, so a read that fails leaks no connection.
            try:
                response.read()
                raise error_from_response(response)
            except httpx.RequestError as exc:
                raise transport_error(exc, call.operation) from exc
            finally:
                response.close()
        return response

    def _execute(self, call: Call[T]) -> T:
        return self._base._finish(call, self._request(call))

    # -- operations

    def get_liveness(self, *, timeout_ms: Optional[int] = None) -> Liveness:
        """Liveness: unauthenticated, no context token is requested."""
        return self._execute(_Calls.liveness(timeout_ms))

    def get_readiness(self, *, timeout_ms: Optional[int] = None) -> ReadyReport:
        """Readiness: unauthenticated. A service that is not ready returns its report; it does not raise."""
        return self._execute(_Calls.readiness(timeout_ms))

    def get_health(self, *, timeout_ms: Optional[int] = None) -> Health:
        """The readiness report plus the caller's application. A service that is not ready returns its report."""
        return self._execute(_Calls.health(timeout_ms))

    def create_session(self, *, timeout_ms: Optional[int] = None) -> Session:
        """Opens the session the context token names (creating it on first sight); opening it again answers the same."""
        return self._execute(_Calls.create_session(timeout_ms))

    def get_session(self, session_id: str, *, timeout_ms: Optional[int] = None) -> Session:
        """Resumes a session, with its structured pending state."""
        return self._execute(_Calls.get_session(session_id, timeout_ms))

    def end_session(self, session_id: str, *, timeout_ms: Optional[int] = None) -> SessionEnded:
        """Ends a session. A prepared action it waits on is invalidated; nothing is ever committed by ending."""
        return self._execute(_Calls.end_session(session_id, timeout_ms))

    def get_pending_state(self, session_id: str, *, timeout_ms: Optional[int] = None) -> Optional[PendingState]:
        """What the active goal waits for, or None. The confirmation secret is not part of it."""
        return self._execute(_Calls.pending_state(session_id, timeout_ms))

    def send_turn(self, session_id: str, input: TurnInput, *, timeout_ms: Optional[int] = None) -> TurnStream:
        """Sends a turn and returns its events as a stream (server-sent events over a POST).

        The request has been sent and its head received when this returns; iterate the stream, or call
        ``result()``. ``input`` is exactly one of ``{"text": ...}``, ``{"choice_id": ...}``,
        ``{"confirmation": {"preparation_id": ..., "token": ...}}`` and ``{"reject": {"preparation_id": ...}}``.
        """
        call = _Calls.turn(session_id, input, timeout_ms, stream=True)
        return TurnStream(self._request(call, stream=True))

    def send_turn_json(self, session_id: str, input: TurnInput, *, timeout_ms: Optional[int] = None) -> TurnResult:
        """Sends a turn and returns it as one JSON document (no streaming). Same checks as :meth:`send_turn`."""
        return self._execute(_Calls.turn(session_id, input, timeout_ms, stream=False))

    def get_action(self, preparation_id: str, *, timeout_ms: Optional[int] = None) -> PreparedAction:
        """The prepared action with its current state."""
        return self._execute(_Calls.get_action(preparation_id, timeout_ms))

    def confirm_action(self, preparation_id: str, token: str, *, timeout_ms: Optional[int] = None) -> ActionOutcome:
        """Presents the single-use confirmation secret and commits the action.

        A commit the host refused (409) or whose outcome is partial or unknown (500) raises an AlixAIError that
        carries the ``receipt`` and ``preparation``; a reused secret is ``replayed``.
        """
        return self._execute(_Calls.confirm_action(preparation_id, token, timeout_ms))

    def reject_action(self, preparation_id: str, *, timeout_ms: Optional[int] = None) -> PreparedAction:
        """Declines a preparation. Nothing is sent to the application."""
        return self._execute(_Calls.reject_action(preparation_id, timeout_ms))

    def get_action_receipt(self, preparation_id: str, *, timeout_ms: Optional[int] = None) -> ActionReceipt:
        """The durable receipt of an action. ``conflict`` while it has no outcome yet."""
        return self._execute(_Calls.action_receipt(preparation_id, timeout_ms))

    def get_application(self, application_id: str, *, timeout_ms: Optional[int] = None) -> Application:
        """The caller's application with its installed packages and states."""
        return self._execute(_Calls.get_application(application_id, timeout_ms))

    def list_capabilities(self, *, timeout_ms: Optional[int] = None) -> CapabilityList:
        """The capabilities the caller may see, with the effective risk level of each."""
        return self._execute(_Calls.list_capabilities(timeout_ms))

    def get_decision_status(self, *, timeout_ms: Optional[int] = None) -> DecisionStatus:
        """The decision-layer status of the caller's own application and tenant. Needs the ``decision.status.read`` scope.

        ADVISORY aggregate state about the optional decision layer: the configured provider and model, the mode (``off``,
        ``shadow`` or ``advisory``), how many results were accepted and rejected since the counters started and how often the
        layer fell back, by reason. It is never an authorisation, a score or a commit result, and no counter in it is
        permission to do anything: authority stays with the deterministic controller. A server without a decision service answers
        ``unavailable``, never an empty status. Read-only; the operator-only configuration is not part of the SDK.
        """
        return self._execute(_Calls.decision_status(timeout_ms))

    def register_application(self, archive: Union[bytes, bytearray, memoryview], *, timeout_ms: Optional[int] = None) -> Registration:
        """Installs a signed ``.tar.gz`` application package into quarantine. Needs the ``applications.register`` scope."""
        return self._execute(_Calls.register_application(archive, timeout_ms))


# ---- the asynchronous client -------------------------------------------------------------------------------------


class AlixAIAsyncClient:
    """The asynchronous AlixAI client: the same operations as :class:`AlixAIClient`, as coroutines.

    ``get_context_token`` may be a plain function or a coroutine function. Cancel a call by cancelling its task; close
    a stream with ``async with`` / ``aclose()``.
    """

    def __init__(
        self,
        base_url: str,
        get_context_token: AsyncContextTokenProvider,
        *,
        timeout_ms: int = DEFAULT_TIMEOUT_MS,
        turn_timeout_ms: int = DEFAULT_TURN_TIMEOUT_MS,
        allow_insecure_http: bool = False,
        http_client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self._base = _ClientBase(base_url, get_context_token, timeout_ms, turn_timeout_ms, allow_insecure_http)
        self.base_url = self._base.base_url
        self._owned = http_client is None
        self._http = http_client if http_client is not None else httpx.AsyncClient(follow_redirects=False)

    async def __aenter__(self) -> "AlixAIAsyncClient":
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        if self._owned:
            await self._http.aclose()

    @property
    def dictations(self) -> "AsyncDictationsApi":
        """Record-to-text dictation (1.1.0); see :class:`alixai.dictation.AsyncDictationsApi`."""
        from alixai.dictation import AsyncDictationsApi

        return AsyncDictationsApi(self)

    @property
    def providers(self) -> "AsyncProvidersApi":
        """The operator-facing providers and OAuth area of External Provider Mode; see :class:`alixai.providers.AsyncProvidersApi`."""
        from alixai.providers import AsyncProvidersApi

        return AsyncProvidersApi(self)

    # -- plumbing

    async def _request(self, call: Call[Any], *, stream: bool = False) -> httpx.Response:
        request_info = self._base._token_request(call)
        token: Optional[str] = None
        if request_info is not None:
            try:
                token = self._base._get_context_token(request_info)
                if inspect.isawaitable(token):
                    token = await token
            except Exception as exc:
                raise self._base._token_failure(call, exc) from exc
        request = self._base._build(self._http, call, token)
        try:
            response = await self._http.send(request, stream=stream, follow_redirects=False)
        except httpx.RequestError as exc:
            raise transport_error(exc, call.operation) from exc
        if 300 <= response.status_code < 400:
            await response.aclose()
            raise self._base._redirect_error(call, response)
        if stream and not response.is_success:
            # The error body is read here; the response is closed whatever happens, so a read that fails leaks no connection.
            try:
                await response.aread()
                raise error_from_response(response)
            except httpx.RequestError as exc:
                raise transport_error(exc, call.operation) from exc
            finally:
                await response.aclose()
        return response

    async def _execute(self, call: Call[T]) -> T:
        return self._base._finish(call, await self._request(call))

    # -- operations

    async def get_liveness(self, *, timeout_ms: Optional[int] = None) -> Liveness:
        """Liveness: unauthenticated, no context token is requested."""
        return await self._execute(_Calls.liveness(timeout_ms))

    async def get_readiness(self, *, timeout_ms: Optional[int] = None) -> ReadyReport:
        """Readiness: unauthenticated. A service that is not ready returns its report; it does not raise."""
        return await self._execute(_Calls.readiness(timeout_ms))

    async def get_health(self, *, timeout_ms: Optional[int] = None) -> Health:
        """The readiness report plus the caller's application. A service that is not ready returns its report."""
        return await self._execute(_Calls.health(timeout_ms))

    async def create_session(self, *, timeout_ms: Optional[int] = None) -> Session:
        """Opens the session the context token names (creating it on first sight); opening it again answers the same."""
        return await self._execute(_Calls.create_session(timeout_ms))

    async def get_session(self, session_id: str, *, timeout_ms: Optional[int] = None) -> Session:
        """Resumes a session, with its structured pending state."""
        return await self._execute(_Calls.get_session(session_id, timeout_ms))

    async def end_session(self, session_id: str, *, timeout_ms: Optional[int] = None) -> SessionEnded:
        """Ends a session. A prepared action it waits on is invalidated; nothing is ever committed by ending."""
        return await self._execute(_Calls.end_session(session_id, timeout_ms))

    async def get_pending_state(self, session_id: str, *, timeout_ms: Optional[int] = None) -> Optional[PendingState]:
        """What the active goal waits for, or None. The confirmation secret is not part of it."""
        return await self._execute(_Calls.pending_state(session_id, timeout_ms))

    async def send_turn(self, session_id: str, input: TurnInput, *, timeout_ms: Optional[int] = None) -> AsyncTurnStream:
        """Sends a turn and returns its events as an async stream; see :meth:`AlixAIClient.send_turn`."""
        call = _Calls.turn(session_id, input, timeout_ms, stream=True)
        return await AsyncTurnStream.open(await self._request(call, stream=True))

    async def send_turn_json(self, session_id: str, input: TurnInput, *, timeout_ms: Optional[int] = None) -> TurnResult:
        """Sends a turn and returns it as one JSON document (no streaming)."""
        return await self._execute(_Calls.turn(session_id, input, timeout_ms, stream=False))

    async def get_action(self, preparation_id: str, *, timeout_ms: Optional[int] = None) -> PreparedAction:
        """The prepared action with its current state."""
        return await self._execute(_Calls.get_action(preparation_id, timeout_ms))

    async def confirm_action(self, preparation_id: str, token: str, *, timeout_ms: Optional[int] = None) -> ActionOutcome:
        """Presents the single-use confirmation secret and commits the action; see :meth:`AlixAIClient.confirm_action`."""
        return await self._execute(_Calls.confirm_action(preparation_id, token, timeout_ms))

    async def reject_action(self, preparation_id: str, *, timeout_ms: Optional[int] = None) -> PreparedAction:
        """Declines a preparation. Nothing is sent to the application."""
        return await self._execute(_Calls.reject_action(preparation_id, timeout_ms))

    async def get_action_receipt(self, preparation_id: str, *, timeout_ms: Optional[int] = None) -> ActionReceipt:
        """The durable receipt of an action. ``conflict`` while it has no outcome yet."""
        return await self._execute(_Calls.action_receipt(preparation_id, timeout_ms))

    async def get_application(self, application_id: str, *, timeout_ms: Optional[int] = None) -> Application:
        """The caller's application with its installed packages and states."""
        return await self._execute(_Calls.get_application(application_id, timeout_ms))

    async def list_capabilities(self, *, timeout_ms: Optional[int] = None) -> CapabilityList:
        """The capabilities the caller may see, with the effective risk level of each."""
        return await self._execute(_Calls.list_capabilities(timeout_ms))

    async def get_decision_status(self, *, timeout_ms: Optional[int] = None) -> DecisionStatus:
        """The decision-layer status of the caller's own application and tenant. Needs the ``decision.status.read`` scope.

        ADVISORY aggregate state about the optional decision layer: the configured provider and model, the mode (``off``,
        ``shadow`` or ``advisory``), how many results were accepted and rejected since the counters started and how often the
        layer fell back, by reason. It is never an authorisation, a score or a commit result, and no counter in it is
        permission to do anything: authority stays with the deterministic controller. A server without a decision service answers
        ``unavailable``, never an empty status. Read-only; the operator-only configuration is not part of the SDK.
        """
        return await self._execute(_Calls.decision_status(timeout_ms))

    async def register_application(self, archive: Union[bytes, bytearray, memoryview], *, timeout_ms: Optional[int] = None) -> Registration:
        """Installs a signed ``.tar.gz`` application package into quarantine. Needs the ``applications.register`` scope."""
        return await self._execute(_Calls.register_application(archive, timeout_ms))
