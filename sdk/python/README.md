# alixai (Python SDK)

Sync and async clients for the AlixAI public API (`schemas/api/openapi.yaml`), typed models generated from the JSON Schemas, SSE turn streaming, an AG-UI proxy
for CopilotKit and AG-UI frontends, and a fake server for the tests of a host application. Python 3.11 and later; runtime dependencies are `httpx` and
`pydantic` only.

**Status: 1.5.0.** The contracts this SDK speaks are frozen for v1 (`schemas/_frozen/v1`, checked by `tools/compat`).
`pyproject.toml` reads `1.5.0` with a Production/Stable classifier. Compatibility and support rules are in
[compatibility](../../docs/policies/compatibility.md) and [migration and support](../../docs/policies/migration-and-support.md).
The version this SDK is tested against is the core in the same repository commit. See [CHANGELOG.md](CHANGELOG.md).

## What is in it, and what is not

This SDK is the narrowest of the three. It covers the host-application path: sessions, turns, confirmations, applications, capabilities, health, the decision
status, the providers area and the AG-UI proxy. It does not have what the TypeScript SDK has beyond that.

| Module | State |
| --- | --- |
| Core: sessions, streamed and JSON turns, confirmations, actions, applications, capabilities, health | Complete. `AlixAIClient`, `AlixAIAsyncClient` |
| Attachments on a turn (up to four uploaded file ids) | The turn member only. **No upload, download or preview**: files must already exist in the session |
| Resume from a cursor (`streamTurnEvents`), WebSocket transport | **Not in this SDK.** A lost stream is `uncertain_outcome`; read `get_pending_state` before sending anything again |
| Conversation helper (stop, regenerate, edit-and-resubmit), history, privacy and settings | **Not in this SDK** |
| RAG client (sources, search, grounded answers) | **Not in this SDK** |
| Voice | **Not in this SDK** (TypeScript only). **Full-duplex voice is not claimed** by any SDK |
| Providers, modes, budgets (External provider mode) | Complete for the providers and OAuth operations (X-14): `client.providers`. **Opt-in and off by default**; a deployment without it answers `unavailable` |
| Per-principal LLM account (ADR 0041) | Complete (1.2.0): `client.me` / async `me` (catalog, LLM bindings, credential, billing ack, OAuth start). `submit_credential` is **host-server only** |
| Long-term memory (ADR 0042) | Complete (1.2.0): `client.memory` / async `memory` (list/create/patch/delete/confirm/reject) |
| Readiness reports (M9-06) | Types only: `ReadinessReport`, `ReadinessFinding`, `ReadinessRequirement`, `ReadinessStatusView` (`alixai.gen.readiness`) read a stored report. **No client method**: readiness stays CLI-only and advisory, never an audit result |
| Decision status | Read-only (D-10): `get_decision_status()`. Advisory information, never an authorisation |
| AG-UI proxy for CopilotKit and AG-UI frontends | Complete (M3-27): `alixai.agui`. CopilotKit itself is not a dependency |
| Operator-only decision configuration | Not in any SDK |
| Lite profile | Uncertified. Nothing in this SDK depends on it |

The same feature-by-SDK table, for all three SDKs, is in [the integration guides](../../docs/integration/guides/README.md).

## Quick start

Every code sample in this README (except the commands under Development) is copied from a test that runs, and names it. The tests use the fake server (`fake.client("ses-1")` is a client whose token
provider mints one fresh token per call). Against a real AlixAI you construct the client with its URL and your own `get_context_token`, as in the constructor
signature `AlixAIClient(base_url, get_context_token)`.

From `tests/test_client.py` (fixtures and `test_a_text_turn_streams_its_events_in_order_and_ends_with_done`):

```python
fake = FakeAlixAIServer()
client = fake.client("ses-1")

fake.on_turn(lambda turn: reply("Hello there"))
with client.send_turn("ses-1", {"text": "hi", "language": "en"}) as stream:
    events = list(stream)
```

`AlixAIAsyncClient` has the same methods as coroutines (`async with`, `async for`); its `get_context_token` may be a coroutine function. From
`tests/test_async_client.py` (`test_a_turn_streams_with_async_for_and_pairs_the_confirmation_secret`):

```python
async with fake.async_client("ses-1") as client:
    events = []
    async with await client.send_turn("ses-1", {"text": "book"}) as stream:
        async for event in stream:
            events.append(event)
        grant = stream.confirmation
```

## Behaviour worth knowing

