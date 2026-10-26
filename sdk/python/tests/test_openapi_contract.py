"""The SDK, the fake server and schemas/api/openapi.yaml must agree. The OpenAPI document is the contract: when it
changes (or when the SDK or the fake drifts from it) this file fails. A port of sdk/typescript/test/openapi-contract.test.ts."""

from __future__ import annotations

import gzip
import json
import struct
from typing import Any

import httpx
import pytest

from alixai import OPERATIONS, AlixAIClient, AlixAIError
from alixai.testing import DECISION_STATUS_CAPABILITY, Commit, FakeAlixAIServer, ask_choice, decision_status_document, fail_turn, prepare_action, reply
from tests.helpers.openapi import Spec, explain
from tests.test_providers import exercise_providers_conformance

# ---- the operation table against the document ------------------------------------------------------------------------


def test_every_operation_the_sdk_speaks_exists_in_the_document_with_the_same_method_path_and_authentication(spec: Spec) -> None:
    for operation_id, op in OPERATIONS.items():
        declared = [o for o in spec.operations if o.operation_id == operation_id]
        assert declared, f"{operation_id} is not in the OpenAPI document"
        assert (declared[0].method, declared[0].path, declared[0].authenticated) == (op.method, op.path, op.authenticated), operation_id


def test_the_sdk_covers_every_operation_of_the_areas_this_version_implements(spec: Spec) -> None:
    implemented = {"root", "sessions", "actions", "apps"}
    # Operations of an implemented area that this SDK version knowingly does not speak yet (as sdk/typescript does):
    # resuming a turn's events needs a fetch-based reader per reconnect, planned with the stream hub client.
    not_yet = {"streamTurnEvents"}
    missing = [o.operation_id for o in spec.operations if o.area in implemented and o.operation_id not in not_yet and o.operation_id not in OPERATIONS]
    assert missing == [], "operations in the document that the SDK does not speak"


# ---- the validator really enforces the document (negative controls) ---------------------------------------------------

SESSION = {"session_id": "ses-1", "status": "active", "turn_count": 0, "created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z", "pending": None}


def test_the_validator_accepts_a_valid_session_and_refuses_one_that_is_wrong(spec: Spec) -> None:
    validate = spec.response_validator("getSession", 200)
    assert validate is not None and validate.is_valid(SESSION), explain(validate, SESSION)
    for bad in ({**SESSION, "status": "paused"}, {**SESSION, "turn_count": -1}, {**SESSION, "extra": 1}, {"session_id": "ses-1"}):
        assert not validate.is_valid(bad), json.dumps(bad)


def test_the_validator_resolves_the_shared_schemas(spec: Spec) -> None:
    error = spec.response_validator("getSession", 403)
    assert error is not None
    assert error.is_valid({"code": "scope_violation", "message": "no", "retryable": False})
    assert not error.is_valid({"code": "made_up", "message": "no", "retryable": False})
    action = spec.response_validator("getAction", 200)
    assert action is not None and not action.is_valid({"preparation_id": "x"})
    assert not spec.turn_event_validator.is_valid({"type": "token", "turn_id": "trn_x", "seq": 0, "text": "a"})


def test_the_request_validator_enforces_the_document_including_exactly_one_of(spec: Spec) -> None:
    turn = spec.request_validator("sendTurn")
    confirm = spec.request_validator("confirmAction")
    token = "cfm_" + "A" * 43
    assert turn is not None and confirm is not None
    for good in ({"text": "hi"}, {"choice_id": "a", "language": "en"}, {"confirmation": {"preparation_id": "p", "token": token}}, {"reject": {"preparation_id": "p"}}):
        assert turn.is_valid(good), explain(turn, good)
    for bad in ({}, {"text": "a", "choice_id": "b"}, {"text": "a", "extra": 1}, {"text": ""}, {"confirmation": {"preparation_id": "p", "token": "nope"}}):
        assert not turn.is_valid(bad), json.dumps(bad)
    assert confirm.is_valid({"token": token}) and not confirm.is_valid({"token": "x"}) and not confirm.is_valid({})
    # attachments: up to four file ids, with a text only
    file_id = "fil_0123456789ABCDEFGHJKMNPQRS"
    for good in ({"text": "hi", "attachments": [file_id]}, {"text": "hi", "language": "it", "attachments": [file_id] * 4}):
        assert turn.is_valid(good), explain(turn, good)
    for bad in (
        {"text": "hi", "attachments": [file_id] * 5},
        {"text": "hi", "attachments": ["../../etc/passwd"]},
        {"choice_id": "c1", "attachments": [file_id]},
        {"reject": {"preparation_id": "p"}, "attachments": [file_id]},
        {"attachments": [file_id]},
        {"text": "hi", "files": [file_id]},
    ):
        assert not turn.is_valid(bad), json.dumps(bad)
    assert list(spec.operation("registerApplication").request_body["content"]) == ["application/gzip"]  # type: ignore[index]
    assert spec.request_validator("getSession") is None


