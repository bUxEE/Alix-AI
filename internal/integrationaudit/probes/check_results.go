package probes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Check IDs. They key the report and the extension each check adds; renaming one
// is an API change.
const (
	IDResults     = "application.deterministic_results"
	IDScope       = "application.scope_probes"
	IDSafety      = "application.safe_execution"
	IDDegradation = "application.failure_degradation"
	IDSmoke       = "application.runtime_smoke"
)

func resultsCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDResults, Title: "Deterministic results: every read, validate, prepare, commit and status answers with a typed, stable result",
		Section: integrationaudit.SectionDeterministic, Stage: integrationaudit.StageResults, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return drive(ctx, o, in, integrationaudit.SectionDeterministic, (*suiteCtx).results)
	})
}

// results is master plan 4.5 check 2 for one suite: every declared read,
// validate, prepare and commit operation of every capability is called through
// the real adapter in the disposable tenant, as a principal the suite names, and
// must answer with a typed DeterministicResult that is the same when asked again.
//
// What a typed, stable result means here, beyond the schema the adapter enforces:
//
//   - the application answers in the contract's form at all (a reply in prose, a
//     bare "ok" or a result with missing or extra members is a critical finding,
//     never partially trusted);
//   - the same request about unchanged data returns the same status, identifiers,
//     canonical values, errors, freshness version and allowed next actions: only
//     freshness.as_of may differ;
//   - freshness is required only for ok, queued and partial results (M1-03). A
//     rejected, uncertain or unavailable result legitimately omits it, and is
//     never held to it;
//   - Validate on unchanged data reports the snapshot version Prepare reported
//     (M1-12): freshness.version is a version of the records the action relies on,
//     not a per-response ETag and not a counter every write to the tenant moves.
//     The coordinator compares exactly this, and a host that breaks it makes every
//     commit stale or invalidates every open preparation;
//   - a successful Prepare can be turned into a preparation: at least one effect
//     and one data version, a freshness version, and one version per record;
//   - a Commit result names only records the preparation targets, and Status for
//     the same key reports the outcome the commit reported.
func (sc *suiteCtx) results(ctx context.Context) {
	r := sc.r
	for _, id := range r.caps {
		if sc.down() {
			return
		}
		if r.covered[id] {
			continue
		}
		sc.resultsOf(ctx, id)
	}
}

func (sc *suiteCtx) resultsOf(ctx context.Context, id string) {
	r := sc.r
	c := r.spec(id)
	ops := operationsOf(c)
	if len(ops) == 0 {
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

	declared, done := len(ops), 0
	stable := func(op string, a, b act.DeterministicResult) {
		if fingerprint(a) == fingerprint(b) {
			r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, id, op+" repeated",
				"the same result both times", "the same result both times", true))
			return
		}
		r.out.Add(newFinding(fdUnstable, id, "two identical %s requests for capability %s returned different results", op, quote(id)))
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, id, op+" repeated",
			"the same result both times", "different results", false))
	}
	// ask calls an operation twice. It returns the first answer when both are
	// typed ok results; anything else has already been recorded and ends the
	// probing of the capability.
	ask := func(op string, call func() (act.DeterministicResult, error)) (act.DeterministicResult, bool) {
		a, good := sc.answer(id, op, call)
		if !good || !acceptable(sc, id, op, a) {
			return a, false
		}
		b, good := sc.answer(id, op, call)
		if !good {
			return a, false
		}
		stable(op, a, b)
		return a, true
	}

	var validateRes, prepareRes act.DeterministicResult
	if hasOp(c, "read") {
		if _, good := ask("read", func() (act.DeterministicResult, error) { return r.h.read(ctx, scope, id, fields) }); !good {
			return
		}
		done++
	}
	if hasOp(c, "validate") {
		res, good := ask("validate", func() (act.DeterministicResult, error) { return r.h.validate(ctx, scope, id, fields) })
		if !good {
			return
		}
		done++
		validateRes = res
	}
	if hasOp(c, "prepare") {
		res, good := ask("prepare", func() (act.DeterministicResult, error) { return r.h.prepare(ctx, scope, id, fields) })
		if !good || !sc.preparationUsable(id, res) {
			return
		}
		done++
		prepareRes = res
		if hasOp(c, "validate") {
			sc.versionCase(id, validateRes, prepareRes)
		}
	}
	if hasOp(c, "commit") {
		good, withStatus := sc.commitResults(ctx, id, c, scope, fields)
		if !good {
			return
		}
		done++
		if withStatus {
			done++
		}
	}
	sc.refusalShape(ctx, id, c, scope)
	r.count("deterministic_result_operations", done, declared)
	if done == declared {
		r.probed(id)
	} else {
		r.notProbed(id, "not every declared operation could be exercised")
	}
}

