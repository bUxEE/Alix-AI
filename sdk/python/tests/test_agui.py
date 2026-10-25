"""The AG-UI proxy of the Python SDK, replayed over the golden streams of the gateway (tests/agui/testdata).

The browser side is the ASGI application itself (httpx.ASGITransport) and the gateway side is httpx.MockTransport: no port, no network.
"""

import ast
import json
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional

import httpx
import pytest

from alixai import AlixAIError
from alixai.agui import (
    HEADER_CONFIRMATION_TOKEN,
    MAX_RUN_REQUEST_BYTES,
    AguiCaller,
    AguiProxy,
    EventType,
    MemorySecretStore,
    NotAnEvent,
    SecretKey,
    encode_frame,
    is_terminal,
    iter_frames,
    parse_event,
    run_error_event,
)

REPO_ROOT = Path(__file__).resolve().parents[3]
GOLDEN_DIR = REPO_ROOT / "tests" / "agui" / "testdata"

#: Has the shape of a real confirmation secret (cfm_ and 43 characters of base64url).
TEST_SECRET = "cfm_" + "Zq9-" * 10 + "Zq9"
FORGED = "cfm_" + "FORGED-" * 6 + "FO"
NOW = datetime(2026, 10, 2, 12, 0, 0, tzinfo=timezone.utc).timestamp()  # the clock of the golden streams
THREAD = "ses_test_01"
PREPARATION = "prep_01J00000000000000000000001"


# ---- the golden streams ----


@dataclass
class Golden:
    name: str
    method: str
    path: str
    request_headers: Dict[str, str]
    request_body: Optional[bytes]
    status: int
    response_headers: Dict[str, str]
    sse: bytes


def load_goldens() -> List[Golden]:
    out = []
    for path in sorted(GOLDEN_DIR.glob("*.json")):
        doc = json.loads(path.read_text(encoding="utf-8"))
        body = doc["request"].get("body")
        out.append(
            Golden(
                name=doc["name"],
                method=doc["request"]["method"],
                path=doc["request"]["path"],
                request_headers=doc["request"].get("headers", {}),
                request_body=json.dumps(body, separators=(",", ":")).encode() if body is not None else None,
                status=doc["response"]["status"],
                response_headers=doc["response"].get("headers", {}),
                sse=(GOLDEN_DIR / doc["response"]["sse_file"]).read_bytes(),
            )
        )
    assert len(out) >= 10, f"the golden streams are not where they should be: {GOLDEN_DIR}"
    return out


GOLDENS = load_goldens()


def golden(name: str) -> Golden:
    return next(g for g in GOLDENS if g.name == name)


# ---- a recording fake gateway and a proxy under test ----


class ChunkedStream(httpx.AsyncByteStream):
    """The body in small chunks that split frames and multi-byte characters anywhere."""

    def __init__(self, data: bytes, size: int = 13) -> None:
        self.data, self.size = data, size

    async def __aiter__(self):
        for start in range(0, len(self.data), self.size):
            yield self.data[start : start + self.size]


def sse_response(data: bytes, secret: Optional[str] = None) -> httpx.Response:
    headers = {"content-type": "text/event-stream"}
    if secret:
        headers[HEADER_CONFIRMATION_TOKEN] = secret
    return httpx.Response(200, headers=headers, stream=ChunkedStream(data))


def error_response(status: int, code: str, message: str) -> httpx.Response:
    return httpx.Response(status, json={"code": code, "message": message, "retryable": False})


class Gateway:
    def __init__(self, handle: Callable[["Gateway", httpx.Request], httpx.Response]) -> None:
        self._handle = handle
        self.requests: List[httpx.Request] = []
        self.transport = httpx.MockTransport(self._call)

    async def _call(self, request: httpx.Request) -> httpx.Response:
        await request.aread()
        self.requests.append(request)
        return self._handle(self, request)

    @property
    def last(self) -> httpx.Request:
        assert self.requests, "the gateway received no request"
        return self.requests[-1]


@dataclass
class Tokens:
    requests: List[Any] = field(default_factory=list)
    minted: List[str] = field(default_factory=list)
    error: Optional[BaseException] = None

    async def __call__(self, request: Any) -> str:
        self.requests.append(request)
        if self.error is not None:
            raise self.error
        token = f"ctx_tok_{len(self.requests)}"
        self.minted.append(token)
        return token


class Rig:
    def __init__(self, gateway: Gateway, **options: Any) -> None:
        self.gateway = gateway
        self.tokens = Tokens()
        self.store = MemorySecretStore(clock=lambda: NOW)
        self.proxy = AguiProxy(
            "http://127.0.0.1:8080",
            self.tokens,
            secrets=self.store,
            clock=lambda: NOW,
            http_client=httpx.AsyncClient(transport=gateway.transport, follow_redirects=False),
            **options,
        )
        self.browser = httpx.AsyncClient(transport=httpx.ASGITransport(app=self.proxy), base_url="http://host")

    async def aclose(self) -> None:
        await self.browser.aclose()
        await self.gateway.transport.aclose()

    async def run(self, body: bytes, headers: Optional[Dict[str, str]] = None) -> httpx.Response:
        return await self.browser.post("/", content=body, headers={"content-type": "application/json", **(headers or {})})

    async def held(self, thread: str = THREAD, preparation: str = PREPARATION) -> Optional[str]:
        return await self.store.get(SecretKey(thread, preparation))

    async def seed(self, thread: str = THREAD, preparation: str = PREPARATION) -> None:
        await self.store.put(SecretKey(thread, preparation), TEST_SECRET, NOW + 60)


