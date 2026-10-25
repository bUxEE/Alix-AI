package probes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func smokeCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDSmoke, Title: "Runtime smoke test: the text and action paths end to end in a disposable tenant, with audit events, scopes and cleanup checked",
		Section: integrationaudit.SectionRuntimeSmoke, Stage: integrationaudit.StageBenchmark, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return driveWith(ctx, o, in, integrationaudit.SectionRuntimeSmoke, modeSmoke, (*suiteCtx).smoke)
	})
}

// producible lists the audit events the pipeline the audit runs (the policy
// gateway and the coordinator over the application's adapter) appends. An event
// the package expects of a path that is not in this list is produced by a layer
// the audit does not run (the dialogue controller, RAG retrieval) and is recorded as
// unverified, never as present and never as missing.
func producible(event string) bool {
	return event == "policy.decision" || strings.HasPrefix(event, "action.")
}

// smoke is master plan 4.5 check 10 for one suite: a disposable tenant with
// synthetic records, and the paths the suite declares run through the platform's
// own pipeline against it, as the principal the path names:
//
//   - the text path: a read through the policy gateway, the coordinator and the
//     adapter, whose answer must be a typed ok result;
//   - the action path: prepare, confirm and commit through the same pipeline, which
//     must end in a committed receipt;
//   - for each, the audit events the path must produce are looked up in the audit
//     log the pipeline wrote, and every event of the run must carry the scope of
//     the disposable tenant it ran in, never another;
//   - at the end the disposable tenants are removed and the application's own
//     account of what remains must be empty, and a second cleanup must find
//     nothing to remove. A tenant left behind is critical.
//
// The voice path needs a speech pipeline and the RAG path a source and an index,
// neither of which the audit's pipeline includes: they are recorded as unverified,
// and the RAG paths are left to the RAG probes (M2-12) when those are registered.
// The text path stops where the dialogue controller starts, because the audit asks
// no model anything: the events the controller writes (dialogue.turn) are unverified.
func (sc *suiteCtx) smoke(ctx context.Context) {
	r := sc.r
	total, done := 0, 0
	for _, p := range sc.suite.Smoke.Paths {
		if sc.down() {
			return
		}
		total++
		var ok bool
		switch p.Path {
		case ap.PackageTestsSmokePathPathText:
			ok = sc.smokeText(ctx, p)
		case ap.PackageTestsSmokePathPathAction:
			ok = sc.smokeAction(ctx, p)
		case ap.PackageTestsSmokePathPathVoice:
			sc.pathUnverified(p, "a voice path needs a speech-to-text pipeline, which the audit's pipeline does not include")
		case ap.PackageTestsSmokePathPathRAG:
			if !r.in.HasOtherCheck(integrationaudit.SectionRuntimeSmoke, IDSmoke) {
				sc.pathUnverified(p, "RAG paths belong to the RAG probes (M2-12), which are not part of this build")
			} else {
				total--
			}
		}
		if ok {
			done++
		}
	}
	r.count("smoke_paths", done, total)
	sc.verifyCleanup(ctx)
}

func (sc *suiteCtx) pathUnverified(p ap.PackageTestsSmokePath, reason string) {
	capID := ""
	if p.CapabilityID != nil {
		capID = string(*p.CapabilityID)
	}
	sc.r.out.Unverify(integrationaudit.Unverified{
		Subject: "smoke_path:" + string(p.ID), Capability: capID, Section: integrationaudit.SectionRuntimeSmoke,
		Target: integrationaudit.TargetApplication, Reason: reason,
	})
}

func (sc *suiteCtx) pathFailed(p ap.PackageTestsSmokePath, capID, why string) {
	sc.r.out.Add(newFinding(fdSmokePath, capID, "smoke path %s (%s) did not complete: %s", quote(string(p.ID)), p.Path, why))
	sc.r.out.Probes = append(sc.r.out.Probes, probe(integrationaudit.ProbeSmoke, capID, string(p.ID)+" ("+string(p.Path)+")",
		"the path completes", why, false))
}

