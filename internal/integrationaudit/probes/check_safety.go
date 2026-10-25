package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func safetyCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDSafety, Title: "Safe execution: prepare, confirm, commit, idempotency, expiry, cancellation, stale-record invalidation and uncertain-response recovery",
		Section: integrationaudit.SectionSafeExecution, Stage: integrationaudit.StageSafety, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return driveWith(ctx, o, in, integrationaudit.SectionSafeExecution, modeConsequential, (*suiteCtx).safety)
	})
}

// The seven kinds of safe-execution probe (docs/integration/application-package.md section 12).
const (
	kindConfirmed   = ap.PackageTestsSafetyProbeKindConfirmedCommit
	kindUnconfirmed = ap.PackageTestsSafetyProbeKindUnconfirmedCommit
	kindReplayed    = ap.PackageTestsSafetyProbeKindReplayedCommit
	kindExpired     = ap.PackageTestsSafetyProbeKindExpiredPreparation
	kindStale       = ap.PackageTestsSafetyProbeKindStaleRecord
	kindCancelled   = ap.PackageTestsSafetyProbeKindCancelledPreparation
	kindUncertain   = ap.PackageTestsSafetyProbeKindUncertainCommitRecovery
)

// What a probe observed, in the vocabulary of the suite's expect.outcome.
const (
	outCommitted = string(ap.PackageTestsSafetyExpectOutcomeCommittedOnce)
	outRejected  = string(ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect)
	outOriginal  = string(ap.PackageTestsSafetyExpectOutcomeOriginalReceiptReturned)
	outRecovered = string(ap.PackageTestsSafetyExpectOutcomeRecoveredViaStatus)
)

// safety is master plan 4.5 check 8 for one suite. For every consequential
// capability it exercises, against the disposable tenant and through the real
// adapter:
//
//   - confirmed commit, replay and uncertain recovery: one preparation is
//     committed, its status read, the commit repeated (as a retry after a lost
//     reply is) and the status read again. The effect must be applied once, the
//     repeat must return the original result, and Status must report what the
//     commit reported. The reply of the first commit is discarded before Status is
//     asked, as a lost reply would be, and the recovery must still hold;
//   - expiry: a successful Prepare must say how long the application honours the
//     preparation (valid_for_seconds), no longer than the capability declares, and
//     a Commit of a preparation whose expiry has passed must be refused and apply
//     nothing;
//   - stale record: a Commit whose data versions are not the current ones must be
//     refused and apply nothing; where the application can change a record on
//     request, Validate must also notice the change, so the core can tell a
//     preparation is stale before it commits;
//   - an action whose confirmation mode is step_up (level 3) is never committed by the
//     platform: the commit through the adapter must be refused with step_up_required and
//     apply nothing, and where the application can complete its own step-up on request the
//     effect must be there afterwards and not before (stepup.go says what each kind of
//     probe means for it);
//   - unconfirmed, cancelled and expired commits go through the platform's own
//     coordinator: a commit without the confirmation the risk level requires, after
//     the user declined the preparation, or after it expired, must be refused
//     before anything reaches the adapter;
//   - cancel: where the adapter offers it, an accepted Cancel means the key can no
//     longer commit, and a Cancel after a commit reverses nothing;
//   - the M1-12 contract case: Validate on unchanged data reports the snapshot
//     version Prepare reported. A host that breaks it makes every commit stale or
//     invalidates every open preparation.
//
// The outcome of every probe is compared with the outcome the package's suite
// declares for it.
func (sc *suiteCtx) safety(ctx context.Context) {
	r := sc.r
	for _, id := range r.caps {
		if sc.down() {
			return
		}
		if r.covered[id] {
			continue
		}
		sc.safetyOf(ctx, id)
	}
}

// declaredFor returns the suite's safety probes of a capability by kind.
func (sc *suiteCtx) declaredFor(id string) map[ap.PackageTestsSafetyProbeKind]ap.PackageTestsSafetyProbe {
	out := map[ap.PackageTestsSafetyProbeKind]ap.PackageTestsSafetyProbe{}
	for _, p := range sc.suite.SafetyProbes {
		if string(p.CapabilityID) == id {
			if _, dup := out[p.Kind]; !dup {
				out[p.Kind] = p
			}
		}
	}
	return out
}