// acceptable reports whether an answer is the ok a valid request deserves, and
// says why the capability could not be probed when it is not.
func acceptable(sc *suiteCtx, id, op string, res act.DeterministicResult) bool {
	if ok(res) {
		return true
	}
	sc.r.notProbed(id, fmt.Sprintf("its %s was answered %s (error code %s) for a valid request in the disposable tenant", op, res.Status, firstCode(res)))
	return false
}

// principalFor returns the fixture principal a probe of the capability acts as:
// the first the suite declares for it that exists in the world.
func (sc *suiteCtx) principalFor(w *world, capID string) string {
	for _, ref := range principalCandidates(sc.suite, capID) {
		if _, ok := w.principals[ref]; ok {
			return ref
		}
	}
	return ""
}

// answer makes one call and classifies a failure: an untyped answer is a critical
// finding, a failure to reach the application stops the check, anything else is a
// reason the capability cannot be probed. It returns the result and whether the
// call produced one.
func (sc *suiteCtx) answer(capID, op string, call func() (act.DeterministicResult, error)) (act.DeterministicResult, bool) {
	res, err := call()
	if err == nil {
		return res, true
	}
	sc.failed(capID, op, err)
	return res, false
}

// failed records why a call produced no result.
func (sc *suiteCtx) failed(capID, op string, err error) {
	r := sc.r
	switch {
	case untyped(err):
		r.out.Add(newFinding(fdUntyped, capID, "the %s answer of capability %s is not a typed DeterministicResult", op, quote(capID)))
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, capID, op,
			"a typed DeterministicResult", "a reply that is not one", false))
	case outage(err):
		// the host is marked down by the wrapper; down() reports the rest
	default:
		r.notProbed(capID, fmt.Sprintf("its %s call failed with error code %s", op, codeOf(err)))
	}
}

// preparationUsable checks that a successful Prepare result can become a
// preparation, as the coordinator requires.
func (sc *suiteCtx) preparationUsable(capID string, res act.DeterministicResult) bool {
	why := ""
	switch p := res.Preparation; {
	case p == nil:
		why = "it carries no preparation"
	case len(p.Effects) == 0:
		why = "its preparation lists no effect"
	case len(p.DataVersions) == 0:
		why = "its preparation lists no data version, so staleness could not be detected"
	case version(res) == "":
		why = "it carries no freshness version"
	default:
		type key struct{ t, i string }
		seen := map[key]bool{}
		for _, dv := range p.DataVersions {
			k := key{string(dv.EntityType), string(dv.EntityID)}
			if seen[k] {
				why = "it gives one record more than one data version"
				break
			}
			seen[k] = true
		}
	}
	if why == "" {
		sc.r.out.Probes = append(sc.r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, capID, "prepare is usable",
			"a preparation the coordinator accepts", "accepted", true))
		return true
	}
	sc.r.out.Add(newFinding(fdPrepareUnusable, capID, "the successful prepare result of capability %s cannot become a preparation: %s", quote(capID), why))
	sc.r.out.Probes = append(sc.r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, capID, "prepare is usable",
		"a preparation the coordinator accepts", why, false))
	return false
}

// versionCase is the M1-12 contract case: Validate on unchanged data reports the
// snapshot version Prepare reported.
func (sc *suiteCtx) versionCase(capID string, validate, prepare act.DeterministicResult) {
	r := sc.r
	if version(validate) == version(prepare) {
		r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, capID, "validate reports the prepare snapshot version",
			"the same freshness.version as prepare", "the same freshness.version", true))
		return
	}
	r.out.Add(newFinding(fdValidateVersion, capID, "Validate for capability %s reported a different freshness.version than Prepare for the same, unchanged data", quote(capID)))
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, capID, "validate reports the prepare snapshot version",
		"the same freshness.version as prepare", "a different freshness.version", false))
}

// withState returns a copy of a prepared action in another state: the state the
// coordinator gives it when it sends the commit.
func committing(p contracts.PreparedAction) contracts.PreparedAction {
	p.State = act.PreparedActionStateCommitting
	return p
}

// sameRecordedOutcome reports whether two results record the same outcome: the
// same status, identifiers, values and errors. Freshness may differ.
func sameRecordedOutcome(a, b act.DeterministicResult) bool {
	enc := func(res act.DeterministicResult) string {
		out, _ := json.Marshal([]any{res.Status, res.Identifiers, res.Values, res.Errors})
		return string(out)
	}
	return enc(a) == enc(b)
}

