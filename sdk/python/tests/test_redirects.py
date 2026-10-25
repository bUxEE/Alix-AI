"""A redirect is never followed, whatever client the host supplies.

A host's own ``httpx.Client`` may be built with ``follow_redirects=True``. Followed, a redirect would carry the bearer token, the
confirmation secret and the body to another origin, so the clients and the AG-UI proxy refuse it: the second origin is never
contacted and the answer is an error, as the TS and Go SDKs do.
"""

from __future__ import annotations

import json
from typing import Any, List

import httpx
import pytest

from alixai import AlixAIAsyncClient, AlixAIClient, AlixAIError
from alixai.agui import HEADER_CONFIRMATION_TOKEN, AguiProxy, MemorySecretStore, SecretKey

SECRET = "cfm_" + "T" * 43  # TEST-ONLY confirmation secret, not a real one
ELSEWHERE = "https://elsewhere.test"


class Recorder:
    """The AlixAI origin answers every request with a redirect to another origin; that origin records what reached it."""

    def __init__(self, status: int = 307) -> None:
        self.status = status
        self.home: List[httpx.Request] = []
        self.elsewhere: List[httpx.Request] = []

    def __call__(self, request: httpx.Request) -> httpx.Response:
        if request.url.host == "elsewhere.test":
            self.elsewhere.append(request)
            return httpx.Response(200, json={})
        self.home.append(request)
        return httpx.Response(self.status, headers={"location": f"{ELSEWHERE}{request.url.path}"})


@pytest.mark.parametrize("status", [301, 302, 303, 307, 308])
def test_the_sync_client_never_follows_a_redirect_even_when_the_hosts_client_would(status: int) -> None:
    rec = Recorder(status)
    host_client = httpx.Client(transport=httpx.MockTransport(rec), follow_redirects=True)
    with AlixAIClient("https://alixai.test", lambda request: "tok", http_client=host_client) as client:
        with pytest.raises(AlixAIError) as raised:
            client.confirm_action("prep_x", SECRET)
        with pytest.raises(AlixAIError):
            client.get_liveness()
    assert raised.value.details.get("reason") == "redirect" and raised.value.status == status
    assert rec.elsewhere == [], "the redirect was followed to another origin"
    assert len(rec.home) == 2
    host_client.close()


async def test_the_async_client_never_follows_a_redirect_even_when_the_hosts_client_would() -> None:
    rec = Recorder()
    host_client = httpx.AsyncClient(transport=httpx.MockTransport(rec), follow_redirects=True)
    async with AlixAIAsyncClient("https://alixai.test", lambda request: "tok", http_client=host_client) as client:
        with pytest.raises(AlixAIError) as raised:
            await client.confirm_action("prep_x", SECRET)
        with pytest.raises(AlixAIError) as streamed:
            await client.send_turn("ses-1", {"text": "hi"})
    assert raised.value.details.get("reason") == "redirect"
    assert streamed.value.details.get("reason") == "redirect"
    assert rec.elsewhere == [], "the redirect was followed to another origin"
    await host_client.aclose()


async def test_the_agui_proxy_never_follows_a_redirect_with_the_confirmation_secret() -> None:
    thread, preparation = "ses_test_01", "prep_test_01"
    rec = Recorder()

    async def gateway(request: httpx.Request) -> httpx.Response:
        await request.aread()
        return rec(request)

    store = MemorySecretStore(clock=lambda: 1_000.0)
    await store.put(SecretKey(thread, preparation), SECRET, 1_060.0)
    host_client = httpx.AsyncClient(transport=httpx.MockTransport(gateway), follow_redirects=True)
    proxy = AguiProxy("http://127.0.0.1:8080", lambda request: "ctx_tok", secrets=store, clock=lambda: 1_000.0, http_client=host_client)
    body: Any = {
        "threadId": thread, "runId": "run_approve", "messages": [], "state": {}, "tools": [], "context": [], "forwardedProps": {},
        "resume": [{"interruptId": preparation, "status": "resolved", "payload": {"approved": True}}],
    }
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=proxy), base_url="http://host") as browser:
        answer = await browser.post("/", content=json.dumps(body).encode(), headers={"content-type": "application/json"})
    assert answer.status_code == 502 and answer.json()["code"] == "unavailable"
    assert "location" not in answer.headers
    assert rec.elsewhere == [], "the redirect was followed to another origin"
    assert rec.home and rec.home[-1].headers.get(HEADER_CONFIRMATION_TOKEN) == SECRET
    await host_client.aclose()
