package probes

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// probeDegradation is the kind of the probe results of check 9.
const probeDegradation integrationaudit.ProbeKind = "degradation"

func degradationCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDDegradation, Title: "Failure and degradation: typed unavailable, partial and uncertain outcomes, and no success-looking answer when the operation did not complete",
		Section: integrationaudit.SectionDegradation, Stage: integrationaudit.StageSafety, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return drive(ctx, o, in, integrationaudit.SectionDegradation, (*suiteCtx).degradation)
	})
}

// degradation is master plan 4.5 check 9 for one suite. The application "must not
// return a success-looking generic message when the operation did not complete".
// The audit makes requests that cannot complete, and an operation that did
// complete must be on record, and the application must answer every one of them in
// typed form:
//
//   - a request with its required fields missing, with a record that does not exist
//     in the caller's own scope, or with a value outside the capability's declared
//     bounds is answered with a typed rejection, never ok;
//   - Status for a key the application never saw is the one-error no-record
//     rejection, the only answer that proves nothing was applied, never ok;
//   - a commit answered ok is on record: Status reports it;
//   - every refusal carries a code the capability documents among its failure states;
//   - an answer that is not a typed DeterministicResult (prose, a bare ok, a result
//     with missing members) is critical wherever it appears: that is the generic
//     success-looking message.
//
// With an application that can make itself fail on request (FaultInjector), the audit
// also asks for the failures it cannot otherwise cause: a failed dependency must be
// reported as status unavailable with nothing applied, and a commit that was applied
// but whose reply was lost must be recoverable through Status and a retry. Without
// that ability those parts are recorded as unverified, never passed.
//
// Queued and partial results are typed by the result schema (follow_up is required
// for queued, errors for partial); the audit cannot make an application produce
// them, and checks every one it sees.
func (sc *suiteCtx) degradation(ctx context.Context) {
	r := sc.r
	for _, id := range r.caps {
		if sc.down() {
			return
		}
		if r.covered[id] {
			continue
		}
		sc.degradationOf(ctx, id)
	}
}

// declaredCodes lists the error codes a capability documents.
func declaredCodes(c ap.Capability) map[string]bool {
	out := map[string]bool{}
	for _, f := range c.FailureStates {
		out[string(f.Code)] = true
	}
	return out
}

func (sc *suiteCtx) degradationOf(ctx context.Context, id string) {
	r := sc.r
	c := r.spec(id)
	op := firstOp(c, "validate", "prepare", "read")
	if op == "" {
		r.notProbed(id, "the capability declares no operation to call")
		return
	}
	w, ok := sc.world(ctx)
	if !ok {
		return
	}
	pref := sc.principalFor(w, id)
	if pref == "" {
		r.notProbed(id, "the suite names no fixture principal for the capability")
		return
	}
	scope, p, _ := w.scopeOf(pref)
	fields, err := w.fieldsFor(c, p, nil)
	if err != nil {
		r.notProbed(id, err.Error())
		return
	}
	call := func(f map[string]any) func() (act.DeterministicResult, error) {
		return func() (act.DeterministicResult, error) {
			switch op {
			case "validate":
				return r.h.validate(ctx, scope, id, f)
			case "prepare":
				return r.h.prepare(ctx, scope, id, f)
			}
			return r.h.read(ctx, scope, id, f)
		}
	}
	codes := declaredCodes(c)

	// 1. required fields missing
	if len(requiredFields(c)) > 0 {
		if !sc.mustRefuse(id, op+" without its required fields", codes, call(map[string]any{})) {
			return
		}
	}
	// 2. a record that does not exist in the caller's own scope
	for _, f := range c.Fields {
		if f.Type != ap.CapabilityFieldTypeEntityRef || !f.Required {
			continue
		}
		bad := cloneFields(fields)
		if cur, ok := bad[string(f.Name)].(string); ok {
			bad[string(f.Name)] = unknownLike(cur, w)
			if !sc.mustRefuse(id, fmt.Sprintf("%s with an unknown %s", op, f.Name), codes, call(bad)) {
				return
			}
		}
		break
	}
	// 3. a value outside the declared bounds
	if name, value, found := outOfBounds(c); found {
		bad := cloneFields(fields)
		bad[name] = value
		if !sc.mustRefuse(id, fmt.Sprintf("%s with %s out of bounds", op, name), codes, call(bad)) {
			return
		}
	}

	if consequential(c) && hasOp(c, "prepare") && hasOp(c, "commit") {
		if !sc.noRecordProbe(ctx, id, c, scope) {
			return
		}
		if !sc.recordProbe(ctx, id, c) {
			return
		}
	}
	if !sc.faults(ctx, id, c, op) || r.h.down != nil {
		return
	}
	r.count("failure_probes", 1, 1)
	r.probed(id)
}

