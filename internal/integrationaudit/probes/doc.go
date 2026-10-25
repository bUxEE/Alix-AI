// Package probes is the runtime and application half of the integration audit
// (master plan 4.5 checks 2, 6, 8, 9 and 10, and 9.4 stages results, scope, safety
// and benchmark). The static checks of internal/integrationaudit read the package;
// these call the application. They are deterministic: no model is asked anything,
// the report body carries no timestamp and none of the identifiers generated for a
// run, and two audits of the same application give the same findings.
//
// Importing this package registers five checks with the audit's default registry
// (register.go); internal/cli imports it for the binary. Checks builds the same
// five from Options, so a test, or a composition root that wires a transport, gives
// them their own provisioner without global state.
//
//	2   application.deterministic_results   typed, stable results for every declared operation
//	6   application.scope_probes             positive and negative scope probes
//	8   application.safe_execution           prepare, confirm, commit, replay, expiry, stale, cancel, recovery
//	9   application.failure_degradation      typed failures, and no generic success
//	10  application.runtime_smoke            text and action paths, audit events, scopes, cleanup
//
// # What the probes run against
//
// Probes run through the adapter the audit is given (the adapters.Wrap wrapper
// around the transport) against the application's disposable tenants, which the
// package declares in its tests/ suites (fixtures.provisioning, tenants, principals,
// records). For every world the checks need, the audit generates the tenant,
// workspace and principal identifiers, has the application create the tenants and
// seed the records through a Provisioner, and removes everything afterwards on a
// context that the audit's own cancellation does not cut short. The logical
// references of the suite are what the report names; the generated identifiers never
// reach it.
//
// AppAdapter has no way to call the package's provisioning operations, and the
// adapter the audit holds is the wrapper, which exposes nothing but the contract, so
// a Provisioner comes from its own seam, SetProvisionerSource. A transport that can
// run the provisioning operations registers one from an init function of its own
// file. The Provisioner may also implement RecordChanger (change a record of the
// disposable tenant on request), FaultInjector (make a dependency fail, or lose
// a commit's reply on request) and StepUpCompleter (complete a level 3 step-up in
// the application's own interface on request); without them the parts that need
// them are recorded as unverified, never passed. A level 3 capability (confirmation
// mode step_up) is probed all the same: its commits through the adapter must be
// refused with step_up_required and apply nothing (stepup.go).
//
// Requests are built from what the package declares and seeds and nothing is
// invented: an entity field takes the identifier of a seeded record, any other
// required field a default, an enum value, an example of the capability or the
// smallest value its bounds allow. A request that cannot be built this way makes the
// capability unprobed, with the exact fields named.
//
// # The platform's own pipeline
//
// Where the core decides, the probe goes through the core: the checks that need a
// preparation, a confirmation or a commit through policy build the platform's
// coordinator, policy gateway and audit log over the application's adapter, in a
// throwaway store removed afterwards (rig.go). The package under audit is
// quarantined, and the gateway only acts for an active package, so the rig builds its
// own policy view of the package with the status active; that view lives only in the
// rig and activates nothing. The rig's clock is its own, so a preparation can be
// made to expire without waiting. A counting adapter proves that a commit the core
// had to refuse never reached the application.
//
// # Unverified is not pass, and critical where it matters
//
// A capability that could not be probed against a disposable tenant (no adapter, no
// provisioner, no suite, a fixture that could not be provisioned, no principal or
// no buildable request for it, an application that stopped answering) is recorded
// with an unverified verdict and unverified items, and raises a finding: critical for
// a consequential capability (risk level 1 or above), a warning for a read-only one.
// An application that stops answering stops the check at once: each further call
// would wait out its own timeout, and what was not reached is recorded as not
// probed. Admin capabilities are never offered in a user conversation and are
// skipped; read-only capabilities have nothing to commit and are skipped by check 8.
//
// # Check 2: deterministic results
//
// Every declared read, validate, prepare, commit and status operation is called
// twice (once, for commit) with a principal the suite names, and must answer with a
// DeterministicResult that is the same both times but for freshness.as_of. An answer
// that is not a typed result is critical wherever it appears. Freshness is required
// only for ok, queued and partial results (M1-03); a rejected, uncertain or
// unavailable result legitimately omits it, and the probe asks for a refusal to show
// that it is not held against the application. The M1-12 contract case is checked
// here and again in check 8: Validate on unchanged data reports the snapshot version
// Prepare reported (not a per-response ETag, not a counter every write moves), which
// is what the coordinator compares.
//
// # Check 6: exact scope enforcement
//
// The suite's scope probes are executed: an allowed request must succeed and its
// result contain no identifier of another scope, a denied one must be refused with a
// typed rejection that carries no identifiers or values. The audit derives more:
// every principal tries every record of another tenant (and of another workspace,
// for a capability scoped to a workspace) through every capability that takes one;
// a principal of another tenant or workspace tries to commit a preparation and to
// read the status of a commit it does not own; and a record of another scope must be
// refused like one that does not exist. Retrieval probes belong to the RAG probes
// (M2-12).
//
// # Check 8: safe execution
//
// For every consequential capability: commit, replay and recovery from a lost reply
// (the effect is applied once and every repeat returns the original result); expiry
// (the application says how long it honours a preparation, no longer than declared,
// and refuses one that has expired); stale data (a commit with versions that are not
// current is refused, and where a record can be changed, Validate notices); Cancel;
// and, through the coordinator, a commit without the confirmation required, after the
// user declined the preparation, or after it expired. The outcome of each is compared
// with what the suite declares.
//
// # Check 9: failure and degradation
//
// Requests that cannot complete (fields missing, an unknown record, a value out of
// bounds, Status for a key never seen) must be refused in typed form with a code the
// capability documents, and a commit answered with success must be on record. With a
// FaultInjector the audit also makes a dependency fail and a reply get lost.
//
// # Check 10: runtime smoke
//
// The text path (a read) and the action path (prepare, confirm, commit) run through
// the platform's pipeline in a disposable tenant; the audit events the path must
// produce are looked up in the pipeline's audit log and must carry the scope of the
// tenant, and the tenants are then removed and proven gone. Voice and RAG paths, and
// the events of the dialogue controller (the audit asks no model anything), are
// recorded as unverified.
package probes
