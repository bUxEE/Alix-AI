# Integration readiness discovery (`alixai readiness`)

Status: implemented by M8-05 and M8-06 on the engine of M8-01 to M8-04, documented by M8-07. Decision: [ADR 0037](../decisions/0037-integration-readiness-discovery.md).

`alixai readiness` is the step before the integration audit. You name a project directory; AlixAI builds the list of what an integration needs (MCP tools, API descriptions, operations, documentation, conversation contracts, RAG sources, identity and permission signals, deployment facts) and reports which of those the project already covers. It is a command-line tool only: no `/v1` route, no OpenAPI change and no SDK helper, and it works offline against the data directory, like `alixai audit`. For the walk-through (how to read a report, how the model is used, how the requirements map to the audit checks) read the [integration guide](../integration/guides/readiness.md); the requirement catalog is [Integration readiness requirements](../integration/readiness-requirements.md).

**A readiness report is advisory.** It certifies nothing, signs nothing, activates nothing and edits no package. A complete report is not an audit pass; the audit still decides activation. Every rendering says so on its first line. Readiness never replaces the audit and is never a step the audit skips.

## Commands

```
alixai readiness scan   DIR [-config FILE] [-app ID] [-lang en|it] [-format md|json|both] [-no-model] [-model ID] [-out FILE] [-scaffold OUTDIR]
alixai readiness status [DIR] [-config FILE] [-app ID] [-lang en|it] [-format md|json] [-diff] [-no-model] [-model ID]
alixai readiness list   [DIR] [-config FILE] [-app ID] [-format md|json]
alixai readiness show   [DIR] [-config FILE] [-app ID] [-report ID] [-lang en|it] [-format md|json]
```

Flags may come before or after the directory.

