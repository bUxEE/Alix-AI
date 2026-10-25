"""The synchronous client against the fake server: the behaviour sdk/typescript/test/client.test.ts pins, in Python."""

from __future__ import annotations

import gzip
from typing import Any

import httpx
import pytest

from alixai import AlixAIClient, AlixAIError, ContextTokenRequest, turn_text
from alixai.testing import Commit, FakeAlixAIServer, ask_choice, fail_turn, prepare_action, reject_turn, reply

BOOK = {"capability_id": "demo.book", "summary": "Book the 10:00 slot", "text": "Shall I?"}
GZIP = gzip.compress(b"a package")


def failure(call: Any) -> AlixAIError:
    with pytest.raises(AlixAIError) as raised:
        call()
    return raised.value


@pytest.fixture
def fake() -> FakeAlixAIServer:
    return FakeAlixAIServer()


@pytest.fixture
def client(fake: FakeAlixAIServer) -> AlixAIClient:
    return fake.client("ses-1")


# ---- probes, sessions ------------------------------------------------------------------------------------------------


def test_the_probes_ask_for_no_token_and_a_service_that_is_not_ready_returns_its_report(fake: FakeAlixAIServer) -> None:
    asked: list[ContextTokenRequest] = []
    client = fake.client(get_context_token=lambda request: asked.append(request) or fake.issue_token("ses-1"))
    assert client.get_liveness().status == "ok"
    assert client.get_readiness().ready is True
    assert asked == []
    fake.set_ready(False)
    report = client.get_readiness()
    assert report.ready is False and report.status == "unavailable"
    health = client.get_health()  # authenticated, and 503 as well
    assert health.ready is False and health.application.application_id == "fake-app"
    assert [r.operation for r in asked] == ["getHealth"]


