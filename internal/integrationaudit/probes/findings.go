package probes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// findingDef is one kind of finding a probe raises. Severity and remediation
// are fixed here: a finding never carries advice composed from text that came
// from the application or the package. Messages name capabilities, probe IDs,
// operations and error codes, never a value the application returned and never
// a generated tenant, principal, record or version identifier, so two audits of
// the same application give the same report.
type findingDef struct {
	id          string
	severity    contracts.Severity
	summary     string
	remediation string
}

var defs = map[string]*findingDef{}

func def(id string, sev contracts.Severity, summary, remediation string) *findingDef {
	if _, dup := defs[id]; dup {
		panic("probes: finding " + id + " defined twice")
	}
	d := &findingDef{id: id, severity: sev, summary: summary, remediation: remediation}
	defs[id] = d
	return d
}

const (
	crit = contracts.SeverityCritical
	warn = contracts.SeverityWarning
	info = contracts.SeverityInfo
)

// newFinding builds a finding from a definition. detail is the specific,
// bounded message; the definition's summary stands in for an empty one.
func newFinding(d *findingDef, capability, format string, args ...any) contracts.Finding {
	msg := d.summary
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return contracts.Finding{
		CheckID:     d.id,
		Severity:    d.severity,
		Capability:  capability,
		Path:        "tests/",
		Message:     msg,
		Remediation: d.remediation,
	}
}

// FindingInfo describes one kind of finding the probes raise.
type FindingInfo struct {
	ID          string
	Severity    contracts.Severity
	Summary     string
	Remediation string
}

