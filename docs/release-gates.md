# Release criteria

AlixAI 1.0 may be called **stable** only when every criterion below holds, each one is proven by tests that run in the public
repository or by a recorded piece of human evidence, and the release record exists. The current state of each criterion — what
passes, what is emulated and what still waits on hardware or a person — is kept on the
[verification status page](verification/README.md).

## How to check them yourself

Every automated part runs from a plain checkout, with no network, no model download and no container:

```bash
go build ./... && go vet ./... && go test ./...   # all Go suites, including the end-to-end, security, recovery and degradation suites
go run ./tools/schemagen --check                  # generated types agree with the schemas
go run ./tools/compat check                       # the frozen v1 contracts are unchanged
go run ./tools/doclint docs                       # documentation consistency (decision layer, ADR 0029)
go run ./tests/security/cmd/coverage              # every security backlog entry has a test that exists
go run ./tools/release reproducible-check         # the release build is byte-for-byte reproducible
pnpm install --frozen-lockfile && pnpm -r test     # SDKs, CopilotKit adapter, UI kits, reference application
cd sdk/python && python -m pytest                 # Python SDK
```

Tests that need Docker, LocalAI, downloaded models or a real provider sit behind the `live` build tag and are run by hand on the
target hardware; their results are summarised under [`docs/evidence/`](evidence/).

## States

| State | Meaning |
| --- | --- |
| `pass` | every automated part of the criterion holds and no human evidence is owed |
| `fail` | a test that proves the criterion does not exist or does not pass, or a check fails |
| `human-evidence-missing` | the automated part holds, but evidence that only a person, real hardware or an account can produce is not recorded yet |

A skipped test counts as a failure. A criterion is never reported green because its human part is pending.

## Release criteria

| # | Criterion | Proven by | Human evidence |
| --- | --- | --- | --- |
| 1 | Self-hosted: no AlixAI cloud service is required, and Local mode never contacts an external provider | the no-cloud and network-posture suites (`tests/security/posture`, `tests/security/local`), compose loopback-only checks | |
| 2 | The Lite profile works on 8 GB | emulated model measurements ([model report](evidence/m5-models/report.md), [efficiency review M5-09](efficiency/M5-09.md)) | a real-hardware certification run and the 24-hour / 7-day soaks |
| 3 | Three unrelated applications integrate without core changes | `alixai-conformance` against the three [reference applications](../examples/reference-apps/) | |
| 4 | Prepare / confirm / commit holds, replays commit nothing | `tests/e2e/m1` (replay, stale confirmation, fail-closed matrix); `tests/dr` (kill during commit) | |
| 5 | Zero unauthorized commits or cross-tenant disclosures | `tests/security/...`; the [security test backlog](security/test-backlog.md) coverage tool | |
| 6 | RAG answers cite original sources, and no other tenant's text reaches the model | `internal/rag/...`; `tests/security/rag`; `tests/security/isolation` | |
| 7 | A model failure creates no unverified action or fact | `tests/e2e/m1` fail-closed matrix; `tests/degradation`; the decision-layer and external-mode verification suites | |
| 8 | A voice failure does not stop text or application control | `internal/voice/...`; `tests/degradation` (STT and TTS failures) | |
| 9 | Signed install, update, rollback, backup and restore | `tests/dr`; `tools/release`; the [production guide](operations/production.md) and the [recovery drill report](evidence/m6-dr/report.md) | release-signing keys held offline |
| 10 | Licences permit the distribution | `tools/release` (SBOM); `LICENSE`, `NOTICE`, the [model registry](../models/registry/) | a licence review of the models and dependencies |
| 11 | API and package migrations are documented and tested | `go run ./tools/compat check`; [compatibility policy](policies/compatibility.md), [migration and support](policies/migration-and-support.md), `schemas/_frozen/v1/` | |
| 12 | The critical integration audit passes before activation, and changes are re-audited | `tests/e2e/m1` activation and tamper tests; `internal/integrationaudit` | |
| 13 | Every feature has a smallest-model review and a resource measurement | the [efficiency reviews](efficiency/) | |
| 14 | No open critical or high security finding | the [internal pentest findings](security/pentest-internal/findings.md), each fixed finding with its regression test; `go run ./tests/security/cmd/coverage -strict` once the human evidence exists | an external security review |

The strict coverage view fails by design until the human evidence behind SEC-090/091 (criteria 9 and 10), SEC-096 (criterion 14)
and SEC-111/112 (criterion 10) is recorded.

## Decision layer (ADR 0029, 0030, 0031)

The decision layer is an optional, bounded, non-generative and non-authoritative scorer; the smallest profile never requires
CLM-8B.

| Id | Criterion | Human evidence |
| --- | --- | --- |
| D.1 | Optional and removable: every flow ends the same with the layer off, unavailable, below threshold, unsupported or rejected by a resource gate | |
| D.2 | Advisory-only authority with typed, audited fail-closed reasons; the status API exposes no authority field | |
| D.3 | The Lite profile never requires CLM-8B | |
| D.4 | No profile enables the scorer by default without recorded evidence ([ADR 0031](decisions/0031-decision-scorer-default-enable.md), `models/registry/clm-v0.1-8b.yaml`) | |
| D.5 | Licence and supply-chain review of the scorer | a licence review |
| D.6 | Real-hardware gates and a per-profile decision | a real-hardware run |

## External provider mode

External and Hybrid modes are opt-in and off by default ([design](architecture/external-provider-mode.md)).

| Id | Criterion | Human evidence |
| --- | --- | --- |
| X.1 | LocalAI remains the default and External mode is off by default | |
| X.2 | Switching between Local, External and Hybrid does not change the host contract | |
| X.3 | Every provider passes the conformance suite (`tools/providerconf`, run with fixtures) | provider accounts and a live conformance run |
| X.4 | A provider's output cannot reach an application adapter without validation | |
| X.5 | Credentials never reach host clients or models | |
| X.6 | The data-egress policy blocks local-only sources deterministically | |
| X.7 | Every external response is subject to action, fact and citation verification | |
| X.8 | Cost, quota, latency, capability and failure state are visible to the host | |
| X.9 | Losing the network leaves deterministic work and local search usable | |
| X.10 | Removing every provider returns to a fully local deployment; a `noexternal` build holds no external code | |
| X.11 | The 4 GB profile is benchmarked separately in Local and connected modes ([report](evidence/x-benchmarks/report.md), emulated) | real-hardware connected runs |
| X.12 | OAuth is used whenever a provider supports it; a manual key is only a protected fallback | a live OAuth consent |
| X.13 | External mode is identified as network-dependent and subject to provider terms and billing | the provider terms and a legal, residency and security review |

## Known state at the time of writing

- The Lite controller is uncertified. The model results are emulated on a limited container; criterion 2 stays
  `human-evidence-missing` until a real-hardware run is recorded.
- On a host where a security product rewrites loopback `GET /mcp`, the tests that speak MCP over loopback fail. That is a host
  fault, reported as a failure, never hidden or skipped.
- Two success criteria have no release gate of their own: natural multi-turn text interaction is exercised only by the
  end-to-end tests, and replacing the model or the hardware without changing application behaviour is covered only by X.2 and
  D.1 (swapping hardware has no test).
