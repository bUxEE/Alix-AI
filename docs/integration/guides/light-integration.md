# Integrate AlixAI as a chat assistant with external AI (no local LLM)

Status: written for the 1.1.0 additive wave. Companion to [light External deployments](../../operations/guides/light-external.md) and [External mode for integrators](../external-mode.md).

This guide is for a host application that wants a **signed help / FAQ chat assistant** on AlixAI in light External mode (hosted chat provider, no local controller model). Every claim below points at code or a doc that proves it.

## What AlixAI is not

AlixAI is **not** a free-form chatbot you point at an OpenAI key.

- A **signed application package** is required. Without one, nothing is served (`docs/integration/application-package.md`; install → audit → activate in [getting started](getting-started.md)).
- The package manifest must declare **at least one endpoint** and **at least one capability**. The loader and schemas enforce that shape (`schemas/application-package/manifest.schema.json`; empty capabilities or endpoints are refused).
- The model only proposes. Reads and writes go through your adapter; consequential actions still prepare / confirm / commit. Chat text that leaves in External mode is subject to the data-egress guard and provider budgets.

**Start from** [`examples/packages/valid-help-assistant/`](../../../examples/packages/valid-help-assistant/): one OpenAPI endpoint, one read capability `help.overview`, en+it contracts and docs, one FAQ RAG source with `egress_mode: allow_excerpt`, context-token exchange, core range `[1.0.0, 2.0.0)`. Validate it with `alixai package validate` (TEST-ONLY key needs `-allow-test-keys`).

## Architecture

```text
browser  →  host backend (mints a single-use context token per request)
         →  AlixAI on loopback or TLS
```

- The **browser never calls AlixAI**. AlixAI sends no CORS headers; a browser `OPTIONS` is refused ([troubleshooting](../../operations/guides/troubleshooting.md) §2; [browser hosts](../../operations/guides/browser-hosts.md)).
- The host authenticates the user, then mints a short-lived, single-use, audience-bound context token (EdDSA, ≤ 15 min) and calls AlixAI through an SDK or HTTP. SDKs take a per-request token callback and never cache one ([getting started](getting-started.md) step 10).
- Run AlixAI on **loopback** for a same-machine sidecar, or with TLS when the listener is beyond loopback (`gateway` refuses a non-loopback listen without certs). The service image under [`deploy/container/alixai/`](../../../deploy/container/alixai/) is the container form of that sidecar.

## Deployment config (light External)

Follow [light External deployments](../../operations/guides/light-external.md): `external_providers.enabled: true`, `routing.controller` (and usually `realisation`) naming a qualified profile, empty `inference.models` when there is no local LLM, RAG and decision layer as you need them.

