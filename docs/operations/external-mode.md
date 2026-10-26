# Operating External and Hybrid mode

Status: written in task X-24 and updated in tasks X-26 and X-27, 2026-10-08. Design: [External provider mode](../architecture/external-provider-mode.md). Every route below is in `schemas/api/paths/providers.yaml` or `oauth.yaml` and in `internal/gateway/routes_v1_providers.go`; nothing here is a command that does not exist.

## 0. Read this first: what external mode is

For a chat-only (or STT-local) deployment with **no local LLM**, see [Light External deployments](guides/light-external.md): per-layer choice through `external_providers.routing`, including setups with no LocalAI at all.

**External mode is optional, off by default, network-dependent and billed by the provider.**

- **It is off.** With `external_providers.enabled` false (the default) AlixAI behaves exactly as Local mode: no external service is built, no connection is opened, every `/v1/providers` and `/v1/oauth` route answers `unavailable`. A binary built with `-tags noexternal` contains no external adapter at all.
- **It needs the network.** When a request is routed to a provider, the permitted part of it leaves your machine and is processed by a third party. If the network or the provider is down, that request fails (or falls back to local only if a policy names that). Deterministic application work and local search do not depend on it.
- **It is billed by the provider, not by AlixAI.** Every external call spends money on the account behind the API credential. AlixAI meters it and can stop it (section 7), but the provider's invoice is the authority. A consumer chat subscription is not API access (section 9).
- **The provider's terms apply.** Retention, training use, region and price are the provider's and can change. AlixAI records them and quarantines a profile whose terms are stale (see `docs/policies/provider-terms.md`).
- **A model never decides.** An external model is an interchangeable text backend. It never executes a tool, owns a permission or commits anything; its output is checked like any untrusted input.

### What is wired in this release

| Piece | State |
| --- | --- |
| Operator API (modes, egress policy, budgets, usage export, kill switch, credentials, billing acknowledgement, OAuth start and callback) | built, `alixai serve` provides it when the section below is enabled; writes are also checked inside the service (a mode, a provider-wide budget, the usage of the whole application, the kill switch and the deployment and application policy need a deployment operator) |
| Credential service (encrypted file), egress guard, budgets, mode resolver, kill switch | built and used by that API |
| Provider adapters | OpenAI, Anthropic, Gemini (API key or OAuth), Meta, OpenRouter, xAI, Moonshot (Kimi) and **Mistral** (EU-default residency; GDPR-relevant) are built into the default binary. OAuth is off until `external_providers.oauth.providers.<id>` is enabled (presets: openai SIWC, gemini/google-stt, openrouter). A Gemini or OpenAI (SIWC) profile uses the API key or the OAuth access token of the bound credential, chosen per call from the kind of the reference (the shared OAuth engine refreshes tokens). Slash-bearing wire ids map through `meta_models` / `openrouter_models` / `xai_models` / `moonshot_models` / `mistral_models`. A manual key is validated against its own provider on submit; a key the provider rejects is refused and not stored |
| Provider profile lifecycle | **operator commands**, section 3a (`alixai providers ...`), offline against the data directory |
| Turn routing in `alixai serve` | the **controller** role is routed by `internal/dialogue/extmode` when the mode is External and `external_providers.routing` names profiles for it; under Hybrid the controller stays local. Everything else stays as it was |
| Hybrid realisation (`internal/modes/hybrid`) | **called** from the response step of the dialogue core (`dialogue.ResponseRealiser`, implemented by the provider selector `serve` builds the engine with). In Hybrid mode, when `routing.realisation` names profiles, the directive goes to Hybrid, which applies its fail-closed rules (egress policy, `hybrid_fact_class`, budget, verification) and answers from the provider, the local model or the template; model text is verified again in the core. In Local and External mode, with the kill switch engaged, or with nothing routed, the seam declines and the reply is written exactly as before. `hybrid_fact_class` empty keeps every Hybrid turn local |
| RAG answers | **External mode** answers a question from the excerpts the egress guard lets through (`allow_excerpt` sources of an allowed class) with the external answerer, over the same citation resolver as the local one, so every reference opens through `/v1/rag/citations`; a source that must stay local gives `no_supported_evidence` with reason `egress_restricted` and nothing is sent. **Local and Hybrid** answer locally, unchanged |
| Scheduled terms revalidation | runs: `providerreg.revalidate` is queued at start and every `revalidate_every` (default one hour) on the RAG stack's queue and pool, under the service identity `providerreg.revalidate`, class 6 (maintenance) |
| Budget sweep of stale reservations | runs: `providerbudget.sweep` is queued at start and every `budget_sweep_every` (default 15 minutes) for each application with a live profile, on the same queue and pool, under the service identity `providerbudget.sweep`, class 6 (maintenance); it marks timed-out reservations expired and drops their holds (a timed-out hold was already ignored, so the sweep changes no figure) |
| Multimodal / speech gate (`extmodal`) | built. With external mode on, routed `stt` / `tts` go through the gates (local first). External TTS needs attested verified text and prefers `characters_in` metering (ADR 0043). External STT adapters: OpenAI, Google STT v2, Deepgram, ElevenLabs, **Mistral (Voxtral)**. External TTS adapters: OpenAI, ElevenLabs, Google Cloud TTS, Amazon Polly, **Mistral (Voxtral TTS)** (`adapters/external/tts/...`). Raw audio leaves only when segmented, modality and raw-audio policy admit it, the data-egress guard allows the audio class under the egress policy's `audio` setting (**off by default**, section 4), and the provider budget reserves `audio_seconds_in`. Dictation: `POST /v1/sessions/{id}/dictations`. Spoken text is `user_message` at the session class |
| SDK helpers for these routes | in the SDKs (task X-14) |

