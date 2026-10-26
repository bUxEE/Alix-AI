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

## Reply text arrives whole in External mode

The turn **event stream** still works in External mode (`Accept: text/event-stream`, AG-UI / TurnEvents): progress and step events arrive live as they happen. What differs from Local is the **assistant text**: the dialogue engine calls `Chat` (not provider `ChatStream`), so the reply is verified first and then emitted as one piece (one `TEXT_MESSAGE_CONTENT` / token event), not fragment by fragment. A JSON turn or the event stream are both fine.

Recommended host pattern (about **total latency**, not about avoiding the stream):

1. Accept the user turn on your backend.
2. Run the AlixAI turn in the background (JSON turn or event stream).
3. Push progress and the verified result to the UI (websocket, SSE from *your* backend, or poll).

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

## Spike order

1. Run AlixAI on loopback; `config check` / `providers layers` green for your light setup ([light-external.md](../../operations/guides/light-external.md)).
2. Copy `examples/packages/valid-help-assistant`, retarget the OpenAPI endpoint, sign with your publisher key, `package validate`, install, audit, activate.
3. Mint a context token from a tiny host handler; complete one turn (JSON or event stream) in Local or with a scripted fake first.
4. Enable External: credentials → qualify profile → egress (`user_message` / `internal`) → budget → mode External at all three levels.
5. Confirm the first real turn; if denied, use the error table above.
6. Add UI on the **host** only (SDK or AG-UI proxy). Optionally add dictation or host-side STT.

## Related

- [Getting started](getting-started.md) (clinic path; heavier than help-assistant)
- [Application package format](../application-package.md)
- [External mode for integrators](../external-mode.md)
- [Light External deployments](../../operations/guides/light-external.md)
- [Troubleshooting](../../operations/guides/troubleshooting.md)