@pytest.fixture
async def make_rig():
    rigs: List[Rig] = []

    def build(handle: Callable[[Gateway, httpx.Request], httpx.Response], **options: Any) -> Rig:
        rig = Rig(Gateway(handle), **options)
        rigs.append(rig)
        return rig

    yield build
    for rig in rigs:
        await rig.aclose()


def run_body(thread: str, run_id: str, extra: Optional[Dict[str, Any]] = None) -> bytes:
    doc: Dict[str, Any] = {"threadId": thread, "runId": run_id, "messages": [], "state": {}, "tools": [], "context": [], "forwardedProps": {}}
    doc.update(extra or {})
    return json.dumps(doc, separators=(",", ":")).encode()


def resume_body(thread: str, run_id: str, preparation: str, status: str, payload: Any) -> bytes:
    return run_body(thread, run_id, {"resume": [{"interruptId": preparation, "status": status, "payload": payload}]})


def assert_no_secret(response: httpx.Response, what: str) -> None:
    assert HEADER_CONFIRMATION_TOKEN not in response.headers, f"{what}: the browser received the secret header"
    for name, value in response.headers.items():
        assert TEST_SECRET not in value and "confirmation" not in name.lower(), f"{what}: header {name} carries or names the secret"
    assert TEST_SECRET.encode() not in response.content, f"{what}: the body carries the secret"


# ---- event types, encoder and decoder ----


def test_event_types_are_the_pinned_release() -> None:
    bundle = json.loads((REPO_ROOT / "schemas" / "agui" / "ag-ui-core.json").read_text(encoding="utf-8"))
    assert sorted(member.value for member in EventType) == sorted(bundle["$defs"]["EventType"]["enum"])
    assert is_terminal("RUN_FINISHED") and is_terminal("RUN_ERROR") and not is_terminal("RUN_STARTED")


def test_parse_event_reads_known_chunk_and_newer_types_and_refuses_the_rest() -> None:
    finished = parse_event(next(m.data for m in iter_frames([golden("prepare_interrupt").sse]) if '"RUN_FINISHED"' in m.data))
    assert finished.known and finished.outcome is not None and finished.outcome.type == "interrupt"
    assert [(i.id, i.reason) for i in finished.outcome.interrupts] == [(PREPARATION, "confirmation_required")]
    assert parse_event('{"type":"REASONING_START"}').known
    newer = parse_event(b'{"type":"SOMETHING_NEWER","x":1}')
    assert not newer.known and newer.data["x"] == 1
    for bad in ("", "[]", "{}", '{"type":""}', '{"type":5}', "nope"):
        with pytest.raises(NotAnEvent):
            parse_event(bad)


@pytest.mark.parametrize("g", GOLDENS, ids=lambda g: g.name)
def test_decoder_and_encoder_round_trip_the_golden_framing(g: Golden) -> None:
    out = b""
    count = 0
    chunks = [g.sse[i : i + 7] for i in range(0, len(g.sse), 7)]  # splits frames and multi-byte characters anywhere
    for message in iter_frames(chunks):
        out += encode_frame(message.data, id=message.id)
        count += 1
    assert count > 0 and out == g.sse


def test_an_id_belongs_to_the_frame_that_carried_it() -> None:
    # The HTML standard keeps the last event id across frames; the AG-UI cursor is on the last event of a TurnEvent only.
    frames = list(iter_frames([b"id: 3\ndata: a\n\ndata: b\n\n: comment\n\nid: 4\r\ndata: c\r\n\r\ndata: d1\ndata: d2\n\n"]))
    assert [(m.id, m.data) for m in frames] == [("3", "a"), (None, "b"), ("4", "c"), (None, "d1\nd2")]
    cut = [m.data for m in iter_frames([b"data: whole\n\ndata: half"])]
    assert cut == ["whole"], "a frame cut off before its blank line was delivered"


def test_the_encoder_refuses_what_it_cannot_frame() -> None:
    for kwargs in ({"data": "a\rb"}, {"data": "x", "id": "1\n2"}, {"data": "x", "event": "e\nf"}):
        with pytest.raises(ValueError):
            encode_frame(**kwargs)
    assert encode_frame(run_error_event("boom", "x"), id="7") == b'id: 7\ndata: {"type":"RUN_ERROR","message":"boom","code":"x"}\n\n'


# ---- TestProxyStreamsGoldenStreamsUnchanged ----