## 1. Enabling it

External mode needs every item below. Missing one leaves the deployment local, which is the safe direction.

1. Set the deployment id, which `serve` requires for this section: `identity.deployment_id`.
2. Add the section to the configuration file (shape from `internal/cli/providers_external.go`):

   ```yaml
   external_providers:
     enabled: true
     deployment_operators: ["app_console/usr_ops"]          # "<application id>/<principal id>"
     tenant_admins: ["app_console/ten_acme/usr_admin"]      # "<application id>/<tenant id>/<principal id>"
     openai_base_url: https://api.openai.com  # where OpenAI is reached (default shown)
     routing:                                 # which profiles serve which role; a role not listed stays local
       controller:
         profiles: [pprf_...]                 # ids printed by `alixai providers register`, in order of preference
         local_fallback: [unavailable, timeout]
       realisation:
         profiles: [pprf_...]
     hybrid_fact_class: ""                    # public or internal lets Hybrid facts leave; empty keeps every turn local
     directive_class: ""                      # public|internal|confidential: External may realise from the directive; empty keeps the template
     revalidate_every: 1h
     budget_sweep_every: 15m
   ```

   Other keys: `anthropic_base_url`, `gemini_base_url`, `meta_base_url`, `meta_dialect` (`meta-model-api`, the default, or `openai-compatible` for a hosted Llama endpoint, which needs `meta_base_url`), `meta_api_prefix` and `meta_models` (maps the slash-free model id of a profile to the wire id, because a registry model id cannot contain `/`), plus `openrouter_base_url` / `xai_base_url` / `moonshot_base_url` / `mistral_base_url` and matching `*_models` maps. Every base URL is a bare origin. OAuth catalog and env/file credential refs: section 3.

   Operators are read from these two lists and from nothing a token says: a token claiming an admin role is as powerless as one claiming nothing, and with both lists empty nobody can call any route. A deployment operator is also a tenant administrator. Every entry names the application whose verified token the operator arrives with: each application's issuer chooses the subject (`sub`) it signs, so `usr_ops` of `app_console` and `usr_ops` of any other application are different principals, and only the listed one passes. An entry without the application (`usr_ops`, `ten_acme/usr_admin`, the form of earlier builds) is refused at start-up.
