# Compatibility and deprecation policy

Status: **in force for the beta**, written in task M5-13, 2026-10-03. Release criterion 11 ([release-gates.md](../release-gates.md)): public API and package migrations are documented and tested.

This policy says what AlixAI promises about its **public contracts**: what may change, in which kind of release, with what notice, and for how long a version is supported. It builds on
[GOVERNANCE.md](../../GOVERNANCE.md), which already says that public contracts "only break in a major version, and every deprecation is announced a release ahead", and on
[SECURITY.md](../../SECURITY.md), which says that until the first stable release only `main` receives security fixes. Where this page is stricter than either, this page applies. The numbers on this page were proposed by the maintainers and **signed off by the owner on 2026-10-08**; tightening one is a normal change, and loosening one needs an ADR (GOVERNANCE, "Material changes need an ADR").

## 1. Two stages

| | **Beta** (every `0.y.z` release, today until 1.0) | **Stable** (`1.0.0` and later, set by milestone 6) |
| --- | --- | --- |
| A public contract may break in | a **minor** release (`0.y`), never in a patch | a **major** release only |
| Notice before a break | announced in the release before it (**deprecation**), unless a security fix forbids it (section 8) | announced at least one **minor** release and 6 months before the major that removes it |
| Migration note | required for every break | required for every break and for every client-visible addition |
| What a patch release may do | fix defects and security issues; **no** contract change | the same |
| Supported releases | the latest release and `main` (as SECURITY.md) | section 6 |

During the beta a `0.y` release stands where a major stands under semantic versioning: the same notice, the same migration note. From 1.0.0 the stable column
governs: the OpenAPI document keeps `info.version: 1.0.0` with the `/v1` path prefix as the contract major (additive minors do not bump `info.version`); the
shipped SDKs and kits carry the product release version (currently `1.1.0`).

Areas that the project documents as optional and opt-in (voice, the decision layer, External provider mode, the 4 GB profile) fall under the **stable** column from the first release whose documentation calls
them stable, and under the beta column until then.

## 2. The public contracts

A thing is a public contract when a host application, an SDK user, an application-package author or an operator can depend on it and a change would break them. Everything else is internal (section 7).

| Contract | Defined in | Version marker |
| --- | --- | --- |
| **HTTP API** | `schemas/api/openapi.yaml` and `schemas/api/paths/*.yaml`, served by `internal/gateway`; the typed error envelope; headers such as `X-AlixAI-Confirmation-Token` and `X-Request-ID` | the `/v1` path prefix; `info.version` |
| **Wire documents** | `schemas/<area>/*.schema.json`; the generated Go types (`pkg/contracts/gen`) and TypeScript types (`sdk/typescript/src/gen`) | `$id: urn:alixai:schema:<area>:<name>:v1`, the `v1` being the schema's major version |
| **Streams** | TurnEvents (`alixai.turn-events.v1`) and the AG-UI mapping (`alixai.agui.v1`, `internal/agui`, `docs/integration/agui.md`) | the format markers; for AG-UI also the **pinned release**, `@ag-ui/core` 1.0.1, protocol 1.0 (`schemas/agui/VERSION`) |
| **Application package format** | `schemas/application-package/`, `docs/integration/application-package.md` | `alixai.application-package/v1`, `alixai.signature-manifest/v1`, `policy/v1`; each capability's `version` and each conversation contract's `contract_version`; the package's `compatibility.alixai_core` range |
| **Context token** | ADR 0013 | `typ` `alixai-context+jwt`, one algorithm, `EdDSA`; at most 15 minutes, single use |
| **Go contracts and SDK** | `pkg/contracts`, `pkg/sdk-go` | the Go module version (the module path is provisional until a public repository exists) |
| **TypeScript, Python and CopilotKit SDKs** | `@alixai/sdk`, `alixai` (`sdk/python`), `@alixai/copilotkit-adapter` | each package's own version (currently 1.1.0) |
| **Command line** | the `alixai` commands, their flags, their exit statuses (`0`, `1`, `2`) and the members of the JSON they print | the version of the binary |
| **Configuration** | YAML sections and keys, and the `ALIXAI_<SECTION>_<FIELD>` environment names | the version of the binary |
| **Operator-facing formats** | the backup manifest (`alixai.backup-manifest.v1`), the release manifest (`alixai.release-manifest.v1`), audit events (`alixai.audit-event/v1`), audit checkpoints (`alixai.audit-checkpoint/v1`), integration-audit reports (`alixai.integration-audit-report/v1`), the diagnostics bundle (`alixai.diagnostics/v1`), the Prometheus metric and label names (`alixai_*`) | the format markers; metric names with the binary |
| **Stored data** | the SQLite schema, per-module migrations recorded in `schema_migrations(module, version, checksum)` | the migration number; forward-only |

## 3. What counts as a break

Because the generated Go and TypeScript decoders **fail closed**, rejecting unknown members, unknown enum values and unknown union variants (`docs/integration/provider-schemas.md`), and
because most schemas are closed (`additionalProperties: false`), an addition is not always harmless to an older client. This policy therefore sorts changes three ways.

