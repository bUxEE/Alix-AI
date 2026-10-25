# 0016 — Several applications in one process, or one isolated deployment per security domain

- **Status:** accepted
- **Date:** 2026-10-03
- **Decided in task:** M0-16 (provisional default); finalised in M3-15; security domain enforced in M3-37

## Context
§25 item 7 asks whether the first stable release supports several applications in one AlixAI
process or recommends one isolated deployment per security domain. The plan already points both
ways. The data model is application-scoped everywhere (every table and index carries
`application_id, tenant_id, workspace_id`, §10.2; cache keys include authorisation scope), and
§7.3 lists "several registered applications" as a Performance-profile use case. Lite and Balanced
describe "one small application". On the other side, rule 17 forbids model duplication and
always-on services the 4 GB direction cannot afford, while rule 10 and §16.2 demand real isolation.

"Security domain" here means a set of applications whose operators accept the same trust level and
the same blast radius (one company's CRM and helpdesk, say), as opposed to different organisations
or data classes that must not share a failure or a compromise.

Kubernetes documentation frames the same trade-off as *soft* multi-tenancy (tenants with some
mutual trust, shared infrastructure, cost-efficient) versus *hard* multi-tenancy (mutually
untrusted tenants, which warrant separate clusters). AlixAI's equivalent of a cluster is a
deployment.

What a shared process shares, which is the whole question:

- the Go process memory, the Go runtime and any bug in it;
- one SQLite file and its single writer (a bulk writer in one application delays all);
- one LocalAI instance, hence one resident model, one set of KV slots and, in current LocalAI
  releases, a llama.cpp prompt cache that is enabled by default (since 4.3.0, per its news page).
  AlixAI's own cache keys include scope, but LocalAI's prompt cache does not know about
  applications, so cross-application reuse of cached prefixes needs review;
- the job queue, quotas and the host's CPU.

## Options
A. **Always one application per process, one deployment per security domain, enforced.** Strongest
   isolation: separate process, database file, OS user and LocalAI. Cost: every deployment repeats
   alixai, LocalAI and the resident model (the model weights alone are 237 MB to 1.02 GB for the
   Lite rungs in [0010](0010-lite-model-selection.md)), so N applications cost N times the
   baseline, against rule 17.
   - Security: strongest; a compromise or noisy neighbour stays in one domain. Resource: worst.
B. **Several applications per process with no stated trust boundary.** Cheapest, but advertises
   isolation the shared process cannot guarantee against a bug, a malicious package or adapter,
   or a noisy neighbour. Rejected.
C. **Several applications per process within one security domain; separate deployments between
   domains** (soft multi-tenancy by scope, hard multi-tenancy by deployment).
   - Applications are isolated by scope everywhere (rows, indexes, caches, jobs), per-application
     issuer keys ([0013](0013-application-context-token.md)), per-application packages, policy and
     egress allowlist (M1-09), and per-application quotas ([0018](0018-resource-accounting-quotas.md)).
   - Different organisations or incompatible data classes get separate deployments, which may
     share model *files* by read-only bind mount but not processes.
   - Security: logical isolation only inside a deployment, so a flaw in the shared core or in an
     adapter reaches every application in it; that is acceptable only because they share a trust
     level. Resource: one baseline plus a measured marginal cost per application.
D. **Per-application SQLite database inside one process.** Adds file-level separation and
   independent backup per application. Does not remove shared memory, writer scheduling or
   LocalAI sharing, and complicates `store`. Costed in M3-15 (see Measurements): every database
   brings its own page-cache budget, 16,000 KiB by default (the writer's cache plus one reader
   cache for each pooled reader), against 0.4 to 0.9 MiB measured for an application in the
   shared store, so it is not adopted as a default; it stays an operator hardening option.

## Decision
**Option C, accepted.** The first stable release **supports several applications in one process
within a single security domain** and **recommends (and documents as the required pattern) one
isolated deployment per security domain**, where domains differ by organisation, data class or
required fault isolation. Lite defaults to one registered application, matching §7.1; the limit is
configuration, not code. The documentation states the isolation class plainly: logical isolation by
scope inside a deployment, process-level isolation between deployments.

It is accepted on the two results that decide it, both measured in M3-15 (below): an additional
application costs a fraction of one percent of what a second deployment costs, and no application
reached another's session, preparation, receipt, audit events or sources in the two-application
scenario. The overturn criteria below are unchanged and stay armed: the scenario is a regression
suite, run on every change (`go test ./tests/conformance/package/`).

## Measurements (M3-15)
Taken on a development host, **not certified**: Windows 11, amd64, Go 1.27, 16 logical CPUs, the
in-process service of `tests/conformance/package` (one store, audit log, policy gateway, capability
registry, action coordinator, session store and dialogue engine for all applications; each
application is the clinic-booking reference package under its own id, signed again with the
TEST-ONLY key, over its own in-process host; the model is the scripted provider). Three runs agree
within 1.1 MiB on every figure and within 0.2 MiB on the marginal ones. Resident set is the working
set after a full collection and a return of freed memory to the system.

| Process state | Resident set | Go heap in use |
| --- | --- | --- |
| before any application (test binary, runtime, store, engine) | 19.6 to 19.7 MiB | 3.4 to 3.6 MiB |
| 1 application registered, idle | 24.3 to 24.6 MiB | 6.2 to 6.3 MiB |
| 16 applications registered, idle | 29.6 to 30.7 MiB | 10.2 to 11.0 MiB |
| 1 application after 25 rounds of traffic (a read and a booking held at its confirmation) | 36.6 to 37.5 MiB | about 12 MiB |
| 16 applications after the same traffic in each | 49.1 to 49.6 MiB | 7.6 to 8.0 MiB |

| Marginal cost | Resident set | Go heap |
| --- | --- | --- |
| the first application (one-time: schemas and package machinery loaded once) | +4.7 to +4.9 MiB | +2.6 to +2.8 MiB |
| each further application, idle (from 1 to 16) | **+0.35 to +0.42 MiB** | +0.26 to +0.32 MiB |
| each further application, under load (the same traffic served in 15 more) | **+0.78 to +0.87 MiB** | not meaningful (the heap shrinks as the collector settles; the negative values are noise) |

A second deployment adds, at the very least, the weights of the smallest Lite model candidate,
resident in a LocalAI process of its own: 237 MB (Q4_K_M, [0010](0010-lite-model-selection.md)),
before alixai's own process (about 12 MiB with the store open, M0-05), LocalAI, the backend's buffers
and the KV cache. An extra application is therefore 0.19 % (idle) to 0.4 % (loaded) of the floor of a
second deployment, against the decision rule of five per cent (11.3 MiB per application). The rule is
asserted in the test, so a regression of that size fails it.

What the numbers do not include, so they are not read as more than they are: the host application
itself (it runs elsewhere), the transport's connection state (one pooled HTTP client per endpoint),
and the RAG index of an application's sources, which is the real storage and memory driver of a
corpus (M0-05, M2-04, M2-07: for instance 38.4 MB per 100,000 chunks if int8 vectors are cached
resident) and belongs to the source, not to the registration. A package larger than the clinic's
costs more to load: the figure scales with the package, not with the number of applications.

Cross-application isolation (`TestTwoApplicationsShareNothing`): two applications are registered in
one set of services with scopes identical in every level but the application (same tenant,
workspace, principal and session ids). A prepares a booking and registers a RAG source. B, holding
A's preparation id and A's own confirmation token, cannot read, confirm, reject, commit or read the
receipt of A's preparation (`scope_violation`, "no such preparation in this scope"), whether it uses
the coordinator or a conversation turn; B's session holds none of A's goal or transcript; B lists,
reads and removes none of A's sources; each audit export names only its own application; neither
host is asked to commit, and B's host is not asked anything until B acts. After A confirms, exactly
one commit reaches A's host, and B can book the same slot of its own host: nothing was shared. Every
request each host received carried its own application id and no other.

## Owed checks, and what became of them
1. Marginal resident memory per application: **done**, above.
2. Cross-application isolation: **done** for sessions, prepared actions, confirmations, receipts,
   audit events and sources, above. The token and its keys are covered by the identity layer's own
   tests, which the scenario does not repeat: a key registered for one application cannot verify a
   token of another (`internal/identity` `TestIssuerKeyScopedToApplication`) and a token for another
   audience is refused (`TestWrongAudienceRejected`). Still owed to the modules, outside M3-15's
   scope: two-application fixtures inside each module's own tests (store, FTS, vectors, jobs, caches,
   adapters), which the mandatory cross-tenant fixtures of §10.2 should extend to cross-application.
