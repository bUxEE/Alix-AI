// Package policy is the AlixAI policy gateway: the deterministic authority
// that decides, before any model sees a tool and again at prepare and commit,
// whether a verified context may use a capability, at which risk level and with
// which fields (master plan 3 rules 5, 6, 14; 4.1; 10.2; 11.4; ADR-0017).
//
// The model never owns permissions. This package does not call a model, touch
// the store, read a clock other than the injected one or run host-supplied
// code. It imports only pkg/contracts, internal/clock and internal/ids.
//
// # Decisions
//
// A Gateway answers Decide(Request) with a Decision. Deny is the default in
// every direction: the zero Decision is a denial, an unknown package status is
// a denial, and a check that cannot be evaluated is a denial. A capability is
// allowed only if every check passes, in this order, and evaluation stops at
// the first failure:
//
//  1. request       the request is well formed and its context is live
//  2. scope         the verified Scope has every level (contracts.Scope.Validate)
//  3. package       the package is exactly "active" (quarantine exposes nothing)
//  4. application   scope, package and binding name the same application
//  5. tenant        the binding's tenant and workspace equal the scope's
//  6. declared      the capability is declared by the package
//  7. audience      the capability is a user capability, not an admin one
//  8. token         the context token's capability scopes name the capability
//  9. role          a role of the principal is declared by the package and
//     granted the capability by role_grants (the only place anything is granted)
//  10. rules        no host rule denies it
//  11. fields       every required field stays visible to the model
//  12. evidence     at commit, the confirmation and step-up the level demands
//
// # Risk
//
// The effective risk level is the highest of the declared level, the platform
// floor derived from the capability's effect class (none 0, reversible 1,
// external 2, privileged 3, anything unrecognised 3), every host risk override
// and every host rule that applies. It is computed with contracts.RiskLevel's
// Escalate, which only ever raises, and nothing in the policy grammar can name
// a lower level: the host may raise a level and cannot lower a platform minimum.
//
// # Fields
//
// The visible fields are what the model may see or propose: the declared fields,
// narrowed by the field projection of every role that grants the capability
// (intersection across roles, the least-capability choice), by host rules that
// hide fields, and by withholding sensitive fields until they are projected
// explicitly or named by the current task.
//
// # Audit
//
// Every Decision carries a Record (schemas/policy/decision-record.schema.json)
// naming the checks that ran, the policy or rule behind each outcome, the risk
// calculation and the visible fields. A Gateway built with WithSink hands the
// sink a record for every decision, allow or deny, and fails closed when the
// sink fails. The records come in two kinds of the one schema, told apart by
// Record.Format:
//
//   - a decision record (RecordFormat) is the full account of one decision. Decide
//     writes one for every call, at every phase, so each decision that matters
//     (prepare and commit above all) has its own audit event, and Expose writes
//     one when the package itself is refused (a quarantined package, a wrong
//     application or tenant);
//   - an exposure record (ExposureFormat) is the compact account of one Expose
//     call: for every capability the package declares, whether the model may be
//     shown it, the typed code and reason of a denial, and the effective risk
//     level. It replaces one decision record per declared capability, which made
//     the audit cost of every turn grow with the size of the catalogue. The
//     decisions Expose returns are unchanged; what the record leaves out of them
//     (the checks, the field lists, the rules) follows from the package, the
//     policy digests the record names and the subject, and is written in full
//     whenever the capability is used. A catalogue too large for one audit event
//     (MaxExposureOutcomes entries or MaxExposureRecordBytes) is written as
//     several numbered parts; see ExposureBatch. A call that wrote fewer parts than
//     it announced exposed nothing.
//
// An exposure whose record cannot be written exposes nothing (Exposure.Code is
// unavailable): a decision that cannot be recorded is not made (SEC-087).
//
// This package does not import the audit log: the audit package's tests import
// this one, so it could not. internal/policy/policyaudit joins them, registers
// the policy.exposure event type and provides the sink that stores each kind of
// record under its own type.
package policy
