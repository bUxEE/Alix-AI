# clinic-booking

A synthetic, privacy-safe clinic appointment application: the first neutral AlixAI reference
application. It is a **test fixture with real business logic**, not a product. Every name, phone number
and address is made up (`+1 555 01xx`, `example.test`).

- TypeScript on Node 22.18 or newer, Koa and `node:sqlite` (no external database, so tests are hermetic). `node:sqlite` needs no flag from 22.13 and `node src/main.ts` runs TypeScript directly from 22.18; the code is erasable-only TypeScript.
- Exposes its operations through an MCP server (streamable HTTP, `POST /mcp`, JSON responses).
- Ships a signed AlixAI Application Integration Package in [`package/`](package/) (English and Italian).
- Depends on no AlixAI SDK: a host application never needs one to be integrated.

```bash
pnpm install --ignore-workspace      # standalone: there is no workspace root
pnpm test                            # vitest: the application, the package and the package's own probes
pnpm typecheck
CLINIC_TEST_SUPPORT=1 CLINIC_DEMO_SEED=1 pnpm start   # {"event":"listening",...,"mcp":"http://127.0.0.1:8787/mcp"}
```

The package is validated and signed from the repository root with the TEST-ONLY key
(`tools/devkeys`, key id `alixai-test-only-publisher`; a real trust store refuses it):

```bash
go run ./cmd/alixai package validate examples/reference-apps/clinic-booking/package
go run ./cmd/alixai package sign -app clinic-booking -version 1.0.0 \
  -key alixai-test-only-publisher=tools/devkeys/test-only-publisher.key.pem \
  -signed-at 2026-10-02T00:00:00Z -expires-at 2036-10-01T00:00:00Z \
  examples/reference-apps/clinic-booking/package
```

## Tenancy and permissions

Tenant = clinic group, workspace = clinic site. Every call carries the minimum identity AlixAI sends
(`application_id`, `tenant_id`, `workspace_id`, `principal_id`, `roles`, `session_id`); the application
reauthorises it on every read, validate, prepare, commit and status call from its own directory and
never trusts the roles asserted. A record outside the caller's site, or of another group, is refused as
`scope_violation` exactly like one that does not exist.

| Role | Capabilities | Records it can reach |
| --- | --- | --- |
| receptionist | all five | every patient and appointment of its own site |
| doctor | `availability.list`, `patients.find`, `appointments.cancel` | patients with an appointment with them, their own appointments |
| patient | all five | only themselves and their own appointments |

## Capabilities

| Capability | Level | Operations (MCP tools) |
| --- | --- | --- |
| `availability.list` | 0 | `availability_list` |
| `patients.find` | 0 | `patients_find` |
| `appointments.book` | 2, explicit confirmation | `appointments_book_{validate,prepare,commit,status}` |
| `appointments.cancel` | 2, explicit confirmation | `appointments_cancel_{validate,prepare,commit,status}` |
| `patients.update_contact` | 1, no separate confirmation | `patients_update_contact_{validate,prepare,commit,status}` |

Business rules that produce conflicts, stale data and permission denials: a slot can be booked once
(a unique index backs it), a patient cannot hold two overlapping appointments, a booking must be 60
minutes to 90 days ahead, an appointment that has started cannot be cancelled, and a prepared action
expires.

## Wire contract

- **Input.** `identity`, then `fields` (read, validate, prepare), or `idempotency_key` and `prepared`
  (commit: the whole PreparedAction as the core holds it), or `idempotency_key` (status).
- **Output.** `structuredContent` is the host members of a DeterministicResult: `status`,
  `identifiers`, `values` (typed canonical facts), `errors` (typed), `freshness`,
  `allowed_next_actions` and, for prepare, `preparation`. `operation` and `capability_id` are the core's.
  The same JSON is mirrored as text. **A refusal is a result with status `rejected`, never an MCP tool
  error**; a tool error means the reply was lost, and after a commit that is resolved through `status`.
