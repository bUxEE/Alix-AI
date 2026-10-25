// Package triggers enforces the re-audit triggers of master plan 4.5 and 9.4 and
// release gate 12: the audit of an integrated application is repeated after any
// change to its package, documentation, conversation contracts, source
// definitions, permission model or adapter, and a change that invalidates what the
// audit established stops live traffic at once. The integration audit
// (internal/integrationaudit) records what to watch, as the hashes of the report;
// this package compares, re-audits and stops. It asks no model anything.
//
// # What is compared with what
//
// The baseline is the newest stored report of the live package, once its
// signature verifies. The live package is content-addressed, so its own files
// cannot change without its hash changing; what can differ from the baseline is
// whether it still verifies (Installer.Open checks the signature, the trust store
// and the recorded hash again), the core version, the adapter surface and the
// application version the adapter reports, and the schemas the MCP adapter pins.
// Everything else changes by uploading a package: a package installed after the
// live one was activated is compared with it facet by facet (documentation,
// capabilities, conversation contracts, source definitions, permission model,
// scope, signer, application version), both opened through the installer. A
// baseline that is missing or does not verify is itself a change: nothing can be
// said to be unchanged, so the package is audited again.
//
// # Critical changes stop traffic first
//
// A change of signature (a package that no longer verifies, or another publisher
// key), of scope (the principal kinds, the scope level of a capability, the scope
// or query scope of a source), of the permission model (role IDs, host policies,
// the permissions and audience of a capability), a pinned MCP schema that moved,
// and a re-audit of the live package that comes back blocked are critical. The
// decision is made from the kind alone (criticalKind) and wording is not a
// permission: rewording a policy is a documentation change.
//
// A critical change is handled in this order, and the order is the point:
//
//  1. Runner.Quarantine moves the package to quarantine in the database and evicts
//     it from the registry;
//  2. the watcher refreshes the capability registry (what /admin/registry/refresh
//     does in a running server), drops the application and asks the registry what
//     it serves: a registry that still serves the application after the quarantine
//     is an error, recorded as reaudit.failed and notified as critical, never a
//     success;
//  3. the ops event reaudit.quarantined is written and the Notifier is told;
//  4. only then is anything audited, and the quarantined package is audited again
//     so that a report postdates the quarantine (activation refuses an older one).
//
// The failure in step 2 is a retryable error (unavailable), and every later look at
// an application whose package the watcher quarantined asks the registry again
// before it does anything else, so a stop that was not confirmed is not forgotten.
//
// The watcher never activates a package and never accepts a warning: the Auditor
// it holds has no such method, so a clean report after a critical change waits for
// an operator. Accepting a warning widens neither the tools nor the scope the
// model receives, and is not a reason to stop watching: the baseline is the report
// of the active package, not what an operator said yes to.
//
// # Non-critical changes keep the previous activation
//
// Any other change (a changed contract, document or source limit, a new host
// operation, a new core or application version) calls for a re-audit while the
// previous activation stays in force: the registry serves the active package
// throughout, and the audit of an uploaded package leaves it quarantined for the
// operator. The re-audit of the live package becomes the baseline, so a change is
// audited once.
//
// # Where the triggers come from
//
//   - Schedule: Sweep queues one watch job per watched application per period
//     (class 6, maintenance, JobType) and HandleJob, the body of the job, runs
//     Check under ServiceIdentity. This package does not import internal/jobs (the
//     module map does not list it for integrationaudit): the composition root
//     registers the type, implements Enqueuer over the queue and attaches HandleJob.
//   - Events: PackageInstalled for an upload and ReportDrift for the schema pinning
//     of the MCP adapter (adapters/mcp.Config.OnDrift as plain data, so no concrete
//     adapter is imported). Both do the part that must be immediate, the stop,
//     before they return and leave the slow part, the audit, to a job when an
//     Enqueuer is configured.
//
// The look at one application is idempotent: with nothing changed it audits
// nothing and writes nothing, and a package that has a report is not audited again.
// Work on one application is serialised.
//
// # Ops events
//
// The audit log carries four events on the application chain (reaudit.triggered,
// reaudit.completed, reaudit.quarantined, reaudit.failed) and an optional Notifier
// receives the same facts. They name packages, runs, kinds of change and typed
// codes, never text an application or a package chose.
package triggers