3. Noisy neighbour (p95 of an interactive turn in B while A runs bulk ingestion): **not measured
   here.** It needs the job queue, a live ingestion and a live model, which a hermetic conformance
   test cannot carry, and the fair per-tenant dequeue and quotas it exercises are decided in
   [0018](0018-resource-accounting-quotas.md), finalised in M5-06. It is measured with them (the
   degradation and performance suites), and a result that shows B starved by A reopens this ADR
   through its overturn criterion. Until then the pattern the ADR recommends contains the risk: several
   applications in one process are, by definition, one security domain, one operator and one blast
   radius.
4. LocalAI's prompt cache across applications: **not measured here; reasoned, review owed to M5-10.**
   The cache is keyed by prompt prefix inside one LocalAI instance and does not know about
   applications. Within a deployment that serves one security domain, reuse of a cached prefix
   between its applications crosses no trust boundary the operator has drawn, and AlixAI's own caches
   keep scope in their keys. The consequence is a rule rather than a setting: one LocalAI instance
   serves one security domain, so deployments of different domains share model *files* (read-only
   bind mount) and never a process. M5-10 reviews the cache's behaviour and, if the review finds a
   way to leak across prompts of one domain that matters, disables or partitions it.
5. A security-domain label on the manifest: **enforced at install and at activation (M3-37).**
   `security_domain` is an optional slug of the manifest schema (M1-01), recorded in the package's
   install record. A process (one data directory) now refuses to install, and refuses to activate, an
   application whose domain differs from the domain of the applications already active in it. The
   refusal is a typed error (code `conflict`, details `reason: security_domain_mismatch`, the phase
   and both applications) and an audit event, `package.domain_refused`, on the refused application's
   own chain and on no other. The rule is `internal/apps`; two readings the schema left open are fixed
   there and are part of this decision:
   - **Only other applications count.** The new version of an application, which supersedes its own
     previous version when it is activated, never conflicts with itself; the domain of a lone
     application may change between versions.
   - **No domain is a domain of its own, compared by equality.** An application that declares none has
     the domain "", which equals only "": undeclared applications share a process with each other (the
     three reference applications declare none, and the two-application suite runs one package twice)
     and with no named domain, and a named domain never silently covers an undeclared one. An operator
     who wants two applications kept apart declares two domains.

   It is enforced by the store, not by the callers: two triggers on the package table
   (`internal/apps/migrations/0001_security_domain.sql`) run inside the transaction that inserts or
   activates a package, so no installer, audit runner or later caller can skip them and a concurrent
   activation of another application cannot fall between a check and a change. A trigger can raise only
   a literal abort, which the installer and the runner report as an unavailable store error, so
   `apps.Guard` and `apps.AsRefusal` give the refusal its type and event, and `alixai package install`
   and `alixai audit run` are wrapped in `internal/cli` (`cmd_package.go`) to call them when the store
   refused. Moving the process to another domain is a deliberate act: take every active application of
   the old one out of service (`alixai audit quarantine`), then activate the new one; only packages that
   are active count. `TestActivationRefusesADifferentSecurityDomain` drives the real commands through
   the whole rule and `TestActivationOfADifferentSecurityDomainNeedsTheRuleToBeRefused` runs the same
   deployment with the triggers dropped to show that what refuses is the rule. The triggers refuse new
   changes, not existing rows: a deployment that already had active applications of two domains when
   it upgraded keeps serving them, and `apps.Guard.Others` lists them, so the operator can split it.

