# pkg/sdk-go (Go SDK)

Import path `github.com/buxee/alix-ai/pkg/sdk-go` (package `sdkgo`; the module path is provisional until a public repository exists). The Go client of the
AlixAI public API (`schemas/api/openapi.yaml`). Documents that have a JSON Schema are the generated types of `pkg/contracts/gen`. The SDK depends on the standard
library and one small WebSocket library (`github.com/coder/websocket`). A fake AlixAI server for the tests of a host application is in `pkg/sdk-go/sdktest`, and an
AG-UI proxy for CopilotKit and AG-UI frontends is in `pkg/sdk-go/agui`.

**Status: 1.3.0.** The contracts this SDK speaks are frozen for v1 (`schemas/_frozen/v1`, checked by `tools/compat`).
`Version` in this package is `"1.3.0"`. Compatibility and support rules are in
[compatibility](../../docs/policies/compatibility.md) and [migration and support](../../docs/policies/migration-and-support.md). The version this SDK is tested against
is the core in the same repository commit. The changelog is the last section of this file.

## What is in it, and what is not

| Module | State |
| --- | --- |
| Core: sessions, streamed and JSON turns, resumable streams, confirmations, actions, applications, capabilities, health | Complete. `NewClient` |
| Resume from a cursor, WebSocket transport | Complete: `StreamTurnEvents` with `WithCursor`, automatic resume **on by default** (`MaxResumes`, default 3), `WithWebSocket()` per call. This differs from the TypeScript SDK, where reconnecting is off unless asked for |
| Attachments on a turn (up to four uploaded file ids) | The turn member only (`WithAttachments`). **No upload, download or preview** |
| Conversation helper, history, privacy and settings | **Not in this SDK** |
| RAG client (sources, search, grounded answers with citations) | Complete: `client.RAG()` |
| Voice conversation (WebSocket) | **Not in this SDK** (TypeScript only). **Full-duplex voice is not claimed** by any SDK |
| Dictation (record-to-text) | Complete (1.1.0): `client.Dictations().Capabilities` / `Create` |
| Per-principal LLM account (ADR 0041) | Complete (1.2.0): `client.Me()` (Catalog, LLM, PutRole/DeleteRole, SubmitCredential, billing ack, OAuth start). SubmitCredential is **host-server only** |
| Long-term memory (ADR 0042) | Complete (1.2.0): `client.Memory()` (List/Create/Patch/Delete/Confirm/Reject) |
| Downloadable exports | Complete (1.2.0): `client.Exports().Create` (`POST /v1/sessions/{session_id}/exports`, capability `exports.create`) |
| Providers, modes, budgets (External provider mode) | Complete for the providers and OAuth operations (X-14): `client.Providers()`. **Opt-in and off by default**; a deployment without it answers `unavailable` |
| Decision status | Read-only (D-10): `client.Decision().Status`. Advisory information, never an authorisation |
| AG-UI proxy for CopilotKit and AG-UI frontends | Complete (M3-27): package `agui`. CopilotKit is not a dependency |
| Operator-only decision configuration (`GET`/`PUT /v1/decision/config`) | Not part of the SDK |
| Lite profile | Uncertified. Nothing in this SDK depends on it |

The same feature-by-SDK table, for all three SDKs, is in [the integration guides](../../docs/integration/guides/README.md).

## Quick start

Every code sample in this README is copied from a test that runs, and names it; where a block is cut short it says so. The tests use the fake server `sdktest.Fake`. Against a real AlixAI you build the
client with `sdkgo.NewClient(sdkgo.ClientOptions{BaseURL, GetContextToken})`, where `GetContextToken` calls your backend.

From `client_test.go` (`newHarness`):

```go
h := &harness{t: t, fake: sdktest.New()}
t.Cleanup(h.fake.Close)
h.claims = sdktest.Claims{CapabilityScopes: []string{"demo.lookup", "demo.book", "demo.close_account"}}
client, err := h.fake.Client(h.claims, opts...)
```

A context token is **single use** (ADR 0013): `GetContextToken` is called once per request and per event-stream open, with a `TokenRequest` that says which
operation, session or preparation it is for. The SDK never mints, caches or retries with a token, and credentials live in memory for the length of one call.
Redirects are an error. `BaseURL` must be https (plain http is accepted for loopback only; `AllowInsecureHTTP` overrides).