3. Check the file: `alixai config check -config file.yaml` (prints `config ok`; a malformed operator entry stops it).
4. Start: `alixai serve`. It creates `<data_dir>/external/` with `modes.json` (written atomically, owner only) and `secrets/` (created owner only by the credential service) holding `credentials.enc` (AES-256-GCM sealed) and `master.key`. Mode settings persist there across restarts.
5. Register and qualify a provider profile (section 3a), then set a mode, an egress policy and a budget (sections 2, 4 and 7).

The destination of every provider call must also be on the network allowlist (`internal/egress`), which is a separate gate from the data policy in section 4.

## 2. Modes

`GET /v1/providers/mode` shows `effective`, `configured`, `decided_by`, `kill_switch` and the three `levels`. `PUT /v1/providers/mode` with `{"level": "deployment|application|tenant", "mode": "local|hybrid|external"}` sets one level and appends an audit record (`providers.mode_changed`); if the record cannot be written the change is not applied. A tenant administrator may set the tenant level; deployment and application levels need a deployment operator.

- **Local** (default): no external processing.
- **Hybrid**: language realisation only. Identity, scope, routing, the controller and RAG stay local; only a verified dialogue directive may be sent for wording, with the local text or the deterministic template as fallback. By default nothing is classed as sendable (`FactClass` and `DirectiveClass` are empty), so a Hybrid deployment sends nothing until the operator names a class.
- **External**: the hosted model may serve the roles the profile and policy permit. By default a response directive (live capability facts) stays local and the deterministic template realises the reply. Set `external_providers.directive_class` to `public`, `internal` or `confidential` to let that directive leave at the named class; the tenant egress policy must still allow categories `dialogue_directive` and `deterministic_fact` at that class, or the template is used and the refusal is recorded. Provider table/chart JSON is accepted only when every number matches the directive's facts (`verify.RealiseRich`); otherwise the template (which may itself include a validated table/chart of host records) is shown.
- **The most restrictive level wins** (Local < Hybrid < External). A level with no setting counts as Local, so all three levels must opt in. The kill switch forces Local.

Switching mode changes no application data and no integration contract.

## 3a. Managing a provider profile (operator commands)

A profile is the record of one provider and model: endpoint, capabilities, terms, price, billing notice and where it may be used. The commands work offline on the data directory (no running server needed), need `external_providers.enabled` and refuse unless `<-application>/<-operator>` is listed in `deployment_operators`. Every command takes `-config`, `-operator ID`, `-application APP` and, for `qualify` and `release`, `-tenant T`, the tenant whose credential the profile is bound to (the credential is checked in that tenant).

| Command | What it does |
| --- | --- |
| `providers register (-template anthropic\|mistral \| -profile-file F) [-permit-tenant T]... [-reviewed-on D] [-terms-hash H] [-pricing-file F] [-billing-notice-file F]` | stores a new candidate profile and prints it (with its `pprf_` id). Shipped templates: `anthropic`, `mistral` (EU). The price table and the billing notice are set here. Change either later with the two commands below, without a new profile |
| `providers set-pricing -profile ID -pricing-file F` | replaces the price table (same id, credential, acknowledgement, report and lifecycle). A later call is priced from the new table; a call already reserved keeps the estimate it was admitted with. A quarantined profile stays quarantined; a retired one is refused |
| `providers set-billing-notice -profile ID -billing-notice-file F` | replaces the billing notice. A changed notice drops the acknowledgement and takes a qualified profile back to candidate until it is acknowledged (`ack-billing`) and qualified again; the same notice is a no-op; a notice that says a subscription is API access is refused |
| `providers list` | prints the application's profiles |
| `providers ack-billing -profile ID -notice-version V -confirm-not-api-access -confirm-separate-billing` | the billing acknowledgement (section 9), as an operator |
| `providers review -profile ID -terms-hash sha256:... [-reviewed-on D]` | records that you read the provider's terms; ends a stale, terms-changed or held quarantine |
| `providers conformance -profile ID -report F` | records a conformance report from `tools/providerconf` |
| `providers qualify -profile ID -tenant T` | qualifies the profile: needs an active credential, the acknowledgement, a passing report and a fresh review |
| `providers release -profile ID [-reviewed-on D] [-region R]...` | ends a region-changed or excessive-permissions quarantine |
| `providers hold` / `providers retire -profile ID` | quarantines by hand / retires for good |