@pytest.mark.parametrize("g", GOLDENS, ids=lambda g: g.name)
async def test_proxy_streams_golden_streams_unchanged(g: Golden, make_rig) -> None:
    is_run = g.method == "POST"
    wants_secret = bool(g.request_headers.get(HEADER_CONFIRMATION_TOKEN))

    def handle(gateway: Gateway, request: httpx.Request) -> httpx.Response:
        assert (request.method, request.url.path) == (g.method, g.path)
        if is_run:
            assert request.content == g.request_body, "the body was not forwarded unchanged"
        assert request.headers["authorization"].startswith("Bearer ctx_tok_")
        assert request.headers.get(HEADER_CONFIRMATION_TOKEN) == (TEST_SECRET if wants_secret else None)
        return sse_response(g.sse, TEST_SECRET if g.response_headers.get(HEADER_CONFIRMATION_TOKEN) else None)

    rig = make_rig(handle)
    if is_run:
        thread = json.loads(g.request_body)["threadId"]
        if wants_secret:  # the run that prepared the action stored it
            await rig.seed(thread, json.loads(g.request_body)["resume"][0]["interruptId"])
        response = await rig.run(g.request_body)
        operation = "runAgui"
    else:
        thread = g.path.split("/")[4]
        response = await rig.browser.get(g.path.removeprefix("/v1/agui"))
        operation = "streamAguiRunEvents"
    assert response.status_code == g.status, response.text
    assert response.headers["content-type"] == "text/event-stream"
    assert response.headers["cache-control"] == "no-store"
    assert_no_secret(response, g.name)
    assert response.content == g.sse, f"the proxied stream differs from the golden {g.name}.sse"
    assert [(r.operation, r.session_id) for r in rig.tokens.requests] == [(operation, thread)]
    if g.response_headers.get(HEADER_CONFIRMATION_TOKEN):  # an interrupt run: the secret is now held server-side
        assert len(rig.store) == 1


# ---- TestProxyKeepsTheConfirmationSecretServerSide ----


class SecretGateway:
    """The part of the gateway the secret rules depend on (docs/integration/agui.md sections 6 and 9)."""

    def __init__(self) -> None:
        self.prepare, self.approve, self.reject = golden("prepare_interrupt"), golden("resume_approve"), golden("resume_reject")
        self.runs: Dict[str, bytes] = {}
        self.confirmed = False

    def __call__(self, gateway: Gateway, request: httpx.Request) -> httpx.Response:
        if request.method == "GET":  # the events route: the recorded run, never the secret
            return sse_response(self.approve.sse)
        body = json.loads(request.content)
        run_id = body["runId"]
        if run_id in self.runs:  # a retry: the recorded run, never the secret again
            return sse_response(self.runs[run_id])
        resume = body.get("resume") or []
        if not resume:
            self.runs[run_id] = self.prepare.sse
            return sse_response(self.prepare.sse, TEST_SECRET)
        entry = resume[0]
        if entry["status"] == "resolved" and entry["payload"] == {"approved": True}:
            got = request.headers.get(HEADER_CONFIRMATION_TOKEN)
            if got is None:
                return error_response(428, "confirmation_required", "the approval needs the confirmation secret")
            if got != TEST_SECRET:
                return error_response(403, "scope_violation", "the thread or the interrupt does not resolve for this caller")
            if self.confirmed:
                return error_response(409, "conflict", "the preparation is no longer awaiting a decision")
            self.confirmed = True
            self.runs[run_id] = self.approve.sse
            return sse_response(self.approve.sse)
        self.runs[run_id] = self.reject.sse
        return sse_response(self.reject.sse)