def test_the_document_declares_the_statuses_the_sdk_relies_on(spec: Spec) -> None:
    assert sorted(spec.declared_statuses("confirmAction")) == [200, 400, 401, 403, 409, 500, 503]
    assert 404 not in spec.declared_statuses("createSession")


# ---- the fake server's answers against the document -------------------------------------------------------------------


class Conformance:
    """A transport that forwards to the fake and checks every answer against the OpenAPI document."""

    def __init__(self, spec: Spec, fake: FakeAlixAIServer) -> None:
        self.spec = spec
        self.fake = fake
        self.violations: list[str] = []
        self.validated = 0
        self.request_bodies = 0
        self.frames = 0
        self.statuses: dict[str, set[int]] = {}

    def check_request(self, operation: str, request: httpx.Request) -> None:
        """What the SDK sent must be what the document says the operation accepts."""
        declared = self.spec.operation(operation).request_body
        if not request.content:
            if declared is not None and declared.get("required"):
                self.violations.append(f"{operation}: the document requires a request body and the SDK sent none")
            return
        if declared is None:
            self.violations.append(f"{operation}: the SDK sent a body to an operation that takes none")
            return
        content_type = request.headers.get("content-type", "").split(";")[0].strip().lower()
        if content_type not in declared["content"]:
            self.violations.append(f"{operation}: the SDK sent {content_type!r}; the document accepts {sorted(declared['content'])}")
            return
        validate = self.spec.request_validator(operation, content_type)
        if content_type == "application/json" and validate is not None:
            body = json.loads(request.content)
            self.request_bodies += 1
            if not validate.is_valid(body):
                self.violations.append(f"{operation} request: {explain(validate, body)}")
        elif content_type == "application/gzip" and request.content[:2] != bytes([0x1F, 0x8B]):
            self.violations.append(f"{operation}: the package is not gzip")

    def handler(self, request: httpx.Request) -> httpx.Response:
        response = self.fake.handler(request)
        entry = self.fake.requests[-1] if self.fake.requests else None
        operation = entry.operation if entry is not None else None
        if operation is None:
            self.violations.append(f"{request.method} {request.url}: no operation matched")
            return response
        self.check_request(operation, request)
        self.statuses.setdefault(operation, set()).add(response.status_code)
        if response.status_code not in self.spec.declared_statuses(operation):
            self.violations.append(f"{operation}: the fake answered {response.status_code}, which the document does not declare")
        content_type = response.headers.get("content-type", "")
        if "application/json" in content_type:
            validate = self.spec.response_validator(operation, response.status_code)
            if validate is None:
                self.violations.append(f"{operation} {response.status_code}: no JSON schema declared")
            else:
                body = json.loads(response.content)
                self.validated += 1
                if not validate.is_valid(body):
                    self.violations.append(f"{operation} {response.status_code}: {explain(validate, body)}\n{json.dumps(body)[:600]}")
        if "text/event-stream" in content_type:
            if operation != "sendTurn":
                self.violations.append(f"{operation}: an event stream from an operation that has none")
            for chunk in [b"".join(response.stream)]:  # type: ignore[arg-type]
                for frame in chunk.decode("utf-8").split("\n\n"):
                    if not frame:
                        continue
                    data = [line[6:] for line in frame.split("\n") if line.startswith("data: ")]
                    self.frames += 1
                    event = json.loads("".join(data))
                    if not self.spec.turn_event_validator.is_valid(event):
                        self.violations.append(f"sendTurn event: {explain(self.spec.turn_event_validator, event)}\n{json.dumps(event)[:600]}")
        return response


BOOK = {"capability_id": "demo.book", "summary": "Book the 10:00 slot", "text": "Shall I?"}
GZIP = gzip.compress(b"a package")