func cloneFields(f map[string]any) map[string]any {
	out := make(map[string]any, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}

// outOfBounds finds a required field with a declared bound and a value just outside
// it: a string one longer than max_length, a number above the maximum or below the
// minimum, a value that is not in an enum.
func outOfBounds(c ap.Capability) (name string, value any, found bool) {
	for _, f := range c.Fields {
		if !f.Required {
			continue
		}
		switch f.Type {
		case ap.CapabilityFieldTypeString:
			if f.MaxLength != nil && *f.MaxLength < 4096 {
				return string(f.Name), repeat("x", int(*f.MaxLength)+1), true
			}
		case ap.CapabilityFieldTypeInteger:
			if f.Maximum != nil {
				return string(f.Name), int64(*f.Maximum) + 1, true
			}
			if f.Minimum != nil {
				return string(f.Name), int64(*f.Minimum) - 1, true
			}
		case ap.CapabilityFieldTypeEnum:
			return string(f.Name), "not-one-of-the-declared-values", true
		}
	}
	return "", nil, false
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

// mustRefuse makes a request that cannot complete and requires a typed rejection.
// It reports false when the check of the capability must stop (the application did
// not answer or was not typed).
func (sc *suiteCtx) mustRefuse(id, label string, codes map[string]bool, call func() (act.DeterministicResult, error)) bool {
	r := sc.r
	res, ok := sc.answer(id, label, call)
	if !ok {
		return false
	}
	switch {
	case recorded(res):
		r.out.Add(newFinding(fdGenericSuccess, id, "capability %s answered %s to %s, which cannot complete", quote(id), res.Status, label))
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, label, "a typed rejection", "answered "+string(res.Status), false))
	case refused(res):
		code := firstCode(res)
		if !codes[code] {
			r.out.Add(newFinding(fdUndeclaredFailure, id, "capability %s refused %s with error code %s, which it does not document among its failure states", quote(id), label, code))
		}
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, label, "a typed rejection", "rejected ("+code+")", true))
	default:
		// unavailable or uncertain without anyone having made a dependency fail
		r.out.Unverify(integrationaudit.Unverified{
			Subject: "degradation:" + label, Capability: id, Section: integrationaudit.SectionDegradation, Target: integrationaudit.TargetApplication,
			Reason: "the application answered " + string(res.Status) + " to a request that is simply invalid",
		})
	}
	return true
}

// idemKey mints a well-formed idempotency key that no commit was ever sent with.
func (w *world) idemKey(label string) act.IdempotencyKey {
	w.sessions++
	return act.IdempotencyKey(fmt.Sprintf("idem_probe_%s_%s_%03d", label, w.suffix, w.sessions))
}