// safetyRun is what one capability's scenarios share.
type safetyRun struct {
	sc    *suiteCtx
	id    string
	c     ap.Capability
	decl  map[ap.PackageTestsSafetyProbeKind]ap.PackageTestsSafetyProbe
	g     *rig
	count int // probes recorded
}

func (sc *suiteCtx) safetyOf(ctx context.Context, id string) {
	r := sc.r
	c := r.spec(id)
	if !hasOp(c, "prepare") || !hasOp(c, "commit") {
		r.notProbed(id, "the capability declares no prepare and commit operation to probe")
		return
	}
	g, err := r.rigOf(ctx)
	if err != nil {
		r.notProbed(id, err.Error())
		return
	}
	sr := &safetyRun{sc: sc, id: id, c: c, decl: sc.declaredFor(id), g: g}
	// Scenarios that must change nothing run in one clean world; the one that
	// applies an effect runs in a world of its own.
	steps := []func(context.Context) bool{sr.stale, sr.expired, sr.coreRefusals, sr.commitAndReplay, sr.cancelBeforeCommit}
	for _, step := range steps {
		if sc.down() {
			return
		}
		if !step(ctx) {
			return
		}
	}
	r.count("safe_execution_probe_kinds", sr.count, 7)
	if sr.count == 7 {
		r.probed(id)
	} else {
		r.notProbed(id, "not every kind of safe-execution probe could be run")
	}
}

// principal returns the fixture principal to act as: the first of the declared
// probes of the given kinds whose principal exists, else one the suite names for
// the capability.
func (sr *safetyRun) principal(w *world, kinds ...ap.PackageTestsSafetyProbeKind) string {
	for _, k := range kinds {
		if p, ok := sr.decl[k]; ok {
			if _, exists := w.principals[string(p.Principal)]; exists {
				return string(p.Principal)
			}
		}
	}
	return sr.sc.principalFor(w, sr.id)
}

// prepared is one preparation made through the coordinator for a scenario.
type scenarioPrep struct {
	w      *world
	scope  contracts.Scope
	p      *wPrincipal
	fields map[string]any
	pr     prepared
}

// prepare makes a world, a scope, the request fields and a preparation for the
// principal of the given kinds. false means the capability cannot be probed and the
// reason has been recorded.
func (sr *safetyRun) prepare(ctx context.Context, fresh bool, kinds ...ap.PackageTestsSafetyProbeKind) (scenarioPrep, bool) {
	sc := sr.sc
	var w *world
	var ok bool
	if fresh {
		w, ok = sc.freshWorld(ctx)
	} else {
		w, ok = sc.world(ctx)
	}
	if !ok {
		return scenarioPrep{}, false
	}
	return sc.preparation(ctx, w, sr.id, sr.c, sr.g, sr.principal(w, kinds...))
}

// preparation builds the scope and fields of the principal pref and has the
// coordinator prepare the action. false means it could not be done and the reason has
// been recorded as a reason the capability was not probed.
func (sc *suiteCtx) preparation(ctx context.Context, w *world, id string, c ap.Capability, g *rig, pref string) (scenarioPrep, bool) {
	r := sc.r
	if pref == "" {
		r.notProbed(id, "the suite names no fixture principal for the capability")
		return scenarioPrep{}, false
	}
	scope, p, _ := w.scopeOf(pref)
	fields, err := w.fieldsFor(c, p, nil)
	if err != nil {
		r.notProbed(id, err.Error())
		return scenarioPrep{}, false
	}
	if r.h.down != nil {
		return scenarioPrep{}, false
	}
	pr, err := g.prepare(ctx, scope, c, fields)
	if err != nil {
		switch {
		case untyped(err):
			r.out.Add(newFinding(fdUntyped, id, "the prepare answer of capability %s is not a typed DeterministicResult", quote(id)))
		case outage(err):
			r.h.note(err)
		default:
			r.notProbed(id, fmt.Sprintf("its preparation through the platform's coordinator failed with error code %s", codeOf(err)))
		}
		return scenarioPrep{}, false
	}
	return scenarioPrep{w: w, scope: scope, p: p, fields: fields, pr: pr}, true
}

