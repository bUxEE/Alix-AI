"""The asynchronous client against the fake server: the same operations as coroutines."""

from __future__ import annotations

import asyncio
from typing import Any

import httpx
import pytest

from alixai import AlixAIAsyncClient, AlixAIError, ContextTokenRequest, turn_text
from alixai.testing import FakeAlixAIServer, prepare_action, reply

BOOK = {"capability_id": "demo.book", "summary": "Book the 10:00 slot", "text": "Shall I?"}


async def failure(call: Any) -> AlixAIError:
    with pytest.raises(AlixAIError) as raised:
        await call()
    return raised.value


@pytest.fixture
def fake() -> FakeAlixAIServer:
    return FakeAlixAIServer()


async def test_probes_and_sessions(fake: FakeAlixAIServer) -> None:
    async with fake.async_client("ses-1") as client:
        assert (await client.get_liveness()).status == "ok"
        assert (await client.get_readiness()).ready is True
        assert (await client.get_health()).application.status == "active"
        assert (await client.create_session()).session_id == "ses-1"
        assert (await client.get_session("ses-1")).turn_count == 0
        assert await client.get_pending_state("ses-1") is None
        assert [c.id for c in (await client.list_capabilities()).capabilities] == ["demo.lookup", "demo.book", "demo.close_account"]
        assert (await client.end_session("ses-1")).status == "ended"
        assert (await failure(lambda: client.get_session("ses-1"))).status == 410


async def test_the_token_provider_may_be_a_coroutine_function_and_is_called_once_per_request(fake: FakeAlixAIServer) -> None:
    asked: list[ContextTokenRequest] = []

    async def provider(request: ContextTokenRequest) -> str:
        await asyncio.sleep(0)
        asked.append(request)
        return fake.issue_token("ses-1")

    async with fake.async_client(get_context_token=provider) as client:
        await client.get_session("ses-1")
        await client.get_session("ses-1")
        await client.get_liveness()
    assert [r.operation for r in asked] == ["getSession", "getSession"]


async def test_nothing_malformed_costs_a_token(fake: FakeAlixAIServer) -> None:
    asked: list[Any] = []
    async with fake.async_client(get_context_token=lambda request: asked.append(request) or "tok") as client:
        assert (await failure(lambda: client.get_session("../x"))).code == "validation_failed"
        assert (await failure(lambda: client.send_turn("ses-1", {"text": ""}))).code == "validation_failed"
    assert asked == [] and fake.requests == []


async def test_a_turn_streams_with_async_for_and_pairs_the_confirmation_secret(fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: prepare_action(**BOOK))
    async with fake.async_client("ses-1") as client:
        events = []
        async with await client.send_turn("ses-1", {"text": "book"}) as stream:
            async for event in stream:
                events.append(event)
            grant = stream.confirmation
        assert [e.seq for e in events] == list(range(len(events))) and events[-1].type == "done"
        assert grant is not None and grant.preparation_id == events[-1].preparation_id
        outcome = await client.confirm_action(grant.preparation_id, grant.token)
        assert outcome.receipt is not None and outcome.receipt.status == "committed"
        assert (await failure(lambda: client.confirm_action(grant.preparation_id, grant.token))).code == "replayed"
        assert (await client.get_action_receipt(grant.preparation_id)).status == "committed"


async def test_chunked_streams_decode_and_the_json_form_matches(fake: FakeAlixAIServer) -> None:
    chunked = FakeAlixAIServer(stream_chunk_bytes=5)
    chunked.on_turn(lambda turn: reply("caf\u00e9 \u2603"))
    async with chunked.async_client("ses-1") as client:
        streamed = await (await client.send_turn("ses-1", {"text": "x"})).result()
        as_json = await client.send_turn_json("ses-1", {"text": "x"})
    assert turn_text(streamed.events) == turn_text(as_json.events) == "caf\u00e9 \u2603"


async def test_a_truncated_stream_is_an_unknown_outcome_and_closing_early_releases_the_connection(fake: FakeAlixAIServer) -> None:
    fake.on_turn(lambda turn: reply("hello"))
    async with fake.async_client("ses-1") as client:
        fake.fail_next(operation="sendTurn", truncate_after_events=2)
        stream = await client.send_turn("ses-1", {"text": "x"})
        with pytest.raises(AlixAIError) as raised:
            async for _ in stream:
                pass
        assert raised.value.code == "uncertain_outcome"
        early = await client.send_turn("ses-1", {"text": "x"})
        await early.__anext__()
        await early.aclose()
        assert early._response.is_closed  # noqa: SLF001
        assert [e async for e in early] == []


async def test_failures_of_the_transport_follow_the_same_rules(fake: FakeAlixAIServer) -> None:
    async with fake.async_client("ses-1") as client:
        fake.fail_next(network_error=httpx.ReadError("reset"))
        assert (await failure(client.list_capabilities)).code == "unavailable"
        fake.fail_next(network_error=httpx.ReadTimeout("slow"))
        assert (await failure(client.create_session)).code == "uncertain_outcome"
        fake.fail_next(network_error=httpx.ConnectError("refused"))
        assert (await failure(client.create_session)).code == "unavailable"


class _ErrorBodyCutOff(httpx.AsyncByteStream):
    """The body of an error answer whose connection dies while it is read."""

    def __init__(self) -> None:
        self.closed = False

    async def __aiter__(self) -> Any:
        raise httpx.ReadError("connection reset")
        yield b""  # pragma: no cover

    async def aclose(self) -> None:
        self.closed = True


async def test_a_stream_whose_error_body_cannot_be_read_still_releases_its_connection() -> None:
    body = _ErrorBodyCutOff()
    transport = httpx.MockTransport(lambda request: httpx.Response(503, stream=body))
    async with AlixAIAsyncClient("https://alixai.test", lambda request: "tok", http_client=httpx.AsyncClient(transport=transport)) as client:
        assert (await failure(lambda: client.send_turn("ses-1", {"text": "hi"}))).code == "uncertain_outcome"
    assert body.closed, "the response of a failed stream request was left open (its pooled connection leaks)"