**Breaking** (a major release; in the beta, a minor release with notice and a migration note):

- removing or renaming a route, a request or response member, a CLI command or flag, a configuration key, an environment variable, a metric or label, an error code, an event type or a block type;
- making an optional request member required, narrowing what a request member accepts, or changing the meaning, type, unit or default of anything;
- changing an exit status, or the meaning of a JSON member a command prints;
- changing the token profile (`typ`, algorithm, lifetime ceiling, single use), the confirmation flow, the signature or manifest formats, or anything that makes data written by an older release unreadable;
- removing a supported format version, or a supported AG-UI or CopilotKit entry of the version matrix;
- changing a default so that a deployment that did nothing now behaves less safely, or fails closed where it used to work (a **tightening** that closes a vulnerability is handled under section 8).

**Client-visible additions** (a minor release; listed as such in the release notes and the migration notes; never in a patch): a new route; a new member, enum value, union variant, event type, block type or error code in a
response or stream; a new CLI command or flag; a new configuration key; a new metric. A client built from an older schema may reject these, so the rule of section 5 applies to the SDKs.

**Invisible changes** (any release): bug fixes that make the code do what the contract already said; performance, memory and latency; the text of log lines and of error messages; internal packages.

A new **optional** request member, a new route and a new optional configuration key never break a caller that does not use them.

## 4. Versioning rules

- **The product** follows semantic versioning 2.0.0: `MAJOR.MINOR.PATCH`. The version of the binary is stamped at build time (`alixai version`).
- **A contract that carries its own major** (`:v1` schema ids, `alixai.application-package/v1`, `policy/v1`, `alixai.release-manifest.v1`, the `/v1` prefix) keeps that major until it breaks. A break introduces
  `v2` **beside** `v1`; a loader or a gateway that does not know a marker refuses the document (the packages and manifests do exactly that today, and tests keep a `v2` document as a refusal fixture). A release that
  introduces `v2` accepts `v1` as well for the whole deprecation window of section 5.
- **An application package** states the core versions it works with, `compatibility.alixai_core` with `min` (inclusive) and `max_exclusive`, both required, so no package claims every future core. A core outside the range
  rejects the package. A package that uses something introduced in core `X.Y` sets `min` to at least `X.Y`. A capability's `version` and a conversation contract's `contract_version` change whenever the contract does, and
  that is a re-audit trigger ([operator commands](../operations/guides/operator-commands.md), section 3).
- **SDKs.** An SDK release `X.m` is supported against the cores `X.k` with `k` less than or equal to `m`: **upgrade the SDK before the core** within a major. Against a newer core minor it works unless that minor lists a
  client-visible addition the SDK rejects. Each SDK release states the core range it was tested against; `@alixai/copilotkit-adapter` states its peer ranges from the version matrix.
- **Stored data.** Migrations are cumulative and forward-only, applied by the new binary at start or by `alixai upgrade apply`. Any release opens the database of any earlier release of the same major. **Running an older
  binary over a migrated database is not supported**: going back is `alixai upgrade rollback`, which restores the pre-upgrade backup ([upgrades](../operations/guides/upgrades.md)). A backup made by an earlier release restores under
  a later one; the reverse is not supported.
- **Audit and receipt records are evidence.** A release that changes the audit-event, checkpoint or receipt format must keep verifying the chains and checkpoints written in the old one (SEC-086); a migration never rewrites
  them.
- **Streams.** The AG-UI release in `schemas/agui/VERSION` is part of the contract. Moving the pin is a client-visible change: the vendored schemas are regenerated (`node tools/aguischema/generate.mjs`), every emitted event is
  re-validated against them, `GET /v1/agui/capabilities` states what is supported, and the version matrix (`sdk/copilotkit-adapter/compat/matrix.json`) is updated in the same release. CopilotKit releases are claimed only when a
  matrix entry says `tested`; dropping an entry is a deprecation. AlixAI stays the authority layer through any of it: client-supplied tools, state, context and messages remain untrusted data, and nothing requires CopilotKit
  Cloud or sends telemetry (ADR 0035).
- **LocalAI, backends and models.** The release manifest pins the qualified runtime, backends and models (`docs/operations/release.md`). A change of the qualified set is announced in the release notes; the operator moves with
  `alixai upgrade check` and `apply`, and a version AlixAI was not qualified against is reported unavailable (`localai.qualified_versions`, SEC-072).

## 5. Deprecation

1. **Decide and write it down.** A deprecation of anything in section 2 that is material is an ADR; a small one is a line in the pull request that makes it.
2. **Announce it** in the release notes under "Deprecations", saying what is deprecated, what replaces it, and the version that will remove it, and **mark it where the contract lives**: `deprecated: true` and a
   replacement note in the OpenAPI fragment or the schema description, a "Deprecated" paragraph in the documentation of the format, command, key or metric.