// declaredExpect returns the declared outcome and code of a probe kind, or the
// default the audit expects when the suite declares none.
func (sr *safetyRun) declaredExpect(kind ap.PackageTestsSafetyProbeKind) (id, principal, outcome, code string, declared bool) {
	if p, ok := sr.decl[kind]; ok {
		code = ""
		if p.Expect.ErrorCode != nil {
			code = string(*p.Expect.ErrorCode)
		}
		return string(p.ID), string(p.Principal), string(p.Expect.Outcome), code, true
	}
	return string(kind), "", "", "", false
}

// report records the result of one probe kind: what was observed, whether a
// specific violation was already raised, and the comparison with what the suite
// declares.
func (sr *safetyRun) report(kind ap.PackageTestsSafetyProbeKind, outcome, code string, violated bool) {
	r := sr.sc.r
	pid, principal, want, wantCode, declared := sr.declaredExpect(kind)
	passed := !violated
	expected, observed := want, outcome
	if wantCode != "" {
		expected += " (" + wantCode + ")"
	}
	if code != "" {
		observed += " (" + code + ")"
	}
	if !declared {
		expected = "no probe of this kind is declared by the suite"
	}
	if declared && !violated && outcome != want {
		r.out.Add(newFinding(fdOutcome, sr.id, "safety probe %s (%s) for capability %s observed %s, the suite declares %s", quote(pid), kind, quote(sr.id), outcome, want))
		passed = false
	}
	if declared && !violated && outcome == want && wantCode != "" && code != "" && code != wantCode {
		r.out.Add(newFinding(fdOutcomeCode, sr.id, "safety probe %s (%s) for capability %s was refused with %s, the suite declares %s; nothing was applied", quote(pid), kind, quote(sr.id), code, wantCode))
	}
	subject := fmt.Sprintf("%s (%s)", pid, kind)
	if principal != "" {
		subject += " as " + principal
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, subject, expected, observed, passed))
	sr.count++
}

// timesFor returns a copy of a prepared action with other times, through its
// JSON form (the timestamps are typed and the audit sets them by value).
func withTimes(p contracts.PreparedAction, preparedAt, expiresAt time.Time) (contracts.PreparedAction, bool) {
	b, err := json.Marshal(p)
	if err != nil {
		return p, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return p, false
	}
	const layout = "2006-01-02T15:04:05Z"
	m["prepared_at"] = preparedAt.UTC().Format(layout)
	m["expires_at"] = expiresAt.UTC().Format(layout)
	nb, err := json.Marshal(m)
	if err != nil {
		return p, false
	}
	var out contracts.PreparedAction
	if json.Unmarshal(nb, &out) != nil {
		return p, false
	}
	return out, true
}

// withStaleVersions returns a copy of a prepared action whose data versions are
// no longer the current ones.
func withStaleVersions(p contracts.PreparedAction) contracts.PreparedAction {
	dv := append([]act.DataVersion(nil), p.DataVersions...)
	for i := range dv {
		dv[i].Version += "~probe-stale"
	}
	p.DataVersions = dv
	return p
}

// refusedWithNoEffect commits a (tampered) preparation and checks that the
// application refused it and applied nothing: the commit is answered rejected and
// Status for its key is the no-record marker, the only answer that proves nothing
// was applied. It returns the code of the refusal, whether the commit was applied
// (a violation), and whether the probe could be judged; when it could not, the
// reason has been recorded.
func (sr *safetyRun) refusedWithNoEffect(ctx context.Context, sp scenarioPrep, tampered contracts.PreparedAction) (code string, applied, judged bool) {
	sc, r := sr.sc, sr.sc.r
	res, err := r.h.commit(ctx, sp.scope, committing(tampered))
	if err != nil {
		sc.failed(sr.id, "commit", err)
		return "", false, false
	}
	if recorded(res) {
		sc.pool.markDirty()
		return "", true, true
	}
	code = firstCode(res)
	st, ok := sc.answer(sr.id, "status", func() (act.DeterministicResult, error) {
		return r.h.status(ctx, sp.scope, sr.id, tampered.IdempotencyKey)
	})
	switch {
	case !ok:
		return code, false, false
	case recorded(st):
		sc.pool.markDirty()
		return code, true, true
	case !refused(res):
		r.notProbed(sr.id, fmt.Sprintf("a commit it had to refuse was answered %s (error code %s), not refused", res.Status, code))
		return code, false, false
	case !noRecord(st):
		r.notProbed(sr.id, "after it refused a commit, Status for its key was not the no-record marker, so it could not be shown that nothing was applied")
		return code, false, false
	}
	return code, false, true
}