- **Validate reports the version stored from Prepare.** `freshness.version` is the snapshot version of
  the records the action relies on (a digest of each record's own revision counter). On unchanged
  data Validate returns exactly the version Prepare returned; any change to one of those records, and
  to nothing else, changes it. It is not a response ETag and not a site-wide or tenant-wide counter.
  Taking the same slot between prepare and commit moves it, and commit answers `stale`.
- **Commit is idempotent.** The key is fixed at prepare time; a repeat returns the original receipt and
  changes nothing; the same key for another action is a `conflict`. `status` returns the recorded
  receipt, and says "nothing recorded" in exactly one shape (`rejected`, one `validation_failed` on
  `idempotency_key`, no identifiers), the only answer that makes a retry safe.
- **Entity resolution is an adapter READ** with the reserved arguments `entity_type`, `mention` and
  `candidate_id` (in `fields`), mapped by the package to the declared `entity_types.<type>.resolve` tool
  (`resolve_patient`, `resolve_doctor`, `resolve_slot`, `resolve_appointment`). A slot is named by its identifier,
  by `YYYY-MM-DD [HH:MM]` or as "the next available one" ("il primo orario libero"), which the host, not the
  model, resolves to the earliest slot that can still be booked. The answer lists the
  candidates the caller may see as `identifiers`, with a text value `label` at the same position for
  each, and the snapshot version as freshness; several matches are several candidates and the user
  chooses. `candidate_id` is the model's proposal, so it is never an instruction: alone it re-resolves
  exactly that record (refused like an unknown one when the caller may not see it); with a mention it
  changes nothing, so a model can never pick which of several matching records is meant.

## Context tokens, disposable tenants, inspection

- **Context tokens** follow the normative profile of ADR 0013 (`src/context-token.ts`): JWS, `EdDSA`,
  closed claim set, single-use `jti`, `session_id` required, no `deployment_id`, at most 15 minutes,
  `node:crypto` only. Minting uses the TEST-ONLY dev key and only in test support mode
  (`POST /host/context-token`).
- **Test support** (`CLINIC_TEST_SUPPORT=1`) mounts the disposable-tenant tools `test_create_tenant`,
  `test_seed` (a record may carry the `given_name` and `family_name` of a patient, the `name` of a doctor or the
  `reference` of an appointment, so the audit's conversations can name what exists) and `test_cleanup` (which proves
  every table is empty for the tenant), plus `test_change_record` (moves the revision counter of one patient, slot or
  appointment of a disposable tenant and nothing else, so the audit can show that `validate` notices a change and that
  a preparation made before it is refused as stale; the adapter configuration approves it, no capability maps it, and
  the package format declares no tool for it, so the integration audit finds it by that name) and
  `test_inject_fault` (arms one failure of the next request of an operation of a capability, for requests of the
  named disposable tenants only: `dependency_down` answers `unavailable`, `reply_lost` applies a commit and loses its
  reply; found by the audit by that name too), per-tool call
  counters, one-shot fault injection over HTTP (`unavailable`, `error_after_commit`) and a clock that can only
  move forward. None of it exists otherwise, and a tenant that was not created as disposable is
  never touched.
- **Inspection** for the integration audit: the MCP tool `inspect` and `GET/POST /inspect` report nine
  aspects (versions, modules, dependencies, health, scopes, credentials, limits, storage, network) as
  typed facts, never a secret.
- The application listens on loopback only and refuses any other address, a foreign `Host` or `Origin`
  header, and (when `CLINIC_MCP_BEARER` is set) a request without the credential.

## The package and its tests

`test/package.test.ts` holds the package to the running application: the mapped tools are exactly the
tools the server offers and the adapter allow-list names; declared fields, bounds, defaults,
validation rules, failure states, risk levels, time-to-live and idempotency window are what the code
does; every response obligation and every placeholder of the conversation contracts names a fact the
host really returns; every real result validates against the package's own `schemas/*.result.schema.json`
(which reject broken results too); the policy equals the permission table; the manifest's context
exchange equals what the application mints. `test/package-probes.test.ts` runs the package's own test
suite (`package/tests/clinic-safety.yaml`) against the application over MCP, through the declared
provisioning tools: every scope probe, and every safe-execution probe the host can observe.

`test/package-signature.test.ts` checks, without the Go tool, that every file of the package still matches
the digest it was signed with and that the Ed25519 signature of the TEST-ONLY key verifies over the
canonical integrity manifest: change a package file and `pnpm test` fails until the package is signed again.

## Examples must route

AlixAI's rules-only router shortlists a capability for an utterance from the words of the capability's
title and from the terminology its conversation contract lists (`conversation-contracts/*.yaml`,
`terminology/clinic-terms.yaml`), and from nothing else: an example is test data, never routing policy.
So a package whose own examples are worded with words no listed term carries (a phone number, an email
address, a date of birth) never reaches the model. Every example of every capability routes to its own
capability, in English and in Italian, with no model call; `tests/conformance/package`
(`TestEveryReferencePackageExampleRoutes`) fails when one stops routing, and `alixai-conformance` replays
all 22 examples through the real engine against this application. When an example is added, list the
nouns it uses as terms of its capability's contract in both languages, then sign the package again.