// pathCapability resolves the capability a path names.
func (sc *suiteCtx) pathCapability(p ap.PackageTestsSmokePath) (string, ap.Capability, bool) {
	if p.CapabilityID == nil {
		sc.pathUnverified(p, "the path names no capability to run it through")
		return "", ap.Capability{}, false
	}
	id := string(*p.CapabilityID)
	c, ok := sc.r.pkg.Capabilities[id]
	if !ok {
		sc.pathUnverified(p, "the package declares no such capability")
		return "", ap.Capability{}, false
	}
	return id, c, true
}

func (sc *suiteCtx) smokeText(ctx context.Context, p ap.PackageTestsSmokePath) bool {
	r := sc.r
	id, c, ok := sc.pathCapability(p)
	if !ok {
		return false
	}
	g, err := r.rigOf(ctx)
	if err != nil {
		r.notProbed(id, err.Error())
		return false
	}
	w, ok := sc.world(ctx)
	if !ok {
		return false
	}
	scope, pr, ok := w.scopeOf(string(p.Principal))
	if !ok {
		sc.pathUnverified(p, "the suite names no such principal")
		return false
	}
	fields, err := w.fieldsFor(c, pr, nil)
	if err != nil {
		r.notProbed(id, err.Error())
		return false
	}
	if r.h.down != nil {
		return false
	}
	out, err := g.read(ctx, scope, c, fields)
	switch {
	case err != nil && outage(err):
		r.h.note(err)
		return false
	case err != nil:
		sc.pathFailed(p, id, "the read failed with error code "+codeOf(err))
		return false
	case !ok2(out.Result):
		sc.pathFailed(p, id, "the read was answered "+string(out.Result.Status))
		return false
	}
	good := sc.checkEvents(ctx, g, p, id, scope, "policy.decision")
	if good {
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSmoke, id, string(p.ID)+" (text)",
			"a typed ok result through policy, coordinator and adapter, with its audit events", "completed", true))
		r.probed(id)
	}
	return good
}

// ok2 reports whether a result is ok.
func ok2(res act.DeterministicResult) bool { return res.Status == act.DeterministicResultStatusOk }

func (sc *suiteCtx) smokeAction(ctx context.Context, p ap.PackageTestsSmokePath) bool {
	r := sc.r
	id, c, ok := sc.pathCapability(p)
	if !ok {
		return false
	}
	if !hasOp(c, "prepare") || !hasOp(c, "commit") {
		sc.pathUnverified(p, "the capability declares no prepare and commit operation to run the action through")
		return false
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
	scope, pr, ok := w.scopeOf(string(p.Principal))
	if !ok {
		sc.pathUnverified(p, "the suite names no such principal")
		return false
	}
	fields, err := w.fieldsFor(c, pr, nil)
	if err != nil {
		r.notProbed(id, err.Error())
		return false
	}
	if r.h.down != nil {
		return false
	}
	prep, err := g.prepare(ctx, scope, c, fields)
	switch {
	case err != nil && outage(err):
		r.h.note(err)
		return false
	case err != nil:
		sc.pathFailed(p, id, "the preparation failed with error code "+codeOf(err))
		return false
	}
	if prep.action.Confirmation.Mode == act.ConfirmationRequirementModeExplicit {
		if _, err := g.confirm(ctx, scope, prep); err != nil {
			sc.pathFailed(p, id, "the confirmation failed with error code "+codeOf(err))
			return false
		}
	}
	receipt, err := g.commit(ctx, scope, prep)
	switch {
	case err != nil && outage(err):
		r.h.note(err)
		return false
	case err != nil:
		sc.pathFailed(p, id, "the commit failed with error code "+codeOf(err))
		return false
	case receipt.Status != act.ActionReceiptStatusCommitted && receipt.Status != act.ActionReceiptStatusQueued:
		sc.pathFailed(p, id, "the receipt says "+string(receipt.Status))
		return false
	}
	good := sc.checkEvents(ctx, g, p, id, scope, "action.prepared", "action.committed")
	if good {
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSmoke, id, string(p.ID)+" (action)",
			"prepare, confirm and commit end to end with a committed receipt and its audit events", "completed", true))
		r.probed(id)
	}
	return good
}