async def test_proxy_keeps_the_confirmation_secret_server_side(make_rig, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG, logger="alixai.agui")
    sg = SecretGateway()
    rig = make_rig(sg)
    key = SecretKey(THREAD, PREPARATION)
    seen: List[httpx.Response] = []

    def check(what: str, response: httpx.Response) -> httpx.Response:
        assert_no_secret(response, what)
        seen.append(response)
        return response

    # 1. The run that prepares the action: the browser's own secret header, cookie and Authorization are not forwarded, the gateway's
    # secret header is not returned, and the secret is kept.
    prepare_body = sg.prepare.request_body
    response = check(
        "prepare",
        await rig.run(prepare_body, {HEADER_CONFIRMATION_TOKEN: FORGED, "cookie": "session=browser-cookie", "authorization": "Bearer browser-credential"}),
    )
    assert response.status_code == 200 and response.content == sg.prepare.sse
    upstream = rig.gateway.last
    assert HEADER_CONFIRMATION_TOKEN not in upstream.headers, "a secret header of the browser reached the gateway"
    assert "cookie" not in upstream.headers and "browser-credential" not in upstream.headers["authorization"]
    assert await rig.store.get(key) == TEST_SECRET, "the secret of the prepared action was not kept under (thread, preparation)"

    # 2. The same run id again: the gateway replays it without the header. The stored secret is neither overwritten nor deleted.
    check("prepare replay", await rig.run(prepare_body))
    assert await rig.store.get(key) == TEST_SECRET

    # 3. Another thread cannot use it, and a forged header does not stand in for it: the approval goes without a header.
    other = check("other thread", await rig.run(resume_body("ses_other", "run_other", PREPARATION, "resolved", {"approved": True}), {HEADER_CONFIRMATION_TOKEN: FORGED}))
    assert other.status_code == 428, "the gateway's 428 is relayed"
    assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers
    assert rig.tokens.requests[-1].session_id == "ses_other"

    # 4. The approval on the right thread attaches the stored secret (not the forged one), and the secret is spent.
    approve_body = resume_body(THREAD, "run_approve", PREPARATION, "resolved", {"approved": True})
    response = check("approve", await rig.run(approve_body, {HEADER_CONFIRMATION_TOKEN: FORGED}))
    assert response.status_code == 200 and response.content == sg.approve.sse
    assert rig.gateway.last.headers[HEADER_CONFIRMATION_TOKEN] == TEST_SECRET
    assert await rig.store.get(key) is None, "the secret was not deleted after it was used"

    # 5. After use: a second approval under a new run id reaches the gateway without a header and is refused there; the proxy relays the
    # status and the document and never wraps it in a stream.
    again = check("second approval", await rig.run(resume_body(THREAD, "run_approve_2", PREPARATION, "resolved", {"approved": True})))
    assert again.status_code == 428 and again.headers["content-type"] == "application/json" and again.json()["code"] == "confirmation_required"
    assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers, "a spent secret was sent again"
    # ... and one that does carry the secret (a host's own store handing it out again) is the gateway's 409, relayed as such.
    await rig.seed()
    conflict = check("conflict", await rig.run(resume_body(THREAD, "run_approve_3", PREPARATION, "resolved", {"approved": True})))
    assert conflict.status_code == 409 and conflict.json()["code"] == "conflict"
    # The same run id of the approval replays the recorded run and never sends the secret again.
    replay = check("approve replay", await rig.run(approve_body))
    assert replay.status_code == 200 and replay.content == sg.approve.sse

    # 6. A reject ends with the cancelled outcome and carries no secret.
    await rig.seed()
    reject = check("reject", await rig.run(resume_body(THREAD, "run_reject", PREPARATION, "resolved", {"approved": False})))
    assert reject.status_code == 200 and b'"outcome":{"type":"cancelled"}' in reject.content
    assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers

    # 7. A gateway that put the secret into an event is not relayed: the run ends with RUN_ERROR and the secret stays here.
    leaky = make_rig(
        lambda gateway, request: sse_response(
            b'data: {"type":"RUN_STARTED","threadId":"ses_test_01","runId":"run_leak"}\n\n'
            b'data: {"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"your code is ' + TEST_SECRET.encode() + b'"}\n\n',
            TEST_SECRET,
        )
    )
    leak = check("leaky gateway", await leaky.run(run_body(THREAD, "run_leak")))
    assert b'"type":"RUN_ERROR"' in leak.content and b"your code" not in leak.content

    # Nothing the browser saw, and nothing in the logs, holds the secret, a context token or an event's text.
    logs = caplog.text
    assert logs, "the proxy logged nothing at debug level: the assertions below prove nothing"
    for banned in [TEST_SECRET, FORGED, "Done. The item", "widget", "browser-credential", *rig.tokens.minted]:
        assert banned not in logs
    for index, item in enumerate(seen):
        assert_no_secret(item, f"answer {index}")


@pytest.mark.parametrize(("status", "spent"), [(400, False), (401, False), (403, False), (415, False), (429, False), (503, False), (409, True), (410, True)])
async def test_proxy_keeps_the_secret_of_an_approval_that_was_not_decided(make_rig, status: int, spent: bool) -> None:
    # A refusal that decided nothing (a bad request, an expired context token, a foreign caller, a server error) leaves the secret for
    # the user's retry; only an answer that used it up (2xx, 409 already decided, 410 expired) spends it.
    rig = make_rig(lambda gateway, request: error_response(status, "refused", "refused"))
    await rig.seed()
    response = await rig.run(resume_body(THREAD, f"run_{status}", PREPARATION, "resolved", {"approved": True}))
    assert response.status_code == status
    assert rig.gateway.last.headers.get(HEADER_CONFIRMATION_TOKEN) == TEST_SECRET
    assert (await rig.held() is None) == spent


# ---- TestProxyForwardsResumeAndCancel ----