End to end: `register` -> `PUT /v1/providers/{provider}/credential` (the key is bound to the profile when it has none; a profile binds exactly ONE credential reference and a reference belongs to one tenant, so another tenant needs its own profile; a second key never rebinds a profile that already has one) -> `ack-billing` -> `conformance` -> `qualify` -> put the id under `routing` and restart. Without a registered profile the credential route answers `no_profile`.

### Per-principal catalog (ADR 0041)

After a profile is qualified, mark it for end-user pickers (no credential on the catalog entry; each principal brings their own):

```text
alixai providers catalog-add -operator ID -application APP -profile pprf_… [-display-name N] [-terms-summary T] [-billing-notice-version V] [-oauth-available]
alixai providers catalog-list -operator ID -application APP
alixai providers catalog-remove -operator ID -application APP -profile pprf_…
```

Principals then use `/v1/me/…` (integrator guide). Keys are stored under the reserved tenant id `alixai-principal-credential` with `owner_principal_id`; that id is never a request or RAG scope. Erasing a principal (`alixai retention erase -principal`) revokes their reserved-bucket credentials and deletes their bindings.

## 3. Connecting a provider

Routes are for operators only. A `GET /v1/providers` lists the application's registered profiles with their connection state (never a key, a fingerprint or a reference id).

**Billing acknowledgement first (section 9).** `POST /v1/providers/{provider}/billing-notice/ack` with `{"notice_version": "...", "consumer_subscription_is_not_api_access": true, "separate_api_billing_understood": true}`. Both confirmations must be true and the version must be the profile's current one; a changed notice invalidates the acknowledgement.

**OAuth is preferred where the provider officially offers it** (ADR [0038](../decisions/0038-popup-oauth-and-provider-catalog.md)). Enable a catalog entry under `external_providers.oauth` (off by default; unknown provider keys are refused):

```yaml
external_providers:
  oauth:
    popup_origins: ["https://console.example.com"]   # exact origins for presentation popup; empty refuses popup
    providers:
      openai:      { enabled: true, loopback: true }                 # SIWC: loopback only; no client_id needed
      gemini:      { enabled: true, client_id: "...", client_secret_ref: "cred_...", redirect_uris: ["https://…/cb"], loopback: true }
      google-stt:  { enabled: true, client_id: "..." }               # same Google client shape; cloud-platform scope
      openrouter:  { enabled: true, loopback: true }
```

`POST /v1/oauth/{provider}/start` returns the authorization URL and consent. With `presentation: "popup"` and `opener_origin` on the allowlist, the response includes `flow_id` / `poll_interval_ms`; poll `GET /v1/oauth/flows/{flow_id}` (cancel with `DELETE`). With `callback: "host"` the host relays `POST /v1/oauth/callback`. With `callback: "loopback"` send redirect_uri `http://127.0.0.1/oauth/loopback`; AlixAI binds an ephemeral `127.0.0.1` listener, completes the exchange, and serves a minimal callback page (postMessage to the exact opener origin, or a close message for the CLI). Tokens and issued keys go only to the credential service.

