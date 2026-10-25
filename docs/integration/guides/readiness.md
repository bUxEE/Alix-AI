# Integration readiness discovery

Status: written in task M8-07 for the commands of M8-05 and M8-06. Decision: [ADR 0037](../../decisions/0037-integration-readiness-discovery.md).

Readiness discovery answers one question before an application has a package, or when its package changes: **what does this project already have that AlixAI integration needs, and what is missing?** You name a project directory;
AlixAI reads it, builds a checklist and reports each requirement as covered, partial, missing, unverified, contradicted or not applicable, with the file, lines and hash that back every claim. It is the step before
[getting started](getting-started.md) and the audit, not a replacement for either.

## What exists, and what does not

| Question | Answer |
| --- | --- |
| How do I run it? | The command line only: `alixai readiness scan`, `status`, `list` and `show`. |
| Is there an HTTP route? | **No.** There is no `/v1` route and the OpenAPI document did not change. |
| Is there an SDK helper? | **No.** None of the TypeScript, Python and Go SDKs has a readiness call. |
| Does it need a running server? | No. Like `alixai audit`, it works offline against the data directory. |
| Is it an audit? | **No.** It is advisory: it certifies nothing, signs nothing, activates nothing and edits no package. **A green readiness report is not an audit pass.** The audit still decides activation, and every report says so on its first line. |
| Languages | English and Italian (`-lang en` or `-lang it`). Both are always available. |
| Can a model help? | Yes, optionally. The model only proposes; every finding is checked against the file. See [the model leg](#the-model-leg). |
| Is a package signed or installed by it? | No. `-scaffold` writes an **unsigned draft** that the package loader refuses. See [the draft scaffold](#the-draft-package-scaffold). |

Voice and external providers are unrelated to readiness, apart from the opt-in external model leg described below. If this page mentions the optional decision layer (CLM) anywhere, it is the same layer the rest of the
documentation describes: optional, bounded, non-generative and non-authoritative. Readiness does not use it.

## What the scan reads, and what it never does

- **Explicit.** You name the directory. Nothing is scanned implicitly.
- **Read-only and bounded.** The scan has size, file-count, depth and directory-entry caps. It skips dependency and VCS directories, follows no symbolic link and makes no network connection. When a cap cuts a scan short, the
  report says so in its last section, which also counts the files that were not read.
- **Secrets are never read.** Secret-looking files (`.env*`, keys, certificates, credential stores) are recorded as present and never opened. Secret-looking values inside readable files are redacted before they reach a
  report, a model or a log.
- **Nothing is written into the scanned directory.** The report goes to the data directory of your configuration and, with `-out`, to the one file you name (owner-only). With `-scaffold`, a draft goes to the new directory
  you name, which must not exist and must not be inside the scanned tree.

## Run it

```bash
alixai readiness scan DIR [-config FILE] [-app ID] [-lang en|it] [-format md|json|both] [-no-model] [-model ID] [-out FILE] [-scaffold OUTDIR]
alixai readiness status [DIR] [-config FILE] [-app ID] [-lang en|it] [-format md|json] [-diff] [-no-model] [-model ID]
alixai readiness list   [DIR] [-config FILE] [-app ID] [-format md|json]
alixai readiness show   [DIR] [-config FILE] [-app ID] [-report ID] [-lang en|it] [-format md|json]
```

These are the usage lines of the commands. The full reference, with the data directory, the exit codes and the audit link, is [operations: readiness](../../operations/readiness.md).

**Name the application.** Reports are stored in the data directory under a key: `-app ID` when you give one, otherwise a key derived from the directory. Use the same one for every command. `alixai audit run` looks up the
key of its own application id, so scan with `-app <application id>` if you want the audit to link your latest report.

Scanning the three fixtures in `internal/readiness/testdata`, with `-no-model` and a configuration whose only key was `data_dir` (**Run**, M8-07, by hand; the commands exit 0, as every completed scan does):

```bash
alixai readiness scan internal/readiness/testdata/clinic    -no-model -config alixai.yaml
alixai readiness scan internal/readiness/testdata/incomplete -no-model -config alixai.yaml
alixai readiness scan internal/readiness/testdata/readme-only -no-model -config alixai.yaml
```

Each also printed two notes on stderr: `the model leg is disabled by -no-model; this report comes from the deterministic scan alone`, and `stored report <id> under <key>`.

| Fixture | What it is | Met | Partial | Missing | Unverified | Contradicted | Not applicable |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `clinic` | a project with a package, a handbook, MCP tools and a compose file | 30 | 0 | 1 | 1 | 0 | 1 |
| `incomplete` | an OpenAPI file, a Dockerfile and a compose file, one document and a server directory | 2 | 5 | 22 | 0 | 1 | 3 |
| `readme-only` | one README | 0 | 0 | 33 | 0 | 0 | 0 |

The same engine runs over the clinic-booking reference application in [getting started, step 1](getting-started.md#1-run-the-readiness-scan-advisory).

## Read the report

A markdown report has the same sections every time. This is the beginning of the report on the `clinic` fixture, copied from the run (the `Generated` and `Scanned directory` lines name the machine it ran on and are left out):

```text
A readiness report is advisory: it is not an integration audit pass, and the audit still decides activation.

# Integration readiness report

- Report: `rrpt_01M4MZVA2DKX8NF3MWPCN9GH5Z`
[...]
- Mode: No model ran: this report comes from the deterministic scan alone.
- Inputs hash: `910f032f316cb0d34446c6b2c75bda12028f28bd7ae5e5fc31245752c7c90e91`
- Catalog: v1 `c3a5b8fb4b47`

## Summary

- Met: 30
- Partial: 0
- Missing: 1
- Unverified: 1
- Contradicted: 0
- Not applicable: 1
```

| Section | What it holds |
| --- | --- |
| **Covered** | Requirements that are met or partial. Each cites the file, the lines and the file hash a verifier re-read. |
| **Not applicable** | Only when it has entries. MCP, OpenAPI and a native adapter are alternative transports: once a requirement of one is met, a missing requirement of another is listed here with the transport in use, and is not counted as missing. With no transport met, all of them stay under Missing, each with its fix. |
| **Missing, and how to fix it** | Requirements for which nothing was found, each with what to add. |
| **Contradictions** | Two pieces of evidence that disagree (for instance a Dockerfile and a compose file that give different container ports). Fix the disagreement before relying on either. |
| **Could not be determined** | Requirements that could not be confirmed. They are **never counted as covered**. |
| **What the scan covered** | Files read and not read, bytes read, secret-looking files recorded and never read, values redacted, and whether a cap cut the scan short. |

Statuses: `found` (shown as **Met**), `partial`, `missing`, `unverified`, `contradicted` and `not_applicable` (a checklist entry only, never a finding; the summary carries its count only when it is not zero). Levels: a `mandatory` requirement is one the audit check it feeds cannot pass without; a `recommended` one gives the audit a
warning when it is absent. A finding comes from `deterministic` (a rule found it), `model_verified` (a model proposed it and the verifier confirmed it) or `model_unverified` (a model proposed it and the evidence did not
confirm, so it counts for nothing).

What the `incomplete` fixture shows, copied from its run: a document-level gap is `partial`, and a conflict is reported in its own section.

```text
### `req.documentation.capabilities` Documentation for every capability

- Partial (mandatory). Feeds the audit check: `package.documentation`
- 1 of 2 API operations treated as capability candidates are described in documentation.
- Evidence: `docs/plants.md` lines 3-3, sha256 `70f181141c70`

[...]

## Contradictions

Two pieces of evidence disagree. Fix the disagreement before relying on either.

### `req.deployment.compose_and_ports` Deployment files, migrations and ports

- Contradicted (recommended). Feeds the audit check: `runtime.inspection`
- A Dockerfile and the compose file next to it disagree about the container port.
- Evidence: `Dockerfile` lines 2-2, sha256 `e30c9566738a`
- Evidence: `docker-compose.yml` lines 5-5, sha256 `8e182bcde595`
- How to fix: Commit the compose files and migrations, and keep one source of truth for ports.
```

And the `readme-only` fixture, whose only file is a README: `Met: 0`, `Missing: 33`, an empty Covered section (`Nothing to list.`), and a Missing section that lists every requirement with what to add.

**Exit status.** A completed scan exits 0 whatever it found: a project with 33 missing requirements is a completed scan. A non-zero status means the scan itself failed (a refused directory, an unusable store, an
`-out` file that cannot be written), and 2 is a malformed call.

### English and Italian

Every title, description, rule and remediation exists in both languages. The first lines of the same stored report, asked for in Italian with `readiness show -lang it` (copied from the run):

```text
Un rapporto di readiness e' consultivo: non e' il superamento dell'audit di integrazione e l'audit decide comunque l'attivazione.

# Rapporto di readiness dell'integrazione

- Rapporto: `rrpt_01M4DH1ABVDPYYW7AG53J94MZW`
[...]
- Modalita': Nessun modello e' stato eseguito: il rapporto deriva solo dalla scansione deterministica.
- Hash degli input: `910f032f316cb0d34446c6b2c75bda12028f28bd7ae5e5fc31245752c7c90e91`
- Catalogo: v1 `c3a5b8fb4b47`
- I riepiloghi dei singoli riscontri sono prodotti dal rilevatore in inglese.

## Riepilogo

- Soddisfatto: 30
- Parziale: 0
- Mancante: 2
- Non verificato: 1
- Contraddittorio: 0
```

The per-finding summaries that a detector writes (for instance `an application package manifest ... found`) stay in English, and the Italian report says so.

### The JSON document

`-format json` prints the document of `schemas/readiness` (schema version 1), with the same content: `advisory`, `inputs_sha256`, the `scan` counters, the `checklist` of requirements with their status, the `findings` with their evidence, and the `summary`. The head of the
document for the `clinic` fixture, copied from the run (`[...]` marks lines left out):

```json
{
  "schema_version": 1,
  "report_id": "rrpt_01M4DH2DPQAVTYERQWJ9DSYKEC",
  "advisory": true,
[...]
  "inputs_sha256": "910f032f316cb0d34446c6b2c75bda12028f28bd7ae5e5fc31245752c7c90e91",
  "catalog_version": 1,
  "catalog_sha256": "c3a5b8fb4b47b2ee7814d38ff5b0809def94385c48467751ed04161495198015",
  "previous_report_id": "rrpt_01M4DH1ABVDPYYW7AG53J94MZW",
  "mode": "deterministic_only",
  "scan": {
    "files_scanned": 25,
    "files_skipped": 0,
    "bytes_scanned": 93676,
    "secret_files_recorded": 0,
    "values_redacted": 0,
    "truncated": false
  },
```

With `-format both` the markdown goes to stdout and the JSON to `-out` (or after the markdown on stdout when there is no `-out`).

## The model leg

The deterministic scan finds what rules can find. A model can read the prose of a project (a README, a wiki page, a handbook) and propose findings a rule cannot make, and write remediation text. The rules for it are strict:

- **It only proposes.** The model reads bounded, redacted excerpts of project files as **untrusted data**, with no tools, and answers in a closed schema. It can add findings and remediation text. It cannot mark a requirement
  met without verified evidence, cannot remove a deterministic finding and cannot change a status that a rule decided.
- **Every finding is checked against the file.** A deterministic verifier re-reads each cited line span and re-hashes the file. A proposal whose evidence does not confirm is `model_unverified` or dropped, and never counts
  as covered. The worst a wrong model can cause is a missing or unverified finding, never a false `found`.
- **Local by default.** The model is the controller of the `localai` and `inference` sections of your configuration, as `alixai serve` builds it. `-model ID` must name one of `inference.models`; anything else is refused
  unless the external leg below applies. With nothing configured, no model runs, and the report and a note on stderr say so. With a model configured but unusable (a model file missing, the runtime down) the leg is
  reported `unavailable`, the cause goes to stderr and no other model is tried; the deterministic report still stands and the exit status stays 0.
- **`-no-model` runs the deterministic leg alone.** Use it for a reproducible run or when no model is wanted.
- **Honest labels.** Any model used is labelled in the report. It is **`unqualified, advisory`** unless the model registry shows a qualified status (`qualified-emulated` or `certified`) for the controller role. The Lite
  controller is uncertified: today every controller entry in the registry is a candidate or rejected, so the Lite controller is always labelled unqualified, advisory.
- **Bounded.** The engine caps the calls, the files and excerpt sizes, and the tokens of one scan.

### The external model leg (opt-in, off by default)

An external (hosted) model can be used only when **all** of these hold, and every refusal is `permission_denied`, sends nothing and tries nothing else:

1. the build is not the `noexternal` build (`go build -tags noexternal ./cmd/alixai` contains none of the external-provider packages);
2. `external_providers.enabled` is true in the configuration;
3. the deployment mode is not Local (in Local mode nothing leaves, whatever the profiles say);
4. you name the model with `-model X`, and `X` is not one of `inference.models` (there is never an automatic choice of a hosted model, and no fallback to the local one after a refusal);
5. the data class of the project, taken from the environment variable `ALIXAI_READINESS_PROJECT_CLASS`, allows it. The default is `local_only`, which **the engine refuses**; the other classes are `public`, `internal`,
   `confidential` and `restricted`, and restricted content never leaves. The data-egress guard decides on every request what part of the excerpts may leave;
6. the model's profile is qualified, its terms review is fresh and its credential valid;
7. you name an operator in the environment variable `ALIXAI_READINESS_OPERATOR`, and `<application>/<operator>` is listed in `external_providers.deployment_operators`, where the application is the run's `-app` (`readiness-project` without one) (the rule of every `alixai providers` command). A missing or unlisted
   name stops the run before anything is opened, and the operator service of the deployment checks it again before the data-egress guard is asked and before the send. `ALIXAI_READINESS_TENANT` names the tenant whose
   credential and budgets are used (default `operator-cli`, the default of `alixai providers -tenant`);
8. a budget that applies to the provider on the **interactive channel** exists (for example a provider-wide one, set with `PUT /v1/providers/budgets`), in the currency of the profile's prices, and it is not exhausted.

**What the call does before and after the send.** Each model call reserves its upper-bound cost with the provider budget service before any project excerpt leaves the machine, settles the reservation with the usage the
provider reports, and gives it back when the call is not made. A provider with no applicable budget on the interactive channel is refused (`no_budget`). The kill switch is read again between the reservation and the send
(the reservation is then released), and a reply that names another model than the one the profile is pinned to is discarded and quarantines the profile. One profile is pinned for the whole run; a profile that stops being
qualified during the run ends the leg instead of being replaced. The call goes through the same router as every external call, with no local runtime and no fallback profile, so nothing else is ever tried. See
[External provider mode](../external-mode.md) for what that mode is.

## Re-run it

Reports are stored in the data directory with a hash of their inputs (the files, the requirement catalog, the scan options and the version of the readiness rules). Run the scan again whenever the project changes.

```bash
alixai readiness status internal/readiness/testdata/clinic -no-model -config alixai.yaml
```

`status` re-evaluates the checklist against the latest report and the current files. If the files, the catalog or the scan options hash differently from the report's inputs, it scans again (with the model leg when one is
configured), stores a new report and shows what changed, marking each requirement with the status it had before. If nothing changed, it shows the stored report as it is. A report made by an older version of the readiness
rules (one whose `evaluator_version` is lower than the current one, or absent) is not current either: `status` evaluates the project again with the current rules, stores a new report and says so, and the old report stays
readable with `readiness show -report ID`. Copied from the run on the `clinic` fixture (the files had not changed since the scan), the beginning of the output reads:

```text
A readiness report is advisory: it is not an integration audit pass, and the audit still decides activation.

# Integration readiness status

- Report: `rrpt_01M4MZVA2DKX8NF3MWPCN9GH5Z`
[...]
- Mode: No model ran: the report comes from the deterministic scan alone.
- The files and the catalog are unchanged since this report was stored.

## Summary

- Contradicted: 0
- Missing: 1
- Unverified: 1
- Partial: 0
- Met: 30
- Not applicable: 1

## Requirements

### Contradicted (0)

None.

### Missing (1)

- `req.rag.synthetic_documents` Synthetic documents for the smoke test (recommended), feeds the audit check `application.rag_smoke`
```

The clinic speaks MCP, so `req.native.adapter` is listed last, under `### Not applicable (1)`, not under Missing.

When something did change, the report adds `The files or the catalog changed since the previous report`, a `Changes since the previous report` section with groups such as `Newly missing`, and a marker such as `[was: Met]`
on the requirements that moved (asserted by `TestReadinessStatusShowsWhatChangedSinceTheLastScan` in `internal/cli/cmd_readiness_test.go`). `list` shows the stored reports, newest first, with their input hashes, and
`show` prints the latest report or the one named by `-report`. `list`, `show` and `status` create no database when there is none.

## How it maps to the audit checks

Every requirement names the integration audit check it feeds, so a readiness report is the checklist of what the audit will verify. The complete table, with the evidence each requirement looks for and the fix it suggests
in English and Italian, is generated from the catalog and lives in [Integration readiness requirements](../readiness-requirements.md); it is not copied here. In short:

| Audit check | Readiness requirements that feed it |
| --- | --- |
| `package.authenticity` | `req.package.manifest`, `req.package.signatures` |
| `package.documentation` | `req.documentation.capabilities`, `req.documentation.fields_and_validation`, `req.documentation.risk_and_outcomes` |
| `package.contracts` | `req.contracts.conversation`, `req.contracts.terminology` |
| `package.inventory` | `req.surface.control_surface`, `req.native.adapter` |
| `package.policy` | `req.permissions.policy_files`, `req.package.tests` |
| `package.rag_inventory` | `req.rag.source_candidates`, `req.rag.source_declarations` |
| `package.external_inventory` | `req.model.external_egress` |
| `application.control_surface` | `req.mcp.server`, `req.mcp.tool_list`, `req.openapi.description` |
| `application.deterministic_results` | `req.data.read`, `req.data.validate_prepare`, `req.data.commit` |
| `application.safe_execution`, `application.failure_degradation` | `req.data.safe_execution`, `req.data.typed_failures` |
| `application.scope_probes` | `req.scope.tenant_workspace`, `req.scope.principal_roles`, `req.permissions.model` |
| `application.rag_sources`, `application.rag_citations`, `application.rag_scope`, `application.rag_smoke` | `req.rag.live_sources`, `req.rag.citation_resolver`, `req.rag.scope_per_source`, `req.rag.synthetic_documents` |
| `application.runtime_smoke` | `req.deployment.smoke_environment` |
| `runtime.inspection` | `req.deployment.compose_and_ports`, `req.deployment.inspection`, `req.model.runtime_profile` |

Readiness checks that something is *there*; the audit checks that it *works*. A signature manifest found by readiness is verified by `package.authenticity`; documentation found by readiness is compared with the capabilities
by `package.documentation`; and the application-level checks probe the running application, which readiness never does (a live probe is off and not offered by this version). That is why `req.rag.live_sources` ("Reachable
sources, none undeclared") is always `unverified` in a readiness report: reachability is verified by the integration audit, not by readiness discovery.

**The audit link.** `alixai audit run` adds a `readiness` object to its JSON output: `linked` with the latest report id, time, mode, inputs hash and summary for the same application, `none` when no report is stored (the
note names the command that creates one), or `unavailable` when the store could not be read. The audit does not read the report, and its outcome, exit status and activation decision do not depend on it
(`TestAuditOutputLinksTheLatestReadinessReport`, `internal/cli/cmd_readiness_test.go`).

## The draft package scaffold

`-scaffold OUTDIR` writes, from the scan that has just been made, an **unsigned, quarantined draft** of a package skeleton for the application owner to finish. It uses only findings that are `found` or `partial` and whose
evidence still holds when re-read and re-hashed; the others are counted and left out. `OUTDIR` must not exist and must not be inside the scanned directory, and both are checked before any work is done.

```bash
alixai readiness scan internal/readiness/testdata/clinic -no-model -config alixai.yaml -scaffold ./draft
```

**Run** (M8-07, by hand, on the `clinic` fixture; exit 0). The draft holds ten files:

```text
alixai.yaml
capabilities/cap-001.yaml
capabilities/cap-002.yaml
documentation/cap-001.md
documentation/cap-002.md
documentation/index.yaml
DRAFT-TODO.md
rag-sources/src-001.yaml
rag-sources/src-002.yaml
rag-sources/src-003.yaml
```

and stderr reported that the draft is unsigned and that three findings were not used (`only found or partial findings whose evidence still holds become part of a draft`). The first lines of `DRAFT-TODO.md` in that run:

```text
<!-- DRAFT - generated by `alixai readiness scan --scaffold`. Stub only: no prose was generated. The application owner writes and signs the documentation. -->

# DRAFT package: what the application owner must complete and sign

This directory is a **draft**. It is not signed, it is not an installable package and it does not certify anything.
The package loader refuses it as unsigned, and an installed copy would be quarantined. A complete draft is not an audit pass:
the integration audit still decides activation. Delete this file before you sign the package.
```

What this means:

- **It is a draft, not a package.** Nothing is signed. The package loader refuses it as unsigned (`TestReadinessScanScaffoldWritesAnUnsignedDraftTheLoaderRefuses`, `internal/cli/cmd_readiness_scaffold_test.go`), and the command never installs,
  signs or activates anything.
- **No prose is generated.** The documentation files are stubs that carry the verified references. The capability stubs set no risk level and the strictest confirmation mode; the RAG source stubs default to the strictest
  classification and `local_only` egress. Every `TODO(owner)` value and every `.invalid` host must be replaced by the owner.
- **The endpoint follows the verified transport.** The one endpoint of `alixai.yaml` is `mcp`, `openapi` or `native`, whichever the verified evidence shows. A `native` endpoint names the bridge language of the cited
  file, local IPC on the placeholder address `127.0.0.1:0` (never reachable) and `local_ipc` context exchange. With no verified transport the endpoint is an `mcp` placeholder and the file says so.
- **A great deal is not generated at all.** Conversation contracts, policies, terminology, result schemas, adapter configurations and the tests (including the safety probes) are listed in `DRAFT-TODO.md` as still required.
- **Next, the usual path.** The owner completes the draft, signs it with `alixai package sign`, validates it with `alixai package validate` ([package format](../application-package.md)), and the operator installs it into
  quarantine and runs the audit ([getting started](getting-started.md), steps 7 and 8). The audit applies in full.

## Where to go next

- [Getting started](getting-started.md): readiness is step 1; the audit is step 8.
- [Operations: readiness](../../operations/readiness.md): the command reference, the data directory, the exit codes and the audit link.
- [Integration readiness requirements](../readiness-requirements.md): the generated catalog of all requirements.
- [Application package format](../application-package.md): what the draft grows into.