def test_every_answer_of_the_fake_server_conforms_to_the_document_across_all_operations(spec: Spec) -> None:
    fake = FakeAlixAIServer()
    watch = Conformance(spec, fake)
    scopes = ["demo.lookup", "demo.book", "demo.close_account", "applications.register", DECISION_STATUS_CAPABILITY]
    client = AlixAIClient(fake.base_url, fake.token_provider("ses-1", capability_scopes=scopes), http_client=httpx.Client(transport=httpx.MockTransport(watch.handler)))
    other = AlixAIClient(fake.base_url, fake.token_provider("ses-9"), http_client=httpx.Client(transport=httpx.MockTransport(watch.handler)))

    def swallow(call: Any) -> None:
        try:
            call()
        except AlixAIError:
            pass

    def plan(turn: Any) -> Any:
        text = turn.input.get("text", "")
        if text == "reply":
            return reply("hello")
        if text.startswith("commit:"):
            return prepare_action(**BOOK, commit=Commit(status=text[7:], facts=[{"field": "slot", "value": {"kind": "time", "value": "10:00"}}]))
        if text == "step-up":
            return prepare_action(**BOOK, risk_level=3)
        if text == "lookup":
            return prepare_action("demo.lookup", "Look the record up", risk_level=0)
        if text == "ask":
            return ask_choice("slot", "Which slot?", [{"choice_id": "a", "label": "10:00"}, {"choice_id": "b", "label": "11:00"}])
        if text == "fail":
            return fail_turn("unavailable", "the application is not answering")
        return prepare_action(**BOOK)

    fake.on_turn(plan)

    client.get_liveness()
    client.get_readiness()
    client.get_health()
    client.create_session()
    client.get_session("ses-1")
    client.get_pending_state("ses-1")
    client.list_capabilities()
    client.get_application("fake-app")
    client.register_application(GZIP)
    client.register_application(GZIP)
    client.send_turn_json("ses-1", {"text": "reply"})
    client.send_turn("ses-1", {"text": "reply"}).result()

    for status in ("committed", "rejected", "partial", "uncertain", "queued"):
        text = "book" if status == "committed" else f"commit:{status}"
        grant = client.send_turn_json("ses-1", {"text": text}).confirmation
        assert grant is not None
        assert client.get_action(grant.preparation_id) is not None
        swallow(lambda: client.confirm_action(grant.preparation_id, grant.token))
        client.get_action_receipt(grant.preparation_id)
        swallow(lambda: client.confirm_action(grant.preparation_id, grant.token))  # replayed
    rejected = client.send_turn_json("ses-1", {"text": "book"}).confirmation
    assert rejected is not None
    client.reject_action(rejected.preparation_id)
    another = client.send_turn_json("ses-1", {"text": "book"}).confirmation
    assert another is not None
    swallow(lambda: client.get_action_receipt(another.preparation_id))  # conflict: no outcome yet
    client.send_turn_json("ses-1", {"text": "step-up"})
    client.send_turn_json("ses-1", {"text": "lookup"})
    client.send_turn_json("ses-1", {"text": "fail"})
    client.send_turn_json("ses-1", {"text": "ask"})
    assert client.get_pending_state("ses-1") is not None  # a clarification is waiting
    client.send_turn_json("ses-1", {"choice_id": "a"})
    grant = client.send_turn("ses-1", {"text": "book"}).result().confirmation
    assert grant is not None
    client.send_turn("ses-1", {"confirmation": {"preparation_id": grant.preparation_id, "token": grant.token}}).result()

    # turn attachments: the ids travel in the request (validated against TurnRequest above); a deleted, an expired and an unknown file are the one scope_violation
    attached = fake.add_file("ses-1")
    client.send_turn_json("ses-1", {"text": "reply", "attachments": [attached]})
    client.send_turn("ses-1", {"text": "reply", "attachments": [attached, attached]}).result()
    gone, lapsed = fake.add_file("ses-1"), fake.add_file("ses-1")
    fake.delete_file(gone)
    fake.expire_file(lapsed)
    for file_id in (gone, lapsed, "fil_0123456789ABCDEFGHJKMNPQRS"):
        swallow(lambda file_id=file_id: client.send_turn_json("ses-1", {"text": "reply", "attachments": [attached, file_id]}))  # 403 scope_violation

    # the decision status (advisory): needs its scope, and a server without the service answers unavailable
    fake.set_decision_status(decision_status_document("shadow", accepted=3, fallbacks={"low_confidence": 2, "timeout": 1}))
    client.get_decision_status()
    swallow(other.get_decision_status)  # 403 permission_denied: the token lacks decision.status.read
    fake.set_decision_unavailable()
    swallow(client.get_decision_status)  # 503 unavailable
    fake.set_decision_unavailable(False)

    # Dictation (record-to-text, 1.1.0): capabilities and one-shot create; not the voice WebSocket.
    def _wav(seconds: float = 0.5, rate: int = 16000) -> bytes:
        n = int(rate * seconds)
        pcm = b"\x00\x10" * n
        return struct.pack(
            "<4sI4s4sIHHIIHH4sI",
            b"RIFF",
            36 + len(pcm),
            b"WAVE",
            b"fmt ",
            16,
            1,
            1,
            rate,
            rate * 2,
            2,
            16,
            b"data",
            len(pcm),
        ) + pcm

    client.dictations.capabilities()
    fake.set_dictation_text("book Tuesday")
    client.dictations.create("ses-1", _wav(), language="en")
    fake.set_dictation_silent(True)
    swallow(lambda: client.dictations.create("ses-1", _wav(0.2)))
    fake.set_dictation_silent(False)
    fake.set_dictation_unavailable(True)
    swallow(client.dictations.capabilities)
    fake.set_dictation_unavailable(False)

    # External Provider Mode: the providers and OAuth areas, every operation, through the recording transport (checked against the
    # document) and through a plain one (the budget create, which the document's request schema, the whole Budget, cannot describe)
    def provider_client(transport: httpx.MockTransport, **claims: Any) -> AlixAIClient:
        return AlixAIClient(fake.base_url, fake.token_provider("ses-1", **claims), http_client=httpx.Client(transport=transport))

    exercise_providers_conformance(
        fake,
        lambda **claims: provider_client(httpx.MockTransport(watch.handler), **claims),
        lambda **claims: provider_client(fake.transport, **claims),
    )

    client.me.catalog()
    client.me.llm()
    client.me.put_role("conversational_controller", {"profile_id": "pprf_01J0000000000000000000000A"})
    client.me.delete_role("conversational_controller")
    client.me.submit_credential("openai", "sk-test")
    client.me.delete_credential("openai")
    client.me.acknowledge_billing_notice(
        "openai",
        {
            "notice_version": "2026-01",
            "consumer_subscription_is_not_api_access": True,
            "separate_api_billing_understood": True,
        },
    )
    client.me.start_oauth("openai", capabilities=["chat"], redirect_uri="http://127.0.0.1:5555/cb")
    client.memory.list()
    created = client.memory.create({"text": "prefer dark mode"})
    mid = created["id"]
    client.memory.patch(mid, {"text": "prefer light mode"})
    client.memory.confirm(mid)
    client.memory.reject(mid)
    client.memory.delete(mid)

    # failure paths: scope, validation, authentication, availability, quarantine, ended session
    swallow(lambda: client.get_session("ses-9"))  # scope_violation
    swallow(lambda: client.get_application("someone-else"))
    swallow(lambda: other.get_action(another.preparation_id))
    fake.fail_next(operation="listCapabilities", status=503, error={"code": "unavailable", "message": "down", "retryable": True})
    swallow(client.list_capabilities)
    fake.set_ready(False)
    client.get_health()
    client.get_readiness()
    fake.set_ready(True)
    fake.set_application_status("quarantined")
    swallow(client.create_session)
    swallow(lambda: client.send_turn_json("ses-1", {"text": "reply"}))
    swallow(client.list_capabilities)
    fake.set_application_status("active")
    client.end_session("ses-1")
    swallow(lambda: client.get_session("ses-1"))  # 410

    assert watch.violations == []
    assert watch.validated > 40, "JSON bodies validated against the document"
    assert watch.request_bodies > 10, "request bodies validated against the document"
    assert watch.frames > 10, "event-stream frames validated against the TurnEvent schema"
    unexercised = sorted(set(OPERATIONS) - set(watch.statuses))
    assert unexercised == [], "operations this scenario never called"
    reached = {s for statuses in watch.statuses.values() for s in statuses}
    for status in (200, 201, 403, 409, 410, 500, 503):
        assert status in reached, f"status {status} was never exercised"
    assert fake.tokens_redeemed > 40


def test_a_conformance_check_that_cannot_fail_would_be_worthless(spec: Spec) -> None:
    """Negative control: a fake that answers with a member the document does not declare is reported."""
    fake = FakeAlixAIServer()
    watch = Conformance(spec, fake)
    original = fake._session_doc  # noqa: SLF001 - deliberately breaks the fake
    fake._session_doc = lambda session: {**original(session), "paused": True}  # type: ignore[method-assign]
    client = AlixAIClient(fake.base_url, fake.token_provider("ses-1"), http_client=httpx.Client(transport=httpx.MockTransport(watch.handler)))
    with pytest.raises(AlixAIError):  # the strict model refuses it as well
        client.create_session()
    assert any("createSession 200" in v for v in watch.violations)