The ADR is accepted without 3 and 4 because neither can change what it decides unless it finds a
failure, and a failure has a defined reversal (below); they are owed, named and owned, not dropped.

**Overturn criteria.** If any cross-application leak cannot be closed structurally, or marginal
memory is not small, downgrade to option A: one application per process, enforced at registration.
That is a configuration cap, so the data model does not change.

## Conformance of each application before it joins a process
`alixai-conformance` (`cmd/alixai-conformance`) is what an application team, or an operator adding an
application to a shared deployment, runs against the package and the running application: package
validation, the full integration audit, and every example of every capability contract replayed
through the dialogue engine against the application, with JSON and JUnit output. Registering a
second application is not a reason to skip it for the second one, and the two-application suite above
is what shows the deployment still isolates them. The replay runs over any transport a package
declares (MCP, OpenAPI or native): the adapter is built by `cli.BuildPackageAdapter`, the same code
path `alixai serve` and `alixai audit run` use, and the disposable tenants come from that transport's
provisioning bridge. It was MCP-only until M3-37; a package this build has no transport for is still
reported as not run, never skipped quietly.

## Consequences
- Easier: one small footprint for related applications, one model resident, one ops surface; the
  documentation has a single clear rule for when to split.
- Harder: every provider and query path needs two-application tests forever; operators must decide
  what a security domain is, and the docs must make wrong answers obvious.
- Security impact: an application package or adapter that is compromised can affect all applications
  in the same deployment, so activation (§9.4) must treat an application as untrusted *towards its
  neighbours' data* even when trusted to run. No application can see another's scope, sessions,
  sources, prepared actions, receipts or audit records (measured, above). The manifest carries an
  optional `security_domain` label, and a process refuses to install or activate an application that
  declares a different one than the applications already active in it (item 5 above): sharing a domain
  is a rule the process enforces, and choosing the domains is the operator's.
- Forbidden: advertising hard isolation inside one deployment; sharing a secret, key or cache entry
  across applications; an application with no `application_id` scope.

## Reversal plan
The scope model is identical in both options, so reversing is a registration limit. To move to A,
cap applications at one and migrate each additional application to its own deployment by exporting
its scoped rows (they all carry `application_id`) and re-importing them; vectors travel with
their chunks, so nothing is re-embedded. To move from A to C, raise the limit and
run the two-application suite (`go test ./tests/conformance/package/`) on the deployment first.

## Sources (checked 2026-10-02; measurements 2026-10-03)
- Kubernetes multi-tenancy (soft versus hard): <https://kubernetes.io/docs/concepts/security/multi-tenancy/>
- LocalAI releases (prompt cache default in 4.3.0): <https://localai.io/blog/>, <https://github.com/mudler/LocalAI/releases>
- Measurements: `go test ./tests/conformance/package/ -run Marginal -v` (`tests/conformance/package/memory_test.go`)
