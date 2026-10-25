# 0017 — Stable policy-expression format for host overrides

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-16 (provisional default); finalised in M1-07

## Context
A host application can tighten what AlixAI does for it: which roles may use which capability, which
capability scopes a token must carry, which fields the model may see, and a higher risk level for an
action. §25 item 8 asks for a *stable* format for those overrides, because it becomes part of the
public Application Integration Package (§9) and cannot be changed lightly once hosts write it.

The constraints that decide the format, in order:

1. **Host policy can raise, never lower.** Platform floors (side-effecting capabilities at risk 2 or
   above, privileged or legal ones at 3 or above, §11.4) are computed in code; a host rule that
   would reduce them must be impossible to express, not merely rejected at run time. Deny by
   default; a capability is visible to the model only if package, scope, role, tenant/workspace and
   policy all allow it (§4.3, rule 6).
2. **Schema-validated**, like every other part of the package contract: a JSON Schema in
   `schemas/` is the contract, validated at package activation (§9.4).
3. **Bounded and explainable:** evaluation cost is bounded by the size of the document, and every
   decision returns a record suitable for the audit log.
4. **Footprint:** rule 16 says to try the smallest method first (a plain data format with no
   engine), and the binary, RSS and wake-up cost count toward the Lite and 4 GB budgets.

What M1-01 actually shipped, which the provisional version of this ADR got wrong: the package's
`policy` schema (`schemas/application-package/policy.schema.json`) is a **closed document** with
three sections, `role_grants`, `risk_overrides` and `field_projection`, no `format` key and no room
for conditions. It does not carry a policy "opaquely under a `format` discriminator". The package
documentation (section 10 and decision 7) says the conditional language "waits for ADR-0017". So
the decision below is an additive extension of that document, not a replacement of it.

## Options
All four candidates were measured on one rule set: 100 host rules over 20 capabilities, in four
kinds (a field threshold that raises the level, a workspace that is closed, a role that must not see
a field, a currency list), evaluated for one capability of the shape "accountant records a payment of
120000 minor units in EUR". Dev box i7-10700K, Windows 11, Go 1.27.0. Harnesses are scratch programs
outside the repository, one `main` per candidate sharing the same measuring code. Sizes are
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w"` over a baseline harness
of 1,556,640 bytes that prints one line. RSS is the Windows working set after a forced GC and 100,000
evaluations, as growth over the baseline harness at the same point (9.37 MB). Latency is the per-call
time (QueryPerformanceCounter) over 100,000 calls after 2,000 warm-up calls; p99 on a desktop
Windows host includes scheduler and GC noise, so it is an upper bound.

| Candidate | Binary growth | RSS growth | p50 | p99 | What was timed |
| --- | --- | --- | --- | --- | --- |
| **A. closed declarative `policy/v1`** (`internal/policy`) | +1.83 MB as a standalone harness, **+1.04 MB in the real `alixai` build** (2,875,552 to 3,915,936 bytes, measured with a temporary reference from `cmd/alixai` that was removed afterwards) | **+2.4 MB** | 6.8 us (mean 8.9 to 13.7 us, 25 allocations) | 39 to 44 us | the whole gateway decision: 12 ordered checks, rules, risk, projection and the audit record |
| B. cedar-go v1.8.0 | **+0.83 MB** | +1.0 MB | 10.6 to 11.3 us | 37 to 80 us | `Authorize` evaluated over all 102 policies in this harness |
| C. cel-go v0.32.0 (`cel.dev/cel-go`) | +8.84 MB | +6.6 MB | 1.9 to 2.6 us | 8 to 13 us | the 5 compiled programs of one capability, evaluation only |
| D. OPA v1.21.1 | +21.6 MB | +14.5 MB | 23.6 to 27.2 us | 174 to 185 us | `PreparedEvalQuery.Eval` of `data.alixai` |

Binary growth of A is mostly stdlib (`regexp`, `encoding/json`, `reflect`) that the finished `alixai`
links anyway for other reasons, so its real cost is lower than the table. The earlier figures in the
provisional ADR (cel-go +6.4 MB, OPA +20.7 MB) were from smaller harnesses; the table is the
current measurement. Row A times the whole gateway decision and rows B, C and D only the evaluation of
the rules, so the rows are not a benchmark of the engines against each other; they show what each would
add to `alixai`.

A. **Closed declarative rule format** (JSON; the YAML in `policies/*.yaml` is converted to the JSON
   data model by the package loader). A fixed vocabulary: conditions on roles, token capability
   scopes, tenant, workspace and the field values of a call; effects `deny`, `risk_min` and
   `hide_fields`. No operator lowers a risk level and none grants, so "can only narrow or raise"
   holds by grammar. No new dependency. Not Turing-complete, trivially bounded, version-tagged
   (`policy/v1`). Limit: no relationships, arithmetic or record lookups; new needs mean a new format
   version.
   - Security: smallest attack surface and no expression engine to sandbox; a hostile document is a
     loader error. Resource: row A above.