// commitResults exercises Commit and Status of a consequential capability: the
// coordinator prepares an action, the commit is sent to the application, and its
// result must be bound to what was prepared and agree with Status. It reports
// whether both were exercised.
func (sc *suiteCtx) commitResults(ctx context.Context, id string, c ap.Capability, scope contracts.Scope, fields map[string]any) (committed, withStatus bool) {
	r := sc.r
	g, err := r.rigOf(ctx)
	if err != nil {
		r.notProbed(id, err.Error())
		return false, false
	}
	if r.h.down != nil {
		return false, false
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
		return false, false
	}
	key := pr.action.IdempotencyKey
	if isStepUp(pr.action) {
		return sc.stepUpResults(ctx, id, c, scope, pr)
	}
	res, ok := sc.answer(id, "commit", func() (act.DeterministicResult, error) { return r.h.commit(ctx, scope, committing(pr.action)) })
	if !ok {
		return false, false
	}
	if !recorded(res) {
		r.notProbed(id, fmt.Sprintf("its commit of a valid preparation was answered %s (error code %s)", res.Status, firstCode(res)))
		return false, false
	}
	sc.pool.markDirty() // the effect is applied

	bound := true
	for _, ref := range res.Identifiers {
		if !covers(pr.action.Effects, ref) {
			bound = false
		}
	}
	if !bound {
		r.out.Add(newFinding(fdCommitUnbound, id, "the commit result of capability %s names a record its prepared action does not target", quote(id)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, id, "commit is bound to the preparation",
		"identifiers the preparation targets", map[bool]string{true: "bound", false: "a record outside the preparation"}[bound], bound))
	if !hasOp(c, "status") {
		return true, false
	}
	st, ok := sc.answer(id, "status", func() (act.DeterministicResult, error) { return r.h.status(ctx, scope, id, key) })
	if !ok {
		return false, false
	}
	agree := recorded(st) && sameRecordedOutcome(res, st)
	if !agree {
		r.out.Add(newFinding(fdStatusDisagrees, id, "Status for capability %s after a successful commit answered %s instead of the recorded outcome", quote(id), st.Status))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, id, "status reports the commit",
		"the outcome the commit reported", map[bool]string{true: "the same outcome", false: "another outcome"}[agree], agree))
	for _, ref := range st.Identifiers {
		if !covers(pr.action.Effects, ref) {
			r.out.Add(newFinding(fdCommitUnbound, id, "the status result of capability %s names a record its prepared action does not target", quote(id)))
			break
		}
	}
	return true, true
}

// refusalShape sends an incomplete request (every field left out) and checks that
// whatever the application answers is a typed result. A refusal is a rejected
// result with at least one typed error; it carries no freshness, because
// freshness is required only for ok, queued and partial results (M1-03), and a
// result that legitimately omits it is not a failure. Whether the application is
// right to refuse is judged by check 9.
func (sc *suiteCtx) refusalShape(ctx context.Context, id string, c ap.Capability, scope contracts.Scope) {
	r := sc.r
	if len(requiredFields(c)) == 0 {
		return
	}
	var op string
	var call func() (act.DeterministicResult, error)
	switch {
	case hasOp(c, "read"):
		op, call = "read", func() (act.DeterministicResult, error) { return r.h.read(ctx, scope, id, map[string]any{}) }
	case hasOp(c, "validate"):
		op, call = "validate", func() (act.DeterministicResult, error) { return r.h.validate(ctx, scope, id, map[string]any{}) }
	case hasOp(c, "prepare"):
		op, call = "prepare", func() (act.DeterministicResult, error) { return r.h.prepare(ctx, scope, id, map[string]any{}) }
	default:
		return
	}
	res, good := sc.answer(id, op+" without its required fields", call)
	if !good {
		return
	}
	observed := fmt.Sprintf("a typed %s result", res.Status)
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, id, op+" without its required fields",
		"a typed result (freshness only when ok, queued or partial)", observed, true))
}

// requiredFields lists the names of a capability's required fields.
func requiredFields(c ap.Capability) []string {
	var out []string
	for _, f := range c.Fields {
		if f.Required {
			out = append(out, string(f.Name))
		}
	}
	return out
}

// stepUpResults is commitResults for an action that needs the application's own step-up:
// the commit is a typed refusal, step_up_required, and Status for its key is the no-record
// marker. There is no outcome to bind to the preparation.
func (sc *suiteCtx) stepUpResults(ctx context.Context, id string, c ap.Capability, scope contracts.Scope, pr prepared) (committed, withStatus bool) {
	r := sc.r
	ans, ok := sc.stepUpCommit(ctx, id, c, scope, pr.action)
	if !ok {
		return false, false
	}
	switch {
	case ans.applied:
		return false, false // critical already: stepUpCommit raised it
	case !ans.stepUp:
		r.notProbed(id, fmt.Sprintf("its commit of a valid preparation was answered %s (error code %s), not step_up_required", ans.res.Status, ans.code))
		return false, false
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeDeterministicResult, id, "commit of a step-up action is a typed refusal",
		"rejected, step_up_required", "rejected, step_up_required", true))
	return true, hasOp(c, "status")
}
