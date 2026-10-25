// Package integrationaudit is the integration audit of master plan 4.5 and 9.4:
// the deterministic check of an installed application package, the signed report
// it produces, and the only code in AlixAI that moves a package from quarantined
// to active. It never asks a model anything.
//
// # The flow
//
// A package is installed into quarantine (apppkg.Installer, 'alixai package
// install'). It exposes nothing there: the capability registry reads only active
// packages. Runner.Audit then
//
//  1. re-opens the stored bytes with full verification (signature, hash, trust
//     store, loader checks) and loads them once more for the loader's findings;
//  2. asks the application's adapter, once, to Describe itself, Inspect the
//     aspects it exposes and report Health;
//  3. runs every registered Check in the order of master plan 9.4 (signature,
//     documentation, inventory, results, scope, RAG, safety, benchmark);
//  4. assembles the Report, a pure function of what the checks returned;
//  5. signs it with the platform key (SignReport), verifies the signature it just
//     made, and stores the signed report and an audit-log event in one
//     transaction.
//
// Runner.Activate is a separate step on a stored report. Runner.AcceptWarning
// records an operator's acceptance of one warning. Runner.Quarantine takes an
// active package out of service.
//
// # The three targets and the ten checks
//
// Every check examines one of the three targets of master plan 4.5: the package
// (signed manifest, schemas, documentation, contracts, policies, source
// declarations), the runtime and system (the aspects the application exposes
// through Inspect and Health) and the application (its control surface and, with
// the probes, its behaviour). The report lists, for each target and for each of
// the ten required checks of 4.5, whether it passed, failed, is unverified or was
// not run.
//
// This package implements checks 1 (authenticity), 3 (the declared half of the
// RAG inventory), 4 (documentation completeness), 5 (conversation contracts) and
// 7 (control surface), plus the supplementary runtime, policy and external-mode
// checks. Checks 2, 6, 8, 9 and 10 need the application to answer probes and
// belong to internal/integrationaudit/probes (M1-18) and, for RAG,
// internal/integrationaudit/rag (M2-12). Until they register, the report says so:
// every section nothing implements is "not_run" and raises the warning
// audit.section_not_run, which an authorised operator must accept before the
// package can activate. It is never a pass.
//
// # Control surface (check 7)
//
// Describe tells the audit, per capability, which adapter operations exist, and
// lists host operations that no capability maps. Against the package:
//
//	unreachable   the package maps an operation Describe does not list for that
//	              capability (critical)
//	over-broad    Describe lists an operation, or a whole capability, the package
//	              does not declare; prepare or commit is critical, a read-only one
//	              a warning. cancel belongs to the adapter contract, has no package
//	              mapping, and is never over-broad.
//	undocumented  a host operation no capability maps (warning; it stays invisible
//	              to the model and uncallable)
//	ambiguous     a host operation that differs from a mapped one only by case or
//	              by '_', '-' and '.' (critical)
//
// # Unverified is not pass
//
// Anything the audit could not look at is recorded as unverified and the
// framework makes it impossible to hide: a check that reports an Unverified item
// is unverified however it reports the rest, a capability no check could look at
// is unverified, and a section or target with an unverified item is unverified.
// With no adapter at all, the application and runtime targets are unverified and
// the warning surface.not_inspected is raised. The audit never scans for what an
// application does not expose.
//
// # The report is deterministic and signed
//
// The Report holds no clock reading, no random identifier and no mutable state of
// the package (not its status, not the report that activated it); every list is
// sorted and every number is an integer, so two audits of the same inputs are
// byte-identical. When the audit was made is in the Attestation, which names the
// report by its SHA-256 and carries its own format constant, and is signed with
// trust.SignPayload by a key registered for trust.PlatformApplicationID. A
// signature by a key of the host application, by a revoked key, over another kind
// of document the platform signs, or over an edited report, does not verify.
// Reports are stored whole (canonical JSON) and are append-only.
//
// # Activation
//
// Runner.Activate sets 'active' by the one statement the table allows, UPDATE
// apppkg_packages SET status='active', audit_report_id=..., and the table refuses
// it without a report. It requires, and re-checks inside the transaction:
//
//   - the package is quarantined and its stored bytes still verify now;
//   - the newest report of the package is the one used, so an older clean report
//     never outlives a later audit that found problems, and a report made before
//     the package was last quarantined is not used;
//   - the report verifies, is not older than Config.MaxReportAge, and is about
//     this application and package;
//   - it has no critical finding, and every warning in it was accepted for this
//     package by an authorised operator.
//
// The package it replaces becomes superseded, and the activation is appended to
// the audit log, in the same transaction. After the commit the capability
// registry is refreshed (Quarantine deactivates it), because the registry caches
// an entry for up to 30 seconds; a registry that cannot load the package serves
// nothing for the application.
//
// # Accepting warnings
//
// AcceptWarning needs an operator the Runner's Authorizer allows (deny by
// default), a reason, and a warning of the package's latest signed report; a
// critical finding can never be accepted. The record (who, when, which finding,
// why, in which run) is bound to the package and to the exact finding, so another
// package, or the same warning with another message, needs its own. It carries
// nothing any capability, policy or scope decision reads: accepting widens
// neither the tools nor the scope the model receives, which the tests prove by
// comparing the registry and the policy gateway's exposure of an application with
// and without accepted warnings.
//
// # Extension points
//
// A later milestone adds checks without editing this package: it implements
// Check (or builds one with NewCheck), declares its section, stage and target,
// and registers it with Register from an init function. The Input it receives
// carries the verified package, the loader's findings, the adapter, the result of
// Describe, Inspect and Health, a valid audit scope and the clock. What it returns
// (an Outcome) merges into the report: findings, per-capability verdicts,
// unverified items, source results, coverage counts, probe results, measurements
// and an Extension of its own data, all of it covered by the signature. A check
// that errors, panics or returns something unreportable becomes a critical
// finding.
//
// # Re-audit triggers
//
// The report records the hash of the package, its documentation, every
// conversation contract, capability contract and source definition, the
// permission model, the adapter's control surface, the application version and
// the core version. Enforcing a re-audit when one changes is M5-10; the hashes
// are plain digests of the typed values as the loader decoded them.
//
// # Information about external mode
//
// The audit lists every RAG source whose egress mode is not local_only, every
// field classified above internal and the package's external.forbid, as
// informational findings (additive plan, external provider mode). It reads them
// by name from the typed package re-encoded as JSON, so the members the schema
// task X-03 adds are picked up without a change here; until then a package
// declares none, and the report says that the safe defaults (local_only,
// internal, not forbidden) were assumed.
package integrationaudit
