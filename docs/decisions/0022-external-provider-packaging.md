# 0022 — Packaging of external provider adapters

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01; finalised by X-10 (binary size measured both ways)

## Context

The open decision asks whether external provider packages ship in the core distribution or as separate optional
packages. Three constraints decide it. The build is pure Go with `CGO_ENABLED=0` and one small native binary
(ADR-0001, ADR-0009), because the Lite and 4 GB profiles need a small footprint.
With external mode off the process must behave exactly as Local mode: no external package initialised, no
outbound connection. And an operator must be able to remove every
external provider and return to a fully local deployment.

## Options

| Option | How | Security | Resource impact | Verdict |
| --- | --- | --- | --- | --- |
| A. One binary, adapters as Go packages under `adapters/external/<provider>`, disabled by default, plus a `noexternal` build tag | the tag drops the registration file, so the linker never sees the packages | the default binary carries external code that is dormant until enabled; the tagged binary carries none | measured by X-10 in both builds | **default** |
| B. Separate optional binaries or sidecar processes per provider, spoken to over loopback IPC | a second process per provider | a new local IPC trust boundary, a second place that holds provider credentials or has to be handed them | one more process and its memory per provider; install and upgrade complexity | not chosen; against rule 12 and the "no secrets outside the credential service" invariant |
| C. Dynamic plugins | load code at run time | loading unsigned code into the process that holds scope and credentials | the Go `plugin` package depends on cgo, which this project does not use (stated from the standard library documentation, not re-verified here) | rejected |
| D. A separate module or repository for the adapters | the core imports nothing, a distribution adds them | similar to A once built, but the dependency direction inverts and releases drift | two release trains | rejected for now; A with the tag is the same isolation at less cost |

## Decision

1. **All adapters are compiled into the one binary as separate Go packages under `adapters/external/<provider>`**
   (OpenAI in X-10, then Anthropic, Gemini, Meta or generic). Shared helpers may live directly under
   `adapters/external/` (X-19 reuses what X-10 puts there). Provider packages never import each other and never
   import `adapters/localai`.
2. **They are DISABLED by default.** Nothing is constructed at import: package-level `init` functions in these
   packages and in the milestone-X internal packages must have no side effect (no goroutine, file, socket or
   network access); a provider is built by an explicit constructor, called by the composition root only when
   external mode is enabled for the deployment.
3. **A `noexternal` build tag produces a local-only binary with no external code at all.** Registration lives in
   `internal/cli/providers_external.go` (`//go:build !noexternal`) and a stub `providers_noexternal.go`, both
   created by X-10. Every package of milestone X is reached from the core only through interfaces the core
   already owns (the dialogue seams of M1-14, the gateway's `Need` and `Want` by type, the `internal/providers`
   roles), and only the `!noexternal` composition-root file imports the concrete packages. The tagged binary's
   dependency list (`go list -tags noexternal -deps ./cmd/alixai`) must contain none of `adapters/external`,
   `internal/credentials`, `internal/oauth`, `internal/dataegress`, `internal/providerreg`,
   `internal/providerbudget` or `internal/modes`. That check is the planned test
   `TestXNoexternalBuildHasNoExternalCode` (X-GATE); X-10 adds the command
   `! go list -tags noexternal -deps ./cmd/alixai | grep -q 'adapters/external'` to its own verify.
4. **Size bar, set before the measurement.** X-10 records the size of `go build ./cmd/alixai` and of
   `go build -tags noexternal ./cmd/alixai`. If the external code adds more than 2 MiB or 5 % of the
   `noexternal` binary, whichever is smaller, the `noexternal` binary is published as the Lite artifact and the
   default one as the connected artifact. Below the bar, one artifact ships. X-10 may overturn the threshold
   with the numbers in hand, in writing.

## Consequences

- Easier: one install, one upgrade path, one conformance run per release; "return to fully local" is a
  configuration change (or the tagged build), not an uninstall.
- Harder: the core must never grow a compile-time dependency on the milestone-X packages. Routes that expose
  providers (X-13, `internal/gateway/routes_v1_providers.go`) and the dialogue wiring (X-12,
  `internal/dialogue/extmode`) are compiled into every binary only if they import nothing from the list above:
  they take their services by interface type and the `!noexternal` file supplies the implementations. X-13 and
  X-12 must be written that way. The gateway refuses to start when an area is only partly provisioned, and the
  `noexternal` stub provides none of the providers-area services, so X-13 must either tag its route file
  `//go:build !noexternal` or take every milestone-X service with `Want` only (the routes then answer 503 or 404
  until provided), never `Need`. X-13's write scope also has no `internal/cli` provider file, so nobody is
  assigned yet to register its services in the `!noexternal` root (a planning gap for the orchestrator). The
  X-GATE list should also name `internal/extmodal` and
  `internal/dialogue/extmode` (reported to the orchestrator, outside this task's scope).
- Forbidden: a milestone-X import from any file that is built without the tag, other than the generated type
  package `pkg/contracts/gen/providers` and `pkg/contracts` (types only).

## What confirms or overturns this (owed)

- X-10: binary size in both builds, `go list` evidence for the tagged build, and a hermetic test that external
  mode off opens no connection (the planned `TestXLocalModeNeverContactsExternalWithNetworkBlocked`).
- If the delta crosses the bar, split the artifacts (still one source tree).

## Reversal plan

Option B or D means moving `adapters/external/<provider>` behind the existing provider contract into its own
process or module; the contract, profiles, schemas and policy do not change. The `noexternal` tag and the two
registration files are the only build-level switch and can stay either way.
