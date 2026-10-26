# Guide: light External deployments (no local LLM)

Status: written for the 1.1.0 additive wave. Companion to [Operating External and Hybrid mode](../external-mode.md).

A light deployment keeps every security and stability feature (policy gateway, prepare / confirm / commit, audit, data-egress guard, budgets, signed packages, adapters, identity, sessions, verifier, readiness) and chooses each AI layer independently through the existing `external_providers.routing` map. A role not listed stays local. There is no second source of truth.

| Layer | Choices today |
| --- | --- |
| LLM (controller + realisation) | local (LocalAI + pinned `inference.models`) or external (OpenAI, Anthropic, Gemini, Meta, OpenRouter, xAI, Moonshot/Kimi) |
| STT | local (active language pack that provides STT, over LocalAI) or external (when an STT adapter and route are configured) |
| TTS | local (active pack that provides TTS); routing `tts` to an external profile is **refused** until an adapter registers (`external TTS is not available yet (roadmap)`) |

Required setups:

1. **Everything local** — today's default. Unchanged.
2. **STT local + LLM external** — LocalAI may run for transcription with an STT language pack; no local controller model.
3. **Everything external** — no LocalAI process at all.

In setups 2 and 3 there is no voice-conversation mode and no VAD. VAD exists only for voice-conversation mode.

## Mode is per tenant (required for setups 2 and 3)

`external_providers.routing` and empty `inference.models` do **not** put every tenant on an external model. Mode is resolved at runtime from `modes.json` (deployment, application, tenant); the most restrictive level wins. Hybrid keeps the controller local. With no local LLM:

- a tenant still in **Local** or **Hybrid** has **no text model** — turns fail closed with `unavailable` and nothing is sent externally;
- only a tenant whose effective mode is **External** uses the routed controller.

`/readyz` may report the controller as ok with reason `external_route: tenants in external mode`. That means the deployment *can* serve External-mode tenants; it does not mean every tenant already has a text model.

After register / qualify / egress / budget, set **every** level to External for each tenant that should chat:

```http
PUT /v1/providers/mode
{"level": "deployment", "mode": "external"}
PUT /v1/providers/mode
{"level": "application", "mode": "external"}
PUT /v1/providers/mode
{"level": "tenant", "mode": "external"}
```

Deployment and application levels need a deployment operator; the tenant level may be set by a tenant administrator. See [External mode](../external-mode.md) section 2. Confirm with `GET /v1/providers/mode` (`effective` must be `external`).

## Facts (what the code does)

With `external_providers.enabled: true`, mode External at every level, `routing.controller` (and usually `realisation`) naming a profile, RAG off, decision layer off, and no LocalAI / empty `inference.models`:

- **`alixai serve` starts.** Empty models with an external controller route skip the failing `inference` readiness check and mark the controller covered (`internal/cli/cmd_serve.go`, `MarkExternallyCovered` in `internal/providers/registry.go`). A pinned local model that fails hash verification still fails closed.
- **`/readyz` is green** for the text controller when that cover is set (`external_route: tenants in external mode`). Without a controller route and without local models, `/readyz` stays down (same as a local deployment with an empty pin list).
- **A full text turn works** through `internal/dialogue/extmode` for External-mode tenants: proposal → prepare → confirm → commit on the host adapter; egress and provider budgets apply to every external send.
- **Local-only roles degrade** per `internal/providers/role.go`: router → `deterministic_routing`; embedder / reranker → reduced semantic recall / fused order (irrelevant when RAG is off); STT → `text_input_only`; TTS → `verified_text_only`; VAD → `manual_turn_taking`.

Check locality with:

```bash
alixai config check -config alixai.yaml          # prints layers: llm / stt / tts after "config ok"
alixai providers layers -config alixai.yaml       # the same three lines alone
```

`stt` / `tts` print `local (pack <id>)` only when `voice.active_pack` names an approved pack that provides that role. A LocalAI endpoint alone is `none`.

`config check` accepts setups 2 and 3, refuses External enabled with no controller route and no `inference.models` (`no text model available`), and refuses `routing.tts` with the roadmap message unless an adapter has called `RegisterExternalTTSAdapter`.

