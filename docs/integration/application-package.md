# Application Integration Package

The package format every application supplies to be integrated with AlixAI. This document is for
integrators: what files a package holds, what every field means, which rules tie the files together
and how a package becomes trusted. The machine-readable contract is the set of JSON Schemas in
[`schemas/application-package/`](../../schemas/application-package/); where this document and a
schema disagree, the schema wins and this document has a bug.

Schema area:
`application-package`, ids `urn:alixai:schema:application-package:<name>:v1`.

## 1. What a package is, and what it is not

An Application Integration Package is a **signed, versioned directory of declarations**. It tells
AlixAI what an application can do, how to reach it, who may do what, how to talk about it and which
documents may be searched. It is the single source from which AlixAI builds its capability registry,
and it is what the integration audit checks before the application can receive live
traffic.

Four principles shape every field:

1. **Deterministic authority.** The package, not the model, decides what exists. The model may
   propose a capability and candidate field values; the package defines the capability, its fields,
   its risk, its operations and its confirmation rule, and the application validates every value.
2. **Deny by default.** Nothing is implied. A capability exists only if the manifest lists it; a
   role may use it only if a policy grants it; a network destination is reachable only if the
   manifest allows it; a source exists only if it is declared. Wildcards do not exist anywhere in
   the format: not in capability IDs, roles, tenants, hosts or MIME types.
3. **Nothing is guessed.** Documentation, a conversation contract and a failure response are
   required per capability. A missing piece fails that capability; it is never an instruction for
   the model to improvise.
4. **Signed and quarantined.** An installed package is in quarantine until its signature verifies
   and the integration audit passes. A package cannot make AlixAI run a process, reach a remote
   host that is not allowlisted, name a model, or hold a secret.

A package does **not** contain business logic, credentials, model files, user data or prompts.
Examples inside it are test-only.

**Analyst domain (1.5, ADR 0047).** Optional additive `assistant` on the manifest declares
`domain` (in/out of scope, knowledge/web allowances) and optional ability ceilings. See
[domain-scope.md](domain-scope.md) and `schemas/analyst/domain.schema.json`. Absent `assistant.domain`
keeps domain classification and knowledge/web off for that application.

**Before you write a package: readiness discovery (advisory).** `alixai readiness scan <dir>` reads a project
directory you name and reports which of the things a package declares (and which the audit later checks) the
project already has: MCP tools, API descriptions, operations, documentation, conversation contracts, RAG
sources, identity and permission signals, deployment facts. It is a command-line tool only (no `/v1` route,
no SDK helper) and **advisory**: it never certifies, signs, activates or edits a package, and **a green readiness
report is not an audit pass**; the integration audit still decides activation. With `-scaffold OUTDIR` it can
write an unsigned draft skeleton from verified findings, which the package loader refuses until the owner has
completed and signed it. See [Integration readiness discovery](guides/readiness.md), the generated
[requirement catalog](readiness-requirements.md) and [operations: readiness](../operations/readiness.md).

## 2. Package layout

```text
application-package/
  alixai.yaml                      the manifest                       manifest
  capabilities/                    one file per capability            capability
  conversation-contracts/          one file per capability+language   conversation-contract
  policies/                        host policy                        policy
  terminology/                     vocabulary                         terminology
  documentation/
    index.yaml                     the documentation index            documentation-index
    ...                            the documents the index points at (Markdown, HTML, text, PDF)
  schemas/                         the application's own contracts: result schemas, OpenAPI
                                   document, MCP tool snapshot, adapter configuration files
  rag-sources/                     one file per document source       rag-source
  tests/                           test suites and seed data          package-tests
  signatures/
    signature-manifest.json        digests and detached signature     signature-manifest
```

| Path | Validated against | Rules |
| --- | --- | --- |
| `alixai.yaml` | [`manifest`](../../schemas/application-package/manifest.schema.json) | Exactly one, at the package root. |
| `capabilities/<name>.yaml` | [`capability`](../../schemas/application-package/capability.schema.json) | Each file is listed in the manifest `capabilities` map under the ID it declares. A file that is not listed is an error, not an ignored file. |
| `conversation-contracts/*.yaml` | [`conversation-contract`](../../schemas/application-package/conversation-contract.schema.json) | File names are free; the pair (`capability_id`, `language`) is what identifies a contract. |
| `policies/*.yaml` | [`policy`](../../schemas/application-package/policy.schema.json) | One policy per file; `policy_id` unique. |
| `terminology/*.yaml` | [`terminology`](../../schemas/application-package/terminology.schema.json) | Term IDs unique across all files. |
| `documentation/index.yaml` | [`documentation-index`](../../schemas/application-package/documentation-index.schema.json) | Fixed name. The documents it points at live below `documentation/`. |
| `schemas/**` | not validated by these schemas | Result schemas, an OpenAPI document, adapter configuration. Referenced by path from the manifest and the capabilities. |
| `rag-sources/<name>.yaml` | [`rag-source`](../../schemas/application-package/rag-source.schema.json) | Each file is listed in the manifest `rag_sources` map under its source ID. |
| `tests/*.yaml` | [`package-tests`](../../schemas/application-package/package-tests.schema.json) | One suite per file; `suite` unique. Seed data files sit below `tests/`. |
| `signatures/signature-manifest.json` | [`signature-manifest`](../../schemas/application-package/signature-manifest.schema.json) | Generated by the signing tool. |

All structured files are **authored as YAML** and the schemas validate the **parsed JSON form**.
The shared value types the schemas use (identifiers, paths, versions and so on) live in
[`defs.schema.json`](../../schemas/application-package/defs.schema.json), a definitions library that
constrains nothing on its own.

### Authoring rules for the YAML files

A package file is plain data. The loader refuses, rather than interprets, anything else:

- UTF-8 text, LF line endings, at most one YAML document per file.
- Mapping keys are strings and unique within their mapping. A duplicate key is an error, never a
  silent override.
- No anchors, aliases, merge keys (`<<`) or custom tags. They make a file mean something other than
  what it says and enable expansion attacks.
- Scalars follow the YAML 1.2 core schema: quote anything that must stay a string (`"2026-11-13"`,
  `"no"`, `"1.0"`).
- Every object is closed: a property the schema does not define is an error
  (`additionalProperties: false`), so a typo or an invented field cannot pass unnoticed.

### Paths

Every path in a package is **package-relative**, uses forward slashes, has no leading slash, no
drive letter and no empty, `.` or `..` segment, and no segment that starts with a dot. Each segment
starts with a letter, digit or underscore and continues with letters, digits, `.`, `_` or `-`. A
path that fails this grammar is rejected before any file is opened, so a package cannot point
outside itself. Symbolic links are not allowed anywhere in a package.

## 3. The minimal package

[`examples/`](../../schemas/application-package/examples/) holds, for every schema, a `valid/` and
an `invalid/` folder; `go run ./tools/schemacheck schemas/application-package` proves every valid
file passes and every invalid file is rejected. The valid files of the schemas together form one
complete, coherent minimal package for a synthetic library desk, `demo-library` (the layout of
`schemacheck` does not allow a directory tree of YAML there, so each file is the parsed JSON form
of the file named in the table):

| Example (under `schemas/application-package/examples/`) | Package file it stands for |
| --- | --- |
| `manifest/valid/minimal.json` | `alixai.yaml` |
| `capability/valid/catalog-find-title.json` | `capabilities/catalog-find-title.yaml` (risk 0, read-only) |
| `capability/valid/reservations-create.json` | `capabilities/reservations-create.yaml` (risk 2, prepare, confirm, commit) |
| `conversation-contract/valid/catalog-find-title.en.json`, `.it.json` | `conversation-contracts/catalog-find-title.{en,it}.yaml` |
| `conversation-contract/valid/reservations-create.en.json`, `.it.json` | `conversation-contracts/reservations-create.{en,it}.yaml` |
| `terminology/valid/library-terms.json` | `terminology/library-terms.yaml` |
| `policy/valid/default-access.json` | `policies/default-access.yaml` |
| `documentation-index/valid/library-docs.json` | `documentation/index.yaml` (26 entries covering every facet of both capabilities) |
| `rag-source/valid/loan-policies.json` | `rag-sources/loan-policies.yaml` |
| `package-tests/valid/core-safety.json` | `tests/core-safety.yaml` |
| `signature-manifest/valid/demo-library-signed.json` | `signatures/signature-manifest.json` |

The files agree with each other (same application ID, capability IDs, endpoint IDs, language set,
documentation IDs, outcome IDs, failure codes, fields and facts) and satisfy the cross-file rules of
section 14; the loader fixtures of task M1-04 should start from this package. The other valid examples are variants that show other shapes (an
all-transports manifest, a level 1 capability that commits immediately, a level 3 step-up
capability, further source types, a second policy); they are not part of the minimal package.

The manifest of the minimal package, in its authored YAML form (abridged):

```yaml
format: alixai.application-package/v1
application: { id: demo-library, name: Demo Library Desk, version: "2.4.0" }
package: { version: 1.0.0 }
compatibility:
  alixai_core: { min: 1.0.0, max_exclusive: 2.0.0 }
endpoints:
  library-mcp:
    transport: mcp
    url: https://library.example.test/mcp
    adapter_config: schemas/adapters/library-mcp.yaml
    credential_ref: cred:demo-library-mcp
context_exchange:
  mode: context_token
  issuer: https://library.example.test
  audience: alixai
  key_ids: [demo-library-ctx-2026]
  max_lifetime_seconds: 600
tenancy:
  tenant: { label: library network, description: "..." }
  workspace: { label: branch, description: "..." }
  principal_kinds: [human]
  roles:
    member: { description: "..." }
    visitor: { description: "..." }
languages: { default: en, supported: [en, it] }
capabilities:
  catalog.find_title: capabilities/catalog-find-title.yaml
  reservations.create: capabilities/reservations-create.yaml
rag_sources:
  loan-policies: rag-sources/loan-policies.yaml
model_requirements:
  minimum_profile: lite
  controller: { need: required, languages: [en, it], min_context_tokens: 4096 }
  embedding: { need: optional }
retention: { session_state_days: 7, transcript_days: 30, action_receipt_days: 365, audit_days: 730, log_days: 14 }
network:
  default: deny
  egress:
    library-api: { host: library.example.test, ports: [443], scheme: https, purpose: Library MCP server }
integrity:
  signature_manifest: signatures/signature-manifest.json
  hash_algorithm: sha256
  signature_algorithm: ed25519
  publisher_key_ids: [demo-library-publisher-2026]
```

The read-only capability, abridged:

```yaml
id: catalog.find_title
version: 1.0.0
audience: user
title: Find a title
purpose: Search the catalogue of the user's branch for titles that match some words.
scope_level: workspace
permissions: [catalog.read]
fields:
  - { name: query, type: string, required: true, description: Words to search for., min_length: 2, max_length: 200 }
  - { name: available_only, type: boolean, required: false, description: Only titles available now., default: { source: literal, value: false } }
entity_types: {}
ambiguity: []
validation:
  - { id: query_not_blank, fields: [query], description: At least two characters., error_code: validation_failed }
failure_states:
  - { code: validation_failed, description: The query is blank or too short. }
  - { code: unavailable, description: The catalogue cannot be reached. }
effect_class: none
risk_level: 0
operations:
  read: { endpoint: library-mcp, mcp_tool: find_title, result_schema: schemas/find-title.result.schema.json }
confirmation: { mode: none }
response_obligations:
  - { fact: search.count, when: success, description: How many titles matched. }
next_actions:
  - { id: reserve_found_title, after: success, capability_id: reservations.create }
doc_refs: [cft-field-query, cft-risk]   # abridged: the example lists all 11 entries
examples:
  - { id: en-gardening, language: en, test_only: true, utterance: Do you have any books about gardening?, expected_fields: { query: gardening } }
```

## 4. Conventions that hold everywhere

- **Required means required.** Every property is required unless its row says otherwise. There is
  no default for a missing declaration, and an absent `risk_level` is never read as 0.
- **Closed objects.** Unknown properties are errors. Open maps (registries keyed by ID) constrain
  their keys instead.
- **Registries are maps, ordered lists are arrays.** Endpoints, capabilities, RAG sources, roles,
  egress entries, event types and terminology labels are maps keyed by their ID, so an ID cannot
  appear twice. Things whose order matters (the fields of a capability, the turns of a test) are
  arrays.
- **Identifiers.** Application, capability, endpoint, source, rule and outcome IDs are lower-case
  slugs; capability IDs and fact paths are dotted lower-snake (`reservations.create`,
  `reservation.pickup_date`). Tenant, workspace, principal and role IDs are opaque host-chosen
  identifiers with no wildcard (`schemas/common`).
- **Error codes** are the platform taxonomy of `schemas/common/error.schema.json`. Failures that
  AlixAI produces by itself (`scope_missing`, `unauthenticated`, `scope_violation`,
  `unknown_capability`, `confirmation_required`, `step_up_required`, `quarantined`, and similar) have
  platform wording and need not be declared; declare every failure the application's operations can
  return and any core failure you want custom wording for.
- **Risk levels** are the integers 0 to 3 of the action safety levels.
- **Secrets never appear.** A credential is an opaque reference (`cred:...`); the grammar is
  deliberately too narrow for a pasted key, token or password.

### Shared value types

