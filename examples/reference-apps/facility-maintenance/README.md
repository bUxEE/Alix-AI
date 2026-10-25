# facility-maintenance

A synthetic facility-maintenance application: the second neutral AlixAI reference application. It is a
**test fixture with real business logic**, not a product. Every name, tag and part is made up.

Where the first reference application (`clinic-booking`) is TypeScript and MCP, with many tenants and
three risk levels, this one is **Python, FastAPI and the OpenAPI/HTTP transport**, with **one tenant
and many sites**, risk levels **0 to 3**, a **mounted manuals folder** and a **history record API** as
RAG sources. It integrates only through its signed package in [`package/`](package/) and the OpenAPI
adapter: nothing in AlixAI core knows it exists.

```bash
python -m venv .venv && .venv/Scripts/python -m pip install -e ".[test]"   # .venv/bin/python on Linux and macOS
.venv/Scripts/python -m pytest                                              # hermetic: no network, no server
FACILITY_TEST_SUPPORT=1 FACILITY_DEMO_SEED=1 python -m facility_maintenance # {"event":"listening","api":"http://127.0.0.1:8788"}
```

Settings come from the environment: `FACILITY_TENANT_ID` (the deployment's one tenant),
`FACILITY_PORT`, `FACILITY_TEST_SUPPORT` (disposable tenants for the integration audit, off by
default), `FACILITY_DEMO_SEED` and `FACILITY_DEMO_PASSCODE` (synthetic demonstration data and the
supervisors' step-up passcode), `FACILITY_FIXED_NOW` (a frozen clock) and `FACILITY_API_TOKEN` (when
set, every operation also requires `Authorization: Bearer`; the package declares no credential
reference, so the adapter sends none). The application binds to loopback only.

## Tenancy and permissions

Tenant = the facility operator (a deployment serves exactly one), workspace = a site. Every call carries
the minimum identity AlixAI sends in `X-AlixAI-Identity` (`application_id`, `tenant_id`, `workspace_id`,
`principal_id`, `roles`, `session_id`); the application reauthorises it on every call from its own
directory and never trusts the roles asserted. A record of another site or tenant, or one the role may
not see, is refused as `scope_violation` exactly like one that does not exist. Identifiers are unique
across tenants, so presenting another tenant's identifier reaches nothing.

| Role | Capabilities | Records it can reach |
| --- | --- | --- |
| technician | `workorders.list`, `workorders.create`, `workorders.close` | the work assigned to them or raised by them; every asset of the site |
| supervisor | all five | every record of the site |
| auditor | `workorders.list` | every work order of the site, read-only |

## Capabilities

| Capability | Level | What happens | Operations (OpenAPI operationIds) |
| --- | --- | --- | --- |
| `workorders.list` | 0 | read the work orders the caller may see, most urgent first | `listWorkOrders` |
| `workorders.create` | 1, no separate confirmation | raise a work order against an active asset | `{validate,prepare,commit,status}CreateWorkOrder` |
| `workorders.assign` | 2, explicit confirmation | assign a technician; the technician is notified | `{validate,prepare,commit,status}AssignTechnician` |
| `workorders.close` | 2, explicit confirmation | close with a resolution, the parts used (`PART:QTY`, a flat list) and labour; stock is consumed | `{validate,prepare,commit,status}CloseWorkOrder` |
| `assets.decommission` | 3, **step-up in the application's own UI** | stage a request the supervisor completes in the UI | `{validate,prepare,commit,status}DecommissionAsset` |

Entity resolution is a read mapped to `resolveAsset`, `resolveWorkOrder` and `resolveTechnician`
(`POST` with `mention` and `candidate_id`; an empty mention matches nothing).

Business rules that produce conflicts, stale data and denials: no work against a decommissioned asset
or twice with one title, a technician takes at most 5 open work orders, a part must be in stock in the
quantity used (two closures cannot both take the last parts), a technician closes only their own work,
an asset with open work orders cannot be retired, and a prepared action expires.

## Wire contract

- **Document.** [`src/facility_maintenance/openapi.yaml`](src/facility_maintenance/openapi.yaml)
  (OpenAPI 3.0.3) is the contract and is shipped byte for byte as `package/schemas/facility-api.openapi.yaml`.
  It is hand-reviewed, not generated from the routes, and pinned by the manifest's `contract_sha256`: a
  change to an approved operation needs a new pin, a new signature and a new audit.
- **Replies.** The host members of a DeterministicResult with the HTTP status a REST host picks (200,
  401, 403, 404, 409, 422, 429, 503). A refusal is a result with status `rejected`, never a bare HTTP
  error, and a malformed body is a typed refusal too.
- **Commit is idempotent.** The key is in the `Idempotency-Key` header and echoed in the reply; a repeat
  returns the original receipt and changes nothing; the same key for another action or another principal
  is a `conflict`. `status` says "nothing recorded" in exactly one shape (`rejected`, one
  `validation_failed` on `idempotency_key`, no identifiers, HTTP 404): the only answer that makes a retry safe.
- **Validate reports the version stored from Prepare** (`freshness.version`, a digest of each relied-on
  record's own revision), so a change to one of them, and to nothing else, makes the commit `stale`.
- **Level 3.** `assets.decommission` prepares and stages a step-up request (idempotently: the same
  reference comes back). Its commit is refused as `step_up_required` once the preparation proves
  current, and its status never has a record: the effect happens only when the supervisor, signed in at
  `/ui/step-up/{reference}` with their own passcode (HTTP Basic, an anti-forgery token, three wrong
  passcodes lock the request), approves it. The UI is reachable through no approved operation.
- **Inspection, health, describe.** `GET /v1/inspect` reports all nine aspects, `GET /v1/health`,
  `GET /v1/describe`. They are management operations of the adapter configuration; no capability maps them.

## RAG sources

| Source | Type | Scope | What it holds |
| --- | --- | --- | --- |
| `equipment-manuals` | `mounted_volume` (mount `facility-manuals` = [`manuals/`](manuals/)) | the tenant, shared by every site | PDF and Markdown manuals, English and Italian |
| `maintenance-history` | `application_document_api` (`GET /rag/maintenance-history`) | the tenant and the site of the request | one Markdown record per closed work order |

The history API is the small read-only contract of AlixAI's document-API provider (keyset-paged list,
content-addressed `sha256:` versions, `GET` and `HEAD` of an item) and takes the tenant and site as query
parameters on every request. The operator registers the mount `facility-manuals` for the manuals folder.

## The package

```bash
go run ./cmd/alixai package validate examples/reference-apps/facility-maintenance/package
go run ./cmd/alixai package sign -app facility-maintenance -version 1.0.0 \
  -key alixai-test-only-publisher=tools/devkeys/test-only-publisher.key.pem \
  -signed-at 2026-10-02T00:00:00Z -expires-at 2036-10-01T00:00:00Z \
  examples/reference-apps/facility-maintenance/package
```

Signed with the TEST-ONLY key (a real trust store refuses it); sign again after every change. The tests
keep the package and the code from drifting: digests, the shipped contract and its pin, roles,
permissions, risk, time to live, and every result against its `result_schema`. They also run the
package's own probes (`tests/facility-safety.yaml`) against the application as the integration audit
does, from the same disposable-tenant operations (`createTenant`, `seedTenant`, `cleanupTenant`).

**Test support and the step-up.** The audit's safe-execution check drives a commit straight through the
adapter, which this application refuses for level 3 by design, and then completes the step-up the way a
supervisor does. For that, test support gives every supervisor of a disposable tenant a random passcode
(held as a salted hash, and in memory with the tenant for test support's own sign-in; never in a reply)
and offers a fourth operation beside the three above, `approveStepUp` (`POST
/v1/test/tenants/{tenant_id}/step-ups`, approved in `management.provisioning`, mapped by no capability).
It signs in to the step-up UI as the supervisor the request was prepared for and approves it through the
same pages, so the passcode, anti-forgery, lock, expiry and changed-asset checks apply; the effect is the
UI's. It exists only with `FACILITY_TEST_SUPPORT=1` and only for a disposable tenant: the deployment's own
tenant is refused, and nothing of it is in the production surface.

**Not part of this fixture.** It does not mint context tokens (the manifest declares the exchange the
host would use).