// stale is the stale-record probe: the preparation is made, then committed with
// data versions that are not the current ones. Where the application can change a
// record on request, a real change is made as well and Validate must notice it.
func (sr *safetyRun) stale(ctx context.Context) bool {
	sc, r := sr.sc, sr.sc.r
	sp, ok := sr.prepare(ctx, false, kindStale)
	if !ok {
		return false
	}
	// The M1-12 contract case, with the preparation just made: unchanged data.
	if hasOp(sr.c, "validate") {
		res, ok := sc.answer(sr.id, "validate", func() (act.DeterministicResult, error) { return r.h.validate(ctx, sp.scope, sr.id, sp.fields) })
		if !ok {
			return false
		}
		same := version(res) == version(sp.pr.result)
		if !same {
			r.out.Add(newFinding(fdValidateVersion, sr.id, "Validate for capability %s reported a different freshness.version than Prepare for the same, unchanged data", quote(sr.id)))
		}
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "validate on unchanged data reports the prepare snapshot version",
			"the version stored at prepare", map[bool]string{true: "the same version", false: "another version"}[same], same))
	}
	tampered := withStaleVersions(sp.pr.action)
	code, applied, judged := sr.refusedWithNoEffect(ctx, sp, tampered)
	if !judged {
		return false
	}
	if applied {
		r.out.Add(newFinding(fdStaleAccepted, sr.id, "capability %s: a commit whose data versions are not current was applied", quote(sr.id)))
		sr.report(kindStale, outCommitted, "", true)
	} else {
		sr.report(kindStale, outRejected, code, false)
	}
	sr.realChange(ctx, sp)
	return true
}