| Value type | Rule |
| --- | --- |
| slug (`slug`) | A lower-case identifier chosen by the package author for an endpoint, rule, outcome, term, source or similar object: starts with a letter, then letters, digits, underscore or hyphen, at most 64 characters. |
| dotted id (`dottedId`) | One to four dot-separated lower_snake segments, for example catalog.find_title or reservation.pickup_date. Used for capability IDs and for the paths of canonical facts in a deterministic result. |
| capability id (`capabilityId`) | The stable ID of a capability: an exact-match common capability ID that is also a dotted lower-snake identifier. There are no wildcards. |
| fact path (`factPath`) | The dotted path of one canonical value in a deterministic result, for example reservation.pickup_date. |
| application id (`applicationId`) | The host application ID: a common identifier restricted to a portable slug, so it is safe in file names and logs. It is the application_id of every Scope the application's tokens carry. |
| doc id (`docId`) | The ID of one documentation index entry: lower-case, starts with a letter, then letters, digits, dot, underscore or hyphen, at most 64 characters. Unique across the whole index. |
| permission id (`permissionId`) | The name of a permission the host application checks before it runs an operation (it reauthorizes every read, prepare and commit). Lower-case, may contain dot, colon, underscore and hyphen. |
| semver (`semver`) | A semantic version 2.0.0 string: MAJOR.MINOR.PATCH with optional pre-release and build metadata. |
| language tag (`languageTag`) | A BCP 47 tag in canonical case: a lower-case 2 or 3 letter language, an optional Title-case 4 letter script and an optional upper-case 2 letter or 3 digit region (en, it, pt-BR, zh-Hant-TW). |
| relative path (`relativePath`) | A portable path inside the package: forward slashes only, no leading slash, no drive letter, no empty, '.' or '..' segment and no segment starting with a dot. Each segment starts with a letter, digit or underscore and continues with letters, digits, dot, underscore or hyphen. |
| schema path (`schemaRef`) | A package-relative path below schemas/ with an optional JSON Pointer fragment (schemas/reservation.result.schema.json or schemas/openapi.yaml#/components/schemas/Reservation). It names a JSON Schema, an OpenAPI document or a similar machine-readable contract shipped in the package. |
| sha-256 hex (`sha256`) | A SHA-256 digest as 64 lower-case hexadecimal characters. |
| credential ref (`credentialRef`) | An opaque ID that names a credential held in the OS secret store or an approved local secret provider. It is never the credential: the cred: prefix, the lower-case-only alphabet and the 64-character cap exist so that a pasted key, token or password does not fit the grammar. AlixAI resolves it outside model context. |
| operation name (`operationName`) | The name of one operation on an adapter: an MCP tool name, an OpenAPI operationId or a native bridge method name. |
| adapter call (`adapterCall`) | Names one operation on one declared endpoint. Exactly one of mcp_tool, openapi_operation_id or native_method is present; its kind must match the transport of the endpoint. There is no events variant: events never trigger an AI commit. |
| risk level 0-3 (`riskLevel`) | The action safety level as a bare integer: 0 read-only, 1 reversible low-impact, 2 consequential or external side effect, 3 high-risk, privileged or legally sensitive. Always required where it appears: the absence of a level is never read as 0. Mirrors pkg/contracts.RiskLevel. |
| error code (`errorCode`) | A code of the platform error taxonomy (schemas/common/error.schema.json). |
| host name (`hostname`) | A lower-case DNS host name or IPv4 address, exact: no wildcard, no scheme, no port, no path. |
| http(s) URL (`httpUrl`) | An absolute http or https URL without user information (credentials never travel in a URL; use a credential reference). |
| label (`label`) | A single-line human-readable label: 1 to 200 characters, no control characters. |
| text (`text`) | Human-readable prose: 1 to 2000 characters. Line breaks and tabs are allowed; other control characters are not. |
| template (`template`) | Fixed wording with {fact.path} placeholders that AlixAI fills from deterministic facts: the canonical values of an operation result, or, under the reserved prefix input., the validated field values collected so far (for example {input.query}). 1 to 1000 characters. Literal braces are written {{ and }}. A result placeholder must name a fact the capability declares in its response_obligations or result schema; an input placeholder must name one of its fields. |
| example value (`exampleValue`) | A scalar, a flat list of scalars or a flat object of scalars. No deeper nesting. |
| map: field name to example value (`exampleFields`) | Field values keyed by capability field name. |
| days 0-3650 (`retentionDays`) | A whole number of days, 0 to 3650. 0 means the data is not retained beyond the operation that needs it. |
| glob (`globPattern`) | A glob relative to a source root: no leading slash, no backslash, no '..' segment, at most 256 characters. * matches within a segment and ** across segments. |
| MIME type (`mimeType`) | A concrete lower-case media type such as text/markdown or application/pdf. No wildcards and no parameters. |

An adapter call is an object with `endpoint` (required slug) and exactly one of `mcp_tool`, `openapi_operation_id` or `native_method` (each an operation name).

## 5. The manifest (`alixai.yaml`)

The manifest is the root of the package and the registry from which everything else is reached.

### Integration contract requirements to fields

| Requirement | Field |
| --- | --- |
| application ID and version | `application.id`, `application.version` |
| integration package version | `package.version` |
| service endpoints and transports | `endpoints` (`mcp`, `openapi`, `native`, `events`, each with an `adapter_config` reference) |
| authentication/context exchange | `context_exchange` (`issuer`, `audience`, `key_ids`, `mode`, `max_lifetime_seconds`) |
| tenant and workspace model | `tenancy` (`tenant`, `workspace`, `principal_kinds`, `roles`) |
| supported languages | `languages` |
| capabilities and owning adapters | `capabilities` (ID to file); the owning adapter of each operation is `operations.<op>.endpoint` in the capability file |
| RAG source definitions | `rag_sources` (ID to file) and the `rag-source` files |
| model/profile requirements | `model_requirements` |
| data-retention defaults | `retention` |
| network permissions | `network` (`default: deny` and the `egress` allowlist) |
| compatibility range | `compatibility.alixai_core` (`min` inclusive, `max_exclusive`) |
| package signature and integrity metadata | `integrity` (points at the signature manifest) |

### Transports

- **`mcp`**: preferred for capability discovery and typed tool exchange. AlixAI filters and wraps
  the individual tools that capabilities map to and never gives a model the whole server. The
  connection is HTTP(S); a package cannot make AlixAI spawn a local process (there is no `stdio`
  transport). `contract_sha256` optionally pins the approved tool list.
- **`openapi`**: an approved subset of an HTTP API; `spec` is the OpenAPI document below
  `schemas/`. Operations that are not in it are unreachable.
- **`native`**: a language-specific bridge in the host application over local IPC only: a Unix
  socket path, a Windows named pipe or a loopback TCP port. There is no remote address form.
- **`events`**: receives application events that invalidate state, refresh projections or notify a
  session. An event type's `effect` has exactly three values and none of them executes anything:
  events never trigger an AI commit, and an events endpoint cannot be the target of a capability
  operation.

MCP and OpenAPI are transports and schemas. They do not replace AlixAI policy, application
authorization or the prepare/confirm/commit protocol.

`adapter_config` names a file below `schemas/` that configures the adapter for that endpoint. The
format of those files belongs to the adapters, not to this contract; the file is signed with the
package like any other.

### Context exchange

The manifest states who will issue context tokens (`issuer`), for whom (`audience`) and with which
registered keys (`key_ids`). The token format and algorithm are chosen by AlixAI core (ADR-0013),
not by the package. `max_lifetime_seconds` can only be 900 or lower. The
application reauthorizes every read, prepare and commit; AlixAI passes it the minimum identity.

### Network

`network.default` is always `deny`. `network.egress` lists the only allowed destinations, by exact
host: no wildcard host exists, `http` is accepted only to `localhost` or `127.0.0.1`, and an empty
map allows nothing. Every host an MCP, OpenAPI or source URL uses must have an entry, loopback hosts
included (exact host, port and scheme), so the operator sees every loopback port a package asks for.
The platform's own ports (the gateway listener, the operator admin interface, the metrics listener and
the LocalAI base URL) are refused at the dialer even when an entry lists them. A native `loopback_tcp`
address needs no entry; the same deny-list covers it. RAG sources refer to egress entries by ID.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `format` | const `"alixai.application-package/v1"` | yes | The package format and its major version. A loader that does not know this value refuses the package. |
| `application` | object (see `application`) | yes | Identity of the host application (application ID and version). |
| `package` | object (see `package`) | yes | Identity of this integration package (integration package version). The package version changes whenever any file in the package changes; the application version is independent. |
| `compatibility` | object (see `compatibility`) | yes | Which AlixAI core versions this package works with (compatibility range). |
| `endpoints` | map: slug to object (see `endpoints.<id> (one variant per transport)`) (min 1) | yes | The service endpoints and transports AlixAI may use to reach the application, keyed by endpoint ID. At least one. Capabilities and RAG sources refer to these IDs; an endpoint nothing refers to is still inventoried and audited. |
| `context_exchange` | object (see `context_exchange`) | yes | How the host application proves a user's identity to AlixAI (authentication and context exchange). |
| `tenancy` | object (see `tenancy`) | yes | What tenant and workspace mean in this application and which roles it asserts. |
| `languages` | object (see `languages`) | yes | The languages the application supports. Every conversational capability needs a conversation contract in each of them. |
| `capabilities` | map: capability id to string (min 1) | yes | The capability registry (capabilities): capability ID to the package-relative capabilities/ file that defines it. This list is the deny-by-default registry: a file under capabilities/ that is not listed here, or a listed ID whose file declares a different ID, fails validation. At least one. |
| `rag_sources` | map: slug to string | yes | The RAG source registry (RAG source definitions): source ID to the package-relative rag-sources/ file that declares it. May be empty for an application without document sources; a source not listed here is undeclared and is reported by the audit. |
| `model_requirements` | object (see `model_requirements`) | yes | What the application needs from the model runtime (model/profile requirements). Roles and qualities only: a package never names a model file, a provider or a runtime. |
| `retention` | object (see `retention`) | yes | Data-retention defaults. The operator may override them per application and tenant; the package states what it expects. |
| `network` | object (see `network`) | yes | Network permissions: outbound access is denied unless an entry here allows it. |
| `integrity` | object (see `integrity`) | yes | Where the package signature and integrity metadata live. |
| `security_domain` | slug | no | Optional. The security domain the application belongs to (ADR-0016): a label for a set of applications whose operators accept the same trust level and blast radius. Applications that share one AlixAI process must declare the same security domain; a registration whose domain differs from the one the deployment already serves is refused by the registration step (the loader only records the value). Absent means the application declares no domain, which the registration step treats as a domain of its own. It is a label chosen by the operator, never a secret and never a grant. |
| `external` | object (see `external`) | no | Optional. What the application says about external model providers. The mode itself (Local, External, Hybrid) is deployment and tenant policy, never a package field, so switching mode never changes the integration contract. The package can only restrict. |

#### `external`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `forbid` | boolean | no | Optional, default false. true: nothing of this application may be sent to an external provider, whatever the deployment, tenant or workspace policy allows. A package with `forbid: true` cannot declare a RAG source with `egress_mode` `allow_excerpt` or `allow_metadata` (the loader refuses the contradiction). |

#### `application`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | application id | yes | The application ID. It must equal the application_id claim of every context token the application issues. |
| `name` | label | yes | Human-readable application name. |
| `version` | string | yes | The host application's own version, for audit records. Not required to be semver: 1 to 64 letters, digits, dot, underscore, plus or hyphen. |
| `vendor` | label | no | Optional name of the organisation that publishes the application. |
| `description` | text | no | Optional short description of what the application does. |

#### `package`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `version` | semver | yes | The integration package version (semver). |
| `description` | text | no | Optional note on what changed in this package version. |

#### `compatibility`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `alixai_core` | object | yes | The AlixAI core versions the package was written and tested for: min inclusive, max_exclusive exclusive. Both bounds are required, so a package can never claim compatibility with every future core. An installed core outside the range rejects the package. |

#### `compatibility.alixai_core`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `min` | semver | yes | Lowest compatible core version (inclusive). |
| `max_exclusive` | semver | yes | First core version that is NOT compatible (exclusive upper bound). Must be greater than min. |