// noRecordProbe: Status for a key the application never saw must be the no-record
// marker (rejected with one validation_failed error on idempotency_key, no
// identifiers): the only answer that proves nothing was applied. An ok is a success
// the application cannot have, a generic success.
func (sc *suiteCtx) noRecordProbe(ctx context.Context, id string, c ap.Capability, scope contracts.Scope) bool {
	r := sc.r
	if !hasOp(c, "status") {
		return true
	}
	w, ok := sc.world(ctx)
	if !ok {
		return false
	}
	res, ok := sc.answer(id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, scope, id, w.idemKey("unknown")) })
	if !ok {
		return false
	}
	switch {
	case noRecord(res):
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "status for a key the application never saw",
			"the no-record rejection", "the no-record rejection", true))
	case recorded(res):
		r.out.Add(newFinding(fdGenericSuccess, id, "capability %s answered %s to Status for a key it never saw, claiming an outcome that never happened", quote(id), res.Status))
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "status for a key the application never saw",
			"the no-record rejection", "answered "+string(res.Status), false))
	default:
		r.out.Add(newFinding(fdNoRecordShape, id, "capability %s answered Status for a key it never saw with %s (%s), not the one-error no-record rejection", quote(id), res.Status, firstCode(res)))
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "status for a key the application never saw",
			"the no-record rejection", string(res.Status)+" ("+firstCode(res)+")", false))
	}
	return true
}

// recordProbe: a commit the application answered with a success must be on record
// under its idempotency key. A success without a record is a success-looking answer
// for an operation that did not complete.
func (sc *suiteCtx) recordProbe(ctx context.Context, id string, c ap.Capability) bool {
	r := sc.r
	if !hasOp(c, "status") {
		return true
	}
	g, err := r.rigOf(ctx)
	if err != nil {
		r.notProbed(id, err.Error())
		return false
	}
	w, ok := sc.freshWorld(ctx)
	if !ok {
		return false
	}
	defer sc.pool.markDirty()
	sp, ok := sc.preparation(ctx, w, id, c, g, sc.principalFor(w, id))
	if !ok {
		return false
	}
	if isStepUp(sp.pr.action) {
		// Nothing is applied by a commit of a step-up action, so there is no success to find
		// on record: the refusal must be typed and leave no record (stepup.go).
		ans, ok := sc.stepUpCommit(ctx, id, c, sp.scope, sp.pr.action)
		switch {
		case !ok:
			return false
		case ans.applied:
			return true // critical already: stepUpCommit raised it
		case !ans.stepUp:
			r.notProbed(id, fmt.Sprintf("its commit of a valid preparation was answered %s (error code %s), not step_up_required", ans.res.Status, ans.code))
			return false
		}
		marked := ans.noRecord || !hasOp(c, "status")
		if !marked {
			r.out.Add(newFinding(fdNoRecordShape, id, "capability %s answered Status for a key whose commit was refused for want of a step-up with something other than the one-error no-record rejection", quote(id)))
		}
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "a refused commit of a step-up action leaves no record",
			"rejected, step_up_required, Status reports no record", "rejected, step_up_required", marked))
		return true
	}
	res, ok := sc.answer(id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, sp.scope, committing(sp.pr.action)) })
	if !ok {
		return false
	}
	if !recorded(res) {
		r.notProbed(id, fmt.Sprintf("its commit of a valid preparation was answered %s (error code %s)", res.Status, firstCode(res)))
		return false
	}
	st, ok := sc.answer(id, "status", func() (act.DeterministicResult, error) {
		return r.h.status(ctx, sp.scope, id, sp.pr.action.IdempotencyKey)
	})
	if !ok {
		return false
	}
	if noRecord(st) {
		r.out.Add(newFinding(fdSuccessNoRecord, id, "capability %s answered a commit with %s, and Status for its key shows no record of it", quote(id), res.Status))
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "a successful commit is on record",
			"Status reports the commit", "Status reports no record", false))
		return true
	}
	if !recorded(st) {
		r.out.Add(newFinding(fdStatusDisagrees, id, "Status for capability %s after a successful commit answered %s instead of the recorded outcome", quote(id), st.Status))
	}
	r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "a successful commit is on record",
		"Status reports the commit", map[bool]string{true: "Status reports the commit", false: "Status reports another outcome"}[recorded(st)], recorded(st)))
	return true
}