| Provider | How to connect | Operator must register |
| --- | --- | --- |
| OpenAI (SIWC) | Browser login, **loopback only** (same-machine). Plan usage is the signed-in person's ChatGPT plan; the credential resolves only for that application+principal (`--for` on CLI login). Remote deployments: manual key. | Nothing (dynamic client registration). Persist `ext_agent_host_id` is automatic under `<data_dir>/external/siwc_host_id`. |
| Gemini / google-stt | Browser login (loopback Desktop client or host https Web client). Gemini OAuth uses `generative-language.retriever` only; google-stt uses `cloud-platform` (whole Cloud project — prefer a dedicated project). | Your own Google OAuth client id (+ secret for Web) in the catalog entry. |
| OpenRouter | Browser PKCE → issued API key (`oauth_issued_key`, no refresh). Prefer `127.0.0.1` (AlixAI refuses `localhost`). | Nothing beyond enabling the catalog entry. |
| Anthropic, xAI, Moonshot, Mistral | **No** official third-party OAuth; use a manual key (or reach their models through OpenRouter where available). | — |

Operator CLI (offline against the data directory, same pattern as other `alixai providers` commands):

- `alixai providers login <openai|gemini|google-stt|openrouter> -operator … -application …` — opens the system browser on a loopback flow (`--no-browser` prints the URL only). For **openai** (SIWC), `--for <application>/<principal>` is required and must match `-application`: that principal owns the ChatGPT plan binding. Other providers ignore `--for`.
- `alixai providers credential add-ref -provider … -label … (-env ALIXAI_SECRET_… \| -file name)` — registers an env or secret-directory file reference (value re-read on every use).

**Three ways to add an API key** (all end in `internal/credentials`; never put a literal key in YAML — `config check` refuses members that look like one):

1. Settings UI → `PUT /v1/providers/{provider}/credential` (relayed by the host server).
2. Env / mounted file via config: `external_providers.credentials: [{provider, label, env: ALIXAI_SECRET_ANTHROPIC_KEY, application, tenant}]` or `{…, file: anthropic.key, application, tenant}` (file is a name under `<data_dir>/external/secrets/`). Bound at serve start when application and tenant are set.
3. CLI `alixai providers credential add-ref` as above.

**Manual key (protected fallback).** `PUT /v1/providers/{provider}/credential` with `{"key": "...", "label": "...", "expires_at": "...", "scopes": []}`, sent only from an authenticated server-side settings flow, never from browser code. AlixAI validates the key with one minimally scoped request (the model list), stores it in the encrypted file and answers with the connection status only; afterwards only a masked fingerprint exists. A manual key can grant broader access than OAuth scopes, and the status says so (`broader_than_oauth_scopes`). Rotation is a new submit; revocation is the disconnect below. A key is never in a prompt, a log, a URL or an error.

**Disconnect.** `DELETE /v1/providers/{provider}/connection` revokes the credential of the caller's tenant (at the provider for OAuth, locally in every case). It takes effect on the next call with no restart. An invalid, expired or revoked credential fails closed; it never causes a switch to another provider.

## 4. Egress policy: what may leave

This is the DATA gate. A level with no policy is disabled; a RAG source with no policy is `local_only`.