async def test_proxy_accepts_every_run_id_the_run_request_accepts(make_rig) -> None:
    # The OpenAPI's run_id is 1 to 128 characters, not the thread id's pattern: a run with a ':' in its id can be read again and
    # cancelled, and the id is escaped into the gateway's path, never pasted.
    def handle(gateway: Gateway, request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(204)
        return httpx.Response(200, json={"status": "finished"})

    rig = make_rig(handle)
    for run, escaped in (("run:2026-10-10:1", "run%3A2026-10-10%3A1"), ("a/b c?d#e%f", "a%2Fb%20c%3Fd%23e%25f"), ("r" * 128, "r" * 128)):
        events = await rig.proxy.events(THREAD, run)
        cancel = await rig.proxy.cancel(THREAD, run)
        assert (events.status, cancel.status) == (204, 200), run
        base = f"/v1/agui/threads/{THREAD}/runs/{escaped}"
        assert [request.url.raw_path.decode() for request in rig.gateway.requests[-2:]] == [f"{base}/events", f"{base}/cancel"], run

    sent, minted = len(rig.gateway.requests), len(rig.tokens.requests)
    for run in ("", ".", "..", "r" * 129, "a\nb", "a\x00b"):
        events = await rig.proxy.events(THREAD, run)
        cancel = await rig.proxy.cancel(THREAD, run)
        assert (events.status, cancel.status) == (400, 400), repr(run)
    assert (len(rig.gateway.requests), len(rig.tokens.requests)) == (sent, minted), "a refused run id cost a token or reached the gateway"


async def test_proxy_forwards_resume_and_cancel(make_rig) -> None:
    approve, reject = golden("resume_approve"), golden("resume_reject")

    def handle(gateway: Gateway, request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method == "POST" and path == "/v1/agui":
            declined = b'"approved":false' in request.content or b'"cancelled"' in request.content
            return sse_response((reject if declined else approve).sse)
        if request.method == "POST" and path.endswith("/cancel"):
            if "run_unknown" in path:
                return error_response(403, "scope_violation", "the run does not resolve for this caller")
            return httpx.Response(200, json={"thread_id": THREAD, "run_id": "run_live", "status": "cancelling"})
        if request.method == "GET" and path.endswith("/events"):
            if "run_done" in path:
                return httpx.Response(204)
            if "run_old" in path:
                return error_response(410, "expired", "the cursor is behind the retained window")
            return sse_response(approve.sse)
        raise AssertionError(f"unexpected upstream request {request.method} {path}")

    rig = make_rig(handle)

    # An approval carries the secret, and the body is forwarded exactly as it came (members this proxy does not read, spacing).
    await rig.seed()
    odd = (
        b'{ "threadId": "ses_test_01",  "runId": "run_a", "messages": [], "newerProtocolMember": {"x": [1, 2]},\n'
        b'  "resume": [ {"interruptId": "prep_01J00000000000000000000001", "status": "resolved", "payload": {"approved": true}} ] }'
    )
    response = await rig.run(odd)
    assert response.status_code == 200 and response.content == approve.sse
    assert rig.gateway.last.headers[HEADER_CONFIRMATION_TOKEN] == TEST_SECRET and rig.gateway.last.content == odd
    assert await rig.held() is None, "an approval that was answered left its secret in the store"

    # The answer in the form of a tool message for the confirmation call is the same approval.
    await rig.seed()
    tool_form = run_body(
        THREAD, "run_tool", {"messages": [{"id": "m9", "role": "tool", "toolCallId": f"alixai_confirm_{PREPARATION}", "content": '{"approved":true}'}]}
    )
    assert (await rig.run(tool_form)).status_code == 200
    assert rig.gateway.last.headers.get(HEADER_CONFIRMATION_TOKEN) == TEST_SECRET

    # A reject, and a dismissed interrupt (cancelled), carry no secret and discard the stored one.
    for name, body in {
        "reject": resume_body(THREAD, "run_rej", PREPARATION, "resolved", {"approved": False}),
        "cancelled": resume_body(THREAD, "run_can", PREPARATION, "cancelled", None),
    }.items():
        await rig.seed()
        response = await rig.run(body)
        assert response.status_code == 200 and b'"outcome":{"type":"cancelled"}' in response.content, name
        assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers and rig.gateway.last.content == body, name
        assert await rig.held() is None, f"{name}: the secret of a declined preparation was not discarded"

    # Anything that is not exactly {"approved": true} is never an approval: no secret leaves, and nothing is discarded.
    bad_payloads = {
        "true": True, "string": "yes", "stringly": {"approved": "true"}, "number": {"approved": 1}, "extra member": {"approved": True, "token": "x"},
        "null": None, "empty": {},
    }
    for name, payload in bad_payloads.items():
        await rig.seed()
        await rig.run(resume_body(THREAD, "run_bad_" + name.replace(" ", "_"), PREPARATION, "resolved", payload))
        assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers, f"payload {name}: a secret was attached to something that is not an approval"
        assert await rig.held() == TEST_SECRET, f"payload {name}: the secret was discarded by an answer that decided nothing"
    two = run_body(
        THREAD,
        "run_two",
        {"resume": [{"interruptId": PREPARATION, "status": "resolved", "payload": {"approved": True}}, {"interruptId": "prep_other", "status": "resolved", "payload": {"approved": True}}]},
    )
    await rig.run(two)
    assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers, "a secret was attached to a resume with two entries"

    # Cancel: a fresh token minted for the thread, the JSON answer relayed with its status, an unknown run is the gateway's 403.
    before = len(rig.tokens.requests)
    cancelled = await rig.browser.post(f"/threads/{THREAD}/runs/run_live/cancel", headers={HEADER_CONFIRMATION_TOKEN: TEST_SECRET})
    assert cancelled.status_code == 200 and cancelled.headers["content-type"] == "application/json" and cancelled.json()["status"] == "cancelling"
    upstream = rig.gateway.last
    assert (upstream.method, upstream.url.path, upstream.content) == ("POST", f"/v1/agui/threads/{THREAD}/runs/run_live/cancel", b"")
    assert HEADER_CONFIRMATION_TOKEN not in upstream.headers
    assert (rig.tokens.requests[before].operation, rig.tokens.requests[before].session_id, len(rig.tokens.requests)) == ("cancelAguiRun", THREAD, before + 1)
    unknown = await rig.browser.post(f"/threads/{THREAD}/runs/run_unknown/cancel")
    assert unknown.status_code == 403 and unknown.json()["code"] == "scope_violation"

    # Reconnect: the cursor of Last-Event-ID or ?cursor= is forwarded after a check, 204 and 410 pass through, each call has its own token.
    before = len(rig.tokens.requests)
    events = await rig.browser.get(f"/threads/{THREAD}/runs/run_live/events?cursor=4", headers={"Last-Event-ID": "5"})
    assert events.status_code == 200 and events.content == approve.sse
    upstream = rig.gateway.last
    assert (upstream.url.path, upstream.url.query, upstream.headers["last-event-id"]) == (f"/v1/agui/threads/{THREAD}/runs/run_live/events", b"cursor=4", "5")
    assert (rig.tokens.requests[before].operation, rig.tokens.requests[before].session_id) == ("streamAguiRunEvents", THREAD)
    done = await rig.browser.get(f"/threads/{THREAD}/runs/run_done/events", headers={"Last-Event-ID": "9"})
    assert done.status_code == 204 and done.content == b""
    old = await rig.browser.get(f"/threads/{THREAD}/runs/run_old/events")
    assert old.status_code == 410 and old.json()["code"] == "expired"
    requests, tokens = len(rig.gateway.requests), len(rig.tokens.requests)
    for bad in ("?cursor=-1", "?cursor=12345678901", "?cursor=1%0d%0aX:y"):
        assert (await rig.browser.get(f"/threads/{THREAD}/runs/run_live/events{bad}")).status_code == 400, bad
    assert (await rig.browser.get(f"/threads/{THREAD}/runs/run_live/events", headers={"Last-Event-ID": "abc"})).status_code == 400
    assert (len(rig.gateway.requests), len(rig.tokens.requests)) == (requests, tokens), "a malformed cursor cost a token or reached the gateway"

    # Capabilities needs a token too; the callback is told the operation and no thread.
    caps = make_rig(lambda gateway, request: httpx.Response(200, json={"protocol": "1.0"}))
    answer = await caps.browser.get("/capabilities")
    assert answer.status_code == 200 and answer.json() == {"protocol": "1.0"}
    assert (caps.tokens.requests[0].operation, caps.tokens.requests[0].session_id) == ("getAguiCapabilities", None)


# ---- refusals before anything is sent, and a misbehaving gateway ----


async def test_proxy_refuses_before_spending_a_token(make_rig) -> None:
    rig = make_rig(lambda gateway, request: sse_response(b""))
    huge = run_body(THREAD, "run_big", {"pad": "x" * MAX_RUN_REQUEST_BYTES})
    cases = {
        "not json content type": (run_body(THREAD, "r1"), "text/plain", 415),
        "too large": (huge, "application/json", 413),
        "not json": (b"nope", "application/json", 400),
        "no thread": (b'{"runId":"r1"}', "application/json", 400),
        "path thread": (run_body("../ses", "r1"), "application/json", 400),
        "dot thread": (run_body("..", "r1"), "application/json", 400),
        "no run": (b'{"threadId":"ses_test_01"}', "application/json", 400),
        "resume is not a list": (run_body(THREAD, "r1", {"resume": "yes"}), "application/json", 400),
    }
    for name, (body, content_type, status) in cases.items():
        response = await rig.browser.post("/", content=body, headers={"content-type": content_type})
        assert response.status_code == status and response.json()["code"] == "validation_failed", name
    assert not rig.tokens.requests and not rig.gateway.requests, "a refused request cost a token or reached the gateway"
    assert (await rig.browser.get("/nowhere")).status_code == 404
    method_refused = await rig.browser.get("/")
    assert method_refused.status_code == 405 and method_refused.headers["allow"] == "POST"

    # The host's callback decides who may use a thread: its refusal is answered, and nothing is sent.
    rig.tokens.error = AlixAIError("permission_denied", "that is not your thread")
    refused = await rig.run(run_body(THREAD, "r2"))
    assert refused.status_code == 403 and "your thread" not in refused.text, "the callback's text must not be echoed"
    rig.tokens.error = RuntimeError("database password is hunter2")
    failed = await rig.run(run_body(THREAD, "r3"))
    assert failed.status_code == 401 and "hunter2" not in failed.text
    assert not rig.gateway.requests, "a request without a token reached the gateway"


async def test_a_truncated_stream_ends_with_run_error_and_nothing_follows_the_end(make_rig) -> None:
    cut = make_rig(
        lambda gateway, request: sse_response(b'data: {"type":"RUN_STARTED","threadId":"ses_test_01","runId":"run_cut"}\n\ndata: {"type":"STEP_STA')
    )
    answer = await cut.run(run_body(THREAD, "run_cut"))
    assert answer.content == (
        b'data: {"type":"RUN_STARTED","threadId":"ses_test_01","runId":"run_cut"}\n\n'
        b'data: {"type":"RUN_ERROR","message":"the event stream ended before the run did","code":"upstream_ended"}\n\n'
    )
    after = make_rig(lambda gateway, request: sse_response(b'data: {"type":"RUN_ERROR","message":"x"}\n\ndata: {"type":"RUN_STARTED","threadId":"t","runId":"r"}\n\n'))
    assert (await after.run(run_body(THREAD, "run_x"))).content == b'data: {"type":"RUN_ERROR","message":"x"}\n\n'
    garbage = make_rig(lambda gateway, request: sse_response(b"data: not json\n\n"))
    assert b'"code":"upstream_malformed"' in (await garbage.run(run_body(THREAD, "run_g"))).content


async def test_response_headers_of_the_gateway_are_not_forwarded(make_rig) -> None:
    def handle(gateway: Gateway, request: httpx.Request) -> httpx.Response:
        response = sse_response(b'data: {"type":"RUN_FINISHED","threadId":"ses_test_01","runId":"r"}\n\n', TEST_SECRET)
        response.headers.update({"set-cookie": "gw=1", "x-internal": "1", "access-control-allow-origin": "*", "x-request-id": "req_1"})
        return response

    answer = await make_rig(handle).run(run_body(THREAD, "r"))
    for name in ("set-cookie", "x-internal", "access-control-allow-origin", HEADER_CONFIRMATION_TOKEN):
        assert name not in answer.headers, f"the gateway's {name} header reached the browser"
    assert answer.headers["x-request-id"] == "req_1", "the request id (for support) was not relayed"


async def test_a_gateway_that_cannot_be_reached_is_an_error_document_not_a_stream(make_rig) -> None:
    def down(gateway: Gateway, request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused", request=request)

    rig = make_rig(down)
    run = await rig.run(run_body(THREAD, "r"))
    assert run.status_code == 502 and run.json()["code"] == "uncertain_outcome" and run.json()["retryable"] is False
    events = await rig.browser.get(f"/threads/{THREAD}/runs/r/events")
    assert events.status_code == 502 and events.json()["code"] == "unavailable"


# ---- the token callback, the store, and what the module imports ----


async def test_the_proxy_works_mounted_under_a_prefix(make_rig) -> None:
    # app.mount("/agui", proxy): the server sends the full path and a root_path, as Starlette's Mount does.
    approve = golden("resume_approve")
    cancelled = {"thread_id": THREAD, "run_id": "run_approve", "status": "finished"}
    rig = make_rig(lambda gateway, request: httpx.Response(200, json=cancelled) if request.url.path.endswith("/cancel") else sse_response(approve.sse))
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=rig.proxy, root_path="/agui"), base_url="http://host") as browser:
        run = await browser.post("/agui/", content=approve.request_body, headers={"content-type": "application/json"})
        events = await browser.get(f"/agui/threads/{THREAD}/runs/run_approve/events", headers={"last-event-id": "2"})
        cancel = await browser.post(f"/agui/threads/{THREAD}/runs/run_approve/cancel")
        assert (await browser.get("/agui/nowhere")).status_code == 404
    assert (run.status_code, run.content) == (200, approve.sse)
    assert (events.status_code, events.content) == (200, approve.sse)
    assert (cancel.status_code, cancel.json()) == (200, cancelled)
    assert [request.url.path for request in rig.gateway.requests] == [
        "/v1/agui",
        f"/v1/agui/threads/{THREAD}/runs/run_approve/events",
        f"/v1/agui/threads/{THREAD}/runs/run_approve/cancel",
    ]


async def test_the_token_callback_may_be_sync_async_or_take_the_caller(make_rig) -> None:
    seen: Dict[str, Any] = {}

    def with_caller(request: Any, caller: AguiCaller) -> str:
        seen["user"], seen["session"], seen["op"] = caller.headers.get("x-host-user"), request.session_id, request.operation
        return "ctx_tok_caller"

    gateway = Gateway(lambda g, r: sse_response(b'data: {"type":"RUN_FINISHED","threadId":"ses_test_01","runId":"r"}\n\n'))
    for provider in (with_caller, lambda request: "ctx_tok_sync", Tokens()):
        proxy = AguiProxy("http://127.0.0.1:8080", provider, http_client=httpx.AsyncClient(transport=gateway.transport))
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=proxy), base_url="http://host") as browser:
            answer = await browser.post("/", content=run_body(THREAD, "r"), headers={"content-type": "application/json", "x-host-user": "usr_1"})
        assert answer.status_code == 200
        assert gateway.last.headers["authorization"].startswith("Bearer ctx_tok_")
    assert seen == {"user": "usr_1", "session": THREAD, "op": "runAgui"}
    await gateway.transport.aclose()