// Findings lists every kind of finding the probes raise, sorted by ID.
func Findings() []FindingInfo {
	out := make([]FindingInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, FindingInfo{ID: d.id, Severity: d.severity, Summary: d.summary, Remediation: d.remediation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// quote renders a name from the package for a message: quoted and cut to a
// bounded length.
func quote(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "..."
	}
	return fmt.Sprintf("%q", s)
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return strings.Join(q, ", ")
}

// What every probe check can find, whatever it probes.
var (
	fdNotProbed = def("probes.capability_not_probed", crit,
		"a consequential capability could not be probed against a disposable tenant, so its behaviour is unverified",
		"Give the audit what it needs to reach the application's disposable tenant: an adapter for the application, a provisioner that runs the package's fixtures.provisioning operations (create_tenant, seed, cleanup), a tests/ suite whose fixtures name a principal for the capability, and records the capability's fields can be built from. A consequential capability whose safety was not exercised cannot be activated.")
	fdNotProbedMinor = def("probes.capability_not_probed_minor", warn,
		"a read-only capability could not be probed against a disposable tenant, so its behaviour is unverified",
		"Give the audit what it needs to reach the application's disposable tenant (an adapter, a provisioner for the package's fixtures.provisioning operations and a tests/ suite naming a principal for the capability), or have an authorised operator accept this warning; acceptance never widens the tools or scope the model receives.")
	fdUntyped = def("probes.untyped_reply", crit,
		"the application answered an operation with something that is not a typed DeterministicResult",
		"Make every read, validate, prepare, commit and status answer a JSON DeterministicResult (status, identifiers, values, errors, freshness, allowed_next_actions). A reply in prose, a bare status or a result with missing or extra members is never partially trusted.")
	fdLeftBehind = def("probes.tenant_left_behind", crit,
		"the disposable tenant was not completely removed by the application's cleanup operation",
		"Make the cleanup provisioning operation remove the disposable tenant and everything created for it, and report what remains; an audit that leaves synthetic data behind is not repeatable and may expose it.")
)

// Check 2: deterministic results.
var (
	fdUnstable = def("results.unstable", crit,
		"the same request returned different results, so the result is not stable",
		"Make the operation deterministic for unchanged data: the same status, identifiers, canonical values, errors, freshness version and allowed next actions for the same request. Only freshness.as_of may differ.")
	fdValidateVersion = def("results.validate_version_mismatch", crit,
		"Validate on unchanged data did not report the snapshot version that Prepare reported",
		"Report in Validate's freshness.version the same record snapshot version that Prepare reported while the data is unchanged. It must be a version of the records the action relies on, not a per-response ETag or a counter that every write to the tenant moves: the core compares it to the version stored at Prepare, so a mismatch makes every commit stale or invalidates every open preparation.")
	fdPrepareUnusable = def("results.prepare_unusable", crit,
		"a successful Prepare result cannot be turned into a preparation",
		"A successful Prepare result must carry a preparation with at least one effect and one data version, a non-empty freshness.version, and one version per record (no record may be given two versions). The core refuses anything else, so the capability could never be prepared.")
	fdCommitUnbound = def("results.commit_unbound", crit,
		"a Commit or Status result names a record the prepared action does not target",
		"Report in a commit or status result only the records the prepared action targets (an identifier's entity type must be that of one of the effects and, where an effect names its record, that record). The core treats any other result as an unknown outcome.")
	fdStatusDisagrees = def("results.status_disagrees", crit,
		"Status did not report the outcome the commit reported",
		"After a Commit with ok, queued or partial, Status for the same idempotency key must report the same status, identifiers and values: the core uses Status to recover an uncertain commit and treats any other answer as an unknown outcome.")
)

// Check 6: exact scope enforcement.
var (
	fdScopeAllowed = def("scope.cross_scope_allowed", crit,
		"a request from outside the scope of the record was answered with ok",
		"Enforce tenant, workspace and principal scope on every read, validate, prepare, commit and status: a request for a record outside the caller's scope must be refused with a typed rejection and must change and disclose nothing.")
	fdScopeLeak = def("scope.leak", crit,
		"a refused cross-scope request disclosed data about the record it was refused for",
		"A refusal must carry no identifiers and no values, and must not name the record or any record of another scope in its errors. A denied probe passes only if nothing about the unauthorised record reaches the caller, and so the model.")
	fdScopeForeignData = def("scope.foreign_data_in_result", crit,
		"a result for an allowed request contained the identifier of a record of another tenant or workspace",
		"Apply the tenant and workspace scope before a result is built: lists, searches and previews must contain only records the caller may see, and no identifier of any other scope.")
	fdScopeAllowRefused = def("scope.allow_refused", crit,
		"a request the package declares as allowed was refused",
		"A principal that the package's scope probes declare as allowed must be able to perform the operation in the disposable tenant. Fix the application's authorisation, or the probe in the package's tests/ suite.")
	fdScopeCode = def("scope.denial_code_differs", warn,
		"a refused cross-scope request was refused with another error code than the probe declares",
		"The isolation held, but the package and the application disagree: refuse with the error code the probe declares, or change the probe's error_code and the capability's documented failure states.")
	fdScopeOracle = def("scope.existence_disclosed", warn,
		"the application answers a request for a record of another scope differently from a request for a record that does not exist",
		"Answer both with the same typed refusal, so the answer cannot be used to find out which identifiers exist in another tenant or workspace.")
)

// Check 8: safe execution.
var (
	fdReplay = def("safety.replay_not_idempotent", crit,
		"a repeated Commit with the same idempotency key did not return the original receipt",
		"Apply the effect once per idempotency key and answer every repeat, and a retry after an uncertain outcome, with the original result (the same status, identifiers, values and freshness version). A commit that applies twice, or answers a repeat differently, can double a booking, a payment or a message.")
	fdStatusNoRecord = def("safety.status_disagrees_with_commit", crit,
		"after a successful Commit, Status did not report the recorded outcome",
		"Record every commit by idempotency key and report it through Status with the same status, identifiers and values as the commit result. An uncertain commit is recovered by asking Status, and only the no-record marker proves that nothing was applied.")
	fdExpiryMissing = def("safety.expiry_missing", crit,
		"the application's Prepare result does not say how long it honours the preparation",
		"Report valid_for_seconds in every successful Prepare result, no longer than the capability's prepared_action.ttl_seconds, so a preparation cannot be committed long after the user confirmed it.")
	fdExpiryLong = def("safety.expiry_too_long", crit,
		"the application honours a preparation longer than the capability's declared time to live",
		"Report a valid_for_seconds that does not exceed the prepared_action.ttl_seconds the capability declares, or change the declared value and its documentation.")
	fdExpiredAccepted = def("safety.expired_commit_accepted", crit,
		"the application committed a preparation whose expiry had passed",
		"Refuse a Commit whose prepared action has expired (its expires_at is in the past by the application's clock) with the typed rejection expired, and apply nothing.")
	fdStaleAccepted = def("safety.stale_commit_accepted", crit,
		"the application committed a preparation made against data versions that are no longer current",
		"Compare the data versions of the prepared action with the current ones on every Commit and refuse a mismatch with the typed rejection stale, applying nothing.")
	fdValidateBlind = def("safety.validate_blind_to_change", crit,
		"after a record changed, Validate still reported the snapshot version that Prepare reported",
		"Make Validate's freshness.version move when a record the action relies on changes, so the core can see that the preparation is stale before it commits.")
	fdCancel = def("safety.cancel_misbehaves", crit,
		"Cancel reversed an applied effect, or a Commit succeeded after a Cancel the application had accepted",
		"Cancel withdraws pending work for an idempotency key and never reverses an applied effect: after an accepted Cancel the key must not commit, and after a commit it must change nothing.")
	fdCoreRefusal = def("safety.core_let_commit_through", crit,
		"a commit that had to be refused before it reached the application was sent to it",
		"This is a fault of the core's prepare/confirm/commit coordinator, not of the application: a commit without the required confirmation, after the preparation was rejected or after it expired must never reach the adapter. Do not activate packages on this build.")
	fdStepUpBypassed = def("safety.step_up_bypassed", crit,
		"a level 3 action that needs the application's own step-up was applied without it",
		"Refuse every Commit of an action whose confirmation mode is step_up with the typed rejection step_up_required and apply nothing, record nothing under its idempotency key and leave the records it relies on as they were. The effect happens only when the user completes the step-up in the application's own authenticated interface.")
	fdStepUpNoEffect = def("safety.step_up_without_effect", crit,
		"after the application's own step-up was completed, the effect of the action was not there",
		"Completing the step-up in the application's own interface must apply the action (or make a Commit with the same preparation apply it, once): a level 3 action that can never be finished is not usable. After it the records the action relies on have a new version, which Validate reports.")
	fdOutcome = def("safety.outcome_differs_from_package", crit,
		"a safety probe's observed outcome is not the outcome the package declares for it",
		"Make the application behave as the probe in the package's tests/ suite declares, or correct the probe; the audit executes what the package states.")
	fdOutcomeCode = def("safety.error_code_differs", info,
		"a safety probe was refused with another error code than the probe declares; nothing was applied",
		"Nothing is needed for safety. Align the probe's error_code with the code the platform and the application return, so the package documents what actually happens.")
)

// Check 9: failure and degradation.
var (
	fdGenericSuccess = def("degradation.generic_success", crit,
		"the application answered ok to a request that could not have completed",
		"Never report ok for an operation that did not happen: reject an invalid, incomplete, unknown or stale request with a typed status and error code, and report unavailable, queued, partial and uncertain outcomes in typed form.")
	fdSuccessNoRecord = def("degradation.success_without_record", crit,
		"a Commit was answered with a success the application has no record of",
		"Record every successful commit under its idempotency key before answering, and report the same through Status. A success-looking answer without a record means the operation did not complete.")
	fdNoRecordShape = def("degradation.no_record_not_marked", warn,
		"Status for a key the application never saw was not the one-error no-record rejection",
		"Answer Status for an unknown idempotency key with status rejected and exactly one validation_failed error on field idempotency_key, no identifiers: it is the only answer that proves nothing was applied and lets the core retry. Any other answer leaves the outcome unknown for good.")
	fdUndeclaredFailure = def("degradation.undeclared_failure_code", warn,
		"the application reported a failure with an error code the capability does not document",
		"Document every failure state the application can return in the capability's failure_states, or return one of the documented codes; missing documentation is a failed capability, not an instruction for the model to guess.")
	fdUnavailableUntyped = def("degradation.unavailable_not_typed", crit,
		"with a dependency of the application down, an operation was not answered with the typed status unavailable",
		"Report a failed dependency as status unavailable with a typed error, apply nothing and make Status for the commit's key show no record, never an ok or a generic message.")
	fdLostReply = def("degradation.lost_reply_unrecoverable", crit,
		"after a Commit whose reply was lost, Status and a retry did not recover the original outcome",
		"After a commit that was applied but whose reply was lost, Status for the idempotency key must report the recorded outcome and a retry must return it without applying twice.")
)

// Check 10: runtime smoke test.
var (
	fdSmokePath = def("smoke.path_failed", crit,
		"a runtime smoke path did not complete as the package declares",
		"Fix the application or the smoke path in the package's tests/ suite: the text and action paths run against the disposable tenant with the real policy gateway, coordinator and adapter and must reach their documented outcome.")
	fdSmokeEvent = def("smoke.audit_event_missing", crit,
		"an audit event the smoke path must produce was not recorded",
		"Every consequential step must leave its audit event: policy.decision for every decision and action.prepared, action.confirmed and action.committed along the action path.")
	fdSmokeScope = def("smoke.audit_scope_wrong", crit,
		"an audit event of a smoke path does not carry the scope of the disposable tenant it ran in",
		"Every audit event must be bound to the application, tenant, workspace, principal and session of the request that caused it.")
)