## Setup 1 — everything local (default)

Leave `external_providers` off (or omit it). Pin local models as usual. See [Local machine](local-machine.md) and [External mode](../external-mode.md) section 10 to return from External.

## Setup 2 — STT local + LLM external

LocalAI answers transcription for the active STT pack. Do not pin a local controller. For a compose profile that runs LocalAI without an LLM, see [`deploy/container/localai/compose.stt-only.yaml`](../../../deploy/container/localai/compose.stt-only.yaml) (owned by another change in this wave).

```yaml
data_dir: ./data
identity:
  deployment_id: dep_light
  audience: alixai
server:
  listen: 127.0.0.1:7470
localai:
  endpoint: http://127.0.0.1:8080   # STT runtime; no controller model required
voice:
  enabled: true
  active_pack: en-stt               # pack id that provides STT (install + activate first)
inference:
  models_dir: ./models
  models: []                         # intentional: text roles are external
external_providers:
  enabled: true
  deployment_operators: ["app_console/usr_ops"]
  tenant_admins: ["app_console/ten_acme/usr_admin"]
  openai_base_url: https://api.openai.com
  # Prefer naming an env var or secret file — never a literal key in YAML:
  credentials:
    - provider: openai
      label: light
      env: ALIXAI_SECRET_OPENAI_KEY
      application: app_console
      tenant: ten_acme
  routing:
    controller:
      profiles: [pprf_...]           # id from `alixai providers register`
    realisation:
      profiles: [pprf_...]
```

After start: register and qualify the profile (or bind the env credential above), set egress and a budget, and set **deployment, application and tenant** mode to `external` (section above). Dictation uses local STT when that path is wired; chat uses the external controller. For browser OAuth on same-machine OpenAI (SIWC) or OpenRouter, see [External mode](../external-mode.md) section 3.

## Setup 3 — everything external (no LocalAI)

No LocalAI process, no voice-conversation mode. Dictation uses external STT when `routing.stt` names a profile with role `stt`, an active credential, an `audio_seconds_in` price, and the egress policy's **`audio` setting enabled** at every required level with `max_class` at least `dictation.audio_class` (default `internal`; off by default — without it external STT is refused). Every tenant that chats must still be switched to External mode.

```yaml
data_dir: ./data
identity:
  deployment_id: dep_light
  audience: alixai
server:
  listen: 127.0.0.1:7470
# no localai: section
dictation:
  max_seconds: 60
  audio_class: internal   # public | internal | confidential; egress audio.max_class must cover it
inference:
  models_dir: ./models
  models: []
external_providers:
  enabled: true
  deployment_operators: ["app_console/usr_ops"]
  tenant_admins: ["app_console/ten_acme/usr_admin"]
  openai_base_url: https://api.openai.com
  routing:
    controller:
      profiles: [pprf_...]
    realisation:
      profiles: [pprf_...]
    stt:                              # openai | google-stt | deepgram | elevenlabs profile
      profiles: [pprf_...]            # role stt; set audio_seconds_in; enable egress audio
    # tts: refused today (roadmap)
```

After register / qualify / credential / budget: enable egress `audio` at deployment, application, tenant and workspace via `PUT /v1/providers/egress-policy` with `audio: { "enabled": true, "max_class": "internal" }` (or higher if `dictation.audio_class` is `confidential`; section 4 of [External mode](../external-mode.md)), set mode External at every level, then `GET /v1/dictations/capabilities` should report `available: true` and `locality: external`.


## What still applies

Everything that applies in a full local or External deployment still applies here: context tokens, tenant scope, policy gateway, action prepare / confirm / commit, audit, data-egress guard, provider budgets, kill switch, signed application packages, adapters, sessions, verifier, and the readiness / `/readyz` scan. External mode remains off by default; nothing above turns it on for you.

## Related

- [Operating External and Hybrid mode](../external-mode.md)
- [Readiness](../readiness.md)
- [Operator commands](operator-commands.md)
- STT-only LocalAI compose: [`deploy/container/localai/compose.stt-only.yaml`](../../../deploy/container/localai/compose.stt-only.yaml)