- `GET /v1/providers/egress-policy?level=...[&source_id=...]` and `PUT /v1/providers/egress-policy` (body: `level`, `source_id` for the source level, `expected_revision`, `enabled`, `source_mode`, `allowed_providers`, `allowed_models`, `allowed_classes`, `allowed_regions`, and optionally `files` and `audio`). Levels: deployment, application, tenant, workspace, source.
- **Most restrictive wins** down the chain. Allowlists must list the provider and model at every level; an empty list allows nothing; an unknown region residency is denied.
- **Per-source choice (`source_mode`):** `deny` (excluded entirely), `local_only` (default), `allow_metadata` (titles and ids only), `allow_excerpt` (bounded authorised excerpts). A package can only restrict: its `egress_mode` and `external.forbid` never grant anything.
- **Uploaded files (`files`, off by default):** `{ "enabled": true, "max_class": "public" | "internal" | "confidential" }` at the deployment, application, tenant and workspace levels (not at source level: an upload has no source). An excerpt of an uploaded file leaves only when all four levels enable it, only up to the lowest `max_class`, only to the providers, models and regions the chain allows and, for public and internal, only when `allowed_classes` lists the class too. Its decision records a `files` check. A `max_class` above public needs `dataegress.approve`.
- **Raw audio (`audio`, off by default):** the same shape and rules as `files`, for the raw audio an external speech recogniser would receive (`audio_content`). A policy that lets text leave does not let audio leave; audio whose session states no class is unclassified and never leaves. Its decision records an `audio` check, and the call is then reserved on the provider budget against that decision.
- **Classes:** public, internal, confidential, restricted. Restricted is never eligible. Credentials, secrets and raw application database content never leave under any policy. An unknown class is treated as restricted.
- **Who may write:** deployment and application levels need a deployment operator; deployment-level policy needs nothing more. Application, tenant, workspace and source writes also need the context token to carry `dataegress.<level>.write`, and a policy that lets internal or confidential data leave also needs `dataegress.approve`.
- Every decision is an audit record without content; a denied request is `permission_denied` and the model cannot override it.

Before a request is made a host can show the user what would leave: `GET /v1/providers/{provider}/disclosure?role=&source=&estimated_toks_in=&max_output_toks=` returns the provider and model, the data categories, whether RAG excerpts are included, the stated retention and training terms (`unknown` stays `unknown`), an upper-bound cost, the budget status and whether it will fail or fall back.

### Routing speech-to-text (`stt`)

Put a profile id under `external_providers.routing.stt` (same shape as `controller`). The profile must list role `stt`, declare media handling for audio, state retention, and set an `audio_seconds_in` price (no price → external STT is refused as not estimable). Supported provider ids: `openai` (POST `/v1/audio/transcriptions`), `google-stt` (Speech-to-Text v2 Recognize / Chirp 3), `deepgram` (POST `/v1/listen`), `elevenlabs` (POST `/v1/speech-to-text`), `mistral` (POST `/v1/audio/transcriptions`, Voxtral). Credentials use the existing manual key flow (Google may use the `google-stt` OAuth connection once connected).

**Audio egress for dictation.** Raw audio stays home until the egress policy's **`audio` setting** is enabled at every required level (deployment, application, tenant, workspace); it stays **off by default**. Use `PUT /v1/providers/egress-policy` with body fields `level`, `expected_revision`, and `audio: { "enabled": true, "max_class": "public" | "internal" | "confidential" }` (same shape as `files`, section 4). `max_class` must be at least the class dictation attaches to the clip. That class is `dictation.audio_class` in config (`public` | `internal` | `confidential`, default `internal`; override `ALIXAI_DICTATION_AUDIO_CLASS`). Example:

```yaml
dictation:
  max_seconds: 60
  audio_class: internal   # must be ≤ every level's audio.max_class
```

Dictation marks the clip user-segmented; voice conversation still needs a VAD cut. See [light-external](guides/light-external.md) for the all-external preset.

### Routing text-to-speech (`tts`)

Put a profile id under `external_providers.routing.tts` (same shape as `stt`). The profile must list role `tts`. Prefer a `characters_in` price (ADR 0043). Estimation rule: `characters_in` when priced; else `toks_in` when priced (legacy); else refused as not estimable (same as STT without `audio_seconds_in`). A profile that prices only `audio_seconds_out` is also refused (length unknown before the call). Supported provider ids: `openai`, `elevenlabs`, `google-tts`, `polly`, `mistral` (POST `/v1/audio/speech`, Voxtral TTS). Credentials: manual key (Polly as `ACCESS_KEY_ID:SECRET[:SESSION_TOKEN]`); Google may use the `google-tts` OAuth connection or a key. External synthesis speaks only attested verified reply text; local fallback runs only when the role policy names it. A principal may bind role `tts` (and optional `voice` / `language`) via `PUT /v1/me/llm/tts`. Each role resolves independently: the same provider or different ones; one credential per (principal or tenant, provider).