- **scan** runs the deterministic scan, then the model leg when a model is configured and not disabled, stores the report and prints it: what is covered, what is missing and how to fix it, contradictions, what could not be determined, and what the model did. It exits 0 for every completed scan, whatever it found. A non-zero status means the scan itself failed (a refused directory, an unusable store, an `-out` file that cannot be written); a malformed call exits 2.
- **`-scaffold OUTDIR`** (on `scan` only) also writes an unsigned draft package skeleton from the verified findings: see [the draft scaffold](#the-draft-package-scaffold).
- **status** re-evaluates the checklist against the latest stored report and the current files. If the files, the catalog, the scan options or the version of the readiness rules hash differently from the report's inputs, it scans again (with the model leg when one is configured), stores a new report and shows what changed; if not, it shows the stored report as is. A report made by older readiness rules (after an upgrade) is evaluated again this way even when the files are unchanged, and status says so. It lists met, partial, missing, unverified, contradicted and not-applicable requirements, each marked with the status it had before when that changed. `-diff` adds the requirement-level changes against the previous report.
- **list** shows the stored reports, newest first, with their input hashes. **show** prints the latest report, or the one named by `-report`.

Reports are stored in the data directory under a key: `-app ID` when you name an application, otherwise a key derived from the directory. Use the same one for every command. `alixai audit run` looks up the `-app` key for its own application id, so scan with `-app <application id>` to have the audit link the report.

Both languages are always available (`-lang en` or `-lang it`). `-format json` is the document of `schemas/readiness`. With `-format both` the markdown goes to stdout and the JSON to `-out`; without `-out` the JSON follows the markdown on stdout.

## What it reads and writes

- The scan is read-only, bounded by file-count and size caps, skips dependency and VCS directories, follows no symbolic link and makes no network connection. Secret-looking files (`.env*`, keys, certificates, credential stores) are recorded as present and never read; secret-looking values in other files are redacted before they reach a report, the model or a log.
- It writes the report into the data directory and, with `-out`, one file you name (owner-only). Nothing is written into the scanned directory. `list`, `show` and `status` create no database when there is none.

## The model leg

A model reads bounded, redacted excerpts of the project's prose as untrusted data, with no tools, and answers in a closed schema. It can only add findings and remediation text; a deterministic verifier re-reads every cited line and hash, and a finding that does not confirm is `unverified` and never counts as covered. It cannot mark a requirement met without verified evidence and cannot change a result a rule decided.

- **Local by default.** The model is the controller of the `localai` and `inference` sections of the configuration, as `alixai serve` builds it. `-model ID` must be one of `inference.models`. Nothing configured: no model runs, and the report and a note on stderr say so. Configured but unusable (model file missing, runtime down): the leg is reported `unavailable` with the cause on stderr; no other model is tried.
- **`-no-model`** runs the deterministic leg alone and says so on stderr.
- **Labelled honestly.** A model is `unqualified, advisory` unless the model registry shows a qualified status (`qualified-emulated` or `certified`) for the controller role. Today every controller entry is a candidate or rejected, so the Lite controller is always labelled unqualified.
- **External is opt-in, off by default and absent from the local-only build.** The composition (`internal/cli/readiness_assist.go`) holds a hook that is nil by default and imports no external-provider package. Only a file built without the `noexternal` tag may fill it, and `go build -tags noexternal ./cmd/alixai` stays free of those packages. An external model is used only when **all** of these hold: `external_providers.enabled` is true, the deployment mode is not Local, you name the model with `-model X` and `X` is not one of `inference.models`, you name yourself as an operator, and the data class of the project allows it. Any refusal is `permission_denied`, sends nothing and tries nothing else: there is no fallback to another model or provider, and no fallback to the local one.
  - **Operator.** Set `ALIXAI_READINESS_OPERATOR` to a principal whose `<application>/<operator>` entry is listed in `external_providers.deployment_operators`, where the application is the run's `-app` (`readiness-project` without one), the rule of every `alixai providers` command. A missing or unlisted name stops the run before the store is opened; the operator service of the deployment checks it again before the guard is asked and before the send. `ALIXAI_READINESS_TENANT` names the tenant whose credential and budgets the leg uses; the default is `operator-cli`, the default of `alixai providers -tenant`.
  - **Class.** `ALIXAI_READINESS_PROJECT_CLASS` is `public`, `internal`, `confidential` or `restricted`; the default is `local_only`, which the engine refuses, and restricted content never leaves. The data-egress guard decides on every request what part of the excerpts may leave.
  - **Budget.** Before a prompt leaves, the call reserves its upper-bound cost with the provider budget service (the estimate floor of the profile's price table), settles it with the usage the provider reports, and gives the reservation back when the call is not made. A budget that applies to the provider on the **interactive channel** (for example a provider-wide one, set with `PUT /v1/providers/budgets`) must exist, in the currency of the profile's prices, or the call is refused with the reason `no_budget`. An exhausted budget refuses too. The assist engine's own caps on calls, files and tokens still bound one scan.
  - **Kill switch and the rest of the route.** The call runs through the same router as every external call: mode, qualified and fresh profile (stale terms quarantine it), data class, valid credential, guard, reservation, the kill switch read again between the reservation and the send, and a check that the reply names the model the profile is pinned to. One profile is pinned for the whole run; the router is given no local runtime and no fallback profile.

  See [External provider mode](external-mode.md).

## Audit link

`alixai audit run` adds a `readiness` object to its JSON output: `linked` with the latest report id, time, mode, inputs hash and summary for the same application, `none` when no report is stored (with the command that creates one), or `unavailable` when the store could not be read. The audit does not read the report, and its outcome, exit status and activation decision do not depend on it.

## The draft package scaffold

`alixai readiness scan DIR -scaffold OUTDIR` writes, after the scan, a **draft** package skeleton into `OUTDIR` from the findings that are `found` or `partial` and whose evidence still holds when re-read and re-hashed (the others are counted on stderr and left out). `OUTDIR` must not exist and must not be inside the scanned directory; both are checked before any work is done, and a refusal writes nothing and stores no report.

- The draft is **unsigned and quarantined**: the package loader refuses it as unsigned, and the command never installs, signs or activates anything. A complete draft is not an audit pass; the audit still decides activation.
- No prose is generated. Documentation files are stubs that carry verified references; capability stubs set no risk level and the strictest confirmation mode; RAG source stubs default to the strictest classification and `local_only` egress.
- `DRAFT-TODO.md` in the draft lists what the application owner must complete (`TODO(owner)` values, `.invalid` hosts) and what is not generated at all (conversation contracts, policies, terminology, schemas, tests). The owner completes it, signs it with `alixai package sign` and checks it with `alixai package validate`; only then can the operator install it into quarantine and audit it.
- The scan itself still writes only to the data directory and `-out`; the draft goes to `OUTDIR` only.

## Not offered

- A `/v1` route and SDK helpers for readiness: the command line only.
- A live probe of the running application: off, and not offered by this version. The scan makes no network connection.
- Model-written package documentation: the scaffold carries verified facts only; the prose of a package is the application owner's signed work.