// checkEvents looks the events the path must produce up in the audit log of the
// pipeline, in the session of the path. required are events every such path
// produces, whether or not the package lists them. It reports whether nothing was
// found wrong.
func (sc *suiteCtx) checkEvents(ctx context.Context, g *rig, p ap.PackageTestsSmokePath, capID string, scope contracts.Scope, required ...string) bool {
	r := sc.r
	evs, err := g.events(ctx, scope.SessionID)
	if err != nil {
		sc.pathFailed(p, capID, "the audit log could not be read")
		return false
	}
	have := map[string]int{}
	order := []string{}
	good := true
	for _, e := range evs {
		have[e.Type]++
		order = append(order, e.Type)
		if e.Application != scope.ApplicationID || e.Tenant != scope.TenantID || e.Workspace != scope.WorkspaceID ||
			e.Principal != scope.PrincipalID || e.Session != scope.SessionID {
			r.out.Add(newFinding(fdSmokeScope, capID, "an audit event %s of smoke path %s does not carry the application, tenant, workspace, principal and session of the request", quote(e.Type), quote(string(p.ID))))
			good = false
		}
	}
	want := map[string]bool{}
	for _, e := range required {
		want[e] = true
	}
	for _, e := range p.Expect.AuditEvents {
		want[string(e)] = true
	}
	names := make([]string, 0, len(want))
	for e := range want {
		names = append(names, e)
	}
	sort.Strings(names)
	for _, e := range names {
		switch {
		case have[e] > 0:
			r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSmoke, capID, string(p.ID)+": audit event "+e,
				"recorded in the scope of the request", "recorded", true))
		case producible(e):
			r.out.Add(newFinding(fdSmokeEvent, capID, "smoke path %s must produce the audit event %s and the audit log has none", quote(string(p.ID)), quote(e)))
			r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSmoke, capID, string(p.ID)+": audit event "+e,
				"recorded in the scope of the request", "missing", false))
			good = false
		default:
			r.out.Unverify(integrationaudit.Unverified{
				Subject: "audit_event:" + e + " (" + string(p.ID) + ")", Capability: capID, Section: integrationaudit.SectionRuntimeSmoke,
				Target: integrationaudit.TargetApplication,
				Reason: "the event is written by a layer the audit does not run (the dialogue controller or RAG retrieval), so it could not be checked",
			})
		}
	}
	// the lifecycle must be in order: prepared before confirmed before committed
	if idx := indexOrder(order, "action.prepared", "action.confirmed", "action.committing", "action.committed"); !idx {
		r.out.Add(newFinding(fdSmokeEvent, capID, "the audit events of smoke path %s are not in the order of the action lifecycle", quote(string(p.ID))))
		good = false
	}
	return good
}

// indexOrder reports whether the named events that are present appear in the
// given relative order.
func indexOrder(events []string, names ...string) bool {
	last := -1
	for _, n := range names {
		for i, e := range events {
			if e == n {
				if i < last {
					return false
				}
				last = i
				break
			}
		}
	}
	return true
}

// verifyCleanup removes the disposable tenants of the suite itself and reports
// whether anything was left behind, as the check's own result: the smoke test is
// where the package says it verifies cleanup (smoke.verify_cleanup).
func (sc *suiteCtx) verifyCleanup(ctx context.Context) {
	r := sc.r
	left := sc.pool.closeAll(ctx)
	r.reportLeftovers(string(sc.suite.Suite), left)
	observed := "nothing left"
	if len(left) > 0 {
		observed = fmt.Sprintf("%d tenant(s) left", len(left))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeSmoke, "", string(sc.suite.Suite)+": the disposable tenants are removed",
		"nothing left of any disposable tenant", observed, len(left) == 0))
}