// realChange makes a real change to a record the preparation relies on, through
// the provisioner, and checks the two things the core relies on: Validate no
// longer reports the version stored at prepare, and a commit with the old
// versions is refused. Without a provisioner that can change a record it is
// recorded as unverified.
func (sr *safetyRun) realChange(ctx context.Context, sp scenarioPrep) {
	sc, r := sr.sc, sr.sc.r
	changer := r.changer
	if changer == nil || !hasOp(sr.c, "validate") || len(sp.pr.action.DataVersions) == 0 {
		r.out.Unverify(integrationaudit.Unverified{
			Subject: "safety:validate_notices_change", Capability: sr.id, Section: integrationaudit.SectionSafeExecution,
			Target: integrationaudit.TargetApplication,
			Reason: "the application offers no way to change a record of the disposable tenant, so it could not be shown that Validate notices a change",
		})
		return
	}
	dv := sp.pr.action.DataVersions[0]
	tenantID := sp.scope.TenantID
	if err := changer.ChangeRecord(ctx, tenantID, string(dv.EntityType), string(dv.EntityID)); err != nil {
		r.out.Unverify(integrationaudit.Unverified{
			Subject: "safety:validate_notices_change", Capability: sr.id, Section: integrationaudit.SectionSafeExecution,
			Target: integrationaudit.TargetApplication, Reason: "the application could not change a record of the disposable tenant (error code " + codeOf(err) + ")",
		})
		return
	}
	sc.pool.markDirty()
	res, ok := sc.answer(sr.id, "validate", func() (act.DeterministicResult, error) { return r.h.validate(ctx, sp.scope, sr.id, sp.fields) })
	if !ok {
		return
	}
	blind := ok && recorded(res) && version(res) == version(sp.pr.result)
	if blind {
		r.out.Add(newFinding(fdValidateBlind, sr.id, "capability %s: after a record it relies on changed, Validate still reported the version stored at prepare", quote(sr.id)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "validate notices a changed record",
		"another version, or a refusal", map[bool]string{true: "the version stored at prepare", false: "another version or a refusal"}[blind], !blind))
	// the preparation made before the change must be refused
	commit, err := r.h.commit(ctx, sp.scope, committing(sp.pr.action))
	if err != nil {
		sc.failed(sr.id, "commit", err)
		return
	}
	if recorded(commit) {
		r.out.Add(newFinding(fdStaleAccepted, sr.id, "capability %s: a commit prepared before a record changed was applied after the change", quote(sr.id)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "a preparation made before a record changed is refused",
		"refused, nothing applied", map[bool]string{true: "applied", false: "refused"}[recorded(commit)], !recorded(commit)))
}

// expired is the expiry probe: how long the application honours a preparation, and
// that it refuses one whose expiry has passed.
func (sr *safetyRun) expired(ctx context.Context) bool {
	r := sr.sc.r
	sp, ok := sr.prepare(ctx, false, kindExpired)
	if !ok {
		return false
	}
	violated := false
	prep := sp.pr.result.Preparation
	declaredTTL := int64(0)
	if sr.c.PreparedAction != nil {
		declaredTTL = sr.c.PreparedAction.TTLSeconds
	}
	switch {
	case prep == nil || prep.ValidForSeconds == nil:
		r.out.Add(newFinding(fdExpiryMissing, sr.id, "the Prepare result of capability %s does not say how long the application honours the preparation", quote(sr.id)))
		violated = true
	case declaredTTL > 0 && int64(*prep.ValidForSeconds) > declaredTTL:
		r.out.Add(newFinding(fdExpiryLong, sr.id, "capability %s declares a time to live of %d seconds and the application honours a preparation for %d", quote(sr.id), declaredTTL, int64(*prep.ValidForSeconds)))
		violated = true
	default:
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "prepare says how long it honours the preparation",
			"valid_for_seconds within the declared time to live", "within the declared time to live", true))
	}
	// A preparation whose expiry has passed (by the application's own clock) must
	// be refused. The presented expiry is the core's, and the application gets it
	// whole so that it can hold it to it.
	now := sr.g.now()
	past, okTimes := withTimes(sp.pr.action, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if !okTimes {
		r.notProbed(sr.id, "the audit could not build an expired preparation")
		return false
	}
	code, applied, judged := sr.refusedWithNoEffect(ctx, sp, past)
	if !judged {
		return false
	}
	switch {
	case applied:
		r.out.Add(newFinding(fdExpiredAccepted, sr.id, "capability %s: a commit of a preparation whose expiry had passed was applied", quote(sr.id)))
		sr.report(kindExpired, outCommitted, "", true)
	default:
		sr.report(kindExpired, outRejected, code, violated)
	}
	return true
}

// coreRefusals are the three kinds the platform's own coordinator decides, tested
// through it: a commit without the confirmation the risk level requires, after the
// user declined the preparation, and after the preparation expired. None of them
// may reach the adapter; the counting adapter proves it.
func (sr *safetyRun) coreRefusals(ctx context.Context) bool {
	if !sr.unconfirmed(ctx) {
		return false
	}
	if !sr.cancelled(ctx) {
		return false
	}
	return sr.coreExpired(ctx)
}

func (sr *safetyRun) unconfirmed(ctx context.Context) bool {
	sc, r := sr.sc, sr.sc.r
	sp, ok := sr.prepare(ctx, false, kindUnconfirmed)
	if !ok {
		return false
	}
	before := sr.g.count.sent(adapters.OpCommit)
	receipt, err := sr.g.commit(ctx, sp.scope, sp.pr)
	reached := sr.g.count.sent(adapters.OpCommit) - before
	mode := sp.pr.action.Confirmation.Mode
	if mode == act.ConfirmationRequirementModeNone {
		// No confirmation is required: committing right after prepare is the
		// designed path, and it applies the effect once.
		if err != nil || receipt.Status != act.ActionReceiptStatusCommitted {
			sr.sc.r.notProbed(sr.id, "the platform's coordinator could not commit a preparation that needs no confirmation")
			return false
		}
		sc.pool.markDirty()
		sr.report(kindUnconfirmed, outCommitted, "", false)
		return true
	}
	if err == nil || reached > 0 {
		r.out.Add(newFinding(fdCoreRefusal, sr.id, "a commit of capability %s without the confirmation its risk level requires was not refused before the adapter", quote(sr.id)))
		sr.report(kindUnconfirmed, outCommitted, "", true)
		sc.pool.markDirty()
		return true
	}
	sr.report(kindUnconfirmed, outRejected, codeOf(err), false)
	return true
}

func (sr *safetyRun) cancelled(ctx context.Context) bool {
	sc, r := sr.sc, sr.sc.r
	sp, ok := sr.prepare(ctx, false, kindCancelled)
	if !ok {
		return false
	}
	if _, err := sr.g.reject(ctx, sp.scope, sp.pr); err != nil {
		r.notProbed(sr.id, "the platform's coordinator could not record that the preparation was declined (error code "+codeOf(err)+")")
		return false
	}
	before := sr.g.count.sent(adapters.OpCommit)
	_, err := sr.g.commit(ctx, sp.scope, sp.pr)
	reached := sr.g.count.sent(adapters.OpCommit) - before
	if err == nil || reached > 0 {
		r.out.Add(newFinding(fdCoreRefusal, sr.id, "a commit of capability %s after the user declined the preparation was not refused before the adapter", quote(sr.id)))
		sr.report(kindCancelled, outCommitted, "", true)
		sc.pool.markDirty()
		return true
	}
	sr.report(kindCancelled, outRejected, codeOf(err), false)
	return true
}

func (sr *safetyRun) coreExpired(ctx context.Context) bool {
	sc, r := sr.sc, sr.sc.r
	sp, ok := sr.prepare(ctx, false, kindExpired)
	if !ok {
		return false
	}
	mode := sp.pr.action.Confirmation.Mode
	if mode == act.ConfirmationRequirementModeExplicit {
		if _, err := sr.g.confirm(ctx, sp.scope, sp.pr); err != nil {
			r.notProbed(sr.id, "the platform's coordinator could not confirm the preparation (error code "+codeOf(err)+")")
			return false
		}
	}
	expires, err := time.Parse(time.RFC3339, string(sp.pr.action.ExpiresAt))
	if err != nil {
		r.notProbed(sr.id, "the preparation carries no readable expiry")
		return false
	}
	start := sr.g.now()
	sr.g.advance(expires.Sub(start) + time.Second)
	before := sr.g.count.sent(adapters.OpCommit)
	_, err = sr.g.commit(ctx, sp.scope, sp.pr)
	reached := sr.g.count.sent(adapters.OpCommit) - before
	sr.g.setTime(start)
	if reached > 0 || !coreExpiryRefusal(mode, err) {
		r.out.Add(newFinding(fdCoreRefusal, sr.id, "a commit of capability %s after its preparation expired was not refused as expired before the adapter", quote(sr.id)))
		sc.pool.markDirty()
		return true
	}
	// An action that needs the application's own step-up may be refused as step_up_required
	// instead: the coordinator never commits it, expired or not, and nothing was sent.
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "the coordinator refuses an expired preparation",
		"expired (step_up_required for a step-up action), nothing sent to the application", "refused, nothing sent", true))
	return true
}

