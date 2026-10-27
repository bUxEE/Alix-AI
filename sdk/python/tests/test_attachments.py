"""Files in a turn (M3-20 gateway, M3-29 SDK): ``attachments`` names up to four uploaded file ids in a text turn.

The SDK checks the list before a context token is spent, the fake resolves the ids under the scope before any handler runs,
and every reference that does not resolve for the caller is the same ``scope_violation``. Mirrors the TypeScript and Go tests.
"""

from __future__ import annotations

from typing import Any, Callable

import pytest

from alixai import MAX_TURN_ATTACHMENTS, AlixAIClient, AlixAIError
from alixai.testing import FakeAlixAIServer, reply

UNKNOWN = "fil_0123456789ABCDEFGHJKMNPQRS"


def failure(call: Callable[[], Any]) -> AlixAIError:
    with pytest.raises(AlixAIError) as raised:
        call()
    return raised.value


@pytest.fixture
def fake() -> FakeAlixAIServer:
    return FakeAlixAIServer()


@pytest.fixture
def client(fake: FakeAlixAIServer) -> AlixAIClient:
    return fake.client("ses-1")


def recording(fake: FakeAlixAIServer) -> list[dict[str, Any]]:
    """Makes the fake answer with a reply and remember what the turns it ran were given."""
    ran: list[dict[str, Any]] = []

    def handler(turn: Any) -> Any:
        ran.append(turn.input)
        return reply("ok")

    fake.on_turn(handler)
    return ran


def test_the_file_ids_travel_with_the_text_and_the_handler_sees_them_in_order(fake: FakeAlixAIServer, client: AlixAIClient) -> None:
    ran = recording(fake)
    first, second = fake.add_file("ses-1"), fake.add_file("ses-1")
    result = client.send_turn("ses-1", {"text": "read these", "language": "en", "attachments": [second, first]}).result()
    assert result.done is not None and result.done.finish_reason == "completed"
    body = fake.requests[-1].body
    assert isinstance(body, dict) and body["text"] == "read these" and body["language"] == "en" and body["attachments"] == [second, first]
    assert body.get("client_capabilities", {}).get("contract") == "1.5"
    assert ran == [{"text": "read these", "attachments": [second, first]}]
    client.send_turn_json("ses-1", {"text": "again", "attachments": (first,)})  # a tuple is a list too
    assert ran[-1] == {"text": "again", "attachments": [first]}


def test_an_empty_list_is_left_out_and_four_files_and_a_repeated_file_are_accepted(fake: FakeAlixAIServer, client: AlixAIClient) -> None:
    client.send_turn_json("ses-1", {"text": "hi", "attachments": []})
    body = fake.requests[-1].body
    assert isinstance(body, dict) and body["text"] == "hi" and "attachments" not in body
    assert body.get("client_capabilities", {}).get("contract") == "1.5"
    ids = [fake.add_file("ses-1") for _ in range(MAX_TURN_ATTACHMENTS)]
    client.send_turn_json("ses-1", {"text": "all", "attachments": ids})
    client.send_turn_json("ses-1", {"text": "twice", "attachments": [ids[0], ids[0]]})


def test_a_malformed_list_is_refused_before_a_context_token_is_spent(fake: FakeAlixAIServer, client: AlixAIClient) -> None:
    ok = fake.add_file("ses-1")
    token = "cfm_" + "A" * 43
    attempts: dict[str, Any] = {
        "more than four": {"text": "x", "attachments": [ok] * 5},
        "not a file id": {"text": "x", "attachments": ["../../etc/passwd"]},
        "a lower-case id": {"text": "x", "attachments": [ok.lower()]},
        "a number": {"text": "x", "attachments": [7]},
        "a bare string, which would be read one character at a time": {"text": "x", "attachments": ok},
        "another kind of id": {"text": "x", "attachments": ["src_0123456789ABCDEFGHJKMNPQRS"]},
        "without a text": {"attachments": [ok]},
        "with a choice": {"choice_id": "c1", "attachments": [ok]},
        "with a confirmation": {"confirmation": {"preparation_id": "prep_1", "token": token}, "attachments": [ok]},
        "with a rejection": {"reject": {"preparation_id": "prep_1"}, "attachments": [ok]},
    }
    for label, value in attempts.items():
        for call in (lambda v=value: client.send_turn("ses-1", v), lambda v=value: client.send_turn_json("ses-1", v)):
            assert failure(call).code == "validation_failed", label
    assert fake.tokens_redeemed == 0
    assert [r for r in fake.requests if r.operation == "sendTurn"] == []


def other(fake: FakeAlixAIServer, **claims: Any) -> str:
    return fake.add_file(claims.pop("session_id", "ses-1"), **claims)


@pytest.mark.parametrize(
    "label, make",
    [
        ("another principal", lambda fake: other(fake, principal_id="user-2")),
        ("another tenant", lambda fake: other(fake, tenant_id="tenant-2")),
        ("another workspace", lambda fake: other(fake, workspace_id="workspace-2")),
        ("another application", lambda fake: other(fake, application_id="other-app")),
        ("another session", lambda fake: other(fake, session_id="ses-9")),
        ("a deleted one", lambda fake: _deleted(fake)),
        ("an expired one", lambda fake: _expired(fake)),
        ("one that never existed", lambda fake: UNKNOWN),
    ],
)
def test_a_file_that_does_not_resolve_is_one_scope_violation_with_no_model_call_and_no_turn(label: str, make: Callable[[FakeAlixAIServer], str]) -> None:
    fake = FakeAlixAIServer()
    client = fake.client("ses-1")
    ran = recording(fake)
    bad = make(fake)
    good = fake.add_file("ses-1")
    before = client.get_session("ses-1").turn_count
    for call in (
        lambda: client.send_turn("ses-1", {"text": "x", "attachments": [good, bad]}).result(),  # one bad reference refuses the whole turn
        lambda: client.send_turn_json("ses-1", {"text": "x", "attachments": [bad]}),
    ):
        error = failure(call)
        assert (error.code, error.status) == ("scope_violation", 403), label
        # Nothing tells the cases apart: one message, no reason, no level.
        assert error.message == "the file reference does not resolve for this caller", label
        assert "reason" not in (error.details or {}) and "levels" not in (error.details or {}), label
    assert ran == [], label
    assert client.get_session("ses-1").turn_count == before, label


def _deleted(fake: FakeAlixAIServer) -> str:
    file_id = fake.add_file("ses-1")
    fake.delete_file(file_id)
    return file_id


def _expired(fake: FakeAlixAIServer) -> str:
    file_id = fake.add_file("ses-1")
    fake.advance_time(60 * 60)  # past the file's 15 minutes
    return file_id


async def test_the_async_client_takes_the_same_member(fake: FakeAlixAIServer) -> None:
    ran = recording(fake)
    file_id = fake.add_file("ses-1")
    async with fake.async_client("ses-1") as client:
        stream = await client.send_turn("ses-1", {"text": "see", "attachments": [file_id]})
        async for _ in stream:
            pass
        assert ran == [{"text": "see", "attachments": [file_id]}]
        with pytest.raises(AlixAIError) as bad:
            await client.send_turn_json("ses-1", {"text": "x", "attachments": [UNKNOWN]})
        assert bad.value.code == "scope_violation"
        with pytest.raises(AlixAIError) as malformed:
            await client.send_turn("ses-1", {"text": "x", "attachments": [file_id] * 5})
        assert malformed.value.code == "validation_failed"
    assert len(ran) == 1