3. **Keep it working, unchanged,** until the removing release. A deprecated CLI flag or configuration key keeps its meaning; a deprecated route keeps its behaviour and its schema.
4. **Window.** Beta: at least the next minor release. Stable: at least one minor release **and** 6 months from the announcement, and removal only in a major.
5. **Remove it** in the announced release, with a migration note (section 7). A removal that was not announced a release ahead is a defect of the release process and is handled as one.

A dual-version period for a format with its own major (section 4) is a deprecation window: both versions are accepted, the old one is deprecated, and `alixai package validate`, `alixai config check` or the equivalent says
which one a document uses.

## 6. Support windows

| | Beta | Stable |
| --- | --- | --- |
| Latest release of the current line (`0.y` or `X.Y`) | defect and security fixes | defect and security fixes |
| The previous minor of the same major | none | security fixes for 90 days after the next minor ships |
| The previous major | not applicable | security fixes for 12 months after the next major's `.0` |
| `main` | the only branch that receives security fixes before the first stable release (SECURITY.md) | development |

A release outside its window gets no fix. A fix is released as a patch of every supported line it affects, with an advisory ([vulnerability response](vulnerability-response.md)). The supported-version table
SECURITY.md promises is published with each stable release.

## 7. Migration notes

Every break and every client-visible addition has a migration note in the release notes. A note that cannot be written is a sign the change is not ready. It has:

- **what changed**, in one sentence, and in which contract of section 2;
- **who is affected**: callers of the API, SDK users, package authors, operators;
- **before and after**, with a minimal example of each;
- **how to find out** whether you are affected: a command (`alixai config check`, `alixai package validate -core-version <new>`, `alixai upgrade check`) or a search;
- **how to move**, in order, and **how to go back** (`alixai upgrade rollback`, a restore from the backup the upgrade took);
- **the first release that drops the old form**, when something is deprecated.

Gate 11 asks that these are tested: a migration that changes a package, a configuration key or stored data comes with a test that runs the old form through the new release. A note longer than a page gets its own file, named
after the target version, in the place the first such release fixes.

## 8. Exceptions

- **Security.** A fix for a vulnerability may break a contract, or remove one, without the notice above when waiting would leave users exposed. It ships in a patch of every supported line, with the advisory and the migration
  note, and says so in both ([vulnerability response](vulnerability-response.md)).
- **Defects in the contract itself.** If a schema or document says one thing and the code and its tests another, the code that fails closed wins until the contract is corrected, and the correction is announced in the release
  notes. This is the rule the repository already follows (the schemas win over a page that disagrees with them).
- **Experimental areas** (section 1) follow the beta column.

## 9. What is not a contract

`internal/**` and everything under it; the text of log lines and error messages (the error `code` and the HTTP status are contracts, the message is not); the exact wording of CLI output on stderr; performance, memory and latency
figures, which are **emulated** until measured on the target host; the layout of the data directory beyond the files and formats named in section 2; the ordering of a response that the contract does not order; the contents of
generated identifiers after their typed prefix; the documentation's prose.

## 10. What exists today, and what does not yet

| Mechanism | State |
| --- | --- |
| Schema ids and format markers with a major; refusal of an unknown marker | Built and tested (`internal/trust`, `internal/apppkg`, `tools/release`). |
| Package core-version range, checkable with `alixai package validate -core-version` | Built and tested. |
| Route and spec agreement (`TestRoutesMatchOpenAPISpec`), generated-code drift (`go run ./tools/schemagen --check`), schema examples (`go run ./tools/schemacheck`) | Built; they catch drift, not a breaking change. |
| AG-UI pin, vendored schemas, version matrix (`node tools/aguischema/generate.mjs --check`, `pnpm --filter @alixai/copilotkit-adapter compat`), the CopilotKit gate (`pnpm --filter @alixai/copilotkit-gate test`) and its published page, `docs/integration/copilotkit-compatibility.md` | Built (on `main`). `node compat/run.mjs` runs the adapter suite and the gate for every matrix entry. Both entries, `copilotkit-1.76.0` (primary) and `copilotkit-1.77.0`, pass both suites (gate 119 of 119, adapter suite 117 of 117 in M6-08), so both are claimed as `tested`: see [the compatibility page](../integration/copilotkit-compatibility.md#versions). |
| Staged upgrade, rollback, backup that restores under a later release | Built; see [upgrades](../operations/guides/upgrades.md) for how much of it has run. |
| **An automated breaking-change check on the OpenAPI document and the schemas** | **Not built.** Until it is, the release manager compares them by hand before each release. |
| **A warning at run time for a deprecated flag, key or route** | **Not built.** Until it is, deprecations are announced in the release notes and the documentation only. |
| **A published release with release notes and a supported-version table** | **1.0.0 and 1.1.0:** release notes live in the root README Project status and the SDK/kit changelogs; the supported-version table is [migration and support](migration-and-support.md). |
| **Stable support windows, frozen v1 contracts, the migration and support policy** | In force from 1.0.0 (`schemas/_frozen/v1`, `tools/compat`); written in docs/policies/migration-and-support.md. The values above are signed off by the owner (2026-10-08). |

## 11. Changing this policy

A change to this page follows GOVERNANCE: a pull request, reviewed in public, with an ADR when it loosens a promise.
