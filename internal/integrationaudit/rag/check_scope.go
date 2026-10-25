package rag

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func scopeCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDScope, Title: "RAG scope: chunk-level cross-scope probes",
		Section: integrationaudit.SectionScope, Stage: integrationaudit.StageRAG, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return runScope(ctx, o, in)
	})
}

// scopeRun is the state of one run of the scope check.
type scopeRun struct {
	ctx    context.Context
	o      Options
	in     *integrationaudit.Input
	out    *integrationaudit.Outcome
	budget int // derived retrievals left
	probes int // retrieve probes of the suites that ran
	ran    int // of which ran
	swept  int // derived probes that ran
}

// runScope is the retrieval half of master plan 4.5 check 6: positive and negative
// cross-scope probes at the level of the chunk, run against the core's own index and query
// pipeline in a throwaway store (lab.go) over the tenants, workspaces and principals of the
// package's test suites.
//
// In every suite, every fixture principal
//
//   - retrieves the passage the audit wrote into its own scope: it must be found, the
//     evidence package must be bound to the caller's scope, and nothing of any other scope
//     may be in it;
//   - asks for the passage of every other scope (another tenant, and another workspace of
//     the same tenant) by its marker, once as a free question and once with the other
//     scope's source named in the request: nothing of the other scope, not its text, its
//     source, its generation or its chunks, may reach the evidence package, and so the model;
//   - tries to read the other scope's chunks by their reference through the index, which must
//     answer exactly as it does for a chunk that does not exist.
//
// The suite's own retrieve probes are then executed: an allow probe must find the source's
// passage in the principal's scope, a deny probe must find nothing of the source's passages
// in other scopes. Only the tenant, workspace and source dimensions can be probed: the
// declaration of a source carries no role, principal or document rule, so a probe along
// those dimensions is recorded as unverified. A denied retrieval that is merely ignored
// (an unreadable source is dropped without an error) passes: what a deny probe requires is
// that nothing of the target reaches the caller, and the declared error code cannot be
// compared with a retrieval that, by design, does not raise one.
func runScope(ctx context.Context, o Options, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
	out := &integrationaudit.Outcome{}
	if in.Package == nil {
		noPackage(out, integrationaudit.SectionScope, "rag_scope:package")
		return out, nil
	}
	pkg := in.Package
	r := &scopeRun{ctx: ctx, o: o, in: in, out: out, budget: o.maxSweep()}
	hosted, reasons := labSources(pkg)

	for _, id := range sortedKeys(reasons) {
		unverify(out, integrationaudit.SectionScope, "rag_scope:source:"+id, reasons[id])
	}
	if len(pkg.Tests) == 0 && len(pkg.Sources) > 0 {
		unverify(out, integrationaudit.SectionScope, "rag_scope:suite",
			"the package declares no test suite, so it names no tenants and principals to probe retrieval with")
	}
	for _, suite := range pkg.Tests {
		r.suite(suite, hosted, reasons)
	}
	if r.probes > 0 {
		out.Coverage = append(out.Coverage, integrationaudit.CoverageItem{Name: "rag_scope_probes", Covered: r.ran, Total: r.probes})
	}
	out.Extension = map[string]any{"hosted_sources": len(hosted), "retrieve_probes": r.probes, "derived_probes": r.swept}
	return out, nil
}

func (r *scopeRun) probeUnverified(p ap.PackageTestsScopeProbe, reason string) {
	unverify(r.out, integrationaudit.SectionScope, "scope_probe:"+string(p.ID), reason)
}

func (r *scopeRun) suite(suite ap.PackageTests, hosted []labSource, reasons map[string]string) {
	probes := retrieveProbes(suite)
	r.probes += len(probes)
	if len(hosted) == 0 {
		reason := noHostable(len(r.in.Package.Sources), reasons)
		for _, p := range probes {
			r.probeUnverified(p, reason)
		}
		return
	}
	withLab(r.ctx, r.o, r.in, suite, hosted, r.out,
		func(reason string) {
			unverify(r.out, integrationaudit.SectionScope, "rag_scope:suite:"+string(suite.Suite), reason)
			for _, p := range probes {
				r.probeUnverified(p, reason)
			}
		},
		func(l *lab) {
			for _, src := range sortedKeys(l.unhosted) {
				unverify(r.out, integrationaudit.SectionScope, "rag_scope:source:"+src, l.unhosted[src])
			}
			r.sweep(l)
			for _, p := range probes {
				if r.ctx.Err() != nil {
					r.probeUnverified(p, "the audit was cancelled before the probe ran")
					continue
				}
				if r.retrieve(l, p, reasons) {
					r.ran++
				}
			}
		})
}