B. **Cedar.** Purpose-built authorisation language with deny-overrides semantics. Smallest binary of
   the engines, and RSS growth is small. Costs: a principal/action/resource/entity model to map onto
   our scope, a second language for host teams, text policies rather than data, no notion of "raise
   the risk level" (it would be policy annotations we interpret), and "cannot lower a floor" becomes a
   run-time check after evaluation instead of a property of the format.
C. **CEL.** Typed, non-Turing-complete expressions with cost limits. Evaluation is the fastest, the
   price is +8.8 MB of binary and +6.6 MB of RSS, and it is only an expression language: effects,
   deny-overrides, risk raising and projection would all be ours to build around it, and "cannot
   lower" would again be a run-time check.
D. **OPA / Rego.** Most expressive and widely known. +21.6 MB of binary (2.4 times CEL's and 26
   times Cedar's), +14.5 MB RSS and a p99 four to twenty times the others'. Rego is a general logic
   language, more power than a security boundary that hosts write needs. Too heavy for Lite and the 4 GB
   direction for what the reference applications need.
E. **Not evaluated:** Casbin, and relationship-based systems (OpenFGA, SpiceDB) that need a server
   and a store of their own, which §5.1 rules out for the minimum profile.

### Do the reference applications need more than A?
The three reference applications do not exist yet (M1-20 clinic-booking, M3-12 facility-maintenance,
M3-13 ledger), so this matrix is built from their **planned deliverables** in the build plan of milestones M1
and M3 (roles, tenancy model, capabilities and risk levels) and from the packaged
`demo-library` examples. It must be re-checked when those applications are written; a need it misses
is a finding against this decision, not something to patch around.

| Host-policy need | Where | Expressed by |
| --- | --- | --- |
| Which role may use which capability (patient may only see availability; auditor is read-only; viewer may only find invoices) | all three | `role_grants` (no rule) |
| A patient changing contact details must see and confirm the exact change (level 1 raised to 2 for one role) | clinic-booking | rule: `roles.any` + `risk_min` |
| Hide clinical or bank fields from the model for some roles | clinic-booking, ledger | `field_projection`, or rule: `roles.any` + `hide_fields` |
| No bookings at a site that is closed; technicians cannot close work orders at a site under audit | clinic-booking, facility-maintenance | rule: `workspaces.in` (+ `roles.any`) + `deny` |
| Closing a work order that consumed expensive parts, or recording a payment above 10,000.00, is completed in the application's own UI | facility-maintenance, ledger | rule: `fields.<name>.gte` + `risk_min: 3` |
| Only some currencies | ledger | rule: `fields.currency` (`present` + `not_in`) + `deny` |
| Accountants cannot void large invoices, managers can | ledger | rule: `roles.none` + `fields.<name>.gt` + `deny` |
| Decommissioning an asset and voiding an invoice are level 3 | facility-maintenance, ledger | platform floor from `effect_class: privileged`; no host policy needed |
| A receptionist of site A must not read site B; an assignee must belong to the site | clinic-booking, facility-maintenance | not policy: tenant/workspace binding is checked by the gateway, record-level permission is the host's reauthorisation (§4.3) |
| Double booking, a slot taken between prepare and commit, four-eyes approval, an amount against a customer's credit limit, time-of-day windows | clinic-booking, ledger | not policy: application invariants and live data (§4.3); the application validates and reports `conflict`, `stale` or `validation_failed` |

Every need that is host policy is expressed with the shipped operators and no addition. The rows that
are not policy are not gaps: they need record lookups or time, which is exactly what a closed,
bounded, side-effect-free format must not do, and §4.3 gives them to the application.

## Decision
**Option A, the closed declarative `policy/v1` format. Cedar stays the planned extension and is not
implemented. CEL and OPA are not adopted.**

### The format
`schemas/policy/host-policy.schema.json` (`urn:alixai:schema:policy:host-policy:v1`) is the M1-01
policy document, same three sections with the same shapes (they are `$ref`s into the package schema,
not copies), plus two optional keys:

- `format`: `"policy/v1"`. Optional, because it is the only defined format and an M1-01 policy has no
  such key. A different value, such as a future `cedar/v1`, is refused by a loader that does not know it.
- `rules`: conditional rules. Each is `{id, capabilities[], when, then, reason}`.
  - `when` is the AND of the keys present (at least one): `roles` and `scopes` take `any`, `all` and
    `none` lists; `tenants` and `workspaces` take `in` and `not_in`; `fields` maps a capability field to a
    predicate of `eq`, `ne`, `in`, `not_in`, `gt`, `gte`, `lt`, `lte` and `present`, ANDed.
  - `then` is `{deny: true}`, or `risk_min` (1 to 3) and `hide_fields` together or alone. Never an
    allow, never a lower level.
  - There are no expressions, no regular expressions, no wildcards, no arithmetic and no references.
    An unconditional rule is not expressible on purpose: it is a role grant left out, a risk override or a
    field projection.

An M1-01 policy with no `rules` and no `format` is a valid `policy/v1` document, and the test
`TestHostPolicySchemaAndParserAgree` runs every policy example M1-01 shipped through both the new
schema and the Go loader. What the package schema rejects, `policy/v1` rejects too.

What changed from the provisional sketch: the sketch had `effect: allow` rules and a `risk: {min}`
object. Allow rules are gone. Two places that can grant make "who may do this" a question of which
file wins; `role_grants` is now the **only** grant, and a rule can only deny, raise or hide, so
"cannot widen what the platform denied" is a property of the grammar (`TestNoRuleShapeCanGrant`). Risk
raising is `risk_min` plus the M1-01 `risk_overrides`.

### How it is evaluated (`internal/policy`)
`Gateway.Decide` runs these checks in a fixed order and stops at the first failure; each check lands in
the audit record. The zero `Decision` is a denial.

1. request well formed and its context live; 2. `Scope` valid (every level, no wildcard);
3. package status is exactly `active` (anything else, including an unknown value, is `quarantined`
and exposes nothing, not even which capabilities exist); 4. scope, package and the stored binding name the
same application (capability confusion, SEC-053); 5. the binding's tenant and workspace equal the
scope's (a token for another tenant or workspace against a session or prepared action is
`scope_violation`); 6. the capability is declared; 7. `audience` is `user` (admin capabilities are never
model-visible); 8. the token's capability scopes name it; 9. a role of the principal that the package
declares in `tenancy.roles` is granted it by `role_grants`; 10. no host rule denies it; 11. every required
field stays visible; 12. at commit, the confirmation and step-up the level demands.