## Turns, resume and cursors

`SendTurn` sends immediately and returns a `*TurnStream`: iterate with `Next`/`Event`/`Err`, with `All()` (a range-over-func iterator, the failure is the last element)
or read it whole with `Result()`. The stream is checked as it arrives and fails closed: one turn id, `seq` from 0 without gaps, `done` last. A stream that cannot be
resumed is an `uncertain_outcome`: read the pending state before sending anything again. A turn that ended in a typed failure is delivered as events; a turn that
could not run at all returns the error before any event. From `client_test.go` (`TestStreamReconnect`):

```go
h.fake.DropNext(2)
stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go")))
result := must[*sdkgo.TurnResult](t, "result")(stream.Result())
```

```go
for name, opts := range map[string][]sdkgo.CallOption{
	"sse": {sdkgo.WithCursor(2)}, "websocket": {sdkgo.WithCursor(2), sdkgo.WithWebSocket()},
} {
	rest := must[*sdkgo.TurnResult](t, name)(must[*sdkgo.TurnStream](t, name)(h.client.StreamTurnEvents(ctx, "ses-1", sent.TurnID, opts...)).Result())
	if got := seqs(t, rest.Events); len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Fatalf("%s: the events after seq 2 are 3 to 5, got %v", name, got)
	}
}
```

The resume only reads: the turn is never run again (the test counts one `sendTurn`) and the confirmation secret is never part of it. `MaxResumes` below zero turns the
automatic resume off.

File ids go on a turn with `WithAttachments`, checked before a token is spent (`attachments_test.go`, `TestSendTurnWithAttachments`):

```go
stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("read these").WithLanguage("en").WithAttachments(second, first)))
```

## Confirmations

When a turn prepares an action that needs the user's explicit confirmation, `done` says `awaiting_confirmation` and the **single-use confirmation secret** is not in any
event: the SDK reads it from the `X-AlixAI-Confirmation-Token` response header and returns it as `TurnResult.Confirmation`. Show it to the user's client only and present
it once. Text can never confirm anything. A level 3 action cannot be confirmed through the API. From `client_test.go` (`TestConfirmFlow`):

```go
stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("book me in")))
result := must[*sdkgo.TurnResult](t, "result")(stream.Result())
```

```go
outcome := must[*sdkgo.ActionOutcome](t, "confirm")(h.client.ConfirmAction(ctx, grant.PreparationID, grant.Token))
```

```go
_, err = h.client.ConfirmAction(ctx, grant.PreparationID, grant.Token)
api := hasCode(t, "a second confirmation", err, contracts.ErrReplayed)
```

```go
rejected := must[*actions.PreparedAction](t, "reject")(h.client.RejectAction(ctx, declined.PreparationID))
```

```go
confirmed := must[*sdkgo.TurnResult](t, "confirm turn")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.ConfirmTurn(viaTurn.PreparationID, viaTurn.Token)))
```

## Errors

Every failure is an `*APIError` (`errors.As`) carrying the server's error document in `Err` (`Code()`, `Err.Details`, `Err.Retryable`), `Status`, `RequestID`, `RetryAfter` and, for a refused or
uncertain commit, the `Receipt` and `Preparation`. `contracts.HasCode(err, code)` and `IsRetryable(err)` test it. A request that may have been processed and whose outcome is unknown is `uncertain_outcome`, never
retryable. **The SDK retries nothing** except resuming a dropped read stream.

## RAG

`client.RAG()` has sources (`RegisterSource`, `ListSources`, `GetSource`, `RemoveSource`, `SyncSource`, `RebuildSource`, `ReauditSources`, generations), `Search`, `Answer` and
`ResolveCitation`. An answer is grounded with a citation for every claim, or `no_supported_evidence`; never an uncited answer (`sdkgo.Answered`). From `client_test.go`
(`TestRAGAnswerWithCitations`):

