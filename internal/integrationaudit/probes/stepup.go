package probes

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Step-up (confirmation mode step_up, level 3): the platform never completes the
// action. Its coordinator refuses every commit of it (step_up_required) before
// anything reaches the application, and the application itself must refuse a commit
// sent without the step-up. The effect happens only when the user completes the step-up
// in the application's own authenticated interface (docs/integration/application-package.md
// section 12). Two application designs exist and both are accepted:
//
//   - the interface applies the effect itself, and a commit through the adapter never
//     succeeds: it is refused (stale, once the records moved) and Status never has a record;
//   - the interface records an approval, and the commit that follows it applies the effect
//     once and is idempotent like any other.
//
// The seven kinds of safe-execution probe mean, for such a capability:
//
//	confirmed_commit           the commit of a fresh preparation, sent through the adapter without
//	                           the step-up, is refused with step_up_required and applies nothing
//	                           (rejected_no_effect, step_up_required);
//	unconfirmed_commit         the same through the platform's coordinator: refused before it
//	                           reaches the application (rejected_no_effect, step_up_required);
//	replayed_commit            the same commit sent again is refused again and still applies
//	                           nothing (rejected_no_effect, step_up_required);
//	uncertain_commit_recovery  Status for the key shows the no-record marker after the refused
//	                           commits, which is what proves a retry is safe (recovered_via_status);
//	expired_preparation, stale_record, cancelled_preparation  as for any capability.
//
// On top of the seven, when the Provisioner can complete a step-up (StepUpCompleter), the
// audit completes it and shows that the effect is there afterwards, and not before.

// isStepUp reports whether the preparation needs the application's own step-up: the mode
// the coordinator stored at prepare, which policy or the application may have raised above
// what the capability declares.
func isStepUp(a contracts.PreparedAction) bool {
	return a.Confirmation.Mode == act.ConfirmationRequirementModeStepUp
}

// coreExpiryRefusal reports whether err is an acceptable refusal, by the platform's
// coordinator, of a commit of an expired preparation: expired, or for an action that needs
// the application's own step-up also step_up_required, which the coordinator answers for
// such an action before it asks anything else. What matters is that it was refused and
// that nothing reached the application, which the caller checks.
func coreExpiryRefusal(mode act.ConfirmationRequirementMode, err error) bool {
	switch {
	case err == nil:
		return false
	case contracts.HasCode(err, contracts.ErrExpired):
		return true
	case mode == act.ConfirmationRequirementModeStepUp:
		return contracts.HasCode(err, contracts.ErrStepUpRequired)
	}
	return false
}

// stepUpAnswer is what the application answered to a commit sent without the step-up.
type stepUpAnswer struct {
	res     act.DeterministicResult
	code    string
	applied bool // the commit was recorded, or Status shows a record under its key
	stepUp  bool // refused with step_up_required, nothing recorded
	// noRecord is true when Status for the key was the no-record marker.
	noRecord bool
}

// stepUpCommit sends a commit of a step-up preparation through the adapter, as the
// application must be able to refuse it, and asks Status whether anything was recorded. A
// commit that applied is a critical finding. false means the call failed and the reason has
// been recorded.
func (sc *suiteCtx) stepUpCommit(ctx context.Context, id string, c ap.Capability, scope contracts.Scope, a contracts.PreparedAction) (stepUpAnswer, bool) {
	r := sc.r
	res, ok := sc.answer(id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, scope, committing(a)) })
	if !ok {
		return stepUpAnswer{}, false
	}
	ans := stepUpAnswer{res: res, code: firstCode(res), applied: recorded(res)}
	if hasOp(c, "status") {
		st, ok := sc.answer(id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, scope, id, a.IdempotencyKey) })
		if !ok {
			return ans, false
		}
		ans.applied = ans.applied || recorded(st)
		ans.noRecord = noRecord(st)
	}
	if ans.applied {
		sc.pool.markDirty()
		r.out.Add(newFinding(fdStepUpBypassed, id, "capability %s: a commit sent through the adapter without the application's own step-up was applied", quote(id)))
		return ans, true
	}
	ans.stepUp = refused(res) && ans.code == string(contracts.ErrStepUpRequired)
	return ans, true
}

// reliedOn asks what the records an action relies on look like now: Validate when the
// capability has it (it stages nothing), else Prepare. It reports whether they are
// still as Prepare saw them (the same version), and whether the question could be asked.
func (sc *suiteCtx) reliedOn(ctx context.Context, id string, c ap.Capability, sp scenarioPrep) (unchanged, ok bool) {
	r := sc.r
	op := "validate"
	call := func() (act.DeterministicResult, error) { return r.h.validate(ctx, sp.scope, id, sp.fields) }
	if !hasOp(c, "validate") {
		op = "prepare"
		call = func() (act.DeterministicResult, error) { return r.h.prepare(ctx, sp.scope, id, sp.fields) }
	}
	res, ok := sc.answer(id, op, call)
	if !ok {
		return false, false
	}
	return recorded(res) && version(res) == version(sp.pr.result), true
}