- **Risk.** `effective = max(declared, platform floor, risk_overrides, applicable rule risk_min)`,
  computed only through `contracts.RiskLevel.Escalate`, which never lowers and resolves an invalid
  level to 3. The floor comes from `effect_class`: none 0, reversible 1, external 2, privileged 3. A
  declaration whose `effect_class` the core does not recognise, whose audience, confirmation mode or
  risk level is invalid, is denied as `schema_invalid` (the risk calculation underneath would give it
  floor 3). A package that declares a level below its floor is raised, and the record says so under
  `raised_by`.
- **Phases.** Policy runs at exposure, prepare and commit. At exposure no field values exist: a deny
  rule with a field condition is **deferred** (recorded under `rules_deferred`, decided at prepare and
  commit with the values); a rule that raises the level or hides fields is **applied as if it held**, so
  the exposed level is an upper bound and the model is never shown a tool labelled lower, or with more
  fields, than prepare will enforce. At prepare and commit a comparison on a missing or wrongly typed
  value is *indeterminate and counts as a match*, so a rule never silently stops narrowing; `present:
  true` in the same predicate scopes a rule to calls that carry the field.
- **Evidence at commit.** The level (or the capability's own `confirmation.mode`) demands an explicit
  confirmation from level 2 and a step-up from level 3. `Evidence` defaults to nothing, so a caller that
  forgets to pass it is denied with `confirmation_required` or `step_up_required`.
- **Fields.** Visible fields are the declared fields, narrowed by the projection of every role that
  grants the capability (**the intersection** across roles, the least-capability choice: a role without
  a projection does not widen it), by applicable `hide_fields` rules, and by withholding sensitive fields
  until the projection names them or the current task needs them. A projection never exposes an
  undeclared field. A required field withheld by a projection or a rule makes the capability unusable and
  is denied. The model never sees a field value through the record: values are never recorded.
- **Merging policies.** A package may hold several policy files. Compilation refuses ambiguity: a role
  granted twice, a capability with two risk overrides, overlapping projections, a repeated policy or
  rule ID. A policy that names a role, capability or field the package does not declare cannot become
  a package at all.
- **Audit.** Every decision, allow or deny, returns a `Record`
  (`schemas/policy/decision-record.schema.json`): the checks that ran with a fixed reason code each, the
  policy or rule behind each outcome, the SHA-256 of each policy in force, the risk calculation, the
  visible and withheld fields, the rules applied and deferred. Only well-formed identifiers are copied
  into it: a context level or a capability ID that fails the identifier grammar (a model proposal, a
  malformed token) is recorded as empty and the failing check says why, so text from a user or a model
  cannot reach the audit log through a decision. A `Gateway` built with `WithSink` hands
  every record to the sink and, if the sink fails, turns the decision into a denial (`unavailable`): a
  decision that cannot be audited is not made.

### Why not Cedar now
Cedar passes the footprint test (+0.83 MB binary, about +1 MB RSS) and the matrix shows no gap A cannot
express, so the overturn criteria are not met: Cedar would be added for expressiveness the reference
applications do not need, as a second language, and it moves "cannot lower a floor" from the grammar to a
run-time check. The extension point is the `format` key. Adding Cedar later means a second parser and a
second compiled form behind the five methods the gateway calls on `*Policy` (`grantedRoles`,
`overrideFor`, `projectionFor`, `rulesFor`, `Refs`), which would become an interface at that point; no
stored `policy/v1` document changes.

**Overturn criteria (unchanged).** Adopt Cedar as a second format if a reference or customer application
needs relationships policy/v1 cannot express **and** the measured growth stays small (it is: +0.83 MB,
+1.0 MB RSS). Adopt CEL or OPA only with a measured need A and Cedar cannot meet and a stated budget for
+8.8 MB or +21.6 MB of binary.

## Consequences
- Easier: the contract is plain data with a JSON Schema, readable by any host stack, evaluated with no
  engine; "cannot lower" and "cannot grant" are properties of the grammar; explanation is built in; a
  decision costs about 10 us with 100 rules loaded.
- Harder: a deliberately small vocabulary means host teams sometimes ask for more; each addition is a
  versioned format change reviewed for security. Conditions on values that need record lookups or time
  stay in the application, by design.
- Security impact: the model never owns permissions (rule 5); host policy is deterministic and deny by
  default, quarantine exposes nothing, a cross-tenant context is `scope_violation`, and every decision
  is audited with the rule ids behind it. SEC-025, 036, 043, 060 and 087 have named tests in
  `internal/policy`; a fuzz target (`FuzzParseHostPolicy`) checks the loader never panics and that
  nothing it accepts can lower a floor or grant to an unlisted role. A stale or unparsable policy
  document fails closed (the package cannot be built, hence cannot be active).
- Forbidden: executing host-supplied code, expressions or regular expressions; any construct that
  grants outside `role_grants` or lowers a platform floor; evaluating policy inside the model; a rule
  that cannot be decided being silently ignored.

## Follow-ups outside this task's write scope
1. `schemas/application-package/policy.schema.json` must accept `format` and `rules` (a `$ref` to
   `urn:alixai:schema:policy:host-policy:v1#/$defs/rule`, or to the whole document). Until then a rule
   cannot be written in a package.
2. `docs/integration/application-package.md` section 10 and decision 7 still say the policy has no
   expression language.
3. `docs/decisions/README.md` still lists this ADR as provisional.
4. M1-04's loader should hand each parsed `policies/*.yaml` to `policy.ParseHostPolicy` and build the
   package with `policy.NewPackage`, so the runtime and the loader enforce the same references.
5. M1-12 (coordinator) should call `Gateway.Decide` at prepare and commit with the stored binding and the
   evidence it has obtained; M1-13 (capability router) should shortlist from `Gateway.Expose`.

## Reversal plan
Callers use `Gateway.Decide` and `Gateway.Expose` and get a `Decision` with a `Record`; the format is
parsed by `policy.ParseHostPolicy`, merged by `policy.CompilePolicies` and evaluated behind
`ActivePackage.Policy()`. Replacing A means translating stored `policy/v1` documents to the new format
with a mechanical converter (the grammar is small enough for that to be exact) and keeping `policy/v1`
accepted for one release. The package signature covers the policy document, so a migration ships as a
new signed package version. Adding Cedar is the second implementation described above, selected by
`format:`.

## Sources (checked 2026-10-02)
- cedar-go: <https://github.com/cedar-policy/cedar-go> (v1.8.0); CEL for Go: <https://pkg.go.dev/cel.dev/cel-go> (v0.32.0)
- OPA: <https://github.com/open-policy-agent/opa> (v1.21.1)
- Measurements: scratch harnesses outside the repository (one `main` per candidate, shared timing and
  working-set code); the `policy/v1` numbers also come from `go test -bench` on `internal/policy`
  (`BenchmarkDecide`, `BenchmarkExpose`, `BenchmarkParseHostPolicy`).

## Update 2026-10-03 (M1-21): one audit record per exposure
`Gateway.Expose` writes ONE record per call (`alixai.policy-exposure/v1`) listing every declared capability with its outcome, typed code and effective risk, instead of one
event per capability; `Decide` at prepare, commit and exposure keeps one full event per decision. A catalogue above 256 entries or 24 KiB is split into numbered parts, and if any
part cannot be written `Expose` returns nothing (SEC-087). The event type `policy.exposure` is registered in `internal/policy/policyaudit` (policy cannot import audit without an
import cycle in audit's own tests). Measured per dialogue read turn at 100 capabilities: 102 to 3 policy events, 366 KB to 14.4 KB, 249 ms to 8.2 ms (docs/efficiency/M1-21.md).