```go
r := h.client.RAG()
source := must[*sdkgo.Source](t, "register")(r.RegisterSource(ctx, "clinic-policies"))
answer := must[*rag.AnswerResult](t, "answer")(r.Answer(ctx, rag.AnswerRequest{Question: "Can I cancel an appointment free of charge?"}))
grounded, ok := sdkgo.Answered(*answer)
resolved := must[*sdkgo.CitationResolution](t, "resolve")(r.ResolveCitation(ctx, string(file.FileID)))
```

## Providers, modes, budgets (External provider mode)

External provider mode is **opt-in and off by default**: `local` is the effective mode everywhere until an operator changes it. The area is operator-only: modes, egress policy and
disclosure, budgets in integer micro-units, usage export, the kill switch, credentials (a `Secret` prints as a placeholder everywhere and is never echoed) and the browser OAuth
flow. An external model is an interchangeable inference backend only; it never executes a tool or owns permissions. From `providers_test.go` (`TestProvidersModeAndKillSwitch`):

```go
p := h.client.Providers()
mode := must[*sdkgo.ProviderMode](t, "mode")(p.GetMode(ctx))
must[*sdkgo.ProviderMode](t, "deployment")(p.SetMode(ctx, sdkgo.ModeLevelDeployment, pv.DeploymentModeExternal))
mode = must[*sdkgo.ProviderMode](t, "tenant")(p.SetMode(ctx, sdkgo.ModeLevelTenant, pv.DeploymentModeHybrid))
ks := must[*sdkgo.KillSwitch](t, "engage")(p.SetKillSwitch(ctx, true, "incident_42"))
```

## Decision status

`client.Decision().Status(ctx)` reads the status of the decision layer: provider, model, mode and fallback counters. The decision layer is optional, bounded,
non-generative and non-authoritative: it scores candidates that were already authorised and never authorises, validates, prepares, confirms or commits anything, and with it
off or failing the controller produces the same deterministic outcomes. The status is advisory operational information (it needs the capability
`sdkgo.DecisionStatusCapability`, `decision.status.read`), and no member of it is permission to do anything. From `decision_test.go`:

```go
h.fake.SetDecisionStatus(sdktest.DecisionStatusFixture(decision.DecisionStatusModeAdvisory, 7,
	map[decision.FallbackReason]int64{decision.FallbackReasonLowConfidence: 2, decision.FallbackReasonTimeout: 1}))
got := must[*decision.DecisionStatus](t, "status")(h.client.Decision().Status(context.Background()))
```

## AG-UI proxy (CopilotKit and AG-UI frontends)

Package `agui` serves a CopilotKit or AG-UI frontend from your backend's own origin, with no CopilotKit runtime in the middle: a `net/http` handler that validates the thread
and run ids, asks your callback for a fresh single-use context token (session = thread), forwards the run to `POST /v1/agui` and streams the answer back one frame at a time.
AlixAI stays the authority layer. The confirmation secret stays server-side in a `SecretStore` and is attached only to a resume that explicitly approves that preparation; the
browser never holds it. The package imports no CopilotKit or AG-UI library and contacts nothing but the AlixAI service you name (no cloud, no telemetry). The published version
matrix is [docs/integration/copilotkit-compatibility.md](../../docs/integration/copilotkit-compatibility.md); the quickstart for a Go host is in
[the CopilotKit guide](../../docs/integration/guides/copilotkit.md). From `agui/proxy_test.go` (`newRig`; the token callback body is abridged to a comment):

```go
p, err := NewProxy(Options{
	BaseURL: gw.srv.URL,
	GetContextToken: func(ctx context.Context, req sdkgo.TokenRequest) (string, error) {
		// ...authenticate the host's user, check they own req.SessionID, return a fresh single-use token
	},
	Secrets: r.store,
})
r.front = httptest.NewServer(http.StripPrefix("/agui", p.Handler()))
```

## Testing your application: `sdktest`

`sdktest.New()` is a fake AlixAI over `httptest`. It speaks the operations of `sdkgo.Operations()` as the OpenAPI document defines them and enforces what a host cares about: single-use
tokens (`replayed`), `scope_violation` for a path that names another session, preparation or application than the token, a confirmation secret used up by its first presentation, an
application that is not active serving nothing, and turn events readable again from a cursor. Script a turn with `OnTurn` and the plans `Reply`, `PrepareAction`, `FailTurn`,
`RejectTurn`; inject faults with `FailNext`, `DropNext`, `SetReady`, `SetApplicationStatus`; read what happened with `Requests`/`RequestsOf`. It runs no model, keeps no key and
reaches no application; none of the controller models is activatable yet, so a live conversation needs a model the operator has qualified. `go test ./pkg/sdk-go/...` runs the SDK
suite; `contract_test.go` and `spec_test.go` hold it to the OpenAPI document.

