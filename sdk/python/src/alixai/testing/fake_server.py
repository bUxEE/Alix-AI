"""An in-process fake of the AlixAI public API (/v1) for the tests of a host application.

It is an ``httpx`` transport handler, so it needs no port and no process: build a client over it with
:meth:`FakeAlixAIServer.client` (or pass :attr:`FakeAlixAIServer.transport` to your own ``httpx.Client``). It implements
the protocol the SDK relies on, not the product, so a host's tests fail the way they would against the real service when
the host gets the protocol wrong:

* every authenticated request needs ``Authorization: Bearer <token>``, and a token is SINGLE USE: a second
  presentation is 401 ``replayed`` (a host that caches its token fails here);
* the session in a path must be the token's (403 ``scope_violation``), and a session cannot change principal;
* a turn answers with TurnEvents (numbered from 0, ``done`` last) as server-sent events when asked for
  ``text/event-stream``, else as one JSON document; the confirmation secret is NOT an event: it is the
  ``X-AlixAI-Confirmation-Token`` header (stream) or the ``confirmation`` member (JSON);
* a text turn may name up to four uploaded files (``attachments``); they are resolved together under the token's scope
  before anything runs, and one that is not the caller's (another tenant's, principal's or session's, deleted, expired,
  unknown) is one 403 ``scope_violation`` with no handler call and no turn recorded (see :meth:`FakeAlixAIServer.add_file`);
* the decision status (``GET /v1/decision/status``, advisory: never an authorisation) needs the ``decision.status.read`` scope and answers
  503 ``unavailable`` while no decision service is wired (see :meth:`FakeAlixAIServer.set_decision_status` and
  :func:`decision_status_document`);
* the providers and OAuth areas of External Provider Mode (off by default: 503 ``unavailable`` until :meth:`FakeAlixAIServer.configure_providers`;
  operator-only, decided from the configuration and before the body is read; a manual credential is never stored, echoed or recorded);
* a confirmation secret works once (second use: 409 ``replayed``); level 3 is ``step_up_required``; a receipt is
  ``conflict`` until there is an outcome; a package that is not active serves nothing (403 ``quarantined``).

What it does NOT do: verify a token's signature (any well-formed token is accepted; the fake is not a security
component, ``issue_token`` makes one), run a model, or reach an application. A test says what each turn does with
:meth:`FakeAlixAIServer.on_turn` and the plan builders (``reply``, ``ask_choice``, ``prepare_action``, ...).
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, AsyncIterator, Callable, Iterator, Optional, Union
from urllib.parse import urlsplit

import httpx

from alixai.client import AlixAIAsyncClient, AlixAIClient, ContextTokenRequest
from alixai.errors import is_retryable_code
from alixai.operations import OPERATIONS, match_path
from alixai.testing.ids import (
    b64url_text,
    confirmation_secret,
    decode_b64url_text,
    idempotency_key,
    random_segment,
    typed_id,
)
from alixai.testing.plans import Clarify, Commit, Fail, HttpFailure, Prepare, Reply, TurnPlan
from alixai.testing.providers import PROVIDERS_OPERATIONS, ProvidersConfig, ProvidersFake
from alixai.turn import CONFIRMATION_TOKEN_HEADER
from alixai.validate import is_confirmation_token, is_file_id

Json = dict[str, Any]

#: The capability a token needs to read the decision status, as the core requires it. The default token claims do not carry it:
#: a test that reads the status passes ``claims={"capability_scopes": [DECISION_STATUS_CAPABILITY]}``.
DECISION_STATUS_CAPABILITY = "decision.status.read"

MAX_TOKEN_EVENT_CHARS = 4096
#: How long a file registered with ``add_file`` resolves.
FILE_TTL_S = 15 * 60.0


def _status_for_code(code: str) -> int:
    if code in ("unauthenticated", "expired", "replayed", "scope_missing"):
        return 401
    if code in ("scope_violation", "permission_denied", "step_up_required", "quarantined"):
        return 403
    if code == "validation_failed":
        return 400
    if code in ("schema_invalid", "unknown_capability", "missing_fields", "no_supported_evidence"):
        return 422
    if code in ("ambiguous", "stale", "conflict"):
        return 409
    if code == "confirmation_required":
        return 428
    if code == "rate_limited":
        return 429
    if code in ("unavailable", "degraded", "resource_exhausted"):
        return 503
    if code == "timeout":
        return 504
    return 500


def decision_status_document(
    mode: str = "off",
    *,
    accepted: int = 0,
    fallbacks: Optional[dict[str, int]] = None,
    provider_id: str = "fake-decision-provider",
    since: str = "2026-10-01T00:00:00.000Z",
) -> Json:
    """A decision-status fixture (the wire document of ``DecisionStatus``) for host tests.

    ADVISORY information about the optional decision layer, never an authorisation. ``fallbacks`` maps a fallback reason to how
    many results fell back for it; ``rejected_count`` is derived from it, so the fixture is always consistent. A layer that is
    not ``off`` reports a model and its pinned revision. ``generated_at`` is stamped by the fake when the status is read.
    """
    entries = sorted((fallbacks or {}).items())
    doc: Json = {
        "mode": mode,
        "provider_id": provider_id,
        "accepted_count": accepted,
        "rejected_count": sum(count for _, count in entries),
        "fallback_counts": [{"reason": reason, "count": count} for reason, count in entries],
        "since": since,
        "generated_at": since,
    }
    if mode != "off":
        doc["model_id"] = "fake-scorer"
        doc["model_revision"] = "r1"
    return doc


def _default_capabilities() -> list[Json]:
    return [
        {"id": "demo.lookup", "title": "Look something up", "risk_level": 0, "requires_confirmation": False, "requires_step_up": False, "fields": ["query"]},
        {"id": "demo.book", "title": "Book something", "risk_level": 2, "requires_confirmation": True, "requires_step_up": False, "fields": ["item", "when"]},
        {"id": "demo.close_account", "title": "Close the account", "risk_level": 3, "requires_confirmation": False, "requires_step_up": True, "fields": []},
    ]


_SCOPE_DEFAULTS = {"application_id": "fake-app", "tenant_id": "tenant-1", "workspace_id": "workspace-1", "principal_id": "user-1"}


def _parse_instant(value: str) -> float:
    """Seconds since the epoch of an RFC 3339 instant with a Z suffix."""
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def _chunk_text(text: str) -> list[str]:
    if not text.strip():
        return []
    return [text[i : i + MAX_TOKEN_EVENT_CHARS] for i in range(0, len(text), MAX_TOKEN_EVENT_CHARS)]


def _redact(value: Any) -> Any:
    if isinstance(value, list):
        return [_redact(v) for v in value]
    if isinstance(value, dict):
        # A confirmation secret, a manual credential (`key`) and an OAuth redirect query (the authorization code) are never recorded.
        return {k: "[redacted]" if k in ("token", "key", "query") else _redact(v) for k, v in value.items()}
    return value


@dataclass
class FakeTurn:
    """What a turn handler is told about an incoming text or choice turn."""

    input: dict[str, Any]
    session: dict[str, str]
    #: 1 for the first turn of the session.
    turn_number: int
    #: What the session waited for when this turn arrived.
    pending: Optional[Json]


TurnHandler = Callable[[FakeTurn], TurnPlan]


@dataclass
class RecordedRequest:
    operation: Optional[str]
    method: str
    path: str
    #: The status the fake answered with (0 when it raised a network error).
    status: int = 0
    accept: Optional[str] = None
    content_type: Optional[str] = None
    #: Whether an Authorization header was presented.
    authenticated: bool = False
    #: The token's ``jti``, when the token decoded. The token itself is never recorded.
    token_id: Optional[str] = None
    #: The parsed JSON body, with every ``token`` member replaced by "[redacted]".
    body: Any = None


@dataclass
class InjectedFailure:
    """Makes the next request(s) fail: an error document, a network error, or a stream cut short."""

    #: Only requests of this operation (default: any authenticated request).
    operation: Optional[str] = None
    status: Optional[int] = None
    error: Optional[Json] = None
    headers: dict[str, str] = field(default_factory=dict)
    #: Raise this httpx error instead of answering, as a network failure would.
    network_error: Optional[httpx.RequestError] = None
    #: For a turn stream: send only the first N events' frames, then end the stream without ``done``.
    truncate_after_events: Optional[int] = None
    times: int = 1


@dataclass
class _Scope:
    application_id: str
    tenant_id: str
    workspace_id: str
    principal_id: str
    session_id: str
    capability_scopes: list[str]

    def session_key(self) -> tuple[str, str, str, str]:
        return (self.application_id, self.tenant_id, self.workspace_id, self.session_id)


@dataclass
class _Session:
    scope: _Scope
    created_at: float
    updated_at: float
    status: str = "active"
    turn_count: int = 0
    clarification: Optional[Json] = None
    preparation_id: Optional[str] = None


@dataclass
class _File:
    owner: _Scope
    expires_at: float
    deleted: bool = False


@dataclass
class _Prepared:
    action: Json
    scope: _Scope
    secret: Optional[str]
    commit: Commit
    secret_used: bool = False
    receipt: Optional[Json] = None


@dataclass
class _Built:
    events: list[Json]
    grant: Optional[Json] = None


class _ChunkedStream(httpx.SyncByteStream, httpx.AsyncByteStream):
    """A response body delivered in the given chunks, to either kind of client."""

    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks

    def __iter__(self) -> Iterator[bytes]:
        yield from self._chunks

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for chunk in self._chunks:
            await asyncio.sleep(0)
            yield chunk


class FakeAlixAIServer:
    def __init__(
        self,
        base_url: str = "https://alixai.test",
        *,
        now: Optional[Callable[[], float]] = None,
        capabilities: Optional[list[Json]] = None,
        application_status: str = "active",
        stream_chunk_bytes: int = 0,
        preparation_ttl_s: float = 600.0,
        turn_handler: Optional[TurnHandler] = None,
    ) -> None:
        parts = urlsplit(base_url)
        self.base_url = f"{parts.scheme}://{parts.netloc}{parts.path.rstrip('/')}"
        self._origin = f"{parts.scheme}://{parts.netloc}"
        self._prefix = parts.path.rstrip("/")
        self._clock = now or time.time
        self._options = (capabilities, application_status, turn_handler)
        self._stream_chunk_bytes = stream_chunk_bytes
        self._ttl_s = preparation_ttl_s
        self.reset()

    # ---- the test's controls ---------------------------------------------------------------------------------------

    def reset(self) -> None:
        """Forgets all state: sessions, preparations, redeemed tokens, recorded requests, injected failures."""
        capabilities, status, handler = self._options
        self._offset = 0.0
        self._handler: TurnHandler = handler or (lambda turn: Reply(f"fake: {turn.input.get('text', turn.input.get('choice_id'))}"))
        self._default_handler = self._handler
        self._capabilities: list[Json] = capabilities if capabilities is not None else _default_capabilities()
        self._app_status = status
        self._ready = True
        self._decision_status: Optional[Json] = None
        self._decision_unavailable = False
        self._dictation_unavailable = False
        self._dictation_disabled = False
        self._dictation_silent = False
        self._dictation_text = "book Tuesday"
        self._providers = ProvidersFake(self)
        self._sessions: dict[tuple[str, str, str, str], _Session] = {}
        self._preparations: dict[str, _Prepared] = {}
        self._redeemed: set[str] = set()
        self._registered: dict[str, Json] = {}
        self._files: dict[str, _File] = {}
        self._failures: list[InjectedFailure] = []
        self.requests: list[RecordedRequest] = []

    def on_turn(self, handler: Optional[TurnHandler]) -> None:
        """Replaces what text and choice turns do. Pass None to restore the default (echo)."""
        self._handler = handler or self._default_handler

    def fail_next(self, **failure: Any) -> None:
        """Makes the next request(s) fail. See :class:`InjectedFailure`."""
        self._failures.append(InjectedFailure(**failure))

    def advance_time(self, seconds: float) -> None:
        """Moves the fake's clock forward, to expire preparations and tokens."""
        self._offset += seconds

    def set_application_status(self, status: str) -> None:
        self._app_status = status

    def set_ready(self, ready: bool) -> None:
        """Makes /readyz and /v1/health report not ready (503 with the report) or ready again."""
        self._ready = ready

    def set_decision_status(self, status: Optional[Json]) -> None:
        """Sets the decision status the fake reports (see :func:`decision_status_document`); None restores the default, the layer ``off``."""
        self._decision_status = dict(status) if status is not None else None

    def set_decision_unavailable(self, unavailable: bool = True) -> None:
        """Makes the status answer 503 ``unavailable``, as a server without a decision service does, or restores it."""
        self._decision_unavailable = unavailable

    def set_dictation_unavailable(self, unavailable: bool = True) -> None:
        """Makes dictation routes answer 503 ``unavailable`` (no transcription service)."""
        self._dictation_unavailable = unavailable

    def set_dictation_disabled(self, disabled: bool = True) -> None:
        """Reports capabilities with ``available: false`` (and create answers unavailable)."""
        self._dictation_disabled = disabled

    def set_dictation_silent(self, silent: bool = True) -> None:
        """Makes create answer ``validation_failed`` with ``details.reason`` ``silent``."""
        self._dictation_silent = silent

    def set_dictation_text(self, text: str) -> None:
        """Sets the scripted transcript returned by create."""
        self._dictation_text = text

    def configure_providers(self, config: Optional[ProvidersConfig] = None) -> None:
        """Switches the providers and OAuth areas of External Provider Mode on with this configuration (default:
        :func:`alixai.testing.default_providers_config`) and resets their state. Until then every providers route answers 503 ``unavailable``."""
        from alixai.testing.providers import default_providers_config

        self._providers.configure(config if config is not None else default_providers_config())

    def set_providers_unavailable(self, unavailable: bool = True) -> None:
        """Makes every providers and OAuth route answer 503 ``unavailable``, as a server without the providers service does, or restores them."""
        self._providers.unavailable = unavailable

    def set_provider_budget_spend(self, budget_id: str, daily_micro_units: int, monthly_micro_units: int) -> None:
        """Sets the settled spend of a budget's daily and monthly windows (micro-units), so a test can see a warning or an exhausted budget."""
        self._providers.spent[budget_id] = (daily_micro_units, monthly_micro_units)

    def set_provider_usage(self, usage: Json) -> None:
        """Sets the document the usage export answers with (records and totals)."""
        self._providers.usage = dict(usage)

    @property
    def tokens_redeemed(self) -> int:
        """How many context tokens have been accepted (and used up)."""
        return len(self._redeemed)

    def session(self, session_id: str) -> Optional[Json]:
        """The wire document of a session, as the API would report it."""
        for record in self._sessions.values():
            if record.scope.session_id == session_id:
                return self._session_doc(record)
        return None

    def preparations(self) -> list[Json]:
        """The prepared actions the fake holds, as the API reports them."""
        return [dict(self._effective(p)) for p in self._preparations.values()]

    def add_file(
        self,
        session_id: str = "ses-1",
        *,
        application_id: Optional[str] = None,
        tenant_id: Optional[str] = None,
        workspace_id: Optional[str] = None,
        principal_id: Optional[str] = None,
    ) -> str:
        """Registers a file the caller these claims describe "uploaded" and returns its id (``fil_`` and a ULID).

        The SDK has no upload (a host uploads through ``POST /v1/files``, or with the TypeScript SDK), so a test registers the
        files its users uploaded and names the ids in a turn's ``attachments``. A file resolves in a turn only for the caller it
        was registered for (application, tenant, workspace, principal and session) while it exists and has not expired (15
        minutes on the fake's clock). Every other case is the same 403 ``scope_violation``: nothing tells a reference's cases apart.
        """
        owner = _Scope(
            application_id or _SCOPE_DEFAULTS["application_id"],
            tenant_id or _SCOPE_DEFAULTS["tenant_id"],
            workspace_id or _SCOPE_DEFAULTS["workspace_id"],
            principal_id or _SCOPE_DEFAULTS["principal_id"],
            session_id,
            [],
        )
        file_id = typed_id("fil", self._ms())
        self._files[file_id] = _File(owner, self._now() + FILE_TTL_S)
        return file_id

    def delete_file(self, file_id: str) -> None:
        """Deletes a file: it stops resolving at once."""
        if file_id in self._files:
            self._files[file_id].deleted = True

    def expire_file(self, file_id: str) -> None:
        """Makes a file expire now, whatever the clock says."""
        if file_id in self._files:
            self._files[file_id].expires_at = self._now()

    def _attachable(self, scope: _Scope, file_ids: list[str]) -> bool:
        """Whether every file id resolves for the caller."""
        for file_id in file_ids:
            record = self._files.get(file_id)
            if record is None or record.deleted or self._now() >= record.expires_at:
                return False
            if record.owner.session_key() != scope.session_key() or record.owner.principal_id != scope.principal_id:
                return False
        return True

    def issue_token(
        self,
        session_id: str,
        *,
        application_id: Optional[str] = None,
        tenant_id: Optional[str] = None,
        workspace_id: Optional[str] = None,
        principal_id: Optional[str] = None,
        roles: Optional[list[str]] = None,
        capability_scopes: Optional[list[str]] = None,
        ttl_seconds: int = 60,
    ) -> str:
        """A fresh single-use token for the claims. Each call is a new token with a new ``jti``.

        The default permits exactly the capabilities the fake serves (and not ``applications.register``): there is no
        wildcard. The token is unsigned on purpose: a real service would refuse it.
        """
        issued = int(self._now())
        payload = {
            "jti": f"jti_{random_segment(12)}",
            "iat": issued,
            "exp": issued + ttl_seconds,
            "application_id": application_id or _SCOPE_DEFAULTS["application_id"],
            "tenant_id": tenant_id or _SCOPE_DEFAULTS["tenant_id"],
            "workspace_id": workspace_id or _SCOPE_DEFAULTS["workspace_id"],
            "sub": principal_id or _SCOPE_DEFAULTS["principal_id"],
            "roles": roles if roles is not None else ["user"],
            "capability_scopes": capability_scopes if capability_scopes is not None else [c["id"] for c in self._capabilities],
            "session_id": session_id,
        }
        header = {"alg": "none", "typ": "alixai-fake+jwt"}
        return f"{b64url_text(json.dumps(header))}.{b64url_text(json.dumps(payload))}.{random_segment(12)}"

    def token_provider(self, session_id: str = "ses-1", **claims: Any) -> Callable[[ContextTokenRequest], str]:
        """A ``get_context_token`` for a client: a fresh token for the session on every call."""
        return lambda request: self.issue_token(session_id, **claims)

    @property
    def transport(self) -> httpx.MockTransport:
        """An httpx transport that routes to the fake and never touches the network (sync and async clients)."""
        return httpx.MockTransport(self.handler)

    def client(
        self,
        session_id: str = "ses-1",
        get_context_token: Optional[Callable[..., Any]] = None,
        claims: Optional[dict[str, Any]] = None,
        **options: Any,
    ) -> AlixAIClient:
        """A sync client over the fake. ``claims`` are the token claims of :meth:`issue_token`; ``options`` go to the client."""
        return AlixAIClient(
            self.base_url,
            get_context_token or self.token_provider(session_id, **(claims or {})),
            http_client=httpx.Client(transport=self.transport),
            **options,
        )

    def async_client(
        self,
        session_id: str = "ses-1",
        get_context_token: Optional[Callable[..., Any]] = None,
        claims: Optional[dict[str, Any]] = None,
        **options: Any,
    ) -> AlixAIAsyncClient:
        """An async client over the fake; see :meth:`client`."""
        return AlixAIAsyncClient(
            self.base_url,
            get_context_token or self.token_provider(session_id, **(claims or {})),
            http_client=httpx.AsyncClient(transport=self.transport),
            **options,
        )

    # ---- time and documents ----------------------------------------------------------------------------------------

    def _now(self) -> float:
        return self._clock() + self._offset

    def _ms(self) -> int:
        return int(self._now() * 1000)

    @staticmethod
    def _iso(seconds: float) -> str:
        """An RFC 3339 instant in UTC with a literal Z, as schemas/common/timestamps requires."""
        return datetime.fromtimestamp(seconds, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"

    @staticmethod
    def _request_id() -> str:
        return f"req_{random_segment(9)}"

    # ---- responses -------------------------------------------------------------------------------------------------

    def _json(self, status: int, body: Any, headers: Optional[dict[str, str]] = None) -> httpx.Response:
        merged = {"Cache-Control": "no-store", "X-Request-ID": self._request_id(), **(headers or {})}
        return httpx.Response(status, json=body, headers=merged)

    def _problem(self, code: str, message: str, status: Optional[int] = None, details: Optional[Json] = None) -> httpx.Response:
        request_id = self._request_id()
        retryable = is_retryable_code(code)
        body = {"code": code, "message": message, "retryable": retryable, "details": {**(details or {}), "request_id": request_id}}
        headers = {"X-Request-ID": request_id}
        if (status or _status_for_code(code)) == 401:
            headers["WWW-Authenticate"] = "Bearer"
        if retryable:
            headers["Retry-After"] = "1"
        return self._json(status or _status_for_code(code), body, headers)

    def _level_violation(self, level: str) -> httpx.Response:
        return self._problem("scope_violation", f"the {level} in the path is not the one the context token is bound to", details={"levels": [level]})

    # ---- authentication --------------------------------------------------------------------------------------------

    def _authenticate(self, request: httpx.Request) -> tuple[Optional[_Scope], Optional[str], Optional[httpx.Response]]:
        header = request.headers.get("authorization")
        if header is None:
            return None, None, self._problem("unauthenticated", "no context token was presented")
        pieces = header.split(" ")
        if len(pieces) != 2 or pieces[0].lower() != "bearer" or not pieces[1]:
            return None, None, self._problem("unauthenticated", "the Authorization header is not a bearer token")
        parts = pieces[1].split(".")
        try:
            if len(parts) != 3:
                raise ValueError("not a compact token")
            claims = json.loads(decode_b64url_text(parts[1]))
            if not isinstance(claims, dict):
                raise ValueError("claims are not an object")
        except ValueError:
            return None, None, self._problem("unauthenticated", "the context token is malformed")

        def text(key: str) -> Optional[str]:
            value = claims.get(key)
            return value if isinstance(value, str) and value else None

        jti, sub, app, tenant, workspace, session = (text(k) for k in ("jti", "sub", "application_id", "tenant_id", "workspace_id", "session_id"))
        if None in (jti, sub, app, tenant, workspace, session):
            return None, None, self._problem("unauthenticated", "the context token does not carry the required claims")
        assert jti and sub and app and tenant and workspace and session
        if isinstance(claims.get("exp"), (int, float)) and claims["exp"] <= self._now():
            return None, jti, self._problem("expired", "the context token has expired")
        if jti in self._redeemed:
            return None, jti, self._problem("replayed", "the context token was already used")
        self._redeemed.add(jti)
        scopes = [s for s in claims.get("capability_scopes", []) if isinstance(s, str)] if isinstance(claims.get("capability_scopes"), list) else []
        return _Scope(app, tenant, workspace, sub, session, scopes), jti, None

    # ---- state -----------------------------------------------------------------------------------------------------

    def _effective(self, record: _Prepared) -> Json:
        action = record.action
        if action["state"] == "prepared" and _parse_instant(action["expires_at"]) <= self._now():
            action["state"] = "expired"
        return action

    def _pending_of(self, session: _Session) -> Optional[Json]:
        if session.preparation_id is not None:
            record = self._preparations.get(session.preparation_id)
            if record is not None:
                action = self._effective(record)
                if action["state"] in ("prepared", "confirmed"):
                    return {
                        "kind": "confirmation",
                        "capability_id": action["capability_id"],
                        "phase": "awaiting_confirmation",
                        "collected_fields": [],
                        "preparation": dict(action),
                        "expires_at": action["expires_at"],
                    }
            session.preparation_id = None
        if session.clarification is not None:
            return {
                "kind": "clarification",
                "capability_id": "demo.clarify",
                "phase": "awaiting_answer",
                "collected_fields": [],
                "question": {"field": session.clarification["field"], "answer_kind": "choice", "choices": session.clarification["choices"]},
                "missing_fields": [session.clarification["field"]],
            }
        return None

    def _session_doc(self, session: _Session) -> Json:
        return {
            "session_id": session.scope.session_id,
            "status": session.status,
            "turn_count": session.turn_count,
            "created_at": self._iso(session.created_at),
            "updated_at": self._iso(session.updated_at),
            "pending": None if session.status == "ended" else self._pending_of(session),
        }

    def _package(self, state: str, manifest_hash: str) -> Json:
        return {
            "manifest_hash": manifest_hash,
            "package_version": "1.0.0",
            "state": state,
            "warnings": 0,
            "installed_at": self._iso(self._now()),
            "status_changed_at": self._iso(self._now()),
        }

    def _packages(self) -> list[Json]:
        own = [] if self._app_status == "not_installed" else [self._package("active" if self._app_status == "active" else self._app_status, "0" * 64)]
        return [*own, *self._registered.values()]

    def _quarantine_refusal(self) -> Optional[httpx.Response]:
        return None if self._app_status == "active" else self._problem("quarantined", "the application's package is not active")

    def _open_session(self, scope: _Scope) -> Union[_Session, httpx.Response]:
        key = scope.session_key()
        existing = self._sessions.get(key)
        if existing is None:
            created = _Session(scope, self._now(), self._now())
            self._sessions[key] = created
            return created
        if existing.scope.principal_id != scope.principal_id:
            return self._problem("scope_violation", "the session belongs to another principal", details={"levels": ["principal"]})
        if existing.status == "ended":
            return self._problem("expired", "the session has ended; a new session needs a new authenticated context", status=410)
        return existing

    def _ready_report(self) -> Json:
        if self._ready:
            return {"status": "ok", "ready": True, "roles": [{"role": "controller", "configured": True, "status": "ok"}], "checks": [{"name": "store", "ok": True}]}
        return {
            "status": "unavailable",
            "ready": False,
            "roles": [{"role": "controller", "configured": True, "status": "unavailable", "reason": "the model runtime is not answering"}],
            "checks": [{"name": "store", "ok": True}, {"name": "controller", "ok": False, "reason": "unavailable"}],
        }

    # ---- actions ---------------------------------------------------------------------------------------------------

    def _owned(self, scope: _Scope, preparation_id: str) -> Union[_Prepared, httpx.Response]:
        record = self._preparations.get(preparation_id)
        if record is None or record.scope.session_key() != scope.session_key() or record.scope.principal_id != scope.principal_id:
            return self._level_violation("preparation")
        return record

    def _receipt_of(self, record: _Prepared) -> Json:
        plan = record.commit
        status = plan.status
        error = plan.error or {"code": "conflict", "message": "the host refused the commit"}
        receipt: Json = {
            "receipt_id": typed_id("rcpt", self._ms()),
            "preparation_id": record.action["preparation_id"],
            "capability_id": record.action["capability_id"],
            "status": status,
            "records": [dict(r) for r in plan.records],
            "facts": [dict(f) for f in plan.facts],
            "errors": [] if status in ("committed", "queued") else [{"code": error["code"], "message": error["message"]}],
            "audit_ref": typed_id("aud", self._ms()),
            "issued_at": self._iso(self._now()),
        }
        if status == "queued":
            receipt["follow_up"] = {"reference": typed_id("job", self._ms()), "state": "queued", "as_of": self._iso(self._now())}
        return receipt

    def _commit(self, record: _Prepared) -> tuple[Json, Optional[Json]]:
        """Commits a confirmed preparation: records the receipt and moves the state. Returns (receipt, error)."""
        receipt = self._receipt_of(record)
        record.receipt = receipt
        status = receipt["status"]
        record.action["state"] = "committed" if status in ("committed", "queued") else "rejected" if status == "rejected" else "uncertain"
        if status in ("committed", "queued"):
            return receipt, None
        failure = record.commit.error
        code = failure["code"] if failure else ("partial_failure" if status == "partial" else "uncertain_outcome" if status == "uncertain" else "conflict")
        message = failure["message"] if failure else f"the commit ended {status}"
        return receipt, {"code": code, "message": message, "retryable": is_retryable_code(code)}

    def _confirm(self, record: _Prepared, token: str) -> Optional[tuple[str, str]]:
        """The coordinator's Confirm: checks, then uses the secret up. Returns (code, message) on failure."""
        action = self._effective(record)
        if record.secret_used:
            return "replayed", "the confirmation was already used"
        state = action["state"]
        if state == "expired":
            return "expired", "the preparation has expired; prepare the action again"
        if state == "invalidated":
            return "stale", "the data the preparation relied on changed; prepare the action again"
        if state != "prepared":
            return "conflict", f"the preparation is {state}"
        mode = action["confirmation"]["mode"]
        if mode == "none":
            return "validation_failed", "this action needs no confirmation"
        if mode == "step_up":
            return "step_up_required", "this action must be completed in the application's own authenticated interface"
        if record.secret is None or token != record.secret:
            return "permission_denied", "the confirmation does not belong to this preparation"
        record.secret_used = True
        action["state"] = "confirmed"
        return None

    def _confirm_endpoint(self, scope: _Scope, preparation_id: str, body: Any) -> httpx.Response:
        refusal = self._quarantine_refusal()
        if refusal:
            return refusal
        record = self._owned(scope, preparation_id)
        if isinstance(record, httpx.Response):
            return record
        if not isinstance(body, dict) or set(body) != {"token"} or not is_confirmation_token(body["token"]):
            return self._problem("validation_failed", 'the request body must be {"token": "cfm_..."}')
        failure = self._confirm(record, body["token"])
        if failure is not None:
            return self._problem(failure[0], failure[1], status=409 if failure[0] == "replayed" else None)
        receipt, error = self._commit(record)
        outcome: Json = {"preparation": dict(record.action), "receipt": receipt}
        if error is None:
            return self._json(200, outcome)
        request_id = self._request_id()
        status = 409 if receipt["status"] == "rejected" else 500
        return self._json(status, {**outcome, "error": {**error, "details": {"request_id": request_id}}}, {"X-Request-ID": request_id})

    # ---- turns -----------------------------------------------------------------------------------------------------

    @staticmethod
    def _finalize(turn_id: str, bodies: list[Json]) -> list[Json]:
        return [{"type": body["type"], "turn_id": turn_id, "seq": seq, **body} for seq, body in enumerate(bodies)]

    @staticmethod
    def _directive(turn_id: str, act: str, **extra: Any) -> Json:
        return {
            "turn_id": turn_id,
            "capability_id": None,
            "act": act,
            "facts": [],
            "must_state": [],
            "missing_fields": [],
            "invalid_fields": [],
            "allowed_suggestions": [],
            "confirmation": {"mode": "none", "bindings": []},
            "permitted_next_actions": [],
            "sources": [],
            **extra,
        }

    @staticmethod
    def _progress(stage: str, capability_id: Optional[str] = None) -> Json:
        event: Json = {"type": "progress", "stage": stage, "state": "completed"}
        if capability_id is not None:
            event["capability_id"] = capability_id
        return event

    @staticmethod
    def _text_events(text: Optional[str]) -> list[Json]:
        return [{"type": "token", "text": chunk} for chunk in _chunk_text(text or "")]

    @staticmethod
    def _confirmation_panel(action: Json) -> Json:
        return {
            "type": "block",
            "block": {
                "type": "confirmation_panel",
                "preparation_id": action["preparation_id"],
                "summary": action["summary"],
                "effects": action["effects"],
                "risk_level": action["risk_level"],
                "expires_at": action["expires_at"],
                "confirmation": action["confirmation"],
            },
        }

    def _error_events(self, code: str, message: str, text: Optional[str] = None) -> list[Json]:
        error = {"code": code, "message": message, "retryable": is_retryable_code(code)}
        return [{"type": "error", "error": error}, *self._text_events(text or message)]

    def _prepare(self, session: _Session, plan: Prepare) -> _Prepared:
        risk = plan.risk_level
        mode = "step_up" if risk >= 3 else "explicit" if risk == 2 else "none"
        prepared_at = self._now()
        action: Json = {
            "preparation_id": typed_id("prep", self._ms()),
            "capability_id": plan.capability_id,
            "state": "prepared",
            "summary": plan.summary,
            "effects": [dict(e) for e in plan.effects]
            if plan.effects is not None
            else [{"operation": "other", "entity_type": "record", "description": plan.summary, "changes": [], "external": False}],
            "risk_level": risk,
            "data_versions": [{"entity_type": "record", "entity_id": "record-1", "version": "v1"}],
            "prepared_at": self._iso(prepared_at),
            "expires_at": self._iso(prepared_at + (plan.valid_for_s if plan.valid_for_s is not None else self._ttl_s)),
            "invalidation_conditions": ["data_version_changed", "session_ended"],
            "confirmation": {"mode": mode, "bindings": [] if mode == "none" else ["preparation_id", "principal", "session", "data_versions"]},
            "idempotency_key": idempotency_key(),
        }
        record = _Prepared(action, session.scope, confirmation_secret() if mode == "explicit" else None, plan.commit)
        self._preparations[action["preparation_id"]] = record
        return record

    def _plan_events(self, turn_id: str, session: _Session, plan: TurnPlan, scope: _Scope) -> _Built:
        session.clarification = None
        events = self._finalize
        if isinstance(plan, Prepare) and plan.capability_id not in scope.capability_scopes:
            # Default deny: the token's capability scopes are exact ids, and anything not listed is refused.
            code, message = "permission_denied", f"the context token does not permit the capability {plan.capability_id}"
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                {"type": "directive", "directive": self._directive(turn_id, "refuse", reason_code=code)},
                *self._error_events(code, message),
                {"type": "done", "finish_reason": "failed"},
            ]))
        if isinstance(plan, Reply):
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                {"type": "directive", "directive": self._directive(turn_id, "present_result")},
                *self._text_events(plan.text),
                {"type": "done", "finish_reason": "completed"},
            ]))
        if isinstance(plan, Clarify):
            choices = [
                {"choice_id": c["choice_id"], "label": c["label"], "value": c.get("value") or {"kind": "identifier", "value": c["choice_id"]}}
                for c in plan.choices
            ]
            session.clarification = {"field": plan.field, "choices": choices}
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                {
                    "type": "directive",
                    "directive": self._directive(
                        turn_id,
                        "disambiguate",
                        next_question={"field": plan.field, "answer_kind": "choice", "choices": choices},
                        allowed_suggestions=choices,
                    ),
                },
                {"type": "clarification", "clarification": {"type": "clarification_choices", "field": plan.field, "answer_kind": "choice", "allow_free_text": True, "choices": choices}},
                *self._text_events(plan.question),
                {"type": "done", "finish_reason": "awaiting_input"},
            ]))
        if isinstance(plan, Fail):
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                {"type": "directive", "directive": self._directive(turn_id, "report_failure", reason_code=plan.code)},
                *self._error_events(plan.code, plan.message, plan.text),
                {"type": "done", "finish_reason": "failed"},
            ]))
        assert isinstance(plan, Prepare)
        record = self._prepare(session, plan)
        action = record.action
        mode = action["confirmation"]["mode"]
        common = {"capability_id": action["capability_id"], "preparation_id": action["preparation_id"], "confirmation": action["confirmation"]}
        text = self._text_events(plan.text or action["summary"])
        if mode == "none":
            receipt, _ = self._commit(record)
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                self._progress("tool_call", action["capability_id"]),
                {"type": "directive", "directive": self._directive(turn_id, "report_outcome", **common, receipt_id=receipt["receipt_id"])},
                {"type": "prepared_action", "prepared_action": dict(action)},
                {"type": "receipt", "receipt": receipt},
                *text,
                {"type": "done", "finish_reason": "completed"},
            ]))
        session.preparation_id = action["preparation_id"]
        if mode == "step_up":
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                self._progress("tool_call", action["capability_id"]),
                {"type": "directive", "directive": self._directive(turn_id, "request_step_up", **common, reason_code="step_up_required")},
                {"type": "prepared_action", "prepared_action": dict(action)},
                self._confirmation_panel(action),
                *text,
                {"type": "done", "finish_reason": "completed"},
            ]))
        return _Built(
            events(turn_id, [
                self._progress("interpreting"),
                self._progress("tool_call", action["capability_id"]),
                {"type": "directive", "directive": self._directive(turn_id, "request_confirmation", **common, permitted_next_actions=[{"action": "confirm"}, {"action": "reject"}])},
                {"type": "prepared_action", "prepared_action": dict(action)},
                self._confirmation_panel(action),
                *text,
                {"type": "done", "finish_reason": "awaiting_confirmation", "preparation_id": action["preparation_id"]},
            ]),
            {"preparation_id": action["preparation_id"], "token": record.secret or ""},
        )

    def _structured_turn(self, turn_id: str, session: _Session, body: Json) -> _Built:
        """A confirmation or rejection arriving as a turn input: structured, never read from text."""
        events = self._finalize
        failed = {"type": "done", "finish_reason": "failed"}
        preparation_id = body["confirmation"]["preparation_id"] if "confirmation" in body else body["reject"]["preparation_id"]
        record = self._owned(session.scope, preparation_id)
        if isinstance(record, httpx.Response):
            return _Built(events(turn_id, [*self._error_events("scope_violation", "the preparation is not one of this session"), failed]))
        action = record.action
        if "reject" in body:
            current = self._effective(record)["state"]
            if current not in ("prepared", "confirmed", "rejected"):
                return _Built(events(turn_id, [*self._error_events("conflict", f"the preparation is {current}"), failed]))
            action["state"] = "rejected"
            session.preparation_id = None
            return _Built(events(turn_id, [
                self._progress("interpreting"),
                {"type": "directive", "directive": self._directive(turn_id, "acknowledge_cancel", capability_id=action["capability_id"])},
                *self._text_events("Cancelled. Nothing was changed."),
                {"type": "done", "finish_reason": "cancelled"},
            ]))
        failure = self._confirm(record, body["confirmation"]["token"])
        if failure is not None:
            return _Built(events(turn_id, [
                self._progress("committing"),
                {"type": "directive", "directive": self._directive(turn_id, "report_failure", capability_id=action["capability_id"], reason_code=failure[0])},
                *self._error_events(*failure),
                failed,
            ]))
        receipt, error = self._commit(record)
        session.preparation_id = None
        bodies: list[Json] = [
            self._progress("committing"),
            {"type": "directive", "directive": self._directive(turn_id, "report_outcome", capability_id=action["capability_id"], preparation_id=preparation_id, receipt_id=receipt["receipt_id"])},
            {"type": "receipt", "receipt": receipt},
        ]
        if error is not None:
            bodies.append({"type": "error", "error": error})
        bodies += self._text_events("Done." if error is None else error["message"])
        bodies.append({"type": "done", "finish_reason": "completed" if error is None else "failed"})
        return _Built(events(turn_id, bodies))

    def _validate_turn_body(self, body: Any) -> Union[Json, httpx.Response]:
        def bad(message: str) -> httpx.Response:
            return self._problem("validation_failed", message)

        if not isinstance(body, dict):
            return bad("the request body must be a JSON object")
        if any(key not in ("text", "language", "choice_id", "confirmation", "reject", "attachments") for key in body):
            return bad("the request body has an unknown member")
        kinds = [key for key in ("text", "choice_id", "confirmation", "reject") if body.get(key) is not None]
        if len(kinds) != 1:
            return bad("exactly one of text, choice_id, confirmation and reject is required")
        if body.get("language") is not None and not isinstance(body["language"], str):
            return bad("language must be a string")
        kind = kinds[0]
        attachments = body.get("attachments")
        if attachments is not None:
            if kind != "text":
                return bad("attachments go with a text message")
            if not isinstance(attachments, list) or len(attachments) > 4:
                return bad("attachments must be a list of at most 4 file ids")
            if not all(is_file_id(file_id) for file_id in attachments):
                return bad("an attachment is not a file id")
        if kind in ("text", "choice_id"):
            if not isinstance(body[kind], str) or not body[kind]:
                return bad(f"{kind} must be a non-empty string")
            return {kind: body[kind], **({"attachments": list(attachments)} if attachments else {})}
        value = body[kind]
        if kind == "confirmation":
            if not isinstance(value, dict) or set(value) != {"preparation_id", "token"} or not isinstance(value["preparation_id"], str) or not is_confirmation_token(value["token"]):
                return bad("confirmation must be {preparation_id, token}")
            return {"confirmation": dict(value)}
        if not isinstance(value, dict) or set(value) != {"preparation_id"} or not isinstance(value["preparation_id"], str):
            return bad("reject must be {preparation_id}")
        return {"reject": dict(value)}

    @staticmethod
    def _frame(event: Json) -> str:
        return f"id: {event['seq']}\nevent: {event['type']}\ndata: {json.dumps(event, separators=(',', ':'))}\n\n"

    def _stream_response(self, built: _Built, truncate_after: Optional[int]) -> httpx.Response:
        events = built.events if truncate_after is None else built.events[:truncate_after]
        data = "".join(self._frame(e) for e in events).encode("utf-8")
        size = self._stream_chunk_bytes
        chunks = [data[i : i + size] for i in range(0, len(data), size)] if size > 0 else [self._frame(e).encode("utf-8") for e in events]
        headers = {
            "Content-Type": "text/event-stream",
            "Cache-Control": "no-store",
            "X-Accel-Buffering": "no",
            "X-Request-ID": self._request_id(),
        }
        if built.grant is not None:
            headers[CONFIRMATION_TOKEN_HEADER] = built.grant["token"]
        return httpx.Response(200, headers=headers, stream=_ChunkedStream(chunks))

    def _turn_endpoint(self, scope: _Scope, session_id: str, request: httpx.Request, body: Any, truncate_after: Optional[int]) -> httpx.Response:
        if session_id != scope.session_id:
            return self._level_violation("session")
        turn_input = self._validate_turn_body(body)
        if isinstance(turn_input, httpx.Response):
            return turn_input
        refusal = self._quarantine_refusal()
        if refusal:
            return refusal
        session = self._open_session(scope)
        if isinstance(session, httpx.Response):
            return session
        # The files are resolved together, under the scope, before anything runs: a refused turn calls no handler and is recorded nowhere.
        if not self._attachable(scope, turn_input.get("attachments", [])):
            return self._problem("scope_violation", "the file reference does not resolve for this caller")
        session.turn_count += 1
        session.updated_at = self._now()
        turn_id = typed_id("trn", self._ms())
        if "confirmation" in turn_input or "reject" in turn_input:
            built = self._structured_turn(turn_id, session, turn_input)
        else:
            plan = self._handler(
                FakeTurn(
                    turn_input,
                    {"session_id": scope.session_id, "application_id": scope.application_id, "tenant_id": scope.tenant_id, "workspace_id": scope.workspace_id, "principal_id": scope.principal_id},
                    session.turn_count,
                    self._pending_of(session),
                )
            )
            if isinstance(plan, HttpFailure):
                session.turn_count -= 1
                return self._json(plan.status, dict(plan.error))
            built = self._plan_events(turn_id, session, plan, scope)
        accept = request.headers.get("accept", "")
        if any(part.split(";")[0].strip().lower() == "text/event-stream" for part in accept.split(",")):
            return self._stream_response(built, truncate_after)
        document: Json = {"turn_id": turn_id, "events": built.events}
        if built.grant is not None:
            document["confirmation"] = built.grant
        return self._json(200, document)

    # ---- routing ---------------------------------------------------------------------------------------------------

    def _register_endpoint(self, scope: _Scope, request: httpx.Request, raw: bytes) -> httpx.Response:
        if "applications.register" not in scope.capability_scopes:
            return self._problem("permission_denied", "the context token does not carry the applications.register scope")
        if request.headers.get("content-type", "").split(";")[0].strip().lower() != "application/gzip":
            return self._problem("validation_failed", "the package must be sent as application/gzip", status=415)
        if len(raw) < 2 or raw[0] != 0x1F or raw[1] != 0x8B:
            return self._problem("validation_failed", "the body is not a gzip archive", status=422)
        digest = hashlib.sha256(raw).hexdigest()
        existing = self._registered.get(digest)
        record = existing or self._package("quarantined", digest)
        if existing is None:
            self._registered[digest] = record
        registration = {"application_id": scope.application_id, "package": record, "created": existing is None, "findings": []}
        return self._json(200 if existing is not None else 201, registration)

    def _handle_authenticated(self, operation: str, scope: _Scope, params: dict[str, str], request: httpx.Request, body: Any, raw: bytes, failure: Optional[InjectedFailure]) -> httpx.Response:
        if operation == "createSession":
            if body is not None and body != {}:
                return self._problem("validation_failed", "the request body, when present, must be {}")
            refusal = self._quarantine_refusal()
            if refusal:
                return refusal
            session = self._open_session(scope)
            return session if isinstance(session, httpx.Response) else self._json(200, self._session_doc(session))
        if operation in ("getSession", "getPendingState", "endSession"):
            if params["session_id"] != scope.session_id:
                return self._level_violation("session")
            refusal = self._quarantine_refusal()
            if refusal:
                return refusal
            if operation == "endSession":
                existing = self._sessions.get(scope.session_key())
                if existing is not None and existing.status == "active":
                    if existing.preparation_id is not None:
                        record = self._preparations.get(existing.preparation_id)
                        if record is not None and record.action["state"] in ("prepared", "confirmed"):
                            record.action["state"] = "invalidated"
                    existing.status = "ended"
                    existing.preparation_id = None
                    existing.clarification = None
                elif existing is None:
                    self._sessions[scope.session_key()] = _Session(scope, self._now(), self._now(), status="ended")
                return self._json(200, {"session_id": scope.session_id, "status": "ended"})
            session = self._open_session(scope)
            if isinstance(session, httpx.Response):
                return session
            return self._json(200, self._session_doc(session) if operation == "getSession" else {"pending": self._pending_of(session)})
        if operation == "sendTurn":
            return self._turn_endpoint(scope, params["session_id"], request, body, failure.truncate_after_events if failure else None)
        if operation in ("getAction", "rejectAction", "getActionReceipt"):
            refusal = self._quarantine_refusal()
            if refusal:
                return refusal
            record = self._owned(scope, params["preparation_id"])
            if isinstance(record, httpx.Response):
                return record
            action = self._effective(record)
            if operation == "getAction":
                return self._json(200, action)
            if operation == "getActionReceipt":
                return self._problem("conflict", "the preparation has no receipt yet") if record.receipt is None else self._json(200, record.receipt)
            if action["state"] == "rejected":
                return self._json(200, action)
            if action["state"] not in ("prepared", "confirmed"):
                return self._problem("conflict", f"the preparation is {action['state']}")
            action["state"] = "rejected"
            session = self._sessions.get(scope.session_key())
            if session is not None and session.preparation_id == action["preparation_id"]:
                session.preparation_id = None
            return self._json(200, action)
        if operation == "confirmAction":
            return self._confirm_endpoint(scope, params["preparation_id"], body)
        if operation == "registerApplication":
            return self._register_endpoint(scope, request, raw)
        if operation == "getApplication":
            if params["application_id"] != scope.application_id:
                return self._level_violation("application")
            doc: Json = {"application_id": scope.application_id, "status": self._app_status, "packages": self._packages()}
            if self._app_status == "active":
                doc["health"] = {"status": "ok", "application_version": "1.0.0"}
            return self._json(200, doc)
        if operation == "listCapabilities":
            refusal = self._quarantine_refusal()
            if refusal:
                return refusal
            # Exact match: a capability the token does not list is not visible (and there is no wildcard).
            return self._json(200, {"capabilities": [dict(c) for c in self._capabilities if c["id"] in scope.capability_scopes]})
        if operation == "getDecisionStatus":
            if request.url.query:
                return self._problem("validation_failed", "this route takes no query string")
            if DECISION_STATUS_CAPABILITY not in scope.capability_scopes:
                return self._problem("permission_denied", f"the token does not carry {DECISION_STATUS_CAPABILITY}")
            refusal = self._quarantine_refusal()
            if refusal:
                return refusal
            if self._decision_unavailable:
                return self._problem("unavailable", "the server has no decision service")
            status = dict(self._decision_status) if self._decision_status is not None else decision_status_document()
            status["generated_at"] = self._iso(self._now())
            return self._json(200, status)
        if operation == "getDictationCapabilities":
            if self._dictation_unavailable:
                return self._problem("unavailable", "the server has no transcription service")
            available = not self._dictation_disabled
            return self._json(
                200,
                {
                    "available": available,
                    "locality": "local" if available else "none",
                    "max_seconds": 60,
                    "sample_rate_hz": 16000,
                    "format": "audio/wav",
                    "languages": ["en", "it"] if available else [],
                },
            )
        if operation == "createSessionDictation":
            if params["session_id"] != scope.session_id:
                return self._level_violation("session")
            if self._dictation_unavailable or self._dictation_disabled:
                return self._problem("unavailable", "the server has no transcription service")
            ctype = (request.headers.get("content-type") or "").split(";")[0].strip().lower()
            if ctype != "audio/wav":
                return self._problem("validation_failed", "Content-Type must be audio/wav", status=415)
            if not raw or len(raw) < 44 or raw[0:4] != b"RIFF":
                return self._problem("validation_failed", "the body is not a WAV", details={"reason": "malformed_wav"})
            if self._dictation_silent:
                return self._problem("validation_failed", "the clip is silent or too short", details={"reason": "silent"})
            language = request.url.params.get("language") or "en"
            return self._json(
                200,
                {
                    "text": self._dictation_text,
                    "language": language,
                    "duration_ms": max(1, (len(raw) - 44) // 32),
                    "locality": "local",
                    "segment_kind": "user_recorded",
                },
            )
        if operation in PROVIDERS_OPERATIONS:
            return self._providers.handle(operation, scope, params, request, body, raw)
        if operation == "getHealth":
            report = self._ready_report()
            health = {**report, "application": {"application_id": scope.application_id, "status": self._app_status}}
            return self._json(200 if report["ready"] else 503, health, {} if report["ready"] else {"Retry-After": "1"})
        return self._problem("internal", "the fake server does not implement this operation")

    def handler(self, request: httpx.Request) -> httpx.Response:
        """The httpx transport handler: routes one request."""
        url = request.url
        if f"{url.scheme}://{url.netloc.decode('ascii')}" != self._origin:
            raise httpx.ConnectError(f"{url.host} is not the fake server ({self._origin})", request=request)
        path = url.path
        if self._prefix:
            if path != self._prefix and not path.startswith(self._prefix + "/"):
                return self._problem("validation_failed", "not found", status=404)
            path = path[len(self._prefix) :] or "/"
        raw = request.content
        body: Any = None
        if raw and "json" in request.headers.get("content-type", ""):
            try:
                body = json.loads(raw)
            except ValueError:
                body = None
        entry = RecordedRequest(
            operation=None,
            method=request.method,
            path=path,
            accept=request.headers.get("accept"),
            content_type=request.headers.get("content-type"),
            authenticated="authorization" in request.headers,
            body=_redact(body),
        )

        def finish(response: httpx.Response) -> httpx.Response:
            entry.status = response.status_code
            self.requests.append(entry)
            return response

        operation: Optional[str] = None
        params: dict[str, str] = {}
        path_known = False
        for operation_id, spec in OPERATIONS.items():
            matched = match_path(spec.path, path)
            if matched is None:
                continue
            path_known = True
            if spec.method != request.method:
                continue
            operation, params = operation_id, matched
            break
        entry.operation = operation
        if operation is None:
            return finish(self._problem("validation_failed", "method not allowed" if path_known else "not found", status=405 if path_known else 404))
        if not OPERATIONS[operation].authenticated:
            if operation == "getLiveness":
                return finish(self._json(200, {"status": "ok"}))
            report = self._ready_report()
            return finish(self._json(200 if report["ready"] else 503, report, {} if report["ready"] else {"Retry-After": "1"}))
        scope, token_id, refusal = self._authenticate(request)
        entry.token_id = token_id
        if refusal is not None:
            return finish(refusal)
        assert scope is not None
        injected = next((f for f in self._failures if f.operation is None or f.operation == operation), None)
        if injected is not None:
            injected.times -= 1
            if injected.times <= 0:
                self._failures.remove(injected)
            if injected.network_error is not None:
                self.requests.append(entry)
                raise injected.network_error
            if injected.status is not None and injected.error is not None:
                return finish(self._json(injected.status, injected.error, injected.headers))
        return finish(self._handle_authenticated(operation, scope, params, request, body, raw, injected))