// commitAndReplay is the confirmed-commit, replay and uncertain-recovery probe, in
// a world of its own because it applies the effect.
func (sr *safetyRun) commitAndReplay(ctx context.Context) bool {
	sc, r := sr.sc, sr.sc.r
	sp, ok := sr.prepare(ctx, true, kindConfirmed, kindReplayed, kindUncertain)
	if !ok {
		return false
	}
	defer sc.pool.markDirty()
	if isStepUp(sp.pr.action) {
		// The effect of a level 3 action happens only in the application's own interface:
		// the commit is refused, and the step-up is what is probed (stepup.go).
		return sr.stepUpFlow(ctx, sp)
	}
	c1, ok := sc.answer(sr.id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, sp.scope, committing(sp.pr.action)) })
	if !ok {
		return false
	}
	if !recorded(c1) {
		r.notProbed(sr.id, fmt.Sprintf("its commit of a valid preparation was answered %s (error code %s)", c1.Status, firstCode(c1)))
		sr.report(kindConfirmed, outRejected, firstCode(c1), false)
		return false
	}
	key := sp.pr.action.IdempotencyKey
	// The reply of the first commit is now discarded, as a lost reply would be:
	// what the caller has is the key, and Status is how it learns what happened.
	st1, ok := sc.answer(sr.id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, sp.scope, sr.id, key) })
	if !ok {
		return false
	}
	statusOK := recorded(st1) && sameRecordedOutcome(c1, st1)
	if !statusOK {
		r.out.Add(newFinding(fdStatusNoRecord, sr.id, "capability %s: after a successful commit, Status answered %s instead of the recorded outcome", quote(sr.id), st1.Status))
	}
	// The retry of the commit with the same key.
	c2, ok := sc.answer(sr.id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, sp.scope, committing(sp.pr.action)) })
	if !ok {
		return false
	}
	st2, ok := sc.answer(sr.id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, sp.scope, sr.id, key) })
	if !ok {
		return false
	}
	replayOutcome, replayCode := "", ""
	replayFine := false
	switch {
	case fingerprint(c1) == fingerprint(c2) && sameRecordedOutcome(c1, st2):
		replayOutcome, replayFine = outOriginal, true
	case refused(c2) && sameRecordedOutcome(c1, st2):
		// A host that rejects a replay without changing anything is safe; the
		// package says whether that is what it declares.
		replayOutcome, replayCode, replayFine = outRejected, firstCode(c2), true
	default:
		replayOutcome = "applied_twice_or_answered_differently"
		r.out.Add(newFinding(fdReplay, sr.id, "capability %s: the same commit sent twice with one idempotency key did not return the original result and leave it unchanged", quote(sr.id)))
	}

	sr.report(kindConfirmed, committedOutcome(replayFine && statusOK), "", !replayFine || !statusOK)
	sr.report(kindReplayed, replayOutcome, replayCode, !replayFine)
	recovered := statusOK && replayFine
	rec := outRecovered
	if !recovered {
		rec = "lost_or_duplicated"
	}
	sr.report(kindUncertain, rec, "", !recovered)
	// cancel after the commit reverses nothing
	sr.cancelAfterCommit(ctx, sp, c1)
	return true
}