def test_a_session_is_created_resumed_and_ended(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    session = client.create_session()
    assert (session.session_id, session.status, session.turn_count, session.pending) == ("ses-1", "active", 0, None)
    assert client.create_session().session_id == "ses-1"  # opening again is the same session
    assert client.get_session("ses-1").status == "active"
    assert client.get_pending_state("ses-1") is None
    assert client.end_session("ses-1").status == "ended"
    gone = failure(lambda: client.get_session("ses-1"))
    assert (gone.code, gone.status) == ("expired", 410)
    assert fake.session("ses-1") is not None


def test_the_token_provider_is_told_which_session_preparation_or_application_the_request_concerns(fake: FakeAlixAIServer) -> None:
    asked: list[ContextTokenRequest] = []
    client = fake.client(get_context_token=lambda request: asked.append(request) or fake.issue_token("ses-1"))
    client.get_session("ses-1")
    client.get_application("fake-app")
    failure(lambda: client.get_action("prep_x"))
    assert [(r.operation, r.session_id, r.preparation_id, r.application_id) for r in asked] == [
        ("getSession", "ses-1", None, None),
        ("getApplication", None, None, "fake-app"),
        ("getAction", None, "prep_x", None),
    ]


def test_a_token_is_single_use_so_a_host_that_caches_it_fails_with_replayed(fake: FakeAlixAIServer) -> None:
    token = fake.issue_token("ses-1")
    client = fake.client(get_context_token=lambda request: token)
    client.get_session("ses-1")
    error = failure(lambda: client.get_session("ses-1"))
    assert (error.code, error.status, error.retryable) == ("replayed", 401, False)
    assert error.request_id is not None and error.details is not None and error.details["request_id"] == error.request_id


def test_a_session_of_another_context_is_a_scope_violation_that_keeps_the_servers_details(client: AlixAIClient) -> None:
    error = failure(lambda: client.get_session("ses-9"))
    assert (error.code, error.status) == ("scope_violation", 403)
    assert error.details is not None and error.details["levels"] == ["session"]


# ---- nothing malformed costs a token ----------------------------------------------------------------------------------


@pytest.mark.parametrize(
    "call",
    [
        lambda c: c.get_session(""),
        lambda c: c.get_session("../etc"),
        lambda c: c.get_session(".."),
        lambda c: c.get_session("a b"),
        lambda c: c.get_action("x" * 129),
        lambda c: c.confirm_action("prep_x", "yes"),
        lambda c: c.confirm_action("prep_x", "cfm_short"),
        lambda c: c.send_turn_json("ses-1", {}),
        lambda c: c.send_turn_json("ses-1", {"text": "a", "choice_id": "b"}),
        lambda c: c.send_turn_json("ses-1", {"text": ""}),
        lambda c: c.send_turn_json("ses-1", {"text": "a", "extra": 1}),
        lambda c: c.send_turn_json("ses-1", {"confirmation": {"preparation_id": "p", "token": "nope"}}),
        lambda c: c.send_turn_json("ses-1", {"reject": {"preparation_id": "p"}, "language": "en"}),
        lambda c: c.register_application(b"not gzip"),
        lambda c: c.register_application("a string"),
        lambda c: c.get_session("ses-1", timeout_ms=-1),
    ],
)
def test_a_malformed_argument_is_refused_before_a_context_token_is_requested(call: Any, fake: FakeAlixAIServer) -> None:
    asked: list[ContextTokenRequest] = []
    client = fake.client(get_context_token=lambda request: asked.append(request) or fake.issue_token("ses-1"))
    assert failure(lambda: call(client)).code == "validation_failed"
    assert asked == [] and fake.requests == []


def test_a_token_provider_that_fails_or_returns_a_bad_token_sends_nothing(fake: FakeAlixAIServer) -> None:
    def boom(request: ContextTokenRequest) -> str:
        raise RuntimeError("secret store down")

    error = failure(fake.client(get_context_token=boom).create_session)
    assert (error.code, error.details) == ("unauthenticated", {"operation": "createSession", "reason": "token_provider_failed"})
    assert "secret" not in str(error)
    for bad in ("Bearer abc", "has space", "", "a\nb"):
        assert failure(fake.client(get_context_token=lambda request, bad=bad: bad).create_session).code == "unauthenticated"
    assert fake.requests == []


@pytest.mark.parametrize(
    "url",
    ["", "alixai.example.com", "ftp://alixai.example.com", "http://alixai.example.com", "https://user:pw@alixai.example.com", "https://alixai.example.com?x=1", "https://alixai.example.com#f"],
)
def test_a_base_url_that_would_expose_the_bearer_token_is_refused(url: str) -> None:
    assert failure(lambda: AlixAIClient(url, lambda request: "t")).code == "validation_failed"


def test_plain_http_is_allowed_for_loopback_and_by_explicit_opt_in() -> None:
    AlixAIClient("http://127.0.0.1:8080", lambda request: "t").close()
    AlixAIClient("http://localhost:8080/", lambda request: "t").close()
    AlixAIClient("http://alixai.internal", lambda request: "t", allow_insecure_http=True).close()


def test_a_path_prefix_in_the_base_url_is_kept() -> None:
    fake = FakeAlixAIServer("https://alixai.test/alixai/")
    client = fake.client("ses-1")
    assert client.base_url == "https://alixai.test/alixai"
    assert client.create_session().session_id == "ses-1"
    assert fake.requests[-1].path == "/v1/sessions"


# ---- turns -----------------------------------------------------------------------------------------------------------


def test_a_text_turn_streams_its_events_in_order_and_ends_with_done(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: reply("Hello there"))
    with client.send_turn("ses-1", {"text": "hi", "language": "en"}) as stream:
        events = list(stream)
    assert [e.seq for e in events] == list(range(len(events)))
    assert events[-1].type == "done" and events[-1].finish_reason == "completed"
    assert turn_text(events) == "Hello there"
    assert fake.requests[-1].accept == "text/event-stream" and fake.requests[-1].body == {"text": "hi", "language": "en"}


def test_json_and_stream_forms_carry_the_same_events(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: ask_choice("slot", "Which slot?", [{"choice_id": "a", "label": "10:00"}, {"choice_id": "b", "label": "11:00"}]))
    streamed = client.send_turn("ses-1", {"text": "book"}).result()
    as_json = client.send_turn_json("ses-1", {"text": "book"})
    assert [e.type for e in streamed.events] == [e.type for e in as_json.events]
    assert streamed.events[-1].finish_reason == "awaiting_input"
    pending = client.get_pending_state("ses-1")
    assert pending is not None and pending.kind == "clarification" and pending.question is not None
    assert [c.choice_id for c in pending.question.choices or []] == ["a", "b"]


@pytest.mark.parametrize("chunk", [1, 7, 64, 100_000])
def test_a_stream_split_into_tiny_chunks_decodes_the_same(chunk: int) -> None:
    fake = FakeAlixAIServer(stream_chunk_bytes=chunk)
    fake.on_turn(lambda turn: reply("café ☃ " * 30))
    client = fake.client("ses-1")
    assert turn_text(client.send_turn("ses-1", {"text": "x"}).result().events) == "café ☃ " * 30


def test_the_confirmation_secret_is_not_an_event_and_pairs_with_the_prepared_action(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: prepare_action(**BOOK))
    with client.send_turn("ses-1", {"text": "book"}) as stream:
        with pytest.raises(RuntimeError):
            _ = stream.confirmation  # not before done
        events = list(stream)
        grant = stream.confirmation
    assert grant is not None and events[-1].finish_reason == "awaiting_confirmation"
    prepared = next(e for e in events if e.type == "prepared_action")
    assert grant.preparation_id == prepared.prepared_action.preparation_id == events[-1].preparation_id
    assert grant.token.startswith("cfm_") and grant.token not in repr(grant)
    assert all(grant.token not in e.model_dump_json() for e in events)
    assert client.send_turn_json("ses-1", {"text": "book"}).confirmation is not None  # the JSON form carries it as a member


def test_a_level_three_action_has_no_secret_and_cannot_be_confirmed_through_the_ai_path(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: prepare_action(**BOOK, risk_level=3))
    result = client.send_turn("ses-1", {"text": "close it"}).result()
    assert result.confirmation is None
    directive = next(e for e in result.events if e.type == "directive")
    assert directive.directive.act == "request_step_up"


def test_confirming_by_a_structured_turn_commits_and_a_text_turn_never_does(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: prepare_action(**BOOK))
    grant = client.send_turn_json("ses-1", {"text": "book"}).confirmation
    assert grant is not None
    assert [e.type for e in client.send_turn_json("ses-1", {"text": "yes, confirm"}).events].count("receipt") == 0
    done = client.send_turn("ses-1", {"confirmation": {"preparation_id": grant.preparation_id, "token": grant.token}}).result()
    assert next(e for e in done.events if e.type == "receipt").receipt.status == "committed"
    assert client.get_action(grant.preparation_id).state == "committed"


def test_stopping_early_releases_the_connection(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: reply("a long answer"))
    stream = client.send_turn("ses-1", {"text": "x"})
    assert next(stream).type == "progress"
    stream.close()
    assert list(stream) == []  # a closed stream is exhausted, not an error
    assert stream._response.is_closed  # noqa: SLF001


def test_a_stream_that_ends_before_done_is_an_unknown_outcome_never_a_quiet_end(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: reply("hello"))
    fake.fail_next(operation="sendTurn", truncate_after_events=3)
    stream = client.send_turn("ses-1", {"text": "x"})
    with pytest.raises(AlixAIError) as raised:
        list(stream)
    assert (raised.value.code, raised.value.retryable) == ("uncertain_outcome", False)
    assert raised.value.details == {"reason": "stream_ended_early", "received": 3}
    assert list(stream) == []  # delivered once


def test_a_turn_that_fails_has_a_typed_error_event_before_done(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: fail_turn("unavailable", "the application is not answering"))
    events = client.send_turn("ses-1", {"text": "x"}).result().events
    error = next(e for e in events if e.type == "error")
    assert (error.error.code, error.error.retryable) == ("unavailable", True)
    assert events[-1].finish_reason == "failed"


def test_a_turn_that_could_not_run_is_an_error_response_never_a_stream(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: reject_turn(429, {"code": "rate_limited", "message": "slow down", "retryable": True}))
    error = failure(lambda: client.send_turn("ses-1", {"text": "x"}))
    assert (error.code, error.status, error.retryable) == ("rate_limited", 429, True)


# ---- actions ---------------------------------------------------------------------------------------------------------


def prepared(client: AlixAIClient, fake: FakeAlixAIServer, **commit: Any) -> Any:
    fake.on_turn(lambda turn: prepare_action(**BOOK, commit=Commit(**commit)))
    grant = client.send_turn_json("ses-1", {"text": "book"}).confirmation
    assert grant is not None
    return grant


def test_confirm_commits_once_and_a_second_use_of_the_secret_is_replayed(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    grant = prepared(client, fake)
    assert client.get_action(grant.preparation_id).state == "prepared"
    assert failure(lambda: client.get_action_receipt(grant.preparation_id)).code == "conflict"  # no outcome yet
    outcome = client.confirm_action(grant.preparation_id, grant.token)
    assert outcome.receipt is not None and outcome.receipt.status == "committed"
    assert client.get_action_receipt(grant.preparation_id).receipt_id == outcome.receipt.receipt_id
    again = failure(lambda: client.confirm_action(grant.preparation_id, grant.token))
    assert (again.code, again.status) == ("replayed", 409)


@pytest.mark.parametrize(("status", "code", "http"), [("rejected", "conflict", 409), ("partial", "partial_failure", 500), ("uncertain", "uncertain_outcome", 500)])
def test_a_commit_that_did_not_succeed_raises_with_the_receipt_beside_the_error(client: AlixAIClient, fake: FakeAlixAIServer, status: str, code: str, http: int) -> None:
    grant = prepared(client, fake, status=status)
    error = failure(lambda: client.confirm_action(grant.preparation_id, grant.token))
    assert (error.code, error.status) == (code, http)
    assert error.receipt is not None and error.receipt.status == status and error.receipt.errors
    assert error.preparation is not None and error.preparation.preparation_id == grant.preparation_id
    assert error.retryable is False


def test_a_queued_commit_succeeds_and_its_receipt_carries_the_follow_up(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    grant = prepared(client, fake, status="queued")
    receipt = client.confirm_action(grant.preparation_id, grant.token).receipt
    assert receipt is not None and receipt.status == "queued" and receipt.follow_up is not None


def test_reject_declines_a_preparation_and_it_can_no_longer_be_confirmed(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    grant = prepared(client, fake)
    assert client.reject_action(grant.preparation_id).state == "rejected"
    assert client.reject_action(grant.preparation_id).state == "rejected"  # rejecting twice is the same
    assert failure(lambda: client.confirm_action(grant.preparation_id, grant.token)).code == "conflict"


def test_an_expired_preparation_cannot_be_confirmed(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    grant = prepared(client, fake)
    fake.advance_time(601)
    assert failure(lambda: client.confirm_action(grant.preparation_id, grant.token)).code == "expired"


def test_a_preparation_of_another_session_is_a_scope_violation(fake: FakeAlixAIServer) -> None:
    mine = fake.client("ses-1")
    fake.on_turn(lambda turn: prepare_action(**BOOK))
    grant = mine.send_turn_json("ses-1", {"text": "book"}).confirmation
    assert grant is not None
    assert failure(lambda: fake.client("ses-2").get_action(grant.preparation_id)).code == "scope_violation"


def test_the_ending_of_a_session_invalidates_its_waiting_preparation(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    grant = prepared(client, fake)
    client.end_session("ses-1")
    assert client.get_action(grant.preparation_id).state == "invalidated"
    assert failure(lambda: client.confirm_action(grant.preparation_id, grant.token)).code == "stale"


# ---- applications and capabilities -----------------------------------------------------------------------------------


def test_capabilities_are_the_ones_the_token_may_see_with_their_risk_levels(fake: FakeAlixAIServer) -> None:
    client = fake.client("ses-1", claims={"capability_scopes": ["demo.lookup", "demo.close_account"]})
    listed = {c.id: (c.risk_level, c.requires_step_up) for c in client.list_capabilities().capabilities}
    assert listed == {"demo.lookup": (0, False), "demo.close_account": (3, True)}


def test_registering_a_package_needs_its_scope_and_it_lands_in_quarantine(fake: FakeAlixAIServer) -> None:
    assert failure(lambda: fake.client("ses-1").register_application(GZIP)).code == "permission_denied"
    client = fake.client("ses-1", claims={"capability_scopes": ["applications.register"]})
    first = client.register_application(GZIP)
    assert (first.created, first.package.state, first.application_id) == (True, "quarantined", "fake-app")
    assert client.register_application(GZIP).created is False  # the same content again
    application = client.get_application("fake-app")
    assert application.status == "active" and any(p.state == "quarantined" for p in application.packages)
    registration = next(r for r in fake.requests if r.operation == "registerApplication" and r.status == 201)
    assert registration.content_type == "application/gzip"


def test_an_application_that_is_not_active_serves_no_traffic(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.set_application_status("quarantined")
    for call in (client.create_session, client.list_capabilities, lambda: client.send_turn_json("ses-1", {"text": "x"})):
        error = failure(call)
        assert (error.code, error.status) == ("quarantined", 403)
    assert client.get_application("fake-app").status == "quarantined"
    assert failure(lambda: client.get_application("someone-else")).code == "scope_violation"


# ---- failures of the transport and of the server ---------------------------------------------------------------------


def test_a_network_failure_of_a_read_is_unavailable_and_retryable(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.fail_next(network_error=httpx.ReadError("connection reset"))
    error = failure(client.list_capabilities)
    assert (error.code, error.retryable) == ("unavailable", True)


def test_a_network_failure_of_a_write_is_an_unknown_outcome_because_the_request_may_have_been_processed(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: prepare_action(**BOOK))
    grant = client.send_turn_json("ses-1", {"text": "book"}).confirmation
    assert grant is not None
    fake.fail_next(network_error=httpx.ReadTimeout("slow"))
    error = failure(lambda: client.confirm_action(grant.preparation_id, grant.token))
    assert (error.code, error.retryable) == ("uncertain_outcome", False)
    assert client.get_action(grant.preparation_id).state == "prepared"  # reading the state is how the host finds out


def test_a_request_that_never_left_the_machine_is_not_an_unknown_outcome(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.fail_next(network_error=httpx.ConnectError("refused"))
    error = failure(client.create_session)
    assert (error.code, error.retryable) == ("unavailable", True)
    fake.fail_next(network_error=httpx.ConnectTimeout("no route"))
    assert failure(client.create_session).code == "timeout"


def test_the_servers_error_document_and_retry_after_are_kept(client: AlixAIClient, fake: FakeAlixAIServer) -> None:
    fake.fail_next(operation="listCapabilities", status=503, error={"code": "unavailable", "message": "down", "retryable": True}, headers={"Retry-After": "7"})
    error = failure(client.list_capabilities)
    assert (error.code, error.status, error.retry_after_ms, error.message) == ("unavailable", 503, 7000, "down")


@pytest.mark.parametrize(
    ("status", "body", "code"),
    [(502, "<html>bad gateway</html>", "unavailable"), (504, "", "timeout"), (401, "nope", "unauthenticated"), (418, "{}", "internal"), (409, '{"code":"made_up"}', "conflict")],
)
def test_a_response_that_is_not_an_error_document_gets_a_typed_fallback_by_status(status: int, body: str, code: str) -> None:
    transport = httpx.MockTransport(lambda request: httpx.Response(status, text=body))
    client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=transport))
    error = failure(lambda: client.get_session("ses-1"))
    assert error.code == code and error.status == status
    assert error.details == {"reason": "non_conforming_error_body"}


def test_a_success_with_the_wrong_shape_or_not_json_is_internal_and_never_leaks_the_body() -> None:
    for text in ('{"session_id": 5}', "<html>login</html>", "[]"):
        client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=httpx.MockTransport(lambda request, text=text: httpx.Response(200, text=text))))
        error = failure(lambda: client.get_session("ses-1"))
        assert error.code == "internal" and "login" not in str(error) and error.details is not None
        assert error.details["reason"] in ("unexpected_shape", "not_json")


class _ErrorBodyCutOff(httpx.SyncByteStream):
    """The body of an error answer whose connection dies while it is read."""

    def __init__(self) -> None:
        self.closed = False

    def __iter__(self) -> Any:
        raise httpx.ReadError("connection reset")
        yield b""  # pragma: no cover

    def close(self) -> None:
        self.closed = True


def test_a_stream_whose_error_body_cannot_be_read_still_releases_its_connection() -> None:
    body = _ErrorBodyCutOff()
    transport = httpx.MockTransport(lambda request: httpx.Response(503, stream=body))
    client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=transport))
    assert failure(lambda: client.send_turn("ses-1", {"text": "hi"})).code == "uncertain_outcome"
    assert body.closed, "the response of a failed stream request was left open (its pooled connection leaks)"


def test_a_200_confirmation_without_a_receipt_is_not_a_success() -> None:
    transport = httpx.MockTransport(lambda request: httpx.Response(200, json={}))
    client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=transport))
    token = "cfm_" + "A" * 43
    assert failure(lambda: client.confirm_action("prep_x", token)).details == {"operation": "confirmAction", "reason": "unexpected_shape"}


def test_a_redirect_is_never_followed_with_the_credential() -> None:
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(302, headers={"Location": "https://evil.example/steal"})

    client = AlixAIClient("https://alixai.test", lambda request: "tok", http_client=httpx.Client(transport=httpx.MockTransport(handler)))
    assert failure(lambda: client.get_session("ses-1")).status == 302
    assert len(seen) == 1 and seen[0].headers["authorization"] == "Bearer tok"