### Provider × role matrix (external adapters in this release)

Which hosted adapters exist for each speech/chat role (✓ = wired; — = not offered by that product or not shipped). Controllers and realisation also use Meta, OpenRouter, xAI and Moonshot (chat only).

| Provider id | Chat (controller / realisation) | STT | TTS |
| --- | --- | --- | --- |
| `openai` | ✓ | ✓ | ✓ |
| `anthropic` | ✓ | — | — |
| `gemini` | ✓ | — | — |
| `meta` | ✓ | — | — |
| `openrouter` | ✓ | — | — |
| `xai` | ✓ | — | — |
| `moonshot` | ✓ | — | — |
| `mistral` | ✓ | ✓ | ✓ |
| `google-stt` | — | ✓ | — |
| `deepgram` | — | ✓ | — |
| `elevenlabs` | — | ✓ | ✓ |
| `google-tts` | — | — | ✓ |
| `polly` | — | — | ✓ |

## 5. Application packages

No package change is needed to run external mode. A package may add `external.forbid: true` in its manifest (nothing of that application is ever sent), and per RAG source `egress_mode` and `classification`, and per field `classification` and `local_only`. `alixai audit run` lists what could leave. See `docs/integration/external-mode.md`.

## 6. Provider terms

A profile records the provider's retention, training use, region and price at the time of review, and goes stale after `review_interval_days` (30 at most). A stale or changed profile is quarantined and not used. See `docs/policies/provider-terms.md`. To produce the conformance report a profile needs: `go run ./tools/providerconf run` (recorded fixtures, no network) and `go run ./tools/providerconf check report.json`. A live run spends real money and is a human-gated step.

## 7. Budgets, usage and the kill switch

**Budgets.** `GET /v1/providers/budgets` returns budgets and their states; `PUT /v1/providers/budgets` creates or changes one (subject provider, tenant, workspace, principal, or `principal_application`; channel `interactive` or `background`; integer micro-unit `daily_micro_units` and `monthly_micro_units`, both required, plus `warn_at_percent`). Kind `principal_application` is application-wide for one principal (application and principal only): one hard stop across every tenant that principal acts in. There is no unlimited budget. **A call with no applicable budget is refused (`no_budget`)**, and an exhausted budget stops external calls with `resource_exhausted` and chooses no other provider. Every applicable budget admits the call; the strictest stop wins. A provider-wide budget needs a deployment operator. Windows are UTC days and months. Estimates are upper bounds; the provider's invoice is the final cost.

**Self-service.** `GET /v1/me/budget` returns the caller's own principal and `principal_application` budgets with their states. `GET /v1/me/usage?from=&to=&tenant_id=&cursor=&limit=` returns the caller's own usage across tenants of the application (or one tenant), grouped by provider, role and UTC day, with meter units and charged micro-units. Never another principal's rows. Operators still set budgets with `PUT /v1/providers/budgets`.

**Usage.** `GET /v1/providers/usage/export?from=&to=&provider=&channel=&format=json|csv[&scope=application][&principal_id=]` (`scope=application` needs a deployment operator; `principal_id` narrows to one principal). It holds counts and costs, never prompts or answers.

**Kill switch (deployment operator).** `PUT /v1/providers/kill-switch` with `{"engaged": true, "reason": "operator_emergency"}` stops all external processing for the deployment at once, whatever any policy says, and survives a restart; `{"engaged": false}` releases it. Releasing enables nothing by itself: the mode and policies must still allow external use. `GET /v1/providers/kill-switch` reads it. The reason is a fixed lower snake_case code.

## 8. Failure behaviour

An invalid credential, a stale or quarantined profile, denied egress and an exhausted budget each end the request with their own typed error and never select another provider. Only a fallback that the profile or policy names, for `unavailable`, `timeout`, `rate_limited` or `unsupported_modality`, is followed, once, and recorded. If a network failure happens after bytes may have reached the provider on a request that could commit something, the outcome is `uncertain_outcome`: it is not retried and must be reconciled first.