func committedOutcome(once bool) string {
	if once {
		return outCommitted
	}
	return "not_committed_once"
}

// supportsCancel reports whether the adapter says it can cancel for the capability.
func (sr *safetyRun) supportsCancel() bool {
	d := sr.sc.r.in.Description
	return d != nil && d.Supports(sr.id, adapters.OpCancel)
}

// cancelAfterCommit: Cancel withdraws pending work and never reverses an applied
// effect.
func (sr *safetyRun) cancelAfterCommit(ctx context.Context, sp scenarioPrep, commit act.DeterministicResult) {
	sc, r := sr.sc, sr.sc.r
	if !sr.supportsCancel() {
		return
	}
	key := sp.pr.action.IdempotencyKey
	if _, err := r.h.cancel(ctx, sp.scope, sr.id, key); err != nil {
		if untyped(err) {
			r.out.Add(newFinding(fdUntyped, sr.id, "the cancel answer of capability %s is not a typed DeterministicResult", quote(sr.id)))
		} else if outage(err) {
			return
		}
	}
	st, ok := sc.answer(sr.id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, sp.scope, sr.id, key) })
	if !ok {
		return
	}
	reversed := !(recorded(st) && sameRecordedOutcome(commit, st))
	if reversed {
		r.out.Add(newFinding(fdCancel, sr.id, "capability %s: Cancel after a successful commit changed the recorded outcome", quote(sr.id)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "cancel after a commit reverses nothing",
		"the recorded outcome is unchanged", map[bool]string{true: "the outcome changed", false: "unchanged"}[reversed], !reversed))
}

// cancelBeforeCommit: a Cancel the application accepted means the key can no
// longer commit.
func (sr *safetyRun) cancelBeforeCommit(ctx context.Context) bool {
	sc, r := sr.sc, sr.sc.r
	if !sr.supportsCancel() {
		return true
	}
	sp, ok := sr.prepare(ctx, true, kindCancelled)
	if !ok {
		return false
	}
	defer sc.pool.markDirty()
	key := sp.pr.action.IdempotencyKey
	cres, err := r.h.cancel(ctx, sp.scope, sr.id, key)
	if err != nil {
		sc.failed(sr.id, "cancel", err)
		return !untyped(err) && !outage(err)
	}
	if !withdrew(cres) {
		// nothing pending: the application did not withdraw anything, and a commit may go on
		return true
	}
	res, err := r.h.commit(ctx, sp.scope, committing(sp.pr.action))
	if err != nil {
		sc.failed(sr.id, "commit", err)
		return false
	}
	bad := recorded(res)
	if bad {
		r.out.Add(newFinding(fdCancel, sr.id, "capability %s: a commit succeeded after the application accepted a Cancel for its key", quote(sr.id)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "a commit after an accepted cancel is refused",
		"refused, nothing applied", map[bool]string{true: "applied", false: "refused"}[bad], !bad))
	return true
}

// withdrew reports whether a Cancel answer means the application withdrew the work.
func withdrew(res act.DeterministicResult) bool { return res.Status == act.DeterministicResultStatusOk }
