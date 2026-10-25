# Getting started: from zero to an audited, active application

Status: written in task M6-05; step 1 (readiness) added in task M8-07.

This guide takes you from an empty directory to the **clinic-booking** reference application installed, audited and active in your own AlixAI, and shows how a host application then talks to it
through an SDK. clinic-booking (`examples/reference-apps/clinic-booking`) is a synthetic clinic appointment application with real business logic and a signed AlixAI package; it is a fixture, not a product.
Two people are involved, and the guide keeps them apart:

- the **operator** installs and runs AlixAI and decides what may be served (steps 1 to 9);
- the **host application** (your backend) mints a context token for each user and calls AlixAI through an SDK (step 10).

AlixAI is headless: everything below is a command line, a configuration file or an HTTP call.

**Step 1 is readiness discovery, and it is advisory.** Before an application has a package, or when its package changes, the operator can ask AlixAI what the project already covers and what the audit will want. Its report is a
checklist, not a verdict: **a green readiness report is not an audit pass**, and the audit (step 8) still decides activation. The full guide is [Integration readiness discovery](readiness.md).

## What is proven, and what is not

Each command block carries one of the states of the [deployment guides](../../operations/guides/README.md#how-to-read-the-commands-in-these-guides).

| State | Meaning here |
| --- | --- |
| **Run** | Run by hand on the dev box (Windows 11, `go run` or a binary built from this tree) in M6-05 against the files of this repository, or exercised by the cited automated test. |
| **Not run by hand** | Taken from the cited automated test, which runs it in process. The MCP end-to-end tests of `tests/e2e/m1` could not be run on the host that wrote this guide (a known issue with loopback MCP there), so the audit that talks to the running application is described from the test, not claimed as run here. |

What you should know before you start:

1. **1.0.0 is released.** SDKs and packages are versioned `1.0.0`. You can still build from source; a signed release archive is optional for this guide. Compatibility and support rules are in
   [migration and support](../../policies/migration-and-support.md).
2. **The keys in this guide are published TEST-ONLY keys.** The clinic's package is signed with `tools/devkeys`, a key anyone can sign with, and the commands need explicit `-allow-test-keys` flags to accept it. A real
   deployment signs its own packages and registers its own keys (step 7, "For a real application").
3. **No controller model is activatable yet.** The service starts with the inference path off (`inference.models: []`), so a live conversation with the clinic is not possible until the operator has qualified a model
   ([local machine guide](../../operations/guides/local-machine.md), section 1). Everything up to and including an active application, the audit trail and the SDK calls that do not need a model works without one; a host is tested
   against the SDK fakes, which script the model. The end-to-end test `tests/e2e/m1` does the same with a scripted provider.
4. **The audit is not a formality.** A package that disagrees with its application, or an application that cannot be reached, does not activate (steps 8 and 9).
5. **External providers are opt-in and off by default**, and the Lite profile is uncertified. Neither matters for this guide.

## What you need

| Need | Notes |
| --- | --- |
| Go | To build `alixai` (`CGO_ENABLED=0`, pure Go). |
| Node 22.18 or newer and pnpm | For the clinic application only (it runs TypeScript directly and uses `node:sqlite`). |
| This repository | The package is `examples/reference-apps/clinic-booking/package`. |

Commands below are written for Git Bash from the repository root, with the binary as `./alixai` and the deployment in a directory you choose, `$DEP`. From a source checkout `go run ./cmd/alixai <command>` is the same program.

## 1. Run the readiness scan (advisory)

Readiness discovery reads a project directory you name and reports, requirement by requirement, what the project already covers (MCP tools, API descriptions, operations, documentation, conversation contracts, RAG sources,
identity and permission signals, deployment facts) and which audit check each requirement feeds. It is a command line tool only: there is no `/v1` route and no SDK helper for it. The scan is read-only, bounded and never reads
a secret-looking file, and nothing is written into the scanned tree. This guide runs the deterministic leg alone (`-no-model`); how the optional model leg works is in the [readiness guide](readiness.md).

The report is stored in the data directory of the configuration you pass, under the application id you give with `-app`. Step 2 builds the binary and step 3 writes `$DEP/alixai.yaml`; if you have not done them yet,
run the same command as `go run ./cmd/alixai readiness scan ...` without `-config`, which keeps the report under `./data`, and scan again after step 3 so that the audit in step 8 finds it.

```bash
./alixai readiness scan examples/reference-apps/clinic-booking -app clinic-booking -no-model -config $DEP/alixai.yaml
```

**Run** (M8-07, by hand, on this repository's own `examples/reference-apps/clinic-booking` tree, with `-no-model` and a configuration whose only key was `data_dir`). It exited 0 and printed the report on stdout and two notes on stderr
(`the model leg is disabled by -no-model; this report comes from the deterministic scan alone`, then `stored report <id> under <key>`). The beginning, one covered requirement, the missing section and the end of the markdown
report, copied from a later run of the same command after not-applicable transports were added (`[...]` marks lines left out, and the `Generated` and `Scanned directory` lines of the run, which name the machine it ran on):

```text
A readiness report is advisory: it is not an integration audit pass, and the audit still decides activation.

# Integration readiness report

- Report: `rrpt_01M4MZV511MF7APMV7RSX1R1XD`
[...]
- Mode: No model ran: this report comes from the deterministic scan alone.
- Inputs hash: `041a60739955df17875bfeffd76ee7e4c7187194e61faf50d7e5d469d2c99525`
- Catalog: v1 `c3a5b8fb4b47`

## Summary

- Met: 28
- Partial: 1
- Missing: 1
- Unverified: 1
- Contradicted: 0
- Not applicable: 2

## Covered

Each item cites the file, the lines and the file hash a verifier re-read.

### `req.package.signatures` Signatures and file hashes

- Met (mandatory). Feeds the audit check: `package.authenticity`
- a signatures directory with a signature manifest found.
- Evidence: `package/signatures/signature-manifest.json` lines 1-1, sha256 `6bdf68a89ae8`

[...]

## Not applicable

MCP, OpenAPI and a native adapter are alternative transports. The project uses another one, so these are not needed; they are not counted as missing.

- `req.openapi.description` OpenAPI or API description (recommended). Transport in use: `req.mcp.server`, `req.mcp.tool_list`
- `req.native.adapter` Native adapter operations (recommended). Transport in use: `req.mcp.server`, `req.mcp.tool_list`

## Missing, and how to fix it

Nothing was found for these. Each says what to add.

### `req.deployment.compose_and_ports` Deployment files, migrations and ports

- Missing (recommended). Feeds the audit check: `runtime.inspection`
- Not found: compose files with published ports and migrations.
- How to fix: Commit the compose files and migrations, and keep one source of truth for ports.

[...]

## What the scan covered

- Files read: 89
- Files not read: 0
- Bytes read: 731971
- Secret-looking files recorded as present and never read: 0
- Secret-looking values redacted: 1
- The scan was not cut short by a cap.
```

How to read it, and what it does not say:

- The clinic speaks MCP, so OpenAPI and a native adapter are not applicable rather than missing. Its one missing item is recommended, not mandatory: it ships no compose file. All 16 mandatory requirements are met in this run, and **that is still not an audit pass**: step 8
  ends `needs_acceptance` (a warning remains), not clean. Readiness checks that a signature manifest is present; the audit verifies the signature. It checks that documentation exists; the audit probes the running application.
- The counts depend on the files in the tree at the time, so yours may differ if the tree has changed. The numbers above are those of the run; they are not asserted by a test. The same engine, the same report headings and the
  same exit behaviour are asserted by `TestReadinessScanProducesBilingualReportsAndStoresThem` and `TestReadinessWorksWithNoModelAndSaysSo` (`internal/cli/cmd_readiness_test.go`) over a synthetic project.
- Ask for Italian with `-lang it`, for the JSON document of `schemas/readiness` with `-format json`, and look at the stored reports with `alixai readiness list -app clinic-booking -config $DEP/alixai.yaml`.

**Next: the audit.** Steps 2 to 7 install the clinic into quarantine and step 8 audits it. Each requirement above names the audit check it feeds, so the list is a map of what step 8 will verify. If a requirement of
yours is `missing`, fix it in the project, scan again, or run `alixai readiness status` to see what changed. `alixai audit run` adds a `readiness` object to its JSON output that links your latest report for the same
application id (`linked`, `none` or `unavailable`); it never reads the report, and the outcome of the audit does not depend on it.

## 2. Build the binary

```bash
CGO_ENABLED=0 go build -o alixai ./cmd/alixai      # Windows: -o alixai.exe
```

**Run** (M6-05, by hand; the same build as the [local machine guide](../../operations/guides/local-machine.md), section 2).

## 3. Configure the deployment

Write `$DEP/alixai.yaml`. The shape below is the one `newOperator` in `tests/e2e/m1/operator_test.go` writes, plus the listener and profile of the local machine guide. **Use an absolute `data_dir`**: with only
`data_dir` relative (the key paths absolute), M6-05 found that `audit run` could not find the installed package directory ("the package directory is not known, so adapter_config cannot be read"), while the same files with an absolute path audited.

```yaml
data_dir: /absolute/path/to/dep/data       # absolute; owner-only
server:
  listen: 127.0.0.1:7470                   # loopback; another address is refused without TLS
profile: lite
identity:
  deployment_id: dep_local                 # without it the /v1 API refuses every request
  audience: alixai                         # the clinic's tokens carry aud=alixai
integration_audit:
  signing_key_id: alixai-audit-signer-2026
  signing_key_file: /absolute/path/to/dep/keys/alixai-audit-signer-2026.key
  accept_operators: [alice]                # who may accept audit warnings; empty means nobody
inference:
  models: []                               # inference path off: see "What is proven"
```

```bash
./alixai config check -config $DEP/alixai.yaml
```

**Run** (M6-05, by hand: `config ok`).

## 4. Provision the platform keys

Two platform keys exist before any application does: the checkpoint key that signs the audit-log chain heads, and the **audit signer** that signs integration-audit reports. The audit signer must not be the TEST-ONLY
key (the command refuses it). The sequence is [operator commands](../../operations/guides/operator-commands.md), section 2, and `newOperator`:

```bash
./alixai keys init -config $DEP/alixai.yaml
./alixai package keygen -key-id alixai-audit-signer-2026 -out $DEP/keys
./alixai trust add-key -config $DEP/alixai.yaml -app alixai.platform -key-id alixai-audit-signer-2026 \
  -public-key-file $DEP/keys/alixai-audit-signer-2026.pub -operator alice
```

**Run** (M6-05, by hand; `TestM1ActivationRequiresCleanAudit` does the same with generated keys).

## 5. Start the clinic application

The audit does not only read the package: it provisions disposable tenants in the running application, probes every capability through its MCP tools and cleans up. That needs the clinic's **test support** mode, and the
package pins the application at `127.0.0.1:8787`, which is the application's default port.

```bash
cd examples/reference-apps/clinic-booking
pnpm install --ignore-workspace
CLINIC_TEST_SUPPORT=1 CLINIC_DEMO_SEED=1 pnpm start
# {"event":"listening",...,"mcp":"http://127.0.0.1:8787/mcp"}
```

**Not run by hand.** The commands are the clinic README's; `tests/e2e/m1` starts the same application with `node src/main.ts`, `CLINIC_PORT=0` and `CLINIC_TEST_SUPPORT=1` (`clinic_test.go`, `startClinic`), and retargets and re-signs
the package for the port it got. Using the default port avoids that step. Test support exists only for fixtures: it also mounts the endpoint that mints context tokens with the TEST-ONLY key (step 10).

## 6. Validate the package

In a second terminal, from the repository root (the clinic keeps running in the first):

```bash
./alixai package validate examples/reference-apps/clinic-booking/package
```

**Run** (M6-05, by hand): `"ok": true`, `application_id: clinic-booking`, `critical: 0`, 45 files, two warnings. One is `adapter.operation_shared` (`resolve_patient` is mapped by two capabilities; the package says
so on purpose), the other is `signature.not_checked`, which goes away when you supply a trust anchor (step 7). [Package format](../application-package.md), section 16.

## 7. Register the keys and install into quarantine

Three things, in this order: the publisher key (who may sign the package), the issuer key (which key verifies the context tokens the host mints), then the install.

```bash
./alixai trust add-key -config $DEP/alixai.yaml -app clinic-booking -key-id alixai-test-only-publisher \
  -public-key-file tools/devkeys/test-only-publisher.pub -allow-test-keys -operator alice

./alixai identity add-issuer-key -config $DEP/alixai.yaml \
  -package examples/reference-apps/clinic-booking/package \
  -key-id alixai-test-only-publisher -public-key-file tools/devkeys/test-only-publisher.pub -allow-test-key

./alixai package install -config $DEP/alixai.yaml -operator alice examples/reference-apps/clinic-booking/package
```

**Run** (M6-05, by hand, each command against the unmodified package, whose shipped signature verifies; `addIssuerKey` and `install` in `tests/e2e/m1/operator_test.go`). The install prints `"state": "quarantined"` and
`"signed_by": "alixai-test-only-publisher"`; `alixai identity list` shows the key `active` and marked `TEST-ONLY`. An installed package is in **quarantine**: it serves nothing until an audit activates it.
The key id is the one the package's `context_exchange.key_ids` declares, and the command refuses any other.

**For a real application.** The publisher generates its own key (`alixai package keygen`), signs the package (`alixai package sign -app ID -version V -key KEYID=KEYFILE DIR`), and the operator registers
that public key with `trust add-key` and no `-allow-test-keys`. The host application keeps its own Ed25519 context-token key and the operator registers the public half with `identity add-issuer-key`
([key provisioning](../../operations/key-provisioning.md)).

## 8. Audit, accept the warnings, activate

```bash
./alixai audit run -config $DEP/alixai.yaml -operator alice clinic-booking
```

`audit run` audits the newest quarantined package, signs the report with the audit signer and **activates the package only if the audit is clear**. It prints one JSON report (`outcome`, `activated`, `summary`,
the ten `sections`, every `findings[]` with a `key`) and exits 0 only when the package is active now.

**What happens, from `TestM1ActivationRequiresCleanAudit` and `TestM1ActivationNeedsTheApplicationToAnswer` (`tests/e2e/m1/activation_test.go`):**

1. **A package that disagrees with its application is blocked.** The test renames the commit tool of `appointments.book` in the package: the audit finds `surface.unreachable_operation` as critical,
   section 7 (the complete control surface) fails, exit 1, `outcome: "blocked"`, the package stays in quarantine. **A critical finding can never be accepted**, by anyone.
2. **The clinic's own package ends `needs_acceptance`.** No critical finding; sections 1 (authenticity), 2 (deterministic results), 7, 8 (safe execution) and 9 (failure and degradation) pass; the audit really probed the
   application (disposable tenants created and cleaned up, nothing left behind); at least one **warning** remains. Exit 1, `activated: false`.
3. **Warnings are not consent.** `audit accept-warning` records that an authorised operator reviewed one warning of the latest report. An operator who is not in `integration_audit.accept_operators` is refused ("not
   authorised"); accepting activates nothing by itself.

   ```bash
   ./alixai audit accept-warning -config $DEP/alixai.yaml -finding KEY -operator alice -reason "reviewed: <why this is acceptable>" clinic-booking
   ```
   Run it once per warning, with the `key` of each finding from the report.
4. **The second audit activates.** Run `audit run` again: with every warning accepted it prints `activated: true` and exits 0. If `alixai serve` is running it says "the running server reloaded the capability registry",
   and the running service serves the application at once. The activating report records who accepted what.

An audit report older than `integration_audit.max_report_age_hours` (24 by default) is not activated on.

**Run by hand, the blocking case** (M6-05): with the clinic not running, `audit run` printed `"outcome": "blocked"`, `activated: false`, `stays_in_quarantine: "the audit found critical problems"`, five critical findings
(`probes.capability_not_probed` for `appointments.book`, `appointments.cancel` and `patients.update_contact`, plus `surface.describe_failed` and `system.health_unavailable`), sections 2, 6, 7, 8, 9 and 10 failing, and exit 1. An
application that cannot be reached cannot be audited clean. The clean path (steps 3 to 5 above) is **Not run by hand**: it needs the clinic running over loopback MCP.

## 9. Check that it is active, and read the trail

```bash
./alixai audit list -config $DEP/alixai.yaml -application clinic-booking
./alixai audit verify-chain -config $DEP/alixai.yaml -application clinic-booking
```

**Run** (M6-05, by hand, on the installed-but-quarantined deployment): `audit list` lists the application's audit events (settings, `identity.issuer_key_added`, and so on, each with a hash); `audit verify-chain` prints
`chains_ok: 1`. After an activation the chain also holds `package.installed`, `audit_run.started`, `audit_run.completed`, `audit_run.warning_accepted` and `package.activated`, in that order (asserted by
`TestM1ActivationRequiresCleanAudit`). Stop the service before reading the database file, or work on a copy ([operator commands](../../operations/guides/operator-commands.md), section 2).

Through the API, a host sees the same fact. This is the fake's answer, which has the shape of the real document (`sdk/typescript/test/client.test.ts`):

```ts
expect(await client.getApplication("fake-app")).toMatchObject({ application_id: "fake-app", status: "active", health: { status: "ok" } });
expect(await caught(client.listCapabilities())).toMatchObject({ code: "quarantined", status: 403 });
```

The first line is an active application; the second is what a quarantined one answers. The test `TestM1ActivationRequiresCleanAudit` reads the real application through the gateway: `status` is `active`, and
exactly the audited package is `active` while any other installed package is not.

To stop serving the application at once, for any reason:

```bash
./alixai audit quarantine -config $DEP/alixai.yaml -reason "why" -operator alice clinic-booking
```

A quarantined application serves nothing, including a booking that was waiting for its confirmation (asserted by the same test). Change the application, its API, its MCP server, its documentation or its permissions: build a new
package version, install it, audit again ([migration and support](../../policies/migration-and-support.md), section 2.3).

## 10. Run the service and call it from your host

```bash
./alixai serve -config $DEP/alixai.yaml
curl -fsS http://127.0.0.1:7470/healthz        # {"status":"ok"}
```

**Run** for `serve` with this configuration shape in the [local machine guide](../../operations/guides/local-machine.md), section 5; `/readyz` answers 503 while the inference path is off, which is the designed state.

**The host mints a context token for every request.** A context token is the application's proof of who the user is: a short-lived JWS signed with the key you registered in step 7, single use, naming the tenant,
workspace, principal, roles, the capabilities the token grants and the session. AlixAI verifies it and never mints one. clinic-booking's test-support mode stands in for the host's login with an endpoint; the request is
(`examples/reference-apps/clinic-booking/test/mcp.test.ts`, "mints a token for a known principal that an independent verifier accepts"):

```ts
const out = await fetchJson("POST", "/host/context-token", {
  tenant_id: "tnt_a",
  workspace_id: "site_a1",
  principal_id: "usr_reception_a1",
});
```

Those ids belong to the test's world. The demo tenant that `CLINIC_DEMO_SEED=1` creates (`src/demo.ts`) uses `tnt_demo`, `site_north` and `usr_reception_site_north`; that is read from the source, not run by hand. The answer
carries `token`, `key_id` and `max_lifetime_seconds` (900). In your own application this endpoint is your backend: authenticate the user, then sign a token with your private key. The SDK asks for a fresh token
before every request through a callback and never stores one.

**Then use an SDK.** The same flow, in the three SDKs, with samples copied from their tests, is in their READMEs: create or resume a session, send a turn (text in, typed events out), take the single-use confirmation
secret from the turn that prepared an action, confirm it with a structured call (text can never confirm), and read the receipt.

| SDK | README | Sample test |
| --- | --- | --- |
| TypeScript | [sdk/typescript/README.md](../../../sdk/typescript/README.md) | `sdk/typescript/test/client.test.ts`, `describe("actions")` |
| Python | [sdk/python/README.md](../../../sdk/python/README.md) | `sdk/python/tests/test_client.py`, `test_confirm_commits_once_and_a_second_use_of_the_secret_is_replayed` |
| Go | [pkg/sdk-go/README.md](../../../pkg/sdk-go/README.md) | `pkg/sdk-go/client_test.go`, `TestConfirmFlow` |

In the clinic, `appointments.book` and `appointments.cancel` are level 2 (explicit confirmation) and `patients.update_contact` is level 1; `availability.list` and `patients.find` are reads. The roles decide what each
user can reach (receptionist, doctor, patient), and the application reauthorises every call from its own directory. The end-to-end test `TestM1MultiTurnBookingE2E` (`tests/e2e/m1/booking_test.go`) is the whole
write workflow against the real application: a booking with a field missing is answered with a clarification, the next turn prepares the action with its exact effects, a typed "yes" confirms nothing, the structured
confirmation commits through the application's idempotent commit, and the answer states exactly the facts of the receipt. Its model is scripted.

## Where to go next

- [Integration guides index](README.md): what each SDK has, side by side.
- [CopilotKit and AG-UI](copilotkit.md): put a chat UI in front of this, from Next.js, Express, Go or Python.
- [Integration readiness discovery](readiness.md): scan your own project, read the report and scaffold a draft package.
- [Application package format](../application-package.md): write a package for your own application.
- [Operator commands](../../operations/guides/operator-commands.md), [key provisioning](../../operations/key-provisioning.md), [troubleshooting](../../operations/guides/troubleshooting.md).
- [Compatibility and deprecation](../../policies/compatibility.md) and [migration and support](../../policies/migration-and-support.md): what is frozen for v1 and what is still proposed.