#### `endpoints.<id> (transport mcp)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `transport` | const `"mcp"` | yes | Selects this variant. |
| `url` | http(s) URL | yes | Absolute http(s) URL of the MCP server. Its host and port must have an entry in network.egress, loopback included. |
| `adapter_config` | schema path | yes | Package-relative path (below schemas/) of the adapter configuration file for this endpoint. Its format is owned by the MCP adapter. It is signed with the package like every other file. |
| `contract_sha256` | sha-256 hex | no | Optional SHA-256 of the approved tool list (names and input schemas) that the capabilities map to. It is the lower-case hex SHA-256 of the canonical JSON array of `{name, input_schema}` of the mapped tools of the endpoint, sorted by name (`mcp.ContractSHA256` computes it). When present, a discovered tool list that hashes differently is an audit finding, not a silent update. |
| `credential_ref` | credential ref | no | Optional credential reference AlixAI uses to authenticate to the server over streamable HTTP / SSE. Resolved by the composition root (`application_credentials`); see [Authenticating AlixAI's calls into your application](guides/light-integration.md#authenticating-alixais-calls-into-your-application). |

##### The adapter configuration of an MCP endpoint

The file `adapter_config` names is YAML. Unknown members are refused. Version 1:

```yaml
server: clinic-mcp        # optional; when present it must be this endpoint's id
auth:                     # optional; only with credential_ref (defaults Authorization / Bearer)
  header: Authorization
  scheme: Bearer          # "" sends the credential as it is
tools:
  allow: [availability_list, appointment_prepare, appointment_commit]
```

| Member | Meaning |
| --- | --- |
| `server` | Optional. The endpoint the file configures; when present it must be the endpoint's ID in the manifest, so a file cannot be pointed at another server. |
| `auth` | Optional. How `credential_ref` is sent on HTTP/SSE (`header` default `Authorization`, `scheme` default `Bearer`). Ignored for stdio (packages cannot declare stdio). |
| `tools.allow` | The tools of the server the package approves, as exact tool names (`A-Z a-z 0-9 _ . -`, at most 128 characters, none twice). An absent or empty list approves nothing. |

`tools.allow` must list every tool the capabilities map on this endpoint; a mapped tool it does not list is an error when the adapter
is built. It may also name tools that no capability maps: those are approved but uncallable through the adapter contract, so a model can never
reach them and the audit does not report them as undocumented. The tools of `fixtures.provisioning` (section 12) are the example, and they
must be listed here to be used. A tool the server offers that is neither mapped, approved nor a management tool is reported by the audit as
an undocumented or over-broad operation. A server may offer the management tools `describe`, `health` and `inspect`; they are a convention
of the MCP adapter, no capability maps them, they need no entry and a model never sees them. A server without them is still usable (its
contract version is the adapter's own, its health is the answer to an MCP ping, and every inspection aspect is `unverified`).

##### How `contract_sha256` is computed

`contract_sha256` pins the approved tool list of one endpoint as a whole. To compute it:

1. Take every tool the capabilities of the package map on this endpoint: the `mcp_tool` of each `read`, `validate`, `prepare`, `commit` and
   `status` operation and of each entity type's `resolve` operation whose `endpoint` is this one, each tool once. Management tools and the
   provisioning tools of section 12 are not part of it.
2. For each, take its name and its `inputSchema` exactly as the server lists them (`tools/list`), the schema as a JSON value.
3. Build the array of `{"name": <name>, "input_schema": <schema>}` objects, sorted by name (byte order).
4. Serialise it as canonical JSON: no whitespace between tokens, object members sorted by key at every level (so each entry is written
   `{"input_schema": ..., "name": ...}`), UTF-8, no escaping of `<`, `>` and `&`, numbers in their shortest form.
5. The value is the lower-case hex SHA-256 of those bytes.

The order the server lists its tools in, the order of the members of a schema, and everything that is not a name or an input schema (titles,
descriptions, annotations, output schemas) do not change it. `mcp.ContractSHA256` in `adapters/mcp` computes it; in JavaScript:

```js
const canon = (v) => Array.isArray(v) ? v.map(canon):
v !== null && typeof v === 'object' ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, canon(v[k])])) : v;
const approved = mapped.map((t) => ({ name: t.name, input_schema: t.inputSchema })).sort((a, b) => (a.name < b.name ? -1 : 1));
const contractSha256 = createHash('sha256').update(JSON.stringify(canon(approved))).digest('hex');
```

A server whose tools no longer hash to the pin (a tool added to the approved set, one gone, one whose input schema changed) is drift: the adapter
refuses every mapped tool of the endpoint (`quarantined`, reason `tool_schema_drift`) until the package is audited again, and the audit's own
calls, provisioning included, refuse as well. Independently of `contract_sha256`, AlixAI pins each tool it calls by the hash of its name,
input schema and output schema, and a tool that differs from its pin is refused the same way.

#### `endpoints.<id> (transport openapi)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `transport` | const `"openapi"` | yes | Selects this variant. |
| `base_url` | http(s) URL | yes | Absolute http(s) base URL of the API. Its host and port must have an entry in network.egress, loopback included. |
| `spec` | schema path | yes | Package-relative path (below schemas/) of the OpenAPI document containing the approved operations. Operations absent from it are unreachable to AlixAI. |
| `adapter_config` | schema path | yes | Package-relative path (below schemas/) of the adapter configuration file for this endpoint. Its format is owned by the OpenAPI adapter. |
| `contract_sha256` | sha-256 hex | no | Optional SHA-256 of the approved operation subset: the hash of the canonical JSON of the approved operations as the document states them, with local `$ref`s resolved and the text meant for people (descriptions, summaries, examples, extensions) left out (`openapi.ContractSHA256` computes it). A document whose subset hashes differently is refused when the adapter is built. |
| `credential_ref` | credential ref | no | Optional credential reference AlixAI uses to authenticate to the API. |

The file `adapter_config` names is strict YAML (an unknown member is an error). Every member has a default:

```yaml
server: fixture-api            # optional; when present it must be this endpoint's id
auth:
  header: Authorization        # where the credential goes; sent only to the endpoint's own origin
  scheme: Bearer
identity:
  header: X-AlixAI-Identity    # adapters.Identity as unpadded base64url JSON
idempotency:
  header: Idempotency-Key      # also sent in a declared idempotency_key path or query parameter
management:
  describe: describeApplication     # operationIds, all optional; no capability maps them, a model never sees them
  health: getHealth
  inspect: inspectApplication       # must declare a query parameter `aspects`
  provisioning: [createTenant, seedTenant, cleanupTenant]   # the audit's three disposable-tenant operations (create_tenant, seed, cleanup)
```

The wire contract (what becomes a path, query or header parameter or a body member, how a Commit sends the PreparedAction, which statuses can carry
a result, and what an unusable reply means) is in the package comment of `adapters/openapi` and decided in ADR-0033. Every approved operation must
describe at least one 2xx `application/json` response with a schema, take only path, query and header parameters in their default styles, and take a
request body only as `application/json` with a schema; a document that needs more is refused when the adapter is built. Without a `health`
operation the adapter can only check that it can connect, so it reports `degraded`, and the audit records a `system.health_degraded` warning that an
operator accepts.

#### `endpoints.<id> (transport native)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `transport` | const `"native"` | yes | Selects this variant. |
| `bridge` | enum: `typescript`, `python`, `go`, `other` | yes | The language of the embedded bridge. |
| `ipc` | object (see `endpoints.<id>.ipc (native)`) | yes | How AlixAI reaches the bridge. |
| `adapter_config` | schema path | yes | Package-relative path (below schemas/) of the adapter configuration file for this endpoint. Its format is owned by the native adapter. |

#### `endpoints.<id>.ipc (native)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | enum: `unix_socket`, `named_pipe`, `loopback_tcp` | yes | unix_socket (an absolute path), named_pipe (a \\.\pipe\ name) or loopback_tcp (127.0.0.1, [::1] or localhost with a port). |
| `address` | string | yes | The address for the kind. No other address form exists, so a package cannot point the native adapter at a remote host. |

#### `endpoints.<id> (transport events)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `transport` | const `"events"` | yes | Selects this variant. |
| `delivery` | enum: `webhook`, `sse`, `poll` | yes | webhook: the application posts signed events to AlixAI (key_id required). sse or poll: AlixAI reads events from url. |
| `url` | http(s) URL | no | Absolute http(s) URL AlixAI reads events from. Required for sse and poll; not allowed for webhook. |
| `key_id` | identifier | no | ID of the registered key that authenticates the event sender. Required for webhook; events with an unknown or missing signature are dropped. |
| `credential_ref` | credential ref | no | Optional credential reference used for sse or poll delivery. |
| `event_types` | map: dotted id to object (see `endpoints.<id>.event_types.<type> (events)`) (min 1) | yes | The event types AlixAI accepts, keyed by event type name. Anything else is dropped. At least one. |
| `adapter_config` | schema path | yes | Package-relative path (below schemas/) of the adapter configuration file for this endpoint. Its format is owned by the event adapter. |

#### `endpoints.<id>.event_types.<type> (events)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `effect` | enum: `invalidate_state`, `refresh_projection`, `notify_session` | yes | What AlixAI does when the event arrives: invalidate_state (drop pending goals, prepared actions and caches that depend on the changed record), refresh_projection (re-read a cached projection) or notify_session (tell an active session). There is no value that executes or commits anything. |
| `description` | text | yes | What the event means in the application. |

#### `context_exchange`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `mode` | enum: `context_token`, `local_ipc` | yes | context_token: the application mints a short-lived, audience-bound token per request. local_ipc: mutually authenticated local IPC with the native adapter. The token format and algorithm are fixed by the AlixAI core (ADR-0013), not by the package. |
| `issuer` | string | yes | The issuer value AlixAI must see on the application's tokens or IPC identity. Exact match, no whitespace. |
| `audience` | string | yes | The audience value the application puts in its tokens, naming this AlixAI deployment's service. Exact match, no whitespace. |
| `key_ids` | array of identifier (min 1, max 8, unique) | yes | IDs of the public keys registered for this application that may sign its tokens (or authenticate the IPC peer). At least one; AlixAI never trusts a key ID that is not registered for this application. |
| `max_lifetime_seconds` | integer (min 1, max 900) | no | Optional. The longest token lifetime the application will issue, 1 to 900 (15 minutes). When absent the platform maximum applies. AlixAI may enforce a lower value. |

#### `tenancy`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `tenant` | object (see `tenancy.tenant and tenancy.workspace`) | yes | What a tenant is in this application. Every request carries a tenant ID; there is no default tenant. |
| `workspace` | object (see `tenancy.tenant and tenancy.workspace`) | yes | What a workspace is inside a tenant. Every request carries a workspace ID; an application with a single workspace per tenant still sends one, and a session can never change tenant or workspace without a new authenticated context. |
| `principal_kinds` | array of enum: `human`, `service` (min 1, max 2, unique) | yes | The kinds of principal the application authenticates: human users, service principals or both. |
| `roles` | map: identifier to object (min 1) | yes | The roles the application asserts in context tokens, keyed by role ID, with what each role means. At least one: a Scope always carries a role. Policy files may only refer to roles listed here. |

#### `tenancy.roles.<role>`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `description` | text | yes |  |

#### `tenancy.tenant and tenancy.workspace`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `label` | label | yes | The application's own noun for this level (for example clinic group, site, organisation, project). |
| `description` | text | yes | What the level means and what data it isolates. |

#### `languages`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `default` | language tag | yes | The language used when the user's language is unknown. Must be one of supported. |
| `supported` | array of language tag (min 1, max 32, unique) | yes | Every supported language. At least one. |

#### `model_requirements`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `minimum_profile` | enum: `experimental-4g`, `lite`, `balanced`, `performance` | yes | The smallest deployment profile the application's complete workload has been shown to fit: experimental-4g (emulated, never certified), lite, balanced or performance. |
| `controller` | object (see `model_requirements.<role>`) | yes | The conversational controller model. Required: AlixAI cannot run a conversation without one. |
| `router` | object (see `model_requirements.<role>`) | no | Optional decision/router model. |
| `embedding` | object (see `model_requirements.<role>`) | no | Embedding model, needed for semantic RAG retrieval. Absent means lexical (FTS) retrieval only. |
| `reranker` | object (see `model_requirements.<role>`) | no | Optional reranker over an authorized shortlist. |
| `stt` | object (see `model_requirements.<role>`) | no | Optional speech recognition. |
| `tts` | object (see `model_requirements.<role>`) | no | Optional speech generation. |

#### `model_requirements.<role>`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `need` | enum: `required`, `optional` | yes | required: the application is not supported without this role. optional: the application degrades gracefully without it. |
| `languages` | array of language tag (min 1, max 32, unique) | no | Optional. Languages the model must cover; when absent, languages.supported applies. |
| `min_context_tokens` | integer (min 256, max 1048576) | no | Optional. The smallest context window, in tokens, the workload was qualified with. |

#### `retention`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `session_state_days` | days 0-3650 | yes | How long goal and slot state of a finished session is kept. |
| `transcript_days` | days 0-3650 | yes | How long the display transcript is kept. Clearing it never deletes records, receipts or audit evidence. |
| `action_receipt_days` | integer (min 1, max 3650) | yes | How long committed or rejected action receipts are kept. At least 1: a receipt is evidence. |
| `audit_days` | integer (min 1, max 3650) | yes | How long audit events are kept. At least 1: audit evidence is never discarded immediately. |
| `log_days` | days 0-3650 | yes | How long redacted operational logs are kept. |

#### `network`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `default` | const `"deny"` | yes | Always deny. The property exists so the policy is stated in the signed file instead of being implied. |
| `egress` | map: slug to object (see `network.egress.<id>`) | yes | The outbound destinations that are allowed, keyed by entry ID. An empty map allows none. RAG sources refer to entry IDs; every host of an MCP or OpenAPI endpoint, loopback included, must appear here. |

#### `network.egress.<id>`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `host` | host name | yes | An exact host name or IPv4 address. Wildcards do not exist. |
| `ports` | array of integer (min 1, max 65535) (min 1, max 8, unique) | yes | The allowed destination ports. At least one. |
| `scheme` | enum: `https`, `http` | yes | https, or plain http. Plain http is accepted only to localhost or 127.0.0.1. |
| `purpose` | label | yes | Why the application needs this destination. |

#### `integrity`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `signature_manifest` | const `"signatures/signature-manifest.json"` | yes | The package-relative path of the signature manifest. Fixed: the manifest cannot hash itself, so integrity is carried by this file, which lists every other file with its digest and carries the detached signature. |
| `hash_algorithm` | enum: `sha256` | yes | Digest algorithm of the signature manifest. |
| `signature_algorithm` | enum: `ed25519` | yes | Signature algorithm of the signature manifest. |
| `publisher_key_ids` | array of identifier (min 1, max 8, unique) | yes | IDs of the publisher keys that may sign this package. The signature manifest's signing key must be one of them and must be registered for this application in the trust store. |

## 6. Capability contracts (`capabilities/*.yaml`)

One file per user-facing capability. The contract is what AlixAI enforces; it is not a prompt.

### Capability contract requirements to fields

| Requirement | Field |
| --- | --- |
| stable capability ID | `id`, `version` |
| purpose and application scope | `purpose`, `audience`, `scope_level`, `permissions` |
| required and optional typed fields | `fields[]` (`required: true or false`) |
| entity-reference types | `entity_types`, and fields of type `entity_ref` |
| deterministic defaults | `fields[].default` (a literal or a context value; never computed by the model) |
| ambiguity and clarification rules | `ambiguity[]` (the policy) and the conversation contract `clarifications[]` (the wording) |
| validation and error codes | `validation[]`, `failure_states[]` |
| risk level | `risk_level` (0 to 3), bounded below by the platform floor of `effect_class` |
| prepare operation | `operations.prepare`, `prepared_action` |
| confirmation rule | `confirmation` |
| commit operation | `operations.commit` |
| idempotency behavior | `idempotency`, `operations.status` |
| response obligations | `response_obligations[]` |
| permitted next actions | `next_actions[]` |
| documentation references | `doc_refs` |
| multilingual examples used for testing, never as sole runtime policy | `examples[]`, each with `test_only: true` |

### Operations and adapter calls

Each operation (`read`, `validate`, `prepare`, `commit`, `status`) is mapped to one adapter call on
one endpoint of the manifest: exactly one of `mcp_tool`, `openapi_operation_id` or `native_method`,
and the kind must match the transport of that endpoint. `result_schema` names the schema, below
`schemas/`, that the canonical values of the result must satisfy; the audit validates real results
against it. `arguments` renames fields to adapter argument names where
they differ.

### Effect class, risk level, operations and confirmation

The application declares what a capability does in `effect_class`, and the **platform risk floor** is
a pure function of it (the host may raise a level, never lower the platform
minimum). The schema refuses a `risk_level` below the floor, so a host cannot silently declare a
lower level:

| `effect_class` | Meaning | Platform floor | Allowed `risk_level` |
| --- | --- | --- | --- |
| `none` | read-only | 0 | 0 |
| `reversible` | changes application data in a way the user can undo, nothing outside the application | 1 | 1 to 3 |
| `external` | a consequential change, or a side effect outside the application (payment, message, third-party call) | 2 | 2 to 3 |
| `privileged` | privileged, legally sensitive or irreversible | 3 | 3 |

The schema also enforces these couplings between the risk level, the operations and the
confirmation, so a contract that breaks them is invalid:

| `risk_level` | Operations | `confirmation.mode` | Also |
| --- | --- | --- | --- |
| 0 read-only | `read` required; `prepare` and `commit` not allowed | `none` | no `idempotency`, no `prepared_action` |
| 1 reversible, low impact | `prepare`, `commit`, `status` required; `read`, `validate` optional | `none` (the application allows immediate commit after a concise summary), `explicit` or `step_up` | `idempotency` and `prepared_action` required |
| 2 consequential or external | as level 1 | `explicit` or `step_up` | as level 1 |
| 3 high-risk, privileged, legal | as level 1 | `step_up` only, with `step_up_channel` | as level 1 |

Every `commit` needs a `prepare` (prepare before commit), a `status` (to
recover from an uncertain commit response instead of retrying blindly) and an
`idempotency` rule. `confirmation.mode` `step_up` requires `step_up_channel` and the other modes
forbid it. The host may raise a level; it cannot lower a platform-enforced minimum (the core applies
the platform floor at runtime; a host policy may only raise it). A read whose sensitivity calls for
more than level 0 is not expressed as a higher level here: sensitivity is handled by roles and field
projection in the policy, and a read that the AI must never complete is not offered as a
capability.

`audience` is `user` for an ordinary capability a user asks for in conversation, which needs a
conversation contract in every supported language, or `admin` for an operator capability that is
never offered to the model in user conversations. It is required with no default.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | capability id | yes | The stable capability ID, exact match, no wildcards. Must equal the key under which the manifest lists this file. Never reused for a different meaning: change the ID and the old one is retired. |
| `version` | semver | yes | The version of this capability contract (semver). A change to fields, operations, risk or confirmation is a version change and a re-audit trigger. |
| `audience` | enum: `user`, `admin` | yes | user: an ordinary capability a user asks for in conversation; it needs a conversation contract in every supported language. admin: an operator or administrative capability that is never offered to the model in user conversations and has no conversation contract. Required, no default: an unlabelled capability is not assumed to be either. |
| `title` | label | yes | Short human-readable name. |
| `purpose` | text | yes | What the capability is for, in one or two sentences. Documentation, not model instruction: the model's tool description is built by AlixAI from the typed contract. |
| `scope_level` | enum: `principal`, `workspace`, `tenant` | yes | The widest data scope one call can touch: principal (only the caller's own records), workspace (records of the current workspace) or tenant (records across the workspaces of the current tenant). Nothing wider exists: a capability can never cross tenants. The audit probes this boundary with cross-scope tests. |
| `permissions` | array of permission id (min 1, max 16, unique) | yes | The application permissions the host checks before it runs any operation of this capability. At least one: a capability with no permission is not allowed (deny by default). Each is documented under the permission facet. |
| `fields` | array of object (see `fields[]`) (max 64) | yes | The typed fields of the capability, required and optional, in the order AlixAI asks for missing ones. Field names are unique within the capability (checked by the loader). The model may only propose candidate values for these fields; the application validates them. |
| `entity_types` | map: slug to object (see `entity_types.<type>`) | yes | Entity-reference types the fields can point at, keyed by entity type name. A field of type entity_ref names one of them. May be empty. |
| `ambiguity` | array of object (see `ambiguity[]`) (max 64) | yes | Ambiguity and clarification rules: when a field cannot be resolved to one value, what AlixAI does. Deterministic policy; the wording of the question lives in the conversation contract under the same rule id. May be empty. |
| `validation` | array of object (see `validation[]`) (max 64) | yes | The validation rules the application applies deterministically, each with the error code it returns. May be empty for a capability that validates nothing beyond field types. Each rule is documented under the validation_rule facet. |
| `failure_states` | array of object (see `failure_states[]`) (min 1, max 64) | yes | Every typed failure the capability can report. At least one. Each is documented under the failure_state facet and has a failure response in each conversation contract. An outcome that is not listed here is reported as an undeclared failure by the audit. |
| `risk_level` | risk level 0-3 | yes | The action safety level, required and never defaulted. The host may declare a level here; the platform floor for side effects and privileged actions still applies, and a host policy may raise the level but never lower the platform minimum. |
| `operations` | object (see `operations`) | yes | The operations that implement the capability, each mapped to an adapter call (an MCP tool name, an OpenAPI operationId or a native method). read: a deterministic read. validate: optional dry-run validation. prepare: validate and stage a consequential action, returning exact effects and data versions. commit: execute a prepared action idempotently. status: look up an action by idempotency key, used to recover from an uncertain commit response. |
| `confirmation` | object (see `confirmation`) | yes | The confirmation rule. |
| `prepared_action` | object (see `prepared_action`) | no | How long a prepared action stays confirmable and what invalidates it. Required when the capability has prepare and commit; not allowed for a read-only capability. |
| `idempotency` | object (see `idempotency`) | no | Idempotency behaviour of commit. Required when the capability has a commit operation; not allowed for a read-only capability. |
| `response_obligations` | array of object (see `response_obligations[]`) (min 1, max 32) | yes | The facts every response about this capability must state (response obligations), as paths into the deterministic result. At least one. The model verbalises them; the response verifier rejects a response that omits one or states a different value. |
| `next_actions` | array of object (see `next_actions[]`) (max 16) | yes | The follow-up capabilities AlixAI may suggest, by situation (permitted next actions). Anything not listed is not suggested. Confirm, cancel and correct are part of the action protocol and are not listed. May be empty: the capability is a dead end. |
| `doc_refs` | array of doc id (min 1, max 128, unique) | yes | IDs of the documentation index entries that document this capability. At least one. Every ID must exist in documentation/index.yaml and name this capability. |
| `examples` | array of object (see `examples[]`) (min 1, max 100) | yes | Multilingual examples used to test the capability, never as the sole runtime policy. Every example is marked test_only. At least one. |

#### `fields[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name` | field name | yes | The field name: lower snake_case, at most 64 characters. |
| `type` | enum: `string`, `integer`, `number`, `boolean`, `date`, `datetime`, `duration`, `enum`, `entity_ref`, `list` | yes | The value type. string, integer, number, boolean, date (YYYY-MM-DD), datetime (RFC 3339 UTC), duration (ISO 8601), enum (needs values), entity_ref (needs entity_type) or list (needs item_type). |
| `required` | boolean | yes | true: AlixAI asks for the field when it is missing and never prepares without it. false: optional. |
| `description` | text | yes | What the field means. |
| `sensitive` | boolean | no | Optional, default false. true marks personal or confidential data: the value is redacted from logs and is withheld from model context unless the current task needs it. |
| `classification` | enum: `public`, `internal`, `confidential`, `restricted` | no | Optional. How sensitive the value of this field is, default `internal` when absent. A field above `internal` is listed by the integration audit as data that could leave the deployment in external mode. It never widens what may be sent: the egress policy decides that. |
| `local_only` | boolean | no | Optional, default false. true: the value of this field never leaves the deployment, whatever the policy says, so it can never be placed in a prompt template (an `{input.<field>}` placeholder of a conversation contract is refused). |
| `values` | array of string (min 1, max 256, unique) | no | The allowed values of an enum field, exact and case-sensitive. At least one. Only for type enum. |
| `entity_type` | slug | no | The entity type of an entity_ref field: a key of entity_types. Only for type entity_ref. |
| `item_type` | enum: `string`, `integer`, `number`, `boolean`, `date`, `datetime`, `enum`, `entity_ref` | no | The element type of a list field. Lists are flat: elements are scalars. Only for type list. |
| `min_length` | integer (min 0, max 4096) | no | Minimum string length. For type string. |
| `max_length` | integer (min 1, max 4096) | no | Maximum string length. For type string. |
| `minimum` | number | no | Smallest allowed number. For type integer or number. |
| `maximum` | number | no | Largest allowed number. For type integer or number. |
| `max_items` | integer (min 1, max 256) | no | Maximum list length. For type list. |
| `default` | object (see `fields[].default (one variant per source)`) | no | The deterministic default applied when an optional field is absent. Never a model guess. A field with a default is not asked for. |

#### `fields[].default (source literal)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `source` | const `"literal"` | yes | Selects this variant. |
| `value` | scalar | yes |  |

#### `fields[].default (source context)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `source` | const `"context"` | yes | Selects this variant. |
| `key` | enum: `today`, `principal_id`, `tenant_id`, `workspace_id` | yes | today: the current date in the application's time zone as decided by the application. principal_id, tenant_id and workspace_id: the verified Scope values. |

#### `entity_types.<type>`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `description` | text | yes | What the entity is. |
| `resolve` | object (see `operations.<op> and entity_types.<type>.resolve`) | no | Optional operation that resolves a mention to candidate entity IDs. Absent means mentions must be exact entity IDs. The application reauthorizes every resolved entity. |

#### `ambiguity[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Rule ID, unique in the capability. The conversation contract's clarification prompt refers to it. |
| `field` | field name | yes | The field the rule applies to. |
| `when` | enum: `multiple_matches`, `no_match`, `low_confidence`, `conflicting_values` | yes | multiple_matches: more than one candidate. no_match: none. low_confidence: the proposed value is uncertain. conflicting_values: the turn gives two different values. |
| `action` | enum: `ask_user`, `use_default`, `reject` | yes | ask_user: ask a clarification question. use_default: apply the field's deterministic default (the field must declare one). reject: fail with an ambiguous error. |
| `max_choices` | integer (min 2, max 10) | no | Optional. For ask_user: the most candidates presented as choices, 2 to 10. |

#### `validation[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Rule ID, unique in the capability. The documentation index covers it under the validation_rule facet. |
| `fields` | array of field name (max 16, unique) | yes | The fields the rule checks. Empty for a rule over the whole request. |
| `description` | text | yes | What the rule requires. |
| `error_code` | error code | yes | The platform error code returned when the rule fails. It must also appear in failure_states. |
| `application_code` | slug | no | Optional stable application-specific code for the same failure, for the host's own support tooling. |

#### `failure_states[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `code` | error code | yes | The platform error code of the failure. |
| `application_code` | slug | no | Optional stable application-specific code distinguishing several failures that share a platform code. |
| `description` | text | yes | When the failure happens and what it means for the user. |

#### `operations.<op> and entity_types.<type>.resolve`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `endpoint` | slug | yes | ID of the endpoint declared in the manifest that owns this operation. |
| `mcp_tool` | operation name | no | MCP tool name, for an mcp endpoint. |
| `openapi_operation_id` | operation name | no | OpenAPI operationId, for an openapi endpoint. |
| `native_method` | operation name | no | Native bridge method name, for a native endpoint. |
| `result_schema` | schema path | yes | The package schema (below schemas/) that the canonical values of this operation's result must satisfy. The audit's deterministic-result probes validate real results against it. |
| `arguments` | map: field name to operation name | no | Optional mapping from capability field name to the adapter argument name when they differ. Fields not listed are passed under their own name. |
| `timeout_ms` | integer (min 100, max 120000) | no | Optional per-call timeout in milliseconds, 100 to 120000. When absent the adapter default applies. A timeout never counts as success. |

#### `operations`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `read` | object (see `operations.<op> and entity_types.<type>.resolve`) | no | Deterministic read. Required for a read-only capability (risk level 0). |
| `validate` | object (see `operations.<op> and entity_types.<type>.resolve`) | no | Optional dry-run validation of candidate field values without staging anything. |
| `prepare` | object (see `operations.<op> and entity_types.<type>.resolve`) | no | Validate and stage the action; returns the canonical summary, exact effects and targets, the data versions used and an expiry. Required with commit. |
| `commit` | object (see `operations.<op> and entity_types.<type>.resolve`) | no | Execute a prepared action, idempotently, after the confirmation the rule demands. Requires prepare and status. |
| `status` | object (see `operations.<op> and entity_types.<type>.resolve`) | no | Look up the state of an action by idempotency key. AlixAI calls it before retrying a commit whose response was lost or uncertain. |

#### `confirmation`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `mode` | enum: `none`, `explicit`, `step_up` | yes | none: no user confirmation before commit (a read, or a level 1 action the application allows to commit immediately after a concise summary). explicit: the user must confirm the exact prepared effects. step_up: the user must complete authenticated application UI or stronger step-up; the AI cannot complete the action alone. The risk level sets the weakest allowed mode (see the rules at the end of this schema). |
| `step_up_channel` | enum: `application_ui`, `step_up_credential` | no | How the step-up is completed: application_ui (the host's authenticated UI) or step_up_credential (a stronger credential presented to the host). Required for step_up, not allowed otherwise. |

#### `prepared_action`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `ttl_seconds` | integer (min 1, max 86400) | yes | How long a prepared action can be confirmed, 1 to 86400. After that it is expired and must be prepared again. |
| `invalidated_by` | array of enum: `record_revision`, `permission_change`, `user_change`, `application_event` (min 1, max 4, unique) | yes | What invalidates a prepared action before it expires: record_revision (a record it used changed), permission_change, user_change (the principal or session changed) or application_event (an event of an events endpoint). At least one. |

#### `idempotency`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `key_source` | enum: `prepared_action_id`, `host_assigned` | yes | prepared_action_id: AlixAI derives the key from the opaque preparation ID. host_assigned: the application returns the key from prepare. In both cases the key is fixed at prepare time and reused by every retry. |
| `window_seconds` | integer (min 1, max 31536000) | yes | How long the application remembers a key and answers a repeat with the original result, 1 to 31536000 (a year). It must be at least prepared_action.ttl_seconds. |
| `on_replay` | enum: `return_original_receipt`, `reject` | yes | return_original_receipt: a repeated commit returns the first receipt and changes nothing. reject: a repeated commit fails with replayed. |

#### `response_obligations[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `fact` | fact path | yes | The path of the canonical value that must be stated, for example reservation.pickup_date. |
| `when` | enum: `success`, `prepared`, `failure`, `always` | yes | success: in a response reporting a read result or a committed action. prepared: when presenting a prepared action for confirmation. failure: in a failure response. always: in every response. |
| `description` | text | yes | What the fact is. |

#### `next_actions[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Next-action ID, unique in the capability. The conversation contract supplies its label per language. |
| `after` | enum: `success`, `prepared`, `failure` | yes | When it may be suggested: success (after a read or a commit), prepared (while an action awaits confirmation) or failure. |
| `capability_id` | capability id | yes | The capability that would be started. It must be declared in the manifest and be an audience user capability. |

#### `examples[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Example ID, unique in the capability. |
| `language` | language tag | yes | The language of the utterance. Should be one of the manifest's supported languages. |
| `test_only` | const `true` | yes | Always true. Examples exist to test the capability in each language; they are never a runtime policy, a prompt or a routing rule, and AlixAI does not use them to decide anything. |
| `utterance` | string | yes | What a user might say. |
| `expected_fields` | map: field name to example value | yes | The candidate field values a correct reading of the utterance contains, keyed by field name. Only fields the utterance states; defaults and missing fields are not listed. |

## 7. Conversation contracts (`conversation-contracts/*.yaml`)

One file per capability and language: the terminology used, a prompt for
every required field, the clarification questions, how corrections and cancellations are worded, the
confirmation wording, one response per user-visible outcome, one response per failure state and the
labels of the permitted next actions. Wording is **fixed text with `{placeholders}`** that AlixAI
fills from deterministic facts: `{reservation.pickup_date}` is a canonical value of the operation
result; `{input.title}` (reserved prefix `input.`) is a validated field value. The model may
verbalise a response but a response that omits an obligated fact or states another value is
rejected by the response verifier.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `capability_id` | capability id | yes | The capability this contract belongs to. |
| `language` | language tag | yes | The language of every text in this file. Must be one of the manifest's supported languages. The pair (capability_id, language) is unique in the package. |
| `contract_version` | semver | yes | The version of this conversation contract (semver). Wording changes bump it; it is covered by the package signature like every file. |
| `terminology` | array of slug (max 64, unique) | yes | IDs of the terms (terminology/ files) this contract's wording relies on. Every ID must exist and have a label in this contract's language. May be empty. |
| `field_prompts` | array of object (see `field_prompts[]`) (max 64) | yes | A prompt for each field of the capability, in the language of the contract. Every required field of the capability needs one (checked by the loader); optional fields may have one. Field names are unique within the list. |
| `clarifications` | array of object (see `clarifications[]`) (max 64) | yes | The clarification questions, one per ambiguity rule of the capability that has action ask_user. Rule IDs are unique within the list. May be empty when the capability has no such rule. |
| `corrections` | object (see `corrections`) | yes | How a user corrects an earlier answer. |
| `cancellations` | object (see `cancellations`) | yes | How a user cancels the task, and the acknowledgement. |
| `confirmations` | object (see `confirmations`) | no | The confirmation wording. Required when the capability's confirmation mode is not none (checked by the loader); omitted for a read-only capability. |
| `success` | array of object (see `success[]`) (min 1, max 32) | yes | The success responses, one per user-visible outcome. At least one. Outcome IDs are unique within the list and must match the user_visible_outcome entries of the documentation index. Every response_obligation of the capability that applies on success must appear as a placeholder in at least one of them. |
| `failures` | array of object (see `failures[]`) (min 1, max 64) | yes | The failure responses, one per failure state of the capability. At least one. A failure state without a response here fails the capability: AlixAI never falls back to a generic success-looking message. |
| `next_actions` | array of object (see `next_actions[]`) (max 16) | yes | The labels of the capability's permitted next actions in this language: one per entry of the capability's next_actions. May be empty when the capability has none. |
| `presentation` | object (see `presentation`) | no | Optional, additive (1.3.0). Hint for `present_result` rich blocks built from host records. Absent: text, and a table when the result has rows. |

#### `presentation`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `chart` | object (see `chart`) | no | When set, the deterministic template (and a verified external realisation) may include a chart of the named fields from the result rows. |

#### `presentation.chart`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `type` | enum: `bar`, `line`, `area`, `point`, `pie` | yes | The chart mark type. |
| `x` | field name | yes | Result field for the x / category channel. |
| `y` | field name | yes | Result field for the y / value channel (a number the host stated). |

#### `field_prompts[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `field` | field name | yes | The capability field the prompt is for. |
| `label` | label | yes | The user-facing name of the field in this language. |
| `prompt` | template | yes | The question asked when the field is missing or invalid. |
| `examples` | array of label (max 5) | no | Optional example answers shown to the user. Display only: never used to fill a field. |

#### `clarifications[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `rule_id` | slug | yes | The ID of an ambiguity rule of the capability. |
| `prompt` | template | yes | The clarification question. The candidate choices are rendered by AlixAI from deterministic results, not written here. |

#### `corrections`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `acknowledgement` | template | yes | What AlixAI says when it applies a correction. |
| `correctable_fields` | array of field name (max 64, unique) | yes | The fields a user may correct. Only these. May be empty, which disables corrections for the capability. |
| `prepared_action_invalidated` | template | no | What AlixAI says when a correction invalidates an already prepared action, so the action is prepared again. Required by the loader when the capability has a prepare operation. |

#### `cancellations`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `acknowledgement` | template | yes | What AlixAI says after the task is cancelled. Cancelling a prepared action discards it; nothing is committed. |

#### `confirmations`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `request` | template | yes | The confirmation request shown with the exact prepared effects. Must use placeholders for the facts the prepared action returns, so the user confirms the real effects and not the model's paraphrase. |
| `declined` | template | yes | What AlixAI says when the user declines. |
| `expired` | template | yes | What AlixAI says when the prepared action expired or was invalidated before confirmation. |
| `step_up_required` | template | no | What AlixAI says when the user must complete the action in the application's authenticated UI or with a stronger credential. Required by the loader when the capability's confirmation mode is step_up. |

#### `success[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `outcome` | slug | yes | The user-visible outcome ID (for example reserved, found, none_found). |
| `text` | template | yes | The response text with {fact.path} placeholders for the canonical facts. |

#### `failures[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `code` | error code | yes | The platform error code of the failure state this response covers. |
| `application_code` | slug | no | The application code of the failure state, when the capability distinguishes several failures with the same platform code. |
| `text` | template | yes | The failure response. It must state that the operation did not complete when it did not; it never reads as a success. |

#### `next_actions[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | The ID of a next action of the capability. |
| `label` | label | yes | The text of the suggestion shown to the user. |

## 8. Documentation index (`documentation/index.yaml`)

The index says which document describes what. For every capability it must hold entries covering
seven **facets**, in at least the manifest's default language:

| `facet` | Subject property | One entry for |
| --- | --- | --- |
| `field` | `field` | every field of the capability |
| `validation_rule` | `rule_id` | every validation rule |
| `permission` | `permission` | every permission the capability lists |
| `failure_state` | `error_code` | every failure state |
| `risk_level` | (none) | the capability, once |
| `confirmation_rule` | (none) | the capability, once |
| `user_visible_outcome` | `outcome` | every success outcome of the conversation contracts |

The schema fixes the **shape** of an entry (each facet has its own required subject property and
forbids the others). **Coverage** is checked by the loader, so a missing facet fails that one
capability with a precise finding instead of making the whole index unreadable. Missing
documentation is a failed capability, not an instruction for the model to guess.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `entries` | array of object (see `entries[]`) (min 1, max 4096) | yes | The documentation entries. At least one. Entry IDs are unique across the index (checked by the loader). |

#### `entries[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | doc id | yes | The entry ID. Capabilities cite it in doc_refs. |
| `capability_id` | capability id | yes | The capability the entry documents. |
| `facet` | enum: `field`, `validation_rule`, `permission`, `failure_state`, `risk_level`, `confirmation_rule`, `user_visible_outcome` | yes | What the entry documents: field (needs field), validation_rule (needs rule_id), permission (needs permission), failure_state (needs error_code), risk_level, confirmation_rule or user_visible_outcome (needs outcome). risk_level and confirmation_rule take no subject: there is one of each per capability. |
| `language` | language tag | yes | The language of the documentation text. The manifest's default language must be covered for every capability; other languages are optional. |
| `title` | label | yes | Title of the entry. |
| `summary` | string | yes | What the documentation says about the subject, in one or two sentences, so the audit can read the claim and not only follow a path. |
| `location` | object (see `entries[].location`) | yes | Where the full documentation is. |
| `field` | field name | no | For facet field: the name of the documented capability field. |
| `rule_id` | slug | no | For facet validation_rule: the ID of the documented validation rule. |
| `permission` | permission id | no | For facet permission: the documented application permission. |
| `error_code` | error code | no | For facet failure_state: the platform error code of the documented failure state. |
| `outcome` | slug | no | For facet user_visible_outcome: the outcome ID, matching a success response of the conversation contracts. |

#### `entries[].location`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `path` | string | yes | Package-relative path of the document, below documentation/. |
| `anchor` | string | no | Optional heading or element anchor inside the document. |

## 9. Terminology (`terminology/*.yaml`)

The application's authoritative vocabulary. A term is one concept with a label set per language, so
the same word means the same thing in conversation contracts, entity resolution hints and
documentation. Terminology guides wording and lookup; it never grants a capability or a permission.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `terms` | array of object (see `terms[]`) (min 1, max 512) | yes | The terms defined by this file. At least one. |

#### `terms[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | The stable term ID, unique across the package. Conversation contracts refer to it. |
| `entity_type` | slug | no | Optional. The entity type this term names; it must be an entity type of some capability. Entity resolution uses the labels as lookup hints. |
| `labels` | map: language tag to label (min 1) | yes | The wording of the concept per language, keyed by language tag. At least one language. A language listed in the manifest but absent here is a coverage gap reported by the validator. |

#### `terms[].labels.<language>`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `preferred` | label | yes | The word AlixAI uses for the concept in this language. |
| `synonyms` | array of label (max 32, unique) | no | Other words users use for the same concept. Lookup hints only. |
| `avoid` | array of label (max 32, unique) | no | Words AlixAI must not use for the concept in its own responses (for example a deprecated name or a word with another meaning in the application). |
| `definition` | text | yes | What the term means in the application, in this language. |

## 10. Policy (`policies/*.yaml`)

The host application's declarative policy. It can only narrow or raise: it
cannot grant a capability the package does not declare, cannot lower a risk level and cannot lower a
platform-enforced minimum, and anything not granted is denied. A capability reaches a model only if
the verified context's roles include a role granted here **and** the context token's capability
scopes include it.

The policy has three unconditional parts (`role_grants`, `risk_overrides`, `field_projection`) and,
optionally, **conditional `rules`** in the closed `policy/v1` format of ADR-0017. A rule is data, not
code: there is no expression language, no regular expression, no wildcard and no reference. Its
`when` is a conjunction of fixed tests (the roles and the capability scopes of the verified context,
the tenant and the workspace, and comparisons on the values of the call's fields), and its `then` can
only **deny** the capability, **raise** its risk level or **hide** fields from the model; no rule
grants anything.

The loader hands every `policies/*.yaml` to the same parser the runtime uses
(`policy.ParseHostPolicy`) and builds the package with the same constructor (`policy.NewPackage`),
so the loader and the policy gateway enforce the same references and cannot disagree about them. A
document that has neither `format` nor `rules` is the original format and stays valid; a document
with a `format` other than `policy/v1` is refused.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `format` | const `"policy/v1"` | no | Optional. The policy format and its major version. The only defined value is policy/v1 (ADR-0017), which is also what a document without this key is. A loader that sees any other value refuses the document. |
| `policy_id` | slug | yes | The policy ID, unique in the package. |
| `description` | text | no | Optional note on what the policy is for. |
| `role_grants` | array of object (see `role_grants[]`) (min 1, max 256) | yes | Which roles may use which capabilities. A capability reaches a model only if the verified context's roles include a role granted here AND the context token's capability scopes include it (least capability). At least one grant: a policy that grants nothing denies everything and is not a policy. |
| `risk_overrides` | array of object (see `risk_overrides[]`) (max 256) | yes | Raises of a capability's risk level. May be empty. |
| `field_projection` | array of object (see `field_projection[]`) (max 256) | yes | Which fields of a capability the model may see, for least capability. A field not listed for the capability is withheld from model context (the application still receives it). May be empty: a capability without a projection exposes the fields it declares, except fields marked sensitive, which stay withheld until the current task needs them. |
| `rules` | array of object (see `rules[]`) (max 256) | no | Optional conditional rules of the policy/v1 format (ADR-0017); absent means none. Every rule narrows or raises and none grants: when its condition holds it denies the capability, raises its risk level to at least a value, or hides fields from the model. Rule IDs are unique across all policies of the package, and every role, capability and field a rule names must exist in the package (checked by the loader). |

#### `role_grants[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `role` | identifier | yes | A role the manifest's tenancy.roles declares. Exact match. |
| `capabilities` | array of capability id (min 1, max 256, unique) | yes | The capability IDs the role may use. Exact IDs from the manifest; wildcards do not exist. At least one. |

#### `risk_overrides[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `capability_id` | capability id | yes | The capability whose level is raised. |
| `minimum_risk_level` | risk level 0-3 | yes | The level the capability is treated as having at least. The effective level is the highest of the capability's declared level, the platform floor and this value; a value below the declared level changes nothing and the loader reports it. |
| `reason` | text | yes | Why the host raises the level, for the audit trail. |

#### `field_projection[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `capability_id` | capability id | yes | The capability the projection applies to. |
| `roles` | array of identifier (min 1, max 64, unique) | no | Optional. The roles the projection applies to. When absent it applies to every role that has no more specific projection for the capability. |
| `model_visible_fields` | array of field name (min 1, max 64, unique) | yes | The only fields of the capability the model may see or propose. At least one. A required field missing here makes the capability unusable, which the loader reports. |

#### `rules[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | The rule ID, unique in the package. It appears in the audit record of every decision the rule influenced. |
| `capabilities` | array of capability id (min 1, max 64) | yes | The capabilities the rule applies to. Exact IDs from the manifest; wildcards do not exist. At least one. |
| `when` | object (see `rules[].when`) | yes | The condition. Every key present must hold (logical AND). At least one key. |
| `then` | object (see `rules[].then`) | yes | What the rule does when the condition holds: deny, or raise the risk level and hide fields. |
| `reason` | text | yes | Why the host wants this, for the audit trail. |

#### `rules[].when`

At least one key. Every key present must hold.

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `roles` | set condition | no | Condition on the roles of the verified context. |
| `scopes` | set condition | no | Condition on the capability scopes of the context token (capability IDs, exact match). |
| `tenants` | value condition | no | Condition on the tenant ID of the verified context. |
| `workspaces` | value condition | no | Condition on the workspace ID of the verified context. |
| `fields` | map: field name to field predicate | no | Conditions on the field values of the call. Evaluated when a call is prepared or committed, when the values are known; at exposure no values exist (ADR-0017). A comparison on a value that is absent or of the wrong type is indeterminate and counts as a match, so a rule never silently fails to narrow; add `present: true` to a predicate to say that it applies only when the field is there. |

A **set condition** tests a set of values the context has several of: `any` (at least one listed
value is present), `all` (every listed value is present) and `none` (no listed value is present).
A **value condition** tests the one value the context has: `in` (the value is listed) and `not_in`
(it is not). A **field predicate** holds when all of its operators hold: `eq` and `ne` (a string,
number or boolean), `in` and `not_in` (a list), `gt`, `gte`, `lt` and `lte` (numbers) and `present`.

#### `rules[].then`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `deny` | const `true` | no | Deny the capability. Takes precedence over every other outcome and cannot be combined with the other effects. |
| `risk_min` | integer 1-3 | no | Raise the risk level of the capability to at least this value. The effective level is the highest of the declared level, the platform floor of the effect class, every risk override and every matching risk_min; no value here can lower anything. A raise to 0 raises nothing and is not accepted. |
| `hide_fields` | array of field name (min 1, max 64) | no | Fields the model must not see or propose when the rule applies. At least one. |

## 11. RAG sources (`rag-sources/*.yaml`)

A source exists only if it is declared. AlixAI never crawls a machine, a share or an API by default,
and an undeclared or unreachable source is reported by the audit.

### Source definition requirements to fields (all required)

| Requirement | Field |
| --- | --- |
| source ID and type | `id`, `type` |
| (owner) | `owner` |
| application/tenant/workspace scope | `scope` (`application_id`, `tenant`, `workspace`) |
| root folder, mount or API endpoint | `root` (`kind`: `folder`, `mount`, `api` or `package`) |
| permitted path or query scope | `include`, `exclude`, and `query_scope` for API roots |
| credential reference held outside model context | `credential_ref` (an opaque `cred:` ID, or `null`) |
| accepted formats and size limits | `formats`, `limits` |
| synchronization method and interval | `sync` |
| deletion behavior | `deletion` |
| parser/OCR policy | `parser` |
| citation/open-preview resolver | `citation` |
| network and egress permissions | `egress` (entry IDs of the manifest's `network.egress`) |
| retention and audit policy | `retention` (`retention.audit`) |

The `type` fixes the valid `root.kind`: `local_folder` uses `folder`, `mounted_volume` uses
`mount`, `application_document_api` and `http_api` use `api`, `exported_dataset` uses `folder` or
`mount`, and `documentation_package` uses `package`.

A source belongs to a tenant and a workspace by binding: `request` (the tenant or workspace of the
verified request, so one declaration serves every tenant while retrieval is always filtered to the
caller's), `fixed` (one named ID) or `shared` (an explicit opt-in for content that is not tenant
specific; if the tenant binding is `shared` the workspace binding must be too). Retrieval scope is
applied before any text reaches a model.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | The source ID, unique in the package and equal to the key the manifest lists this file under. It is the package-level name; the core mints its own src_ identifier per registered instance. |
| `type` | enum: `local_folder`, `mounted_volume`, `application_document_api`, `http_api`, `exported_dataset`, `documentation_package` | yes | The source type. local_folder and mounted_volume: files on the host (the root kind must be folder or mount). application_document_api and http_api: documents or records from a scoped API (root kind api). exported_dataset: a read-only export in a folder or mount. documentation_package: versioned documentation shipped in the package (root kind package). The type fixes which root kinds are valid. |
| `title` | label | no | Optional human-readable name of the source, shown in citations when the source has no better display name. |
| `description` | text | no | Optional note on what the source contains. |
| `languages` | array of language tag (min 1, max 32, unique) | no | Optional. The languages the documents are written in; used for OCR and tokenisation hints. When absent the manifest languages apply. |
| `owner` | object (see `owner`) | yes | Who is accountable for the source. An ownerless source fails the audit. |
| `scope` | object (see `scope`) | yes | The application, tenant and workspace the source belongs to. Retrieval scope is applied before any text reaches a model. |
| `root` | object (see `root (one variant per kind)`) | yes | The root folder, mount or API endpoint. Nothing outside the root is ever read. |
| `include` | array of glob (min 1, max 64) | yes | The permitted path scope: globs relative to the root (for an API, request path patterns below the base). Only matching items are candidates. At least one: a source with no include rule matches nothing, which is not a declaration. |
| `exclude` | array of glob (max 64) | yes | Globs that remove items from the included set. Exclusion wins over inclusion. May be empty. |
| `query_scope` | object (see `query_scope`) | no | Optional, only for api roots: the permitted query scope. Without it only the root path and include patterns restrict an API source. |
| `credential_ref` | credential ref or null | yes | The credential reference held outside model context: an opaque cred: ID resolved from the OS secret store or an approved secret provider (`application_credentials` for api sources), or null when the source needs no credential. Never the secret itself. |
| `scope_params` | object | no | Optional, api roots only. `include_principal_id` (default false) adds the requesting principal id on every list/fetch so the host can filter per user inside a tenant. Optional `principal_param` renames that query parameter (default `principal_id`). The RAG index remains per tenant/workspace; per-principal filtering then happens on the host for every call. Do not mix principals' documents in one index without per-principal ACL at query time. |
| `formats` | array of MIME type (min 1, max 64, unique) | yes | The accepted formats as concrete MIME types. The type is sniffed from content, not trusted from the file name. Anything else is skipped and counted. At least one. |
| `limits` | object (see `limits`) | yes | Size limits. All three are required: an unbounded source is not accepted. |
| `sync` | object (see `sync`) | yes | The synchronisation method and interval. |
| `deletion` | object (see `deletion`) | yes | What happens when an item or the whole source disappears. |
| `parser` | object (see `parser`) | yes | The parser and OCR policy. Parsing and OCR run in disposable sandboxed workers regardless of this policy. |
| `citation` | object (see `citation (one variant per kind)`) | yes | The citation and open-preview resolver: how a citation becomes a permission-checked reference the user can open. |
| `egress` | object (see `egress`) | yes | The network and egress permissions of the source. |
| `retention` | object (see `retention`) | yes | The retention and audit policy of the source's derived data. |
| `egress_mode` | enum: `local_only`, `allow_excerpt`, `allow_metadata`, `deny` | no | Optional, default `local_only`. What an external model provider may receive from this source (external provider mode plan section 8). This is DATA egress, not the network allowlist `egress` above. `allow_excerpt` sends bounded, authorised excerpts, needs an explicit `classification` and is refused for a `restricted` source; `allow_metadata` sends titles, IDs and status only; `deny` excludes the source from the AlixAI capability entirely. A package can only restrict: the most restrictive mode along deployment, application, tenant, workspace and source wins, so declaring `allow_excerpt` grants nothing by itself. Ignored while external mode is off. |
| `classification` | enum: `public`, `internal`, `confidential`, `restricted` | no | Optional, default `internal`. How sensitive the content of the source is. A restricted source never leaves the deployment. |

#### `owner`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name` | label | yes | The accountable person, team or role. |
| `contact` | label | no | Optional contact, preferably a role mailbox or team channel. |

#### `scope`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `application_id` | application id | yes | The application ID. Must equal application.id of the manifest. |
| `tenant` | object (see `scope.tenant and scope.workspace`) | yes | How the source is bound to a tenant. |
| `workspace` | object (see `scope.tenant and scope.workspace`) | yes | How the source is bound to a workspace. If the tenant binding is shared, so must this be: a source common to every tenant cannot belong to one workspace. |

#### `scope.tenant and scope.workspace`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `mode` | enum: `request`, `fixed`, `shared` | yes | request: the tenant (or workspace) of the verified request scope; the source definition serves every tenant and retrieval is always filtered to the caller's. fixed: exactly the tenant or workspace named in id. shared: visible across all tenants (or all workspaces of the tenant); an explicit opt-in for content that is not tenant-specific, such as public documentation. |
| `id` | identifier | no | The tenant or workspace ID. Required for fixed, not allowed otherwise. |

#### `root (kind folder)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"folder"` | yes | Selects this variant. |
| `path` | string | yes | Absolute path (POSIX, or a Windows drive path) with no '..' segment. |

#### `root (kind mount)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"mount"` | yes | Selects this variant. |
| `mount_id` | slug | yes | The name of a mount the operator registered (HDD, SSD or NAS path visible to the host). The package never carries the host path of a mount. |
| `path` | relative path | no | Optional package-relative sub-path inside the mount. |

#### `root (kind api)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"api"` | yes | Selects this variant. |
| `endpoint` | slug | no | The ID of an mcp or openapi endpoint declared in the manifest. Exactly one of endpoint and url. |
| `url` | http(s) URL | no | Absolute http(s) URL of the API. Its host must have an entry in the egress allowlist the source references. Exactly one of endpoint and url. |
| `base_path` | string | no | Optional path prefix below the endpoint or URL; include patterns are relative to it. |

#### `root (kind package)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"package"` | yes | Selects this variant. |
| `path` | string | yes | Package-relative folder below documentation/. |

#### `query_scope`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `allowed_parameters` | array of string (max 32, unique) | yes | The only query parameter names AlixAI may send. May be empty: no parameters at all. |
| `fixed_parameters` | map: string to string | no | Parameters always sent with a fixed value, for example a dataset or collection name. Their names must not also appear in allowed_parameters. |

#### `limits`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `max_file_bytes` | integer (min 1, max 2147483648) | yes | The largest single item accepted, in bytes, 1 to 2 GiB. Larger items are skipped and reported. |
| `max_total_bytes` | integer (min 1, max 1099511627776) | yes | The most data the source may hold in total, in bytes, up to 1 TiB. Reaching it pauses ingestion and reports the source. |
| `max_files` | integer (min 1, max 10000000) | yes | The most items the source may hold, 1 to 10 million. |

#### `sync`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `method` | enum: `poll`, `event`, `manual` | yes | poll: AlixAI rescans every interval_seconds. event: application events mark items changed; interval_seconds optionally adds a reconciliation scan. manual: only an operator-triggered sync runs; there is no interval. |
| `interval_seconds` | integer (min 60, max 31536000) | no | Seconds between scans, 60 to 31536000. Required for poll, optional for event, not allowed for manual. |
| `event` | object | no | For method event: the events endpoint and event type that signal a change. Required for event, not allowed otherwise. |

#### `sync.event`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `endpoint` | slug | yes | The ID of an events endpoint of the manifest. |
| `event_type` | dotted id | yes | A key of that endpoint's event_types. |

#### `deletion`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `purge_after_hours` | integer (min 0, max 8760) | yes | When an item disappears from the source it is tombstoned and excluded from retrieval at once; its chunks, embeddings and cached answers are physically purged after this many hours, 0 to 8760. 0 purges immediately. |
| `on_source_removed` | enum: `purge`, `disable` | yes | What happens to derived data when the source declaration is removed or disabled. purge: tombstone everything and purge after purge_after_hours. disable: keep the derived data inert (never retrievable) until an operator purges it. |

#### `parser`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `ocr` | enum: `disabled`, `when_no_text_layer`, `always` | yes | disabled: no OCR. when_no_text_layer: OCR only pages without extractable text. always: OCR every page. |
| `ocr_languages` | array of language tag (min 1, max 8, unique) | no | The languages OCR is run in. Required unless ocr is disabled, and not allowed when it is. |
| `max_pages_per_file` | integer (min 1, max 10000) | yes | Pages parsed per file, 1 to 10000; the rest are skipped and reported. |
| `timeout_seconds` | integer (min 1, max 600) | yes | Wall-clock limit for parsing one file, 1 to 600. A parse that times out marks the item failed, never half-indexed. |

#### `citation (kind alixai_preview)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"alixai_preview"` | yes | Selects this variant. |

#### `citation (kind adapter_operation)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"adapter_operation"` | yes | Selects this variant. |
| `call` | adapter call | yes | The adapter operation that returns the open or preview reference for an item. |

#### `citation (kind url_template)`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `kind` | const `"url_template"` | yes | Selects this variant. |
| `url_template` | string | yes | An https URL with a fixed host and exactly one {item_id} placeholder in the path or query. The host cannot be templated. |

#### `egress`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `entries` | array of slug (max 16, unique) | yes | IDs of entries of the manifest's network.egress that this source may use. Empty means the source makes no network connection. Anything not listed is denied. |

#### `retention`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `derived_data_days` | integer (min 1, max 3650) or null | yes | How long derived data (chunks, embeddings, indexes) is kept, 1 to 3650 days, or null to keep it for as long as the source item exists. Derived data can always be rebuilt from the source. |
| `import_originals` | boolean | yes | false: originals stay owned by their provider and are read in place. true: AlixAI keeps an imported copy so previews survive a deleted original; the copy follows the deletion policy. |
| `audit` | object | yes | The audit policy for access to the source. |

#### `retention.audit`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `record_access` | boolean | yes | true: every retrieval from the source and every citation open is written to the audit log. |
| `retention_days` | integer (min 1, max 3650) | yes | How long those audit events are kept, 1 to 3650. |

## 12. Package tests (`tests/*.yaml`)

What the integration audit runs: synthetic fixtures for a
disposable tenant, scope probes, safe-execution probes, optional conversation cases and a runtime
smoke test. Everything is synthetic and test-only. A suite must contain at least one probe that
expects `allow` and at least one that expects `deny`, because a suite that only checks what works
proves nothing about isolation, and the smoke test must have a `text` path and verify cleanup.
That every capability and every consequential operation is covered is checked by the loader and
the audit.

The seven safe-execution probe kinds are `confirmed_commit`, `unconfirmed_commit`,
`replayed_commit`, `expired_preparation`, `stale_record`, `cancelled_preparation` and
`uncertain_commit_recovery`.

#### Step-up capabilities

A capability whose confirmation mode is `step_up` (level 3) is never committed by AlixAI: the
platform's coordinator refuses every commit of it with `step_up_required` before anything reaches
the application, and the application must refuse a commit it receives without the step-up in the same
way and apply nothing. The effect happens only when the user completes the step-up in the
application's own authenticated interface. That interface includes a dialog the host opens from its
embedded chat panel — it is still the host's authenticated UI; the AlixAI confirm path can never
complete level 3.

Both designs are accepted for applying the effect: the interface applies it itself (a commit through
the adapter then stays refused, and Status never has a record), or it records an approval and the
commit that follows applies the effect once, like any other commit.

**Reporting the outcome to AlixAI (1.5.0, ADR 0048).** `POST /v1/events` only invalidates. The
AG-UI `alixai_continue_in_app` view strips the idempotency key on purpose (the browser sees the
call). The host **backend** recovers the key and closes the loop:

1. `GET /v1/actions/{preparation_id}` with a context token of the same principal and session →
   `PreparedAction` with `idempotency_key` and effects.
2. Apply the effect in the host's own UI under that key. The host may link its own staged payload
   via a data version it returned from Prepare — data versions round-trip exactly.
3. Status answers `ok` | `queued` | `partial` with identifiers inside the prepared effects when
   done, or `rejected` with exactly one `validation_failed` error on field `idempotency_key` and
   no identifiers when there is no such effect.
4. Then `POST /v1/sessions/{session_id}/preparations/{preparation_id}/step-up-outcome` with body
   `{outcome: completed|declined|failed, reference?}`.

AlixAI always asks the adapter's Status for the preparation's key: `completed` is accepted only
when Status shows the effect; `declined` / `failed` are refused with `step_up_effect_present` when
Status shows the effect (report `completed` instead). A durable `ActionReceipt` is recorded
(committed or rejected) with provenance that it completed in the host's interface. The session
goal that still references the preparation is completed the same way an in-turn commit is
(success / cancelled / failure capsule with `ReceiptID`, and a `ResultCommit` when records exist),
so the **next turn sees the outcome** and must not re-propose that action. The receipt is also at
`GET /v1/actions/{preparation_id}/receipt`. Mid-stream `receipt` events are not published from this
route (the stream hub needs a turn id). Sequence:

```text
AG-UI alixai_continue_in_app (idempotency_key stripped for the browser)
  → host backend GET /v1/actions/{preparation_id}  (context token → PreparedAction)
  → host UI applies effect under idempotency_key (optional staged payload ↔ data version)
  → host Status: ok|queued|partial + identifiers  OR  rejected / validation_failed on idempotency_key
  → host backend POST …/step-up-outcome {outcome}
  → AlixAI Status-checks → ActionReceipt → goal completed / cancelled → next turn sees it
```

The seven probe kinds mean, for such a capability:

| Kind | What it means for a `step_up` capability | Declared outcome |
| --- | --- | --- |
| `confirmed_commit` | The commit of a fresh preparation, sent to the application without the step-up, is refused with `step_up_required` and applies nothing: no record under its key, and the records the action relies on keep the version Prepare reported. | `rejected_no_effect`, `step_up_required` |
| `unconfirmed_commit` | The same through the platform's coordinator: refused before it reaches the application. | `rejected_no_effect`, `step_up_required` |
| `replayed_commit` | The same commit sent again is refused again and still applies nothing. | `rejected_no_effect`, `step_up_required` |
| `expired_preparation` | A commit after the expiry is refused as `expired` by the application, and by the coordinator as `expired` or `step_up_required`; nothing reaches the application. | `rejected_no_effect`, `expired` |
| `stale_record` | As for any capability: a commit whose data versions are not current is refused as `stale` before the step-up is asked for. | `rejected_no_effect`, `stale` |
| `cancelled_preparation` | As for any capability. | `rejected_no_effect`, `conflict` |
| `uncertain_commit_recovery` | Status for the key is the no-record marker after the refused commits, which is what proves that a retry is safe. | `recovered_via_status` |

On top of the seven, the audit shows that the effect happens only after the step-up. Where the
audit's provisioner can complete a step-up in a disposable tenant (an optional ability, like changing
a record or making a dependency fail: a test-support operation, see Step-up approval in the fixtures below, that does what the supervisor does in
the application's interface; the audit holds no credential and learns none), it completes it and
checks that the effect is there afterwards and not before: either the records the action relies on
have a new version, which Validate reports, and a commit stays refused, or the commit that follows
applies the effect once, Status reports it and a repeat returns the original receipt. Without that
ability this part is recorded as unverified, never as passed, and the refusals above are still
proved. A commit that applies without the step-up is the critical finding `safety.step_up_bypassed`;
a step-up that changes nothing is `safety.step_up_without_effect`.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `suite` | slug | yes | The suite ID, unique in the package. |
| `description` | text | no | Optional note on what the suite covers. |
| `synthetic` | const `true` | yes | Always true. States that every tenant, principal and record in the suite is synthetic and privacy-safe. |
| `fixtures` | object (see `fixtures`) | yes | The disposable tenants, principals and records the probes use, and the application operations that create and remove them. |
| `scope_probes` | any | yes | Positive and negative scope probes. At least one probe must expect allow and at least one must expect deny: a suite that only checks what works proves nothing about isolation. A denied probe passes only if the unauthorized record or chunk never reaches the model. |
| `safety_probes` | array of object (see `safety_probes[]`) (max 1024) | yes | Safe-execution probes: prepare/confirm/commit, idempotency, expiry, cancellation, stale-record invalidation and uncertain-response recovery. Every consequential operation (risk level 1 or higher) needs probes of every kind; the loader and the audit check coverage. May be empty only for an application with read-only capabilities. |
| `conversation_cases` | array of object (see `conversation_cases[]`) (max 256) | no | Optional multi-turn conversation cases for the conversation test harness. Test-only: expectations are checked against deterministic results, never used as runtime policy. |
| `smoke` | object (see `smoke`) | yes | The runtime smoke test against a disposable tenant. |

#### `fixtures`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `provisioning` | object (see `fixtures.provisioning`) | yes | The application operations that create and remove the synthetic data. |
| `tenants` | array of object (min 2, max 16) | yes | The disposable tenants, by logical reference. At least two, so cross-tenant probes exist. The references are logical names: the audit generates the real tenant and workspace IDs for the disposable run. |
| `principals` | array of object (see `fixtures.principals[]`) (min 2, max 64) | yes | The synthetic principals, by logical reference. At least two, so cross-principal probes exist. |
| `records` | array of object (see `fixtures.records[]`) (min 1, max 1024) | yes | The synthetic records the seed operation creates, by logical reference. At least one. |

#### `fixtures.tenants[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `ref` | slug | yes | Logical tenant reference, unique in the suite. |
| `workspaces` | array of slug (min 1, max 16, unique) | yes | Logical references of the tenant's workspaces. At least one. |

#### `fixtures.provisioning`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `create_tenant` | adapter call | yes | The inspection or management operation that creates a disposable tenant with its workspaces and principals. |
| `seed` | adapter call | yes | The operation that loads the synthetic records. |
| `cleanup` | adapter call | yes | The operation that removes the disposable tenant and everything the run created. The smoke test verifies nothing is left. |
| `seed_data` | string | no | Optional package-relative path (below tests/) of the synthetic data file the seed operation reads. |

##### Provisioning over OpenAPI

The audit calls the three provisioning operations of an `openapi` endpoint itself, through the OpenAPI transport's provisioning bridge
(`internal/cli/transport_openapi_provision.go`, which calls `Adapter.Manage`); a model never can. The three calls of `fixtures.provisioning` must be three distinct
`openapi_operation_id`s of one `openapi` endpoint, and every one must be listed in that endpoint's `management.provisioning` (section 5); a package that lists fewer, or names
an operation that is not listed, answers `validation_failed` to the audit's provisioning, and a package with no suites leaves the runtime probes unverified as before.
The outcome is carried by the HTTP status, with the same egress, document and size checks as any call and no retry:

- **create_tenant:** a 2xx is success (a `status` member, if present, must be `ok`; `rejected` is a refusal; a `tenant_id` in the reply is optional but must match). `409` is `conflict` and
  the tenant is not adopted. A 5xx, no reply or an unusable reply means the tenant is adopted for cleanup but never seeded.
- **seed:** the reply must answer every record asked for exactly once, each with an identifier of 1 to 256 characters and no control characters.
- **cleanup:** a 2xx must state `existed` (boolean) and `remaining` (non-negative counts). A `404` means the tenant is already gone (`existed: false`, nothing remaining) unless the body
  says it existed or rows remain, which is refused as contradictory.
- **Other failures:** the application's `errors[0].code` if it is a platform error code; otherwise 400, 404 and 422 are `validation_failed`, 401 and 403 `permission_denied`, 409 `conflict`,
  429 `rate_limited` and 5xx `unavailable`. No server text ever reaches an error.

The bridge acts only on tenants the audit created in the run. The package format declares no operation for changing a record or injecting a fault over OpenAPI, so
`safety:validate_notices_change` stays unverified for an OpenAPI application.

##### Provisioning over MCP

The audit calls the three provisioning tools of an `mcp` endpoint itself, through the MCP transport's provisioning bridge
(`internal/cli/transport_mcp_provision.go`); a model never can. For the bridge to run them:

- All three calls name the same `mcp` endpoint and an `mcp_tool` each, and every suite of the package names the same three tools.
- Each tool is listed in that endpoint's `tools.allow` (section 5) and should be mapped by no capability: what a capability maps can be reached from a conversation.
- The server offers them while the application runs for an audit, in a test support mode, and does not offer them to a production deployment.
  AlixAI never calls them outside an audit. A server that does not offer all three leaves the audit's runtime probes (checks 2, 6, 8, 9 and 10)
  as they would be with no provisioning at all: every capability is `unverified` because no disposable tenant could be reached, critical for a
  capability that changes data (risk level 1 or higher) and a warning for a read-only one. The same happens when the tools are declared but
  not approved in `tools.allow`.

Three further test tools are conventions of the bridge, and are used only when the package approves them in `tools.allow`, no capability maps them and the server lists them
(a server without them leaves the checks they serve unverified, never passed):

- `test_change_record` lets the audit change a record of a disposable tenant behind the application's back, so the check that a prepared action notices a changed record
  (`safety:validate_notices_change`) can run.
- `test_inject_fault` makes the application fail a named operation in a named way for the disposable tenants it is given only, with the arguments
  `{operation, capability_id, kind, tenant_ids}`; the application must confine the fault to those tenants.
- `test_approve_step_up` completes the step-up of a level 3 preparation, as the user it was prepared for does in the application's own interface (see Step-up approval below).

The identifiers are generated by the audit for the run: a tenant `tnt_<ref>_<suffix>`, a workspace `ws_<ref>_<suffix>` and a principal
`usr_<ref>_<suffix>`, where `<ref>` is the logical reference of the suite with `-` written as `_`, and `<suffix>` is the run's lower-case hex
suffix, the same in every identifier of one disposable world. The logical references never reach the application, except as the `ref` of a
seeded record. Every argument and every reply is the `structuredContent` of the tool result; text is never read, and a tool error
(`isError`) is an error, not a refusal.

| Tool | Arguments | Reply when it succeeds |
| --- | --- | --- |
| `create_tenant` | `tenant_id`; `workspaces`: `[{workspace_id}]`; `principals`: `[{principal_id, workspace_id, roles, service?}]` (`service` is present, and true, only for a service principal) | `{status: "ok", tenant_id, ...}` |
| `seed` | `tenant_id`; `records`: `[{ref, entity_type, workspace_id, principal_id?, ...}]` | `{status: "ok", tenant_id, records: [{ref, entity_id, ...}]}`: the identifier the application gave each record, by `ref` |
| `cleanup` | `tenant_id` | `{status: "ok", tenant_id, existed, remaining}`: `existed` is a boolean and `remaining` an object of non-negative counts by kind of record, `{}` or all zeros when nothing is left; both must be present |

Every reply names the `tenant_id` it answers for; a reply for another tenant is refused. A refusal is a result, never a tool error:
`{status: "rejected", errors: [{code, ...}]}`, and `code` is one of the platform's error codes (section 4). The application:

- fails `create_tenant` with `conflict` when the tenant exists, and refuses `seed` and `cleanup` for a tenant it did not create as disposable
  (`permission_denied`): the audit only asks about its own tenants, but the application must not rely on that;
- answers `cleanup` of a tenant that does not exist with `existed: false` and nothing remaining, so that every call can be repeated, and
  removes everything the run created, which the audit proves by calling `cleanup` again;
- seeds exactly the records it is given and returns an identifier for each; a record it cannot create fails the whole call.

The audit refuses, before it sends anything, to seed or clean up a tenant it did not itself create in the run (`permission_denied`),
to seed into a workspace the tenant does not have, and to send an argument that is not an identifier. A `create_tenant` the application
refused leaves that tenant the application's; one whose reply was lost may have created it, so it is cleaned up and never seeded. The calls
use the connection of the endpoint's adapter and its egress allowlist, are bounded in size, are never repeated and carry no credential
(there is no credential service yet).

The seed data file (`seed_data`) is JSON with a `records` array of objects, below `tests/` of the package, at most 1 MiB, a regular file. The entry
whose `ref` a seeded record has adds its members to the record sent to `seed`, so the application can be told what the audit's conversations
call a record (a patient's name, an appointment's reference). Three members belong to the audit and are never sent as they are:
`workspace` (the logical workspace; the record carries the generated `workspace_id`), `principal` (the logical reference of a fixture
principal, replaced by that principal's generated `principal_id`, which links the record to the principal's identity) and `entity_type`
(it must be the suite's, or the seed is refused); the members `tenant`, `tenant_id`, `workspace_id` and `principal_id` of an entry are ignored.
A `principal` that is not a fixture principal of the tenant, a `ref` listed twice, and an entry without a `ref` refuse the seed. Every other
member is the application's own and is passed through unchanged. The clinic reference application is the worked example:
`examples/reference-apps/clinic-booking/package/tests/seed/clinic.json`.

##### Step-up approval

The audit completes the step-up of a level 3 preparation in a disposable tenant (see Step-up capabilities above) with one more operation of the application. The package
format declares no call for it, so its name is a convention of each transport, and it is used only when the package approves it, no capability maps it and the application
offers it while it runs for an audit:

| Transport | The operation | Approved in |
| --- | --- | --- |
| `mcp` | the tool `test_approve_step_up` | the endpoint's `tools.allow` (section 5) |
| `openapi` | the operationId `approveStepUp` | the endpoint's `management.provisioning` (section 5) |
| `native` | the management method `approve_step_up` | the endpoint's `management.provisioning` (section 5) |

The arguments are the same for all three: `tenant_id`, `workspace_id`, `principal_id`, `capability_id`, `idempotency_key` and `values`, the text and identifier facts
of the application's own Prepare reply by field, from which the application takes what it told the user to quote (a reference); for OpenAPI the `tenant_id` may be a path
parameter. The reply states `status: "ok"`, the `tenant_id` asked about and `approved: true`; for OpenAPI that is a 2xx reply, and a refusal is the
`{status: "rejected", errors}` the other operations answer with. The audit sends only a preparation of a disposable tenant it created, for one of that tenant's principals,
and the application applies every rule its own step-up interface applies. Without the operation the provisioner does not offer the ability and the effect-after-step-up part
of the audit is recorded as `unverified`, never as passed.

#### `fixtures.principals[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `ref` | slug | yes | Logical principal reference, unique in the suite. |
| `tenant` | slug | yes | The reference of one of the fixture tenants. |
| `workspace` | slug | yes | The reference of one of that tenant's workspaces. |
| `kind` | enum: `human`, `service` | yes | human or service principal. |
| `roles` | array of identifier (min 1, max 16, unique) | yes | Roles the principal's context carries. Each must be declared in the manifest's tenancy.roles. At least one. |

#### `fixtures.records[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `ref` | slug | yes | Logical record reference, unique in the suite. |
| `entity_type` | slug | yes | The entity type of the record. |
| `tenant` | slug | yes | The fixture tenant the record belongs to. |
| `workspace` | slug | yes | The fixture workspace the record belongs to. |

#### `scope_probes[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Probe ID, unique in the suite. |
| `dimension` | enum: `tenant`, `workspace`, `principal`, `role`, `record`, `document`, `source` | yes | The scope boundary the probe tests: tenant, workspace, principal, role, record, document or source. |
| `operation` | enum: `read`, `validate`, `prepare`, `commit`, `status`, `retrieve` | yes | What is attempted: a capability operation (read, validate, prepare, commit, status) or retrieve, a RAG retrieval. |
| `capability_id` | capability id | no | The capability attempted. Required for every operation except retrieve. |
| `principal` | slug | yes | The reference of the fixture principal making the attempt. |
| `target_record` | slug | no | The reference of the fixture record the attempt targets, for record-level probes. |
| `target_source` | slug | no | The ID of the RAG source retrieved from, for source and document probes. |
| `expect` | enum: `allow`, `deny` | yes | allow: the attempt succeeds. deny: it is refused with error_code, and nothing about the target is disclosed. |
| `error_code` | error code | no | The error code a denied attempt must produce. Required for deny, not allowed for allow. |

#### `safety_probes[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Probe ID, unique in the suite. |
| `capability_id` | capability id | yes | The consequential capability probed. |
| `principal` | slug | yes | The reference of the fixture principal acting. |
| `kind` | enum: `confirmed_commit`, `unconfirmed_commit`, `replayed_commit`, `expired_preparation`, `stale_record`, `cancelled_preparation`, `uncertain_commit_recovery` | yes | confirmed_commit: prepare, confirm, commit once. unconfirmed_commit: commit without the required confirmation. replayed_commit: the same commit sent twice. expired_preparation: commit after the preparation expired. stale_record: a record changes between prepare and commit. cancelled_preparation: commit after cancellation. uncertain_commit_recovery: the commit response is lost, and status decides whether to retry. |
| `expect` | object (see `safety_probes[].expect`) | yes | The required outcome. |

#### `safety_probes[].expect`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `outcome` | enum: `committed_once`, `rejected_no_effect`, `original_receipt_returned`, `recovered_via_status` | yes | committed_once: the effect was applied exactly once. rejected_no_effect: the attempt failed with error_code and changed nothing. original_receipt_returned: a repeat returned the first receipt and changed nothing more. recovered_via_status: status established what happened and the action was neither lost nor duplicated. |
| `error_code` | error code | no | The error code of a rejected attempt. Required for rejected_no_effect, not allowed otherwise. |

#### `conversation_cases[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Case ID, unique in the suite. |
| `language` | language tag | yes | The language of the user turns. |
| `capability_id` | capability id | yes | The capability the conversation is about. |
| `principal` | slug | yes | The reference of the fixture principal talking. |
| `turns` | array of object (see `conversation_cases[].turns[]`) (min 1, max 20) | yes | The user turns in order, each with what the controller must do. At least one. |

#### `conversation_cases[].turns[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `user` | string | yes | What the user says in this turn. |
| `expect` | object | yes |  |

#### `conversation_cases[].turns[].expect`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `act` | slug | yes | The dialogue act of the resulting DialogueDirective (the vocabulary is defined by the conversation object schemas, docs/integration/conversation-objects.md), for example ask_missing, disambiguate, request_confirmation, request_step_up, report_outcome or report_failure. The generated act enum is the vocabulary; ask_missing_field is read as ask_missing. |
| `missing_fields` | array of field name (max 64, unique) | no | The fields the controller must report as missing. |
| `fields` | map: field name to example value | no | Field values the controller must hold after the turn. |
| `error_code` | error code | no | The error code the turn must produce, for a failing turn. |

#### `smoke`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `paths` | array of object (see `smoke.paths[]`) (min 1, max 64) | yes | The end-to-end paths exercised in the disposable tenant. A text path is required; voice, rag and action paths are listed when the application has them. |
| `verify_cleanup` | const `true` | yes | Always true. After the run the audit verifies that the disposable tenant and every record the run created are gone. |

#### `smoke.paths[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | slug | yes | Path ID, unique in the suite. |
| `path` | enum: `text`, `voice`, `rag`, `action` | yes | text, voice (a synthetic transcript), rag (a question answered from a source) or action (a prepare, confirm, commit round trip). |
| `principal` | slug | yes | The reference of the fixture principal. |
| `capability_id` | capability id | no | The capability exercised. Required for the action path. |
| `utterance` | string | yes | What the synthetic user says or asks. |
| `expect` | object | yes |  |

#### `smoke.paths[].expect`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `audit_events` | array of dotted id (min 1, max 32, unique) | yes | The audit event types the path must produce, so the audit trail is exercised along with the behaviour. At least one. |
| `citations` | boolean | no | For the rag path: true if the answer must carry original-source citations, false if the path must return no_supported_evidence. |

## 13. Signatures and integrity (`signatures/signature-manifest.json`)

The signature manifest is generated by the signing tool and has two parts.

- `signed` is the **integrity manifest**: the application ID, the package version, the creation
  time, the hash algorithm and the list of **every file of the package except those under
  `signatures/`** with its SHA-256 and size, sorted ascending by path. Excluding `signatures/` is a
  fixed rule: the signature material cannot cover itself. Everything else is listed, so a file that
  is found in the package but absent from the list, or listed with another digest, is tampering.
- `signatures` holds detached **Ed25519** signatures over the **canonical JSON** of `signed`:
  UTF-8, object keys sorted, no insignificant whitespace (the payload has only strings and integers,
  so this is also the RFC 8785 form). Signatures are standard base64 with padding (88 characters).
  Several signatures allow a key rotation overlap; the package verifies if at least one is valid,
  unexpired, from an unrevoked key registered for the application and listed in the manifest's
  `integrity.publisher_key_ids`.

`alixai.yaml` carries no digest of its own: it cannot hash itself, so its `integrity` block only
points at this file and names the publisher keys. The signature is verified before anything else in
the package is trusted. A package without a valid signature is unsigned and cannot leave
quarantine. Every signature has an `expires_at`; an expired signature fails closed.

The signing and verification code (`internal/trust`, M1-05) is stricter than the schema alone, and
the loader relies on every one of these rules:

- **`signatures/` holds exactly one file, `signature-manifest.json`.** Any other file or directory in
  it is unsigned content and is refused (tampering), as is a package entry named exactly `signatures`
  where the directory belongs. The manifest itself is capped at 32 MiB.
- **No two paths differ only by letter case.** Such a pair collides on a case-insensitive file system,
  where one file would silently replace the other after the digests were checked.
- **No duplicate `key_id`** among `signatures`. At most four signatures; at least one must be
  acceptable.
- **`signed_at` may not lie more than five minutes in the future** (clock skew); a signature dated
  further ahead is refused, and so is one past its `expires_at`.
- **Canonical JSON is integer-only.** The signed payload has strings and integers only; a float, an
  exponent form or a duplicate object key makes the document malformed. Digests and the byte-order
  sorting of the file list are exact.
- **Key states.** A key is `active`, `retired` or `revoked` in the trust store. A **retired** key was
  rotated out in the normal course: signatures made before the retirement still verify until they
  expire, later ones do not. A **revoked** key verifies nothing, whatever the signature says about
  when it was made, and revocation is permanent: the key material cannot be registered again, under
  any name or for any application. A key must be registered for the package's own application and be
  listed in `integrity.publisher_key_ids`; nothing is trusted by default.
- **The platform's application ID is reserved.** `alixai.platform` carries AlixAI's own signing keys
  (audit reports, release manifests); a package whose `application.id` is `alixai.platform` is
  refused by the loader.

The loader verifies the signature on the **same in-memory snapshot** it then parses (section 14), so
there is no gap between the bytes that were checked and the bytes that were used.

The example `signature-manifest/valid/demo-library-signed.json` carries a real Ed25519 signature
over its `signed` object, made with a throwaway test-only key (the file digests are placeholders,
because the example files are the parsed JSON form and not the YAML files they stand for). It is
not a trust anchor for anything. Its public key (raw 32 bytes, base64) is `jS/ItlLHFpgCvo2a/LFu4R5GdYX3r6vOZ/Z+EMjVuZc=`, registered nowhere.

### Reference

#### Top-level properties

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `format` | const `"alixai.signature-manifest/v1"` | yes | The file format and its major version. |
| `signed` | object (see `signed`) | yes | The signed payload. Every byte of it is covered by every signature. |
| `signatures` | array of object (see `signatures[]`) (min 1, max 4) | yes | Detached signatures over the canonical JSON of signed. At least one. Several allow a key rotation overlap; a package verifies if at least one signature is valid, unexpired, by an unrevoked key registered for application_id and listed in the manifest's integrity.publisher_key_ids. |

#### `signed`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `application_id` | application id | yes | The application the package is for. Must equal the manifest's application.id; it binds the signature to one application so a package cannot be replayed for another. |
| `package_version` | semver | yes | The package version the signature covers. Must equal the manifest's package.version. |
| `created_at` | UTC timestamp | yes | When the integrity manifest was generated. |
| `hash_algorithm` | enum: `sha256` | yes | The digest algorithm of the files list. Must equal the manifest's integrity.hash_algorithm. |
| `files` | array of object (see `signed.files[]`) (min 1, max 100000) | yes | Every file of the package outside signatures/, sorted ascending by path (byte order), one entry per path (the loader rejects duplicates and unsorted lists). At least one: alixai.yaml itself is always listed. |

#### `signed.files[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `path` | relative path | yes | The package-relative path. Portable and traversal-free by grammar; nothing under signatures/ is ever listed. Symbolic links are refused by the signing tool and have no entry. |
| `sha256` | sha-256 hex | yes | SHA-256 of the file's bytes. |
| `size` | integer (min 0, max 1099511627776) | yes | The file size in bytes. |

#### `signatures[]`

| Property | Type | Required | Meaning |
| --- | --- | --- | --- |
| `key_id` | identifier | yes | The ID of the publisher key that made the signature, as registered in the trust store for the application. |
| `algorithm` | enum: `ed25519` | yes | The signature algorithm. Only ed25519 exists; the verifier accepts exactly this one. |
| `signature` | string | yes | The 64-byte Ed25519 signature in standard base64 with padding (88 characters). |
| `signed_at` | UTC timestamp | yes | When the signature was made. |
| `expires_at` | UTC timestamp | yes | When the signature stops being valid. Required: a signature never lasts forever, and an expired one fails closed. Must be later than signed_at. |

## 14. What the loader checks beyond the schemas

The schemas check the shape of each file. The loader (M1-04, `internal/apppkg`) checks that the files
agree with each other, with this list as its specification. Failures that concern one capability
fail that capability; failures of the package as a whole keep it in quarantine.

**How the loader works.** It is deterministic: it never asks a model anything ("the
audit itself is deterministic"), reads no file outside the package, and gives the same findings in the
same order for the same bytes.

1. The package (a directory or a `.tar.gz`) is read **once** into an immutable in-memory snapshot.
   Symbolic links, devices, hidden or unsafely named paths, duplicates, case collisions and anything
   over a limit (file count, size, depth, compression ratio) are refused here, before anything is
   parsed. An archive is never extracted to disk.
2. `alixai.yaml` is parsed and its schema checked. A manifest that declares the reserved application
   ID `alixai.platform` is refused.
3. The signature is verified with `trust.Verifier.VerifyPackage` against the snapshot, using the
   application ID, package version and `integrity.publisher_key_ids` that `alixai.yaml` declares.
4. Every other file is parsed as plain YAML (section 2: no anchors, aliases, merge keys or tags,
   unique keys, quoted look-alikes), validated against its schema and decoded into the typed package.
5. The cross-file checks below run, then the policies are compiled by `policy.ParseHostPolicy` and
   `policy.NewPackage`.

Each problem is one **finding**: `check_id` (a stable dotted name such as `docs.facet_missing`),
`severity` (`critical`, `warning` or `info`), the `capability` it concerns when there is one, the
package-relative `path`, a `message` and a `remediation`. Any critical finding means no typed package
is returned, and nothing in the package can reach a model. Warnings are facts worth fixing that do not
block. `alixai package validate` prints the findings as JSON and exits 1 when any is critical.

An install (`apppkg.Installer`) re-runs the load with a trust store, refuses anything critical or not
verified, writes the verified bytes under `<data_dir>/packages/<application>/<manifest hash>/`
(content-addressed by the SHA-256 of the canonical signed integrity manifest, directories 0700 and
files 0600) and records the package as `quarantined` in the module's table. The states are
`quarantined`, `active`, `rejected` and `superseded`; **only the integration audit (M1-17) may make a
package `active`**, and neither the loader nor the installer has a function that does. Reading an
installed package back verifies it again, so a changed file or a key revoked since the install means
the package no longer opens.

**The package as a whole**

1. `format` is known; every YAML file obeys the authoring rules and validates against its schema;
   every path in the package passes the path grammar and no path is a symbolic link.
2. Signature: the signature manifest validates, every file outside `signatures/` is listed with the
   right digest and nothing else is present, `signed.application_id` and `signed.package_version`
   equal the manifest's, and at least one signature is valid, unexpired, by a key registered for the
   application and listed in `integrity.publisher_key_ids`.
3. Compatibility: `compatibility.alixai_core.min` is lower than `max_exclusive` and the installed
   core version lies in `[min, max_exclusive)`.
4. Uniqueness: endpoint, capability, source, term, documentation-entry, policy and suite IDs;
   within a capability: field names, ambiguity rule IDs, validation rule IDs, next-action IDs and
   example IDs; one conversation contract per (capability, language).
5. Manifest consistency: `languages.default` is in `languages.supported`; every `capabilities` key
   equals the `id` of its file and every listed file exists; no file under `capabilities/` or
   `rag-sources/` is unlisted; every `rag_sources` key equals the `id` of its file.
6. Endpoints: every operation, resolver and provisioning call names an endpoint that exists and is
   `mcp`, `openapi` or `native`, with a call key that matches its transport; events endpoints appear
   only as event sources; every `adapter_config`, `spec` and `result_schema` file exists; the host
   of every MCP and OpenAPI endpoint (and port), loopback included, has a `network.egress` entry.

**Each capability**

7. Fields: every `entity_ref` field names an entity type of the capability; a `default` fits the
   field type; a constraint (`min_length`, `minimum`, `max_items`, ...) applies to the field type; an
   ambiguity rule with action `use_default` refers to a field that has a default; ambiguity rules,
   examples and next actions refer to existing fields and capabilities, and a next action points at
   a capability whose audience is `user`.
8. Validation: every rule's `fields` exist and its `error_code` is one of the capability's
   `failure_states`.
9. Idempotency: `idempotency.window_seconds` is at least `prepared_action.ttl_seconds`.
10. Documentation: every entry named in `doc_refs` exists and belongs to the capability; every entry
    of the capability is in `doc_refs`; the seven facets of section 8 are covered in the default
    language, for every field, validation rule, permission, failure state, the risk level, the
    confirmation rule and every user-visible outcome; every document location exists.
11. Conversation contract (audience `user`): one per supported language; a prompt for every required
    field; a clarification for every ambiguity rule with action `ask_user`; a failure response for
    every failure state; `confirmations` present exactly when `confirmation.mode` is not `none`, with
    `step_up_required` when it is `step_up`; `corrections.prepared_action_invalidated` when there is
    a `prepare`; success outcomes equal to the documented `user_visible_outcome` entries; a label for
    every next action; correctable fields exist; terminology IDs exist and have a label in the
    language of the contract.
12. Placeholders: a `{input.x}` placeholder names a field of the capability; any other placeholder
    names a fact declared in `response_obligations`; every obligation that applies on success or
    always is stated in at least one success response, and every obligation that applies on prepare
    or always is stated in the confirmation request.

**Policy, terminology, sources and tests**

13. Policy: every role is in `tenancy.roles`, every capability is in `capabilities`, every projected
    field exists and no required field is hidden by a projection; no role is granted twice and no
    capability has two risk overrides or two overlapping projections; a risk override that is not
    higher than the declared level is reported. Conditional `rules` (section 10) are checked the same
    way: every role in `when.roles`, every capability and every field named in `when.fields` or
    `then.hide_fields` must exist, rule IDs are unique across the package, and a rule that can hide a
    required field is reported as a warning (it can make the capability unusable while it applies).
    The effective level of a capability is the highest of its declared `risk_level`, the floor of its
    `effect_class`, any platform rule of the core, any policy override and any matching rule's
    `risk_min`.
14. Terminology: an `entity_type` is an entity type of some capability; a manifest language missing
    from a term's labels is reported.
15. RAG sources: `scope.application_id` equals `application.id`; every `egress.entries` ID exists in
    `network.egress`; the `api` root names an `mcp` or `openapi` endpoint that exists or a URL whose
    host has an egress entry; a `sync.event` names an `events` endpoint and one of its event types;
    a `package` root exists inside the package; every required source-definition field is present (the
    schema makes this structural).
16. Tests: every tenant, workspace, principal, record, capability and source reference resolves;
    principal roles are in `tenancy.roles`; every capability of risk level 1 or higher has all
    seven safe-execution probe kinds; every capability and source has probes; `seed_data` exists.
17. Egress declarations (external provider mode; all members optional, the default egress mode is
    local_only and the default classification internal): a `restricted` source cannot declare `egress_mode`
    `allow_excerpt` or `allow_metadata` (`egress.restricted_source_exposed`); `allow_excerpt` needs an
    explicit `classification` (`egress.excerpt_classification_missing`); with `external.forbid: true`
    no source declares `allow_excerpt` or `allow_metadata` (`egress.forbidden_by_package`); no
    template of a conversation contract contains an `{input.<field>}` placeholder for a field marked
    `local_only` (`egress.local_only_field_in_template`). All four are critical findings.

**Audit (against the running application, not the package)**

The audit then compares the declared surface with the discovered MCP, OpenAPI and native
operations, validates real results against `result_schema`, executes the scope, safe-execution and
smoke tests, and reports undocumented, unreachable, over-broad or ambiguous operations. Anything the
application does not expose for inspection is recorded as `unverified`.

## 15. Decisions this contract takes

The design leaves these open; they are decided here so that every consumer reads the same
format, and each can be revisited by changing the schema version.

1. **Authoring format.** YAML on disk, schemas over the parsed JSON, with the plain-data rules of
   section 2. The signature manifest is JSON because it is generated and hashed byte for byte.
2. **Compatibility range** is a structured pair, `min` inclusive and `max_exclusive`, both required,
   rather than an npm-style range string: no range parser is needed and a package can never claim
   compatibility with every future core.
3. **Adapter configuration** is a file below `schemas/` referenced from the endpoint, because the
   package layout has no adapters directory. Its format belongs to the adapters.
4. **Operations are mapped per operation** with a transport-specific key (`mcp_tool`,
   `openapi_operation_id`, `native_method`), and an events endpoint cannot own an operation.
5. **Effect class, risk, operations and confirmation are coupled in the schema** as section 6
   shows. The application declares `effect_class`, from which the platform risk floor is derived
   deterministically, and a `risk_level` below that floor is invalid; level 0 means read-only;
   every state-changing operation is prepared, has a status lookup and is idempotent.
6. **`audience`** (`user` or `admin`) decides whether a capability needs a conversation contract.
7. **The policy is declarative only.** It has no expression language, no regular expression and no
   wildcard. Conditional host overrides are the closed `policy/v1` `rules` of ADR-0017: a fixed set of
   tests (roles, scopes, tenant, workspace, field comparisons) and three effects (deny, raise the risk
   level, hide fields). No rule grants anything, and the policy parser and the policy gateway are the
   same code in the loader and at runtime.
8. **The context token format is not pinned** by the package; ADR-0013 decides it.
9. **Documentation coverage is a loader check**, not a schema check, so that a gap fails one
   capability with a precise finding.
10. **Response wording is template text** with `{fact}` and `{input.field}` placeholders, one
    contract per capability and language.
11. **Sources are bound by `request`, `fixed` or `shared` scope** and always have an owner; a
    shared tenant binding forces a shared workspace binding.
12. **Egress is exact-host**, `http` only to loopback, a loopback endpoint needs an entry like any other (and the platform's own ports are refused even when listed), and sources refer to manifest egress entries
    instead of repeating hosts.
13. **Signature manifest.** `signed` is canonical-JSON signed with detached Ed25519 signatures,
    `signatures/` is excluded from its own file list, signatures carry a mandatory expiry. The
    signing and trust-store code (`internal/trust`, M1-05) implements this shape, with the stricter
    rules listed in section 13.
14. **Test probes use logical references** (`network-a`, `member-a1`); the audit generates the real
    identifiers for the disposable run.
15. **Schema features, for tools that read the schemas** (code generators, editors): JSON Schema
    2020-12 with `$ref` into `schemas/common` and `defs.schema.json`, `$defs`, `oneOf` unions
    discriminated by a `const` property (`transport`, `kind`, `source`) or by which of `mcp_tool` /
    `openapi_operation_id` / `native_method` is present, `if`/`then`/`else`, `dependentRequired`,
    `contains`, open maps built from `propertyNames` plus a typed `additionalProperties`, and a
    property schema of `false` to forbid a property under a condition. Patterns avoid lookahead so
    they run unchanged on RE2 and ECMA-262 engines.

16. **Egress declarations restrict, never grant.** `egress_mode`, `classification`, `local_only`
    and `external.forbid` are optional and read with the safest value when absent, so a package that
    declares none behaves exactly as in Local mode and the integration audit says the defaults were
    assumed (`external.defaults_assumed`). The mode itself (Local, External, Hybrid) is deployment
    and tenant policy, not a package field. The data-egress `egress_mode` is a separate member from
    the network allowlist `egress`: the first says what content may leave, the second where
    AlixAI may connect.

## 16. Validate your package

```bash
# the schemas and their examples
go run ./tools/schemacheck schemas/application-package

# list why each invalid example is rejected
go run ./tools/schemacheck -v schemas/application-package
```

A package is validated by `alixai package validate` (task M1-04), which applies the schemas of this
document to each file and then the rules of section 14. It takes a directory or a `.tar.gz`, prints
the report and its findings as JSON on stdout, and exits 1 when any finding is critical (2 for a
mistake on the command line).

```bash
# a valid package: exit 0; without a trust anchor the digests are checked but nobody vouched for the
# signature, which is the warning signature.not_checked
go run ./cmd/alixai package validate examples/packages/valid-minimal

# establish who signed it: trust a publisher key for this run (base64 of the raw 32-byte public key)
go run ./cmd/alixai package validate -trust-key KEYID=PUBLICKEY package-dir

# check compatibility against another core version
go run ./cmd/alixai package validate -core-version 0.5.0 package-dir

# sign a package (the signature must be redone after every change)
go run ./cmd/alixai package sign -app demo-library -version 1.0.0 -key KEYID=KEYFILE package-dir
```

`examples/packages/` holds a complete valid package (`valid-minimal`, the synthetic `demo-library`
application) and several broken ones, each one defect away from it: `broken-unsigned`,
`broken-tampered`, `broken-missing-doc-facet`, `broken-missing-contract`,
`broken-no-adapter-mapping`, `broken-rag-no-scope`, `broken-missing-safety-probes` and
`broken-incompatible-core`. They are signed with the published TEST-ONLY publisher key
(`tools/devkeys`), which every real trust store refuses; pass `-allow-test-keys` together with
`-trust-key` to verify them in a test.

Installing a package (`apppkg.Installer.Install`) needs a trust store with the publisher key
registered for the application; there is no way to install a package that is not verified.