// own is the positive probe: a principal retrieves the passage of its own scope.
func (r *scopeRun) own(l *lab, p *labPrincipal, reg *labReg, label string) verdict {
	subject := fmt.Sprintf("%s retrieves its own scope's passage of %s", p.ref, reg.source)
	if label != "" {
		subject = label
	}
	res, err := l.ask(r.ctx, p, reg.doc.marker)
	if err != nil {
		if outage(r.ctx, err) {
			unverify(r.out, integrationaudit.SectionScope, "rag_scope:probe:"+subject, "retrieval could not answer ("+codeString(err)+")")
			return verdictNotShown
		}
		r.out.Add(newFinding(fdOwnChunkMissing, pathTests, "retrieval refused %s the passage of its own scope in source %s (%s)", quote(p.ref), quote(reg.source), codeString(err)))
		record(r.out, integrationaudit.ProbePositiveScope, subject, "the principal's own passage is retrieved", "refused ("+codeString(err)+")", false)
		return verdictOwnMissing
	}
	v := checkEvidence(r.out, l, p, "own passage of "+reg.source, res.Package)
	if v != verdictHeld {
		record(r.out, integrationaudit.ProbePositiveScope, subject, "the principal's own passage is retrieved, and only that scope's", "the evidence package was not as required", false)
		return v
	}
	if !foundOwn(res.Package, reg) {
		r.out.Add(newFinding(fdOwnChunkMissing, pathTests, "retrieval did not return the passage the audit indexed in the scope of %s, source %s", quote(p.ref), quote(reg.source)))
		record(r.out, integrationaudit.ProbePositiveScope, subject, "the principal's own passage is retrieved", "not retrieved", false)
		return verdictOwnMissing
	}
	record(r.out, integrationaudit.ProbePositiveScope, subject, "the principal's own passage is retrieved", "retrieved", true)
	return verdictHeld
}

// foreignProbe is the negative probe: a principal asks for the passage of another scope.
func (r *scopeRun) foreignProbe(l *lab, p *labPrincipal, q *labReg, namedSource bool) verdict {
	how := "by its marker"
	if namedSource {
		how = "by its marker, naming the other scope's source"
	}
	subject := fmt.Sprintf("%s asks for the passage of %s/%s (%s) %s", p.ref, q.pair.tenantRef, q.pair.workspaceRef, q.source, how)
	var (
		res query.Result
		err error
	)
	if namedSource {
		res, err = l.ask(r.ctx, p, q.doc.marker, q.srcID)
	} else {
		res, err = l.ask(r.ctx, p, q.doc.marker)
	}
	if err != nil {
		switch {
		case refusal(err):
			record(r.out, integrationaudit.ProbeNegativeScope, subject, "nothing of the other scope reaches the caller", "refused ("+codeString(err)+")", true)
			return verdictHeld
		default:
			unverify(r.out, integrationaudit.SectionScope, "rag_scope:probe:"+subject, "retrieval could not answer ("+codeString(err)+")")
			return verdictNotShown
		}
	}
	v := checkEvidence(r.out, l, p, "passage of another scope, "+how, res.Package)
	if v != verdictHeld {
		record(r.out, integrationaudit.ProbeNegativeScope, subject, "nothing of the other scope reaches the caller", "the evidence package was not as required", false)
		return v
	}
	record(r.out, integrationaudit.ProbeNegativeScope, subject, "nothing of the other scope reaches the caller", "nothing of the other scope", true)
	return verdictHeld
}

// byReference is the negative probe at the level of the chunk itself: the principal presents
// the reference of a chunk of another scope to the index, to read it and to have it
// rechecked as live. Both must answer as they do for a chunk that does not exist.
func (r *scopeRun) byReference(l *lab, p *labPrincipal, q *labReg) verdict {
	subject := fmt.Sprintf("%s reads a chunk of %s/%s (%s) by its reference", p.ref, q.pair.tenantRef, q.pair.workspaceRef, q.source)
	refs := make([]index.Ref, 0, len(q.children))
	for _, c := range q.children {
		refs = append(refs, index.Ref{ChunkID: c, GenerationID: q.genID})
	}
	if len(refs) == 0 {
		return verdictHeld
	}
	hits, herr := l.ix.Hits(r.ctx, p.scope, refs, true)
	verdicts, verr := l.ix.Recheck(r.ctx, p.scope, refs, true)
	for _, err := range []error{herr, verr} {
		if err != nil && !refusal(err) {
			unverify(r.out, integrationaudit.SectionScope, "rag_scope:probe:"+subject, "the index could not answer ("+codeString(err)+")")
			return verdictNotShown
		}
	}
	readable := len(hits) > 0
	for _, v := range verdicts {
		if v.Visible {
			readable = true
		}
	}
	if readable {
		r.out.Add(newFinding(fdByReference, pathTests, "%s could read, or recheck as live, a chunk of another scope by its reference (%s/%s, source %s)",
			quote(p.ref), quote(q.pair.tenantRef), quote(q.pair.workspaceRef), quote(q.source)))
		record(r.out, integrationaudit.ProbeNegativeScope, subject, "the chunk is not readable and not live for the caller", "the chunk was readable or live", false)
		return verdictFailed
	}
	record(r.out, integrationaudit.ProbeNegativeScope, subject, "the chunk is not readable and not live for the caller", "not readable, not live", true)
	return verdictHeld
}