def test_a_proxy_is_refused_what_the_sdk_client_refuses() -> None:
    def token(request: Any) -> str:
        return "t"

    for base in ("", "http://alixai.example.com", "https://user:pw@alixai.example.com", "https://alixai.example.com?a=b"):
        with pytest.raises(AlixAIError):
            AguiProxy(base, token)
    with pytest.raises(ValueError):
        AguiProxy("http://127.0.0.1:1", None)  # type: ignore[arg-type]
    assert AguiProxy("https://alixai.example.com/prefix/", token).base_url == "https://alixai.example.com/prefix"


async def test_the_memory_secret_store_expires_and_is_bounded() -> None:
    now = [NOW]
    store = MemorySecretStore(clock=lambda: now[0], max_entries=2)
    a, b, c = SecretKey("t", "a"), SecretKey("t", "b"), SecretKey("t", "c")
    for key in (a, b, c):  # the third evicts the oldest
        await store.put(key, "s" + key.preparation_id, NOW + 60)
    assert await store.get(a) is None and await store.get(c) == "sc"
    assert await store.get(SecretKey("other", "c")) is None
    now[0] = NOW + 120
    assert await store.get(c) is None
    await store.put(a, "sa", NOW)  # already expired: not kept
    assert len(store) <= 1


def test_the_module_imports_no_copilotkit_or_ag_ui_library() -> None:
    source = (REPO_ROOT / "sdk" / "python" / "src" / "alixai" / "agui.py").read_text(encoding="utf-8")
    imported = []
    for node in ast.walk(ast.parse(source)):
        if isinstance(node, ast.Import):
            imported += [alias.name for alias in node.names]
        elif isinstance(node, ast.ImportFrom):
            imported.append(node.module or "")
    for name in imported:
        assert "copilotkit" not in name.lower() and "ag_ui" not in name.lower() and "ag-ui" not in name.lower(), name
    # FastAPI is imported inside router() only: the ASGI application needs nothing but the SDK.
    top_level = [node for node in ast.parse(source).body if isinstance(node, (ast.Import, ast.ImportFrom))]
    assert not any("fastapi" in ast.dump(node) or "starlette" in ast.dump(node) for node in top_level)