// faults asks a cooperating application to fail: a dependency down, and a commit
// whose reply is lost. Without the ability to ask, or when the application does not
// arm the fault it is asked for, both are recorded as unverified. It reports whether
// it got to the end: false means the reason it did not has been recorded.
func (sc *suiteCtx) faults(ctx context.Context, id string, c ap.Capability, op string) bool {
	r := sc.r
	inj := r.injector
	if inj == nil {
		r.out.Unverify(integrationaudit.Unverified{
			Subject: "degradation:dependency_failure", Capability: id, Section: integrationaudit.SectionDegradation, Target: integrationaudit.TargetApplication,
			Reason: "the application offers no way to make a dependency fail or a reply get lost, so its typed handling of unavailable and uncertain outcomes could not be shown",
		})
		return true
	}
	w, ok := sc.world(ctx)
	if !ok {
		return false
	}
	pref := sc.principalFor(w, id)
	scope, p, _ := w.scopeOf(pref)
	fields, err := w.fieldsFor(c, p, nil)
	if err != nil {
		r.notProbed(id, err.Error())
		return false
	}
	// a dependency down: the first operation answers unavailable
	fault := Fault{Operation: adapters.Operation(op), CapabilityID: id, Kind: FaultDependencyDown}
	if err := inj.InjectFault(ctx, fault); err != nil {
		r.out.Unverify(faultRefused(id, "degradation:dependency_failure", err))
		return true
	}
	var res act.DeterministicResult
	var cerr error
	switch op {
	case "validate":
		res, cerr = r.h.validate(ctx, scope, id, fields)
	case "prepare":
		res, cerr = r.h.prepare(ctx, scope, id, fields)
	default:
		res, cerr = r.h.read(ctx, scope, id, fields)
	}
	switch {
	case cerr != nil && untyped(cerr):
		r.out.Add(newFinding(fdUntyped, id, "the %s answer of capability %s with a dependency down is not a typed DeterministicResult", op, quote(id)))
	case cerr != nil:
		// an adapter-level failure (unavailable, timeout) is typed by the adapter
	case res.Status == act.DeterministicResultStatusUnavailable:
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, op+" with a dependency down",
			"status unavailable, typed", "status unavailable", true))
	case recorded(res):
		r.out.Add(newFinding(fdGenericSuccess, id, "capability %s answered %s to %s while a dependency was down", quote(id), res.Status, op))
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, op+" with a dependency down",
			"status unavailable, typed", "answered "+string(res.Status), false))
	default:
		r.out.Add(newFinding(fdUnavailableUntyped, id, "capability %s answered %s (%s) to %s while a dependency was down, not unavailable", quote(id), res.Status, firstCode(res), op))
		r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, op+" with a dependency down",
			"status unavailable, typed", "answered "+string(res.Status), false))
	}
	if !consequential(c) || !hasOp(c, "prepare") || !hasOp(c, "commit") || !hasOp(c, "status") {
		return true
	}
	return sc.commitFaults(ctx, id, c, inj)
}

// faultRefused is the unverified item of a fault the application did not arm when
// asked to.
func faultRefused(id, subject string, err error) integrationaudit.Unverified {
	return integrationaudit.Unverified{
		Subject: subject, Capability: id, Section: integrationaudit.SectionDegradation, Target: integrationaudit.TargetApplication,
		Reason: "the application did not arm the failure it was asked for (error code " + codeOf(err) + "), so its typed handling of it could not be shown",
	}
}