- **Tokens.** A context token is **single use** (ADR 0013). `get_context_token` is called once per authenticated request or stream open, after the arguments are
  validated, never for `/healthz` or `/readyz`, and receives a `ContextTokenRequest` (`operation`, `session_id`, `preparation_id`, ...). The SDK never caches or
  retries with a token, and never puts one in an error. A host that caches its token fails with `replayed`.
- **Streams.** A stream that ends before `done`, a gap in `seq`, a changed `turn_id` or an unknown event type raises; it is never a quiet end. `with` / `close()`
  releases the connection.
- **Verified streaming.** With `dialogue.stream_verified: true` the core may emit `text_replaced` (ADR 0046). Read turns over **SSE** from your backend (this SDK's only
  stream transport); on `text_replaced`, discard the assistant text already shown for that turn (`turn_text` resets) and render only the tokens that follow.
- **Errors.** Every failure is an `AlixAIError` with a closed `code` (`ERROR_CODES`), `retryable`, `status`, `details` and `request_id`. A commit the host refused or
  left uncertain carries the `receipt` and `preparation`. A request that may have been processed but whose answer was lost is `uncertain_outcome` (never
  retryable): read the state first. **The SDK retries nothing.**
- **Models** are pydantic models in `alixai.gen` (`common`, `actions`, `conversation`, `api`, `decision`, ...). Closed schemas (`additionalProperties: false`)
  forbid unknown members; open ones ignore them. Cross-member rules (`if/then`, `allOf`) are not modelled: the schemas stay the authority.
- `base_url` must be https, or http to a loopback host (`allow_insecure_http=True` to override). Redirects are not followed.

## Confirmations

When a turn prepares an action that needs the user's explicit confirmation, the `done` event says `awaiting_confirmation` and the **single-use confirmation
secret** is not in any event: it is the `X-AlixAI-Confirmation-Token` response header of the stream (or the `confirmation` member of the JSON turn). Show it to the
user's client only and present it once. From `tests/test_client.py` (`prepared`, `test_confirm_commits_once_and_a_second_use_of_the_secret_is_replayed`):

```python
fake.on_turn(lambda turn: prepare_action(**BOOK, commit=Commit(**commit)))
grant = client.send_turn_json("ses-1", {"text": "book"}).confirmation
```

```python
outcome = client.confirm_action(grant.preparation_id, grant.token)
client.get_action_receipt(grant.preparation_id)
again = failure(lambda: client.confirm_action(grant.preparation_id, grant.token))
assert (again.code, again.status) == ("replayed", 409)
```

A structured confirmation turn also commits, and a text turn never does (`test_confirming_by_a_structured_turn_commits_and_a_text_turn_never_does`):

```python
done = client.send_turn("ses-1", {"confirmation": {"preparation_id": grant.preparation_id, "token": grant.token}}).result()
```

A level 3 action returns a `request_step_up` directive and no secret; it cannot be confirmed through the API (`test_a_level_three_action_has_no_secret_and_cannot_be_confirmed_through_the_ai_path`):

```python
fake.on_turn(lambda turn: prepare_action(**BOOK, risk_level=3))
result = client.send_turn("ses-1", {"text": "close it"}).result()
```

## Applications, capabilities, health

| Method | Operation (OpenAPI `operationId`) |
| --- | --- |
| `create_session()`, `get_session(id)`, `get_pending_state(id)`, `end_session(id)` | `createSession`, `getSession`, `getPendingState`, `endSession` |
| `send_turn(id, input)`, `send_turn_json(id, input)` | `sendTurn` |
| `get_action(id)`, `confirm_action(id, token)`, `reject_action(id)`, `get_action_receipt(id)` | the action routes |
| `get_application(id)`, `list_capabilities()`, `register_application(archive)` | the application routes (a registered package lands in quarantine) |
| `get_health()`, `get_readiness()`, `get_liveness()` | `/v1/health`, `/readyz`, `/healthz` |
| `get_decision_status()` | `getDecisionStatus` |
| `providers.*` | the providers and OAuth operations |

`OPERATIONS` is the table of operations the SDK speaks, checked against `schemas/api/openapi.yaml` by `tests/test_openapi_contract.py`. An application is served only
while its package is active, and a package becomes active only through a clean integration audit run by the operator
([getting started](../../docs/integration/guides/getting-started.md)); an application that is not active serves nothing (`quarantined`).

## Private LLM and `me.put_role` (ADR 0045)

Operator routes on `client.ops` return generated models (`PrivateLLMEndpoint`, `TenantPrivateLLMPin`, …). After an endpoint is registered and ready, bind a
role with `private_endpoint_id` and that endpoint's `profile_id`:

```python
endpoint_id = "ple_01J0000000000000000000000A"
ep = client.ops.put_private_llm_endpoint(
    endpoint_id,
    {
        "origin": "http://127.0.0.1:8080",
        "api_dialect": "openai-compatible",
        "model_ids": ["local-model"],
        "roles": ["conversational_controller"],
        "owner_kind": "shared",
    },
)
client.me.put_role(
    "conversational_controller",
    {"profile_id": ep.profile_id, "private_endpoint_id": endpoint_id},
)
```

## Providers, modes, budgets (External provider mode)

External provider mode is **opt-in and off by default**: `local` is the effective mode everywhere until an operator changes it, and a deployment without the
feature answers every method `unavailable`. The area is operator-only: modes, egress policy and disclosure, budgets in integer micro-units, usage export, the kill
switch, credential submission (a `Secret` prints as a placeholder and is never returned or recorded) and the browser OAuth flow. An external model is an
interchangeable inference backend only; it never executes a tool or owns permissions. From `tests/test_providers.py`
(`test_local_is_the_default_the_most_restrictive_level_decides_and_the_kill_switch_wins`):

```python
p = operator(fake).providers
mode = p.get_mode()
p.set_mode("deployment", "external")
mode = p.set_mode("tenant", "hybrid")
switch = p.set_kill_switch(True, "incident_42")
mode = p.get_mode()
```

## Decision status

`client.get_decision_status()` reads the status of the decision layer: provider, model, mode and fallback counters. The decision layer is optional, bounded,
non-generative and non-authoritative: it scores candidates that were already authorised and never authorises, validates, prepares, confirms or commits anything,
and with it off or failing the controller produces the same deterministic outcomes. The status is advisory operational information (it needs the capability
`decision.status.read`), and no member of it is permission to do anything. From `tests/test_decision.py`:

```python
fake.set_decision_status(decision_status_document("advisory", accepted=7, fallbacks={"timeout": 1, "low_confidence": 2}))
client = AlixAIClient(fake.base_url, provider, http_client=httpx.Client(transport=fake.transport))
status = client.get_decision_status()
```

## AG-UI proxy (CopilotKit and AG-UI frontends)

`alixai.agui` serves a CopilotKit or AG-UI frontend from your backend's own origin, with no CopilotKit runtime in the middle: an ASGI app (and a FastAPI-compatible
router) that validates the thread and run ids, asks your callback for a fresh single-use context token (session = thread), forwards the run to `POST /v1/agui` and
streams the answer back. AlixAI stays the authority layer. The confirmation secret stays server-side in a `SecretStore`; the browser never holds it, and it is attached
only to a resume that explicitly approves that preparation. The module imports no CopilotKit or AG-UI library and calls nothing but the base URL it was given (no cloud,
no telemetry). The published CopilotKit version matrix is [docs/integration/copilotkit-compatibility.md](../../docs/integration/copilotkit-compatibility.md); the
quickstart for a Python host is in [the CopilotKit guide](../../docs/integration/guides/copilotkit.md). From `tests/test_agui.py`
(`test_the_token_callback_may_be_sync_async_or_take_the_caller`, `test_the_router_serves_the_same_routes_in_a_fastapi_application`):

```python
def with_caller(request: Any, caller: AguiCaller) -> str:
    seen["user"], seen["session"], seen["op"] = caller.headers.get("x-host-user"), request.session_id, request.operation
    return "ctx_tok_caller"
```

```python
proxy = AguiProxy("http://127.0.0.1:8080", provider, http_client=httpx.AsyncClient(transport=gateway.transport))
```

```python
app.include_router(rig.proxy.router(dependencies=[fastapi.Depends(audit)]), prefix="/agui")
```

## Tests for your application

The fake server enforces single-use tokens, scope, the confirmation protocol, quarantine and the sequence rules; `fail_next(...)` injects errors, network failures and cut
streams. It runs no model, does not verify signatures and reaches no application. None of the controller models is activatable yet, so a live conversation needs a model the
operator has qualified; a host is tested against the fake. From `tests/test_client.py`:

```python
fake = FakeAlixAIServer()
fake.on_turn(lambda turn: prepare_action(**BOOK))
client = fake.client("ses-1")
```

## Development

```sh
python -m venv .venv && .venv/Scripts/python -m pip install -e ".[test]" && .venv/Scripts/python -m pytest
python tools/gen_models.py            # regenerate src/alixai/gen after a change in schemas/
python tools/gen_models.py --check    # the drift check (also a test)
```

`tests/test_openapi_contract.py` fails when the SDK's operation table or the fake server's answers drift from `schemas/api/openapi.yaml`.