## 9. Billing boundary and data handling: text shown at setup

The text below is the recommended wording. AlixAI does not ship a notice: the text a host shows is the `billing_notice` recorded on the provider profile by the operator, with a version, and the acknowledgement route above records who confirmed which version and a digest of its text. Both confirmations are mandatory before a profile can be qualified. Provider facts were read on 2026-10-03 through a summarising tool and are not verified by AlixAI (ADR 0021); check the provider's own pages before you rely on them.

**For every provider.** AlixAI uses the provider's official API with API credentials. It does not use, scrape or automate a consumer chat website, and it does not reuse a consumer subscription session. You may already pay for a consumer subscription and still need separate API access, billed separately. Every request costs money on the API account.

| Provider | Billing-boundary text |
| --- | --- |
| OpenAI | A ChatGPT subscription does not include separate API-key billing. For same-machine deployments, Sign in with ChatGPT (SIWC) may use the signed-in person's own ChatGPT plan for eligible Responses API calls (`store: false`); that connection is bound to that principal only. Remote or multi-user hosts use a manual OpenAI API key, billed separately. |
| Anthropic | A Claude subscription does not include API access. Anthropic Console / API usage is a separate product with its own billing and keys. |
| Google Gemini | Gemini API access uses API credentials tied to a Google Cloud project; quota and billing follow that project's configuration, not a consumer Gemini or Google One plan. |
| Meta | Consumer Meta AI access must not be assumed to grant Meta Model API or hosted Llama access. Check that your endpoint and account actually offer the API you configure. |

**Data-handling disclosure** (show with the billing text):

- When external mode is on and a request is routed to the provider, the data categories your policy allows (for example the user's message, conversation history, verified facts, and RAG excerpts only for sources set to `allow_excerpt`) are sent to the provider over the network and processed outside your deployment.
- Credentials, secrets, filesystem paths, database handles, raw application database content and restricted data are never sent. Content in `local_only` or `deny` sources is never sent.
- The provider's retention, training-use and region terms apply to what is sent. They are recorded on the profile; where the provider has not stated them AlixAI shows `unknown`. AlixAI sends `store: false` to OpenAI to avoid provider-side storage of responses, but it cannot change the provider's own abuse-monitoring retention.
- You are billed by the provider for every request. Budgets here limit spend but do not replace the provider's own limits.
- You can stop everything at any time with the kill switch, and remove external mode entirely (section 10).

## 10. Returning to a fully local deployment

This changes no application data, no package and no integration contract; Local mode is the baseline and is unchanged.

1. **Stop traffic now:** `PUT /v1/providers/kill-switch` `{"engaged": true}`. From this call every external decision is denied.
2. **Set every mode level back to Local:** `PUT /v1/providers/mode` for `tenant`, `application` and `deployment` with `"mode": "local"`. There is no route that unsets a level; `local` is the value. (A level with no setting also counts as Local.)
3. **Revoke every credential:** `DELETE /v1/providers/{provider}/connection` for each provider, as an administrator of each tenant that connected one. The route is per tenant; there is no single call that revokes a whole deployment.
4. **Switch the feature off:** set `external_providers.enabled: false` (or delete the section) and restart `alixai serve`; or deploy a binary built with `go build -tags noexternal ./cmd/alixai`, which has no external adapter.
5. **Confirm:** `GET /v1/providers` now answers `unavailable`; `alixai config check` is clean; no outbound connection to a provider exists.
6. **Optionally remove the secrets:** delete `<data_dir>/external/secrets/` and `modes.json`. Credential, profile, policy and usage rows that external mode wrote stay in the store, unread and inert; they are not removed by these steps, and no application table is touched.

Packages do not change: `external.forbid`, `egress_mode` and `classification` members are ignored while external mode is off.