## Changelog

### 1.3.0 — 2026-10-27

**Added.** Principal LLM role `tts` and optional `voice` / `language` on `Me().PutRole` (ADR 0043); meter unit `characters_in`; `Me().Usage()` / `Me().Budget()`; optional `PrincipalID` on usage export; me OAuth callback/flows; ops erasure (tenant vs application scope). Core: external TTS (OpenAI, ElevenLabs, Google, Polly, Mistral) and per-role provider choice; per-user usage and application-wide principal budgets; browser-URL hardening for CLI OAuth; light External hardware guide; opt-in `external_providers.directive_class` with verified table/chart blocks and conversation-contract `presentation.chart` (ADR 0044); outbound `credential_ref` for OpenAPI, MCP and RAG api (`application_credentials`, optional RAG `principal_id`); Mistral chat/STT/TTS; kit fullscreen z-index; npm publish readiness. Dev cores report `1.3.0-dev`.

### 1.2.0 — 2026-10-26

**Added.** Per-principal LLM account (`client.Me()`) and long-term memory (`client.Memory()`) for ADR 0041 / 0042; downloadable exports (`client.Exports().Create`) from verified turn blocks; fake-server routes in `sdktest`. `Me().SubmitCredential` is host-server only — never call it from browser code. Upgrade before a core that emits `memory_proposal` turn events. Dev cores report `1.2.0-dev`.

### 1.1.0 — 2026-10-26

**Added.** Generated contract types for popup OAuth (`flow_id` polling, `oauth-popup-result` postMessage shape, `oauth_issued_key` credential kind) and for dictation (`TranscriptionCapabilities`, `DictationTranscript`). Core serve wires `TranscriptionService` for `GET /v1/dictations/capabilities` and `POST /v1/sessions/{id}/dictations` (local or external STT; body cap from `max_seconds`). Dictation HTTP client: `Dictations().Capabilities` and `Dictations().Create` (record-to-text; not the voice WebSocket). Providers popup OAuth: `GetOAuthFlow` / `CancelOAuthFlow` and optional `StartOAuth` members `presentation`, `callback`, `opener_origin`. Dev cores report `1.1.0-dev` so packages with `alixai_core` `[1.0.0, 2.0.0)` load under source and release builds; light External chat-assistant guide and `examples/packages/valid-help-assistant`.

### 1.0.0

First public release. Tested against the core in the same repository commit.

**Stable surface (what 1.0 freezes).** The wire documents of `schemas/` behind `pkg/contracts/gen` (frozen for v1 in `schemas/_frozen/v1`, M6-01); the operations of `Operations()`, checked
against `schemas/api/openapi.yaml` by `spec_test.go` and `contract_test.go`; the `contracts.ErrCode` strings and their retry classes.

**Added.**

- Client, sessions, streamed and JSON turns with sequence checks that fail closed, the single-use confirmation secret, actions, applications, capabilities, health (M3-10).
- Resumable streams: `StreamTurnEvents`, `WithCursor`, automatic resume on by default, opt-in WebSocket per call (M3-10).
- File ids on a turn (M3-29).
- RAG client: sources, search, grounded answers, citation resolution.
- `Decision().Status`: the read-only decision-layer status (D-10).
- `Providers()`: modes, egress policy and disclosure, budgets, usage export, the kill switch, write-only credentials and the OAuth flow (X-14).
- Package `agui`: the AG-UI proxy with a server-side secret store (M3-27).
- Package `sdktest`: the fake AlixAI server.

**Not in this SDK.** Voice conversation WebSocket (TypeScript only; full-duplex voice is not claimed by any SDK); file upload, download and preview; conversation helpers; history, privacy and settings; the
operator-only decision configuration routes. The Lite profile is uncertified and nothing here depends on it.