# ---- the FastAPI router ----


async def test_the_router_serves_the_same_routes_in_a_fastapi_application(make_rig) -> None:
    fastapi = pytest.importorskip("fastapi")
    sg = SecretGateway()
    rig = make_rig(sg)
    app = fastapi.FastAPI()
    audited: List[str] = []

    async def audit(request: fastapi.Request) -> None:
        audited.append(request.url.path)

    app.include_router(rig.proxy.router(dependencies=[fastapi.Depends(audit)]), prefix="/agui")
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://host") as browser:
        prepared = await browser.post("/agui/", content=sg.prepare.request_body, headers={"content-type": "application/json", HEADER_CONFIRMATION_TOKEN: FORGED})
        assert prepared.status_code == 200 and prepared.content == sg.prepare.sse
        assert_no_secret(prepared, "router prepare")
        assert HEADER_CONFIRMATION_TOKEN not in rig.gateway.last.headers
        approved = await browser.post(
            "/agui/", content=resume_body(THREAD, "run_approve", PREPARATION, "resolved", {"approved": True}), headers={"content-type": "application/json"}
        )
        assert approved.status_code == 200 and approved.content == sg.approve.sse
        assert rig.gateway.last.headers[HEADER_CONFIRMATION_TOKEN] == TEST_SECRET
        again = await browser.post(
            "/agui/", content=resume_body(THREAD, "run_approve_2", PREPARATION, "resolved", {"approved": True}), headers={"content-type": "application/json"}
        )
        assert again.status_code == 428 and again.json()["code"] == "confirmation_required"
        assert (await browser.post("/agui/", content=b"x", headers={"content-type": "text/plain"})).status_code == 415
        replay = await browser.get(f"/agui/threads/{THREAD}/runs/run_approve/events?cursor=2", headers={"last-event-id": "3"})
        assert replay.status_code == 200 and replay.content == sg.approve.sse
        assert rig.gateway.last.url.query == b"cursor=2" and rig.gateway.last.headers["last-event-id"] == "3"
    assert len(audited) == 5, "the router's dependencies did not run for every route"
    assert rig.tokens.requests[0].session_id == THREAD