// commitFaults: a commit with a dependency down applies nothing and is recoverable;
// a commit applied with its reply lost is recoverable through Status and a retry. It
// reports whether it got to the end, as faults does.
func (sc *suiteCtx) commitFaults(ctx context.Context, id string, c ap.Capability, inj FaultInjector) bool {
	r := sc.r
	g, err := r.rigOf(ctx)
	if err != nil {
		r.notProbed(id, err.Error())
		return false
	}

	// a dependency down at commit
	w, ok := sc.freshWorld(ctx)
	if !ok {
		return false
	}
	sp, ok := sc.preparation(ctx, w, id, c, g, sc.principalFor(w, id))
	if !ok {
		return false
	}
	if err := inj.InjectFault(ctx, Fault{Operation: adapters.OpCommit, CapabilityID: id, Kind: FaultDependencyDown}); err != nil {
		r.out.Unverify(faultRefused(id, "degradation:dependency_failure", err))
	} else {
		res, cerr := r.h.commit(ctx, sp.scope, committing(sp.pr.action))
		switch {
		case cerr != nil && untyped(cerr):
			r.out.Add(newFinding(fdUntyped, id, "the commit answer of capability %s with a dependency down is not a typed DeterministicResult", quote(id)))
		case cerr != nil:
		case res.Status != act.DeterministicResultStatusUnavailable:
			if recorded(res) {
				r.out.Add(newFinding(fdGenericSuccess, id, "capability %s answered a commit with %s while a dependency was down", quote(id), res.Status))
			} else {
				r.out.Add(newFinding(fdUnavailableUntyped, id, "capability %s answered a commit with %s (%s) while a dependency was down, not unavailable", quote(id), res.Status, firstCode(res)))
			}
			sc.pool.markDirty()
		default:
			st, serr := r.h.status(ctx, sp.scope, id, sp.pr.action.IdempotencyKey)
			switch {
			case serr == nil && recorded(st):
				r.out.Add(newFinding(fdUnavailableUntyped, id, "after capability %s answered a commit with unavailable, Status did not show that nothing was applied", quote(id)))
				r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "commit with a dependency down applies nothing",
					"status unavailable, Status shows no record", "unavailable, but Status shows a record", false))
			case serr == nil && noRecord(st):
				r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "commit with a dependency down applies nothing",
					"status unavailable, Status shows no record", "unavailable, no record", true))
			default:
				// only the no-record marker shows that nothing was applied
				r.out.Unverify(integrationaudit.Unverified{
					Subject: "degradation:dependency_failure", Capability: id, Section: integrationaudit.SectionDegradation, Target: integrationaudit.TargetApplication,
					Reason: "after a commit was answered unavailable, Status for its key was not the no-record marker, so it could not be shown that nothing was applied",
				})
			}
		}
	}
	sc.pool.markDirty()

	// a lost reply
	w, ok = sc.freshWorld(ctx)
	if !ok {
		return false
	}
	sp, ok = sc.preparation(ctx, w, id, c, g, sc.principalFor(w, id))
	if !ok {
		return false
	}
	defer sc.pool.markDirty()
	if isStepUp(sp.pr.action) {
		// A commit of a step-up action applies nothing, so no reply of it can be lost.
		return true
	}
	if err := inj.InjectFault(ctx, Fault{Operation: adapters.OpCommit, CapabilityID: id, Kind: FaultReplyLost}); err != nil {
		r.out.Unverify(faultRefused(id, "degradation:lost_reply", err))
		return true
	}
	_, cerr := r.h.commit(ctx, sp.scope, committing(sp.pr.action))
	if cerr == nil || !contracts.HasCode(cerr, contracts.ErrUncertainOutcome) {
		// The application did not lose the reply, or the adapter did not classify
		// the loss: nothing can be said about recovery.
		r.out.Unverify(integrationaudit.Unverified{
			Subject: "degradation:lost_reply", Capability: id, Section: integrationaudit.SectionDegradation, Target: integrationaudit.TargetApplication,
			Reason: "the application did not lose the reply of a commit when asked to",
		})
		return true
	}
	st, serr := r.h.status(ctx, sp.scope, id, sp.pr.action.IdempotencyKey)
	retry, rerr := r.h.commit(ctx, sp.scope, committing(sp.pr.action))
	recovered := serr == nil && rerr == nil && recorded(st) && recorded(retry) && sameRecordedOutcome(st, retry)
	if !recovered {
		r.out.Add(newFinding(fdLostReply, id, "capability %s: after a commit whose reply was lost, Status and a retry did not recover the original outcome", quote(id)))
	}
	r.out.Probes = append(r.out.Probes, probe(probeDegradation, id, "a commit whose reply was lost is recovered",
		"Status reports the outcome and a retry returns it", map[bool]string{true: "recovered", false: "not recovered"}[recovered], recovered))
	return true
}