// stepUpFlow is the confirmed-commit, replay and uncertain-recovery probe of a capability
// that needs the application's own step-up, in a world of its own.
func (sr *safetyRun) stepUpFlow(ctx context.Context, sp scenarioPrep) bool {
	sc, r := sr.sc, sr.sc.r

	// confirmed_commit and replayed_commit: refused, twice, nothing applied
	first, ok := sc.stepUpCommit(ctx, sr.id, sr.c, sp.scope, sp.pr.action)
	if !ok {
		return false
	}
	if !first.applied && !first.stepUp {
		r.notProbed(sr.id, fmt.Sprintf("its commit of a valid preparation was answered %s (error code %s), not step_up_required", first.res.Status, first.code))
		return false
	}
	again, ok := sc.stepUpCommit(ctx, sr.id, sr.c, sp.scope, sp.pr.action)
	if !ok {
		return false
	}
	sr.reportStepUp(kindConfirmed, first)
	sr.reportStepUp(kindReplayed, again)

	// nothing applied: the records the action relies on are as Prepare saw them
	unchanged, ok := sc.reliedOn(ctx, sr.id, sr.c, sp)
	if !ok {
		return false
	}
	clean := unchanged && !first.applied && !again.applied
	if !unchanged && !first.applied && !again.applied {
		r.out.Add(newFinding(fdStepUpBypassed, sr.id, "capability %s: after its commits were refused for want of the application's own step-up, a record the action relies on had changed", quote(sr.id)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, "a commit without the step-up applies nothing",
		"refused with step_up_required, no record, the relied-on records unchanged",
		map[bool]string{true: "refused, nothing applied", false: "something was applied"}[clean], clean))

	// uncertain_commit_recovery: Status is the no-record marker, so a retry is safe
	recovered := first.noRecord && again.noRecord && !first.applied && !again.applied
	if !recovered && !first.applied && !again.applied {
		r.out.Add(newFinding(fdNoRecordShape, sr.id, "capability %s answered Status for a key whose commit was refused for want of a step-up with something other than the one-error no-record rejection", quote(sr.id)))
	}
	rec := outRecovered
	if !recovered {
		rec = "lost_or_duplicated"
	}
	sr.report(kindUncertain, rec, "", !recovered && (first.applied || again.applied))
	if !clean {
		// The application applied the effect without the step-up, which is critical already:
		// completing a step-up on top of that would prove nothing.
		return true
	}
	return sr.completeStepUp(ctx, sp)
}

// reportStepUp records the result of one probe kind of a step-up capability.
func (sr *safetyRun) reportStepUp(kind ap.PackageTestsSafetyProbeKind, a stepUpAnswer) {
	if a.applied {
		sr.report(kind, outCommitted, "", true)
		return
	}
	sr.report(kind, outRejected, a.code, false)
}

// completeStepUp has the application complete the step-up and shows that the effect is
// there afterwards: either the commit that follows applies it once (and agrees with Status
// and with its own replay), or the records the action relies on have moved and the commit
// is refused.
func (sr *safetyRun) completeStepUp(ctx context.Context, sp scenarioPrep) bool {
	sc, r := sr.sc, sr.sc.r
	if r.stepUp == nil {
		r.out.Unverify(integrationaudit.Unverified{
			Subject: "safety:step_up_completion", Capability: sr.id, Section: integrationaudit.SectionSafeExecution,
			Target: integrationaudit.TargetApplication,
			Reason: "the application offers no way to complete a step-up in the disposable tenant, so it could not be shown that the effect happens only after the application's own step-up",
		})
		return true
	}
	if err := r.stepUp.CompleteStepUp(ctx, StepUp{CapabilityID: sr.id, Scope: sp.scope, Prepared: sp.pr.action, Result: sp.pr.result}); err != nil {
		if outage(err) {
			r.h.note(err)
			return false
		}
		r.notProbed(sr.id, "the application's own step-up could not be completed in the disposable tenant (error code "+codeOf(err)+")")
		return false
	}
	sc.pool.markDirty()
	key := sp.pr.action.IdempotencyKey
	res, ok := sc.answer(sr.id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, sp.scope, committing(sp.pr.action)) })
	if !ok {
		return false
	}
	subject := "the effect is there after the application's own step-up"
	switch {
	case recorded(res):
		// an approval was recorded, and this commit applied the effect: once, on record
		st, ok := sc.answer(sr.id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, sp.scope, sr.id, key) })
		if !ok {
			return false
		}
		agree := recorded(st) && sameRecordedOutcome(res, st)
		if !agree {
			r.out.Add(newFinding(fdStatusNoRecord, sr.id, "capability %s: after its step-up was completed and its commit succeeded, Status answered %s instead of the recorded outcome", quote(sr.id), st.Status))
		}
		repeat, ok := sc.answer(sr.id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, sp.scope, committing(sp.pr.action)) })
		if !ok {
			return false
		}
		once := fingerprint(res) == fingerprint(repeat) || (refused(repeat) && agree)
		if !once {
			r.out.Add(newFinding(fdReplay, sr.id, "capability %s: the same commit sent twice after its step-up was completed did not return the original result and leave it unchanged", quote(sr.id)))
		}
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, subject,
			"the commit applies the effect once and Status records it",
			map[bool]string{true: "applied once, on record", false: "not once, or not on record"}[agree && once], agree && once))
	case refused(res):
		// the application's own interface applied the effect itself
		unchanged, ok := sc.reliedOn(ctx, sr.id, sr.c, sp)
		if !ok {
			return false
		}
		if unchanged {
			r.out.Add(newFinding(fdStepUpNoEffect, sr.id, "capability %s: after its step-up was completed the records the action relies on are unchanged and its commit is still refused (%s)", quote(sr.id), firstCode(res)))
		}
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSafeExecution, sr.id, subject,
			"the relied-on records have a new version, and the commit is refused",
			map[bool]string{true: "no effect", false: "applied by the application, the commit is refused"}[unchanged], !unchanged))
	default:
		r.notProbed(sr.id, fmt.Sprintf("its commit after the step-up was completed was answered %s (error code %s)", res.Status, firstCode(res)))
		return false
	}
	return true
}