Minimum recommended hosts for light mode (every model external; ceiling **4 vCPU / 8 GB**): [Hardware](../../operations/guides/light-external.md#hardware).

Credentials are **env or file refs only** through the credentials service — never in the package, prompts, logs, URLs or browser storage (`internal/credentials`; [External mode](../external-mode.md) §3).

## Per-tenant onboarding checklist

`routing` and empty local models do **not** put every tenant on an external model. Mode is resolved from `modes.json` (deployment, application, tenant); the most restrictive level wins ([light-external.md](../../operations/guides/light-external.md) §Mode is per tenant).

For each tenant that should chat:

1. **Mode External** at deployment, application and tenant:

   ```http
   PUT /v1/providers/mode
   {"level": "deployment", "mode": "external"}
   PUT /v1/providers/mode
   {"level": "application", "mode": "external"}
   PUT /v1/providers/mode
   {"level": "tenant", "mode": "external"}
   ```

   Confirm with `GET /v1/providers/mode` (`effective` must be `external`).

2. **Egress policy** allowing category `user_message` at class **`internal`** for the provider/model you use. Chat text is always sent as `internal` for the egress decision: the core classes user-authored context confidential, and External mode maps it to `internal` (`internal/dialogue/extmode/mapping.go` `part`, `internal/dialogue/extmode/doc.go`). A tenant whose egress policy denies `user_message` / `internal` gets a typed denial on the first turn (`permission_denied` with egress details — [external-mode.md](../external-mode.md) §4).

3. **Budget** for that provider/profile (integer micro-units). No budget → `resource_exhausted` with reason `no_budget`.

Miss any one of these and the turn fails closed with a typed error; there is no silent fallback to another provider.

## Answers from live data: templates or external wording

Read capabilities return deterministic facts. How the user sees them:

| Setting | What leaves | Who writes the reply |
| --- | --- | --- |
| `directive_class` empty (default) | Not the directive | Deterministic template (text; plus a validated table of host records when the result has rows, and a chart when the conversation contract sets `presentation.chart`) |
| `directive_class: public\|internal\|confidential` **and** egress allows `dialogue_directive` / `deterministic_fact` at that class | The verified directive | External model wording, re-checked: every number in text, table cells and chart data must be a directive fact; otherwise the template |

`alixai config check` / `providers layers` print `directive_class`. See [external-mode.md](../../operations/external-mode.md) §2 and [ADR 0044](../../decisions/0044-external-directive-class-and-result-blocks.md).

## Reply text: whole by default, optional verified streaming

The turn **event stream** works in External mode (`Accept: text/event-stream`, AG-UI / TurnEvents): progress and step events arrive live. By default (`dialogue.stream_verified` off, 1.3 behaviour) the assistant text is verified as a whole then emitted once. With **`dialogue.stream_verified: true`** (ADR 0046) the engine uses `ChatStream` when the provider supports it, splits the reply into language-aware sentences, verifies each before emit, and maps them to `TEXT_MESSAGE_CONTENT`. Nothing unverified is shown. If a later segment fails, TurnEvent `text_replaced` → AG-UI `CUSTOM` `alixai.text_replaced` clears shown text and the deterministic template follows. Default stays off so 1.3 clients are unchanged; turn it on for smoother first paint when the provider streams.

Recommended host pattern (about **total latency**, not about avoiding the stream):

1. Accept the user turn on your backend.
2. Read the AlixAI turn from your backend over **SSE** (the stable server-to-server option; WebSocket is server-to-client only and adds nothing there). Resume uses the cursor / `Last-Event-ID`.
3. Relay progress and segments to the browser over the host's own channel (for example its WebSocket), in order, including the replace instruction. On `text_replaced` / AG-UI `alixai.text_replaced`, discard the superseded assistant text for that turn; the deterministic template follows as new tokens.

Set timeouts on the host call above the provider's own limits; a slow or dead provider surfaces as `unavailable` or `timeout` ([external-mode.md](../external-mode.md) §4).

## STT choices

| Approach | Use when | Notes |
| --- | --- | --- |
| **Host-side transcription** | Arbitrary audio files / formats | Transcribe in your backend; send **text** to AlixAI as a normal turn. No AlixAI audio egress. |
| **AlixAI dictation** | Record-to-text into the composer | `GET /v1/dictations/capabilities`, `POST /v1/sessions/{id}/dictations`. Body is **WAV PCM s16le mono 16 kHz**; length/energy limits apply ([ADR 0039](../../decisions/0039-dictation-without-vad.md), [voice.md](../voice.md)). Dictation inserts editable text; it never submits a turn. |

External STT for dictation needs `routing.stt` naming a profile with role `stt`, an `audio_seconds_in` price, and egress **`audio` enabled** at every level with `max_class` at least `dictation.audio_class` (default `internal`; audio egress is off by default) — [light-external.md](../../operations/guides/light-external.md) setup 3.

## Error → cause → fix

| Symptom | Cause | Fix |
| --- | --- | --- |
| `unavailable` on a turn | Mode not External, no controller route, or provider down | Set External at every level; check provider health ([light-external.md](../../operations/guides/light-external.md)) |
| `permission_denied` / egress denied (`egress_denied` in details) | Egress policy denies the category/class (often `user_message` / `internal` on the first turn) | Allow `user_message` at `internal` for that tenant; see mapping above |
| `resource_exhausted` reason `no_budget` | No budget configured or exhausted | Set / raise budget (`PUT` budgets routes; [external-mode.md](../external-mode.md) §4) |
| `compat.core_out_of_range` / package refused at install | Core version outside `compatibility.alixai_core` | Use a package with `[1.0.0, 2.0.0)` (e.g. `valid-help-assistant`); source builds report `1.1.0-dev` (`apppkg.DevCoreVersion`) |
| Context token refused (`unknown_key`, `wrong_audience`, `replayed`, …) | Issuer key, audience, or single-use misuse | Register the issuer key; mint one token per request ([troubleshooting](../../operations/guides/troubleshooting.md) §3) |
| `/readyz` 503 | No text model cover, LocalAI down, or warming | Light External: ensure controller route + External cover; see troubleshooting §2 and light-external readiness notes |
| Dictation `validation_failed` | Wrong audio format/length/energy | Send WAV PCM s16le mono 16 kHz within limits ([ADR 0039](../../decisions/0039-dictation-without-vad.md)) |

## Multiple companies, shared views and per-user accounts

Hosts that serve several companies for one signed-in person map scope like this **today** (1.1 behaviour; no core
cross-tenant read):

| Host idea | AlixAI scope |
| --- | --- |
| Company | `tenant_id` |
| Optional area / site | `workspace_id` |
| Signed-in user | `principal_id` (same id in every company) |
| Shared / “all my companies” view | A **personal tenant** for that user; the HOST fills it (aggregate of the user’s companies) through an `api` RAG source with tenant mode `request` |

Rules that must stay:

- Never use binding mode `shared` for user data.
- One session per scope; mint a fresh context token per view (and per request).
- AlixAI never mixes tenants in one turn; the shared view is still one tenant (the personal one).

**Per-user LLM accounts (1.2):** each principal picks a catalog provider/model and brings their own key;
companies still gate egress — [ADR 0041](../../decisions/0041-per-principal-llm-accounts.md). Host flow:

1. Operator qualifies a profile and marks it for the catalog (`alixai providers catalog-add`).
2. Host reads `GET /v1/providers/catalog` (no secrets) and shows terms, prices and billing notice.
3. Principal acknowledges billing (`POST /v1/me/providers/{provider}/billing-notice/ack`), then submits a key
   through the **host server** only (`PUT /v1/me/providers/{provider}/credential`, write-only) or starts
   `POST /v1/me/oauth/{provider}/start` where OAuth is configured.
4. Principal binds a role (`PUT /v1/me/llm/{role}` with a catalog `profile_id`). Roles are `conversational_controller`, `language_realization`, `stt` and `tts` (optional `voice` / `language` on speech roles). Each role may use a different provider.
5. In External mode the router uses the principal binding, else the deployment default route, else
   `unavailable` with `details.reason` `llm_not_configured`. Tenant egress still applies to that company's data.
6. **Usage and budget (1.3):** the principal reads their own spend with `GET /v1/me/usage` (all tenants of the
   application, or `tenant_id=…`) and their principal budgets with `GET /v1/me/budget`. An operator sets an
   application-wide principal budget with `PUT /v1/providers/budgets` (subject `kind: principal_application`,
   application and principal only); that limit applies in every company the user acts in, together with any
   provider/tenant/workspace/principal budgets (strictest wins). SDKs: `client.me.usage()` / `client.me.budget()`.

**Per-user browser login (1.3):** complete principal OAuth on the host callback path, not the operator routes.

| Provider | Remote host callback (`callback: host`) | What the operator registers |
| --- | --- | --- |
| OpenAI Sign in with ChatGPT | No — start answers `manual_fallback`; use a manual API key (SIWC is loopback-only) | n/a for remote |
| Google (`gemini`, `google-stt`, `google-tts`) | Yes | https redirect URI + `external_providers.oauth.popup_origins` |
| OpenRouter | Yes | https redirect URI + `external_providers.oauth.popup_origins` |

Host flow for a remote user: `POST /v1/me/oauth/{provider}/start` with `callback: "host"` and your https `redirect_uri` → open `authorization_url` → on the provider redirect, relay to `POST /v1/me/oauth/callback` with a fresh token of the **same** principal. For popup, poll `GET /v1/me/oauth/flows/{flow_id}` (cancel with `DELETE`). Do not use `/v1/oauth/callback` — that route is operator-only.

**Erasing a user (1.3):** two principal erasures — pick by who asks.

- **Left one company (tenant-scoped):** a tenant admin (or deployment operator) calls `POST /v1/ops/erasure` with `{ "level": "principal", "principal_id": "…", "reason": "…" }` (optional `"scope": "tenant"`). Erases that principal's data **inside that tenant only**. Never revokes their personal LLM credentials or bindings (those serve every tenant — ADR 0041).
- **GDPR erase this person (application-wide):** a **deployment operator** calls with `{ "level": "principal", "scope": "application", "principal_id": "…", "reason": "…" }`. Erases the principal in every tenant of the application and revokes credentials/bindings. Same as `alixai retention erase -principal` (add `-tenant-scoped` on the CLI for the tenant-local path).

Both answer `202` + `job_id`; poll `GET /v1/ops/erasure/{job_id}`. Levels `tenant` and `workspace` are also accepted. SDKs: `client.ops.erasure.start` / `status` (TypeScript), `client.ops.start_erasure` / `erasure_status` (Python), `client.Ops().StartErasure` / `ErasureStatus` (Go). See [retention-erasure.md](../../operations/retention-erasure.md).

**Long-term memory (1.2):** set `memory.enabled: true`, put `memory.read` / `memory.write` / `memory.confirm` (and
related) in the context token, then use `GET|POST /v1/memory` and confirm/reject on proposals. Assistant proposals
arrive as TurnEvent `memory_proposal` / AG-UI `alixai.memory_proposal`; typed chat text never confirms. See
[memory.md](../memory.md) and [ADR 0042](../../decisions/0042-long-term-memory.md).

## Reports and downloads (1.2)

When the host needs a file the user can download (a report or a data export), call
`POST /v1/sessions/{session_id}/exports` with `{turn_id, block_ids?, format}` where `format` is
`csv` \| `xlsx` \| `html` \| `md`. Put `exports.create` in the context token. The response is a
`FileRef` (`origin: generated`); the host downloads bytes with `GET /v1/files/{file_id}/content`
(same permission re-check as any other file). AlixAI builds the file only from blocks it has already
verified for that turn — never from model-written file bytes. See
[conversation-objects.md](../conversation-objects.md) §10a.

SDKs: `client.exports.create(sessionId, { turnId, format, blockIds? })` (TypeScript),
`client.exports.create(session_id, body)` (Python), `client.Exports().Create(...)` (Go).

## Authenticating AlixAI's calls into your application

When the package declares `credential_ref` on an OpenAPI or MCP endpoint (or on a RAG `api` source), AlixAI authenticates its outbound calls into the host. The secret never lives in the package.

1. **Declare.** In the manifest (or RAG source), set `credential_ref: cred:help-api` (grammar `cred:` + lower-case slug). The adapter configuration may set `auth.header` / `auth.scheme` (defaults `Authorization` / `Bearer`, same for OpenAPI and MCP HTTP/SSE). Stdio MCP ignores credentials (packages cannot declare stdio).
2. **Provision.** In the AlixAI config, under `application_credentials.credentials`, bind that ref to an env or file backend (prefix `ALIXAI_SECRET_`, or a plain file name in the secrets directory). Example:

   ```yaml
   application_credentials:
     credentials:
       - ref: cred:help-api
         application: help-assistant
         tenant: acme
         env: ALIXAI_SECRET_HELP_API
   ```

   This uses the same credential service as `external_providers` (encrypted file / env / file backends) and works with External mode off (default build). A binary built with `-tags noexternal` has no credential service: do not set `application_credentials.credentials` there. Every use is audited; the value is re-read on every call (rotate by changing the env or file, no restart).
3. **Header.** The host sees `Authorization: Bearer <secret>` by default. Verify with a constant-time compare (`crypto/subtle.ConstantTimeCompare` or equivalent); never log the secret.
4. **Network isolation.** Keep AlixAI→host on loopback or a private network allowlisted in `network.egress`. Mutual TLS (client certificates) for these adapter calls is **not** supported: authenticate with `credential_ref` (and TLS server certs for https destinations) only.
5. **Missing / wrong secret.** A declared ref with no provider or an unreadable value fails closed (adapter build or call refused; audited reason, no secret in logs or errors).

See [application-package.md](../application-package.md) (`credential_ref` on endpoints and RAG sources). The `valid-help-assistant` example declares `cred:help-api` on its OpenAPI endpoint. The serve-level proof that each transport sends `Authorization: Bearer <its own secret>` (rotation, missing-secret isolation, no secret in logs or audit export) is `TestServeSendsCredentialRefToHostOnEveryTransport` in `internal/cli`.

## UI kit fullscreen z-index

Optional Vue / React chat panels support a fullscreen overlay (default z-index **2400**, above typical framework overlays at ~2000). Each kit keeps its own CSS token prefix:

| Kit | CSS custom property | Prop override |
| --- | --- | --- |
| `@alixai/vue` | `--alix-fullscreen-z` | `fullscreenZIndex` |
| `@alixai/react` | `--alixai-z-fullscreen` (also sets `--alix-fullscreen-z`) | `fullscreenZIndex` |

Body-teleported dialogs/menus opened from the panel must use a higher z-index than that value. See `ui/vue/demo/README.md` and `ui/react/demo/README.md`.

## Private self-hosted LLM (optional, 1.4.0)

When the host runs its own OpenAI-compatible server (vLLM, LocalAI, …), register it as a **private endpoint** instead of (or beside) a catalog provider ([ADR 0045](../../decisions/0045-private-self-hosted-llm-endpoints.md), [operations/private-llm.md](../../operations/private-llm.md)):

1. `PUT /v1/ops/private-llm/endpoints/{endpoint_id}` with `origin`, models, roles, `owner_kind`.
2. Allowlist provider `self_hosted` and those model ids on egress; set mode External.
3. Either **pin the tenant** (`PUT /v1/ops/tenants/{tenant_id}/private-llm`) so every principal in that company uses it, or **bind a principal** (`PUT /v1/ops/principals/{id}/private-llm` / `PUT /v1/me/llm/{role}` with `private_endpoint_id`).
4. While the endpoint is warming, turns refuse with `private_llm_starting` — there is no silent fallback to OpenAI.

## Spike order

1. Run AlixAI on loopback; `config check` / `providers layers` green for your light setup ([light-external.md](../../operations/guides/light-external.md)).
2. Copy `examples/packages/valid-help-assistant`, retarget the OpenAPI endpoint, sign with your publisher key, `package validate`, install, audit, activate. Provision `cred:help-api` if the host requires auth (section above).
3. Mint a context token from a tiny host handler; complete one turn (JSON or event stream) in Local or with a scripted fake first.
4. Enable External: credentials → qualify profile → egress (`user_message` / `internal`) → budget → mode External at all three levels.
5. Confirm the first real turn; if denied, use the error table above.
6. Add UI on the **host** only (SDK or AG-UI proxy). Optionally add dictation or host-side STT.
7. Optionally register a private LLM endpoint and pin a tenant or bind a principal ([private-llm.md](../../operations/private-llm.md)).

## Related

- [Getting started](getting-started.md) (clinic path; heavier than help-assistant)
- [Application package format](../application-package.md) (including `credential_ref` and outbound auth)
- [External mode for integrators](../external-mode.md)
- [Light External deployments](../../operations/guides/light-external.md)
- [Private self-hosted LLM](../../operations/private-llm.md)
- [Troubleshooting](../../operations/guides/troubleshooting.md)