// sweep runs the derived probes: every principal against its own passages and against every
// other scope's, within the budget.
func (r *scopeRun) sweep(l *lab) {
	for _, ref := range l.principalRefs() {
		p := l.principals[ref]
		for _, reg := range l.regsOf(p.pair) {
			if !r.spend(3) {
				return
			}
			r.own(l, p, reg, "")
		}
		for _, q := range l.foreign(p) {
			if !r.spend(3) {
				return
			}
			r.foreignProbe(l, p, q, false)
			r.foreignProbe(l, p, q, true)
			r.byReference(l, p, q)
		}
	}
}

// spend takes n retrievals from the budget. When it is gone, what is left is unverified.
func (r *scopeRun) spend(n int) bool {
	if r.budget < n {
		if r.budget >= 0 {
			unverify(r.out, integrationaudit.SectionScope, "rag_scope:sweep",
				"the derived cross-scope probes were cut at the audit's bound ("+fmt.Sprint(r.o.maxSweep())+" retrievals); the rest were not run")
			r.budget = -1
		}
		return false
	}
	r.budget -= n
	r.swept += n
	return true
}

// retrieve executes one retrieve probe of the suite. It reports whether the probe ran.
func (r *scopeRun) retrieve(l *lab, p ap.PackageTestsScopeProbe, reasons map[string]string) bool {
	switch p.Dimension {
	case ap.PackageTestsScopeProbeDimensionTenant, ap.PackageTestsScopeProbeDimensionWorkspace, ap.PackageTestsScopeProbeDimensionSource:
	default:
		r.probeUnverified(p, "a source's declaration carries no "+string(p.Dimension)+" rule, so retrieval cannot be probed along that dimension")
		return false
	}
	if p.TargetSource == nil {
		r.probeUnverified(p, "the probe names no source to retrieve from")
		return false
	}
	target := string(*p.TargetSource)
	principal := l.principals[string(p.Principal)]
	switch {
	case principal == nil:
		r.probeUnverified(p, "the probe names a principal the suite does not define")
		return false
	case l.unhosted[target] != "":
		r.probeUnverified(p, l.unhosted[target])
		return false
	case reasons[target] != "":
		r.probeUnverified(p, reasons[target])
		return false
	}
	if _, declared := r.in.Package.Sources[target]; !declared {
		r.probeUnverified(p, "the probe names a source the package does not declare")
		return false
	}
	own := l.regFor(principal.pair, target)
	subject := fmt.Sprintf("%s: %s retrieves from %s", p.ID, p.Principal, target)

	if p.Expect == ap.PackageTestsScopeProbeExpectAllow {
		if own == nil {
			r.probeUnverified(p, "the source is not registered in the principal's scope, so there is nothing for the principal to retrieve")
			return false
		}
		v := r.own(l, principal, own, subject)
		return v != verdictNotShown
	}

	// deny: nothing of the source's passages in other scopes reaches the principal.
	var others []*labReg
	for _, reg := range l.regs {
		if reg.source == target && reg.pair != principal.pair {
			others = append(others, reg)
		}
	}
	if len(others) == 0 {
		r.probeUnverified(p, "the suite's fixtures hold no other scope in which the source could be registered, so there is nothing for the principal to be denied")
		return false
	}
	held := true
	for _, q := range others {
		for _, named := range []bool{false, true} {
			switch r.foreignProbe(l, principal, q, named) {
			case verdictNotShown:
				r.probeUnverified(p, "retrieval could not answer")
				return false
			case verdictHeld:
			default:
				held = false
			}
		}
	}
	record(r.out, integrationaudit.ProbeNegativeScope, subject, "denied: nothing of the source reaches the principal", map[bool]string{true: "nothing of the source's other scopes", false: "something reached the principal"}[held], held)
	return true
}
