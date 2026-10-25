package rag

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func smokeCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDSmoke, Title: "RAG runtime smoke test: retrieval, grounded answer, citations, scope and cleanup",
		Section: integrationaudit.SectionRuntimeSmoke, Stage: integrationaudit.StageBenchmark, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return runSmoke(ctx, o, in)
	})
}

// smokeRun is the state of one run of the smoke check.
type smokeRun struct {
	ctx     context.Context
	o       Options
	in      *integrationaudit.Input
	out     *integrationaudit.Outcome
	total   int
	covered int
}

// runSmoke is the RAG path of master plan 4.5 check 10, run for every `rag` smoke path of
// every suite, in the core's own retrieval stack over the lab (lab.go):
//
//   - retrieval for the path's principal returns the passage of the principal's own scope, in
//     an evidence package bound to that scope and holding nothing of any other;
//   - the answer step writes a grounded answer from it with at least one citation, every
//     citation is of a source registered in the principal's scope and resolves for the
//     principal;
//   - the path's own utterance is put to the same pipeline, and what it comes to is compared
//     with what the path expects of citations;
//   - at the end the synthetic sources are removed and nothing of them may be served.
//
// What the audit cannot establish is recorded as unverified, never as present and never as
// missing. The audit's passages are its own synthetic text, so the application's own question
// can be answered from them only by accident: a path that expects a cited answer to its
// utterance is verified only if the pipeline happens to produce one, and a path that expects
// none is verified only if retrieval finds nothing (it never fails a path for what the
// synthetic corpus does or does not say). The audit events a path expects (rag.retrieved,
// rag.no_supported_evidence, dialogue.turn and the like) are appended by the dialogue layer,
// which the audit does not run and which no code of this build's RAG pipeline writes: they
// are unverified, and the audit never appends one itself.
func runSmoke(ctx context.Context, o Options, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
	out := &integrationaudit.Outcome{}
	if in.Package == nil {
		noPackage(out, integrationaudit.SectionRuntimeSmoke, "rag_smoke:package")
		return out, nil
	}
	pkg := in.Package
	r := &smokeRun{ctx: ctx, o: o, in: in, out: out}
	hosted, reasons := labSources(pkg)

	for _, suite := range pkg.Tests {
		paths := ragSmokePaths(suite)
		if len(paths) == 0 {
			continue
		}
		r.total += len(paths)
		if len(hosted) == 0 {
			reason := noHostable(len(pkg.Sources), reasons)
			for _, p := range paths {
				r.pathUnverified(p, reason)
			}
			continue
		}
		withLab(ctx, o, in, suite, hosted, out,
			func(reason string) {
				for _, p := range paths {
					r.pathUnverified(p, reason)
				}
			},
			func(l *lab) {
				for _, p := range paths {
					if ctx.Err() != nil {
						r.pathUnverified(p, "the audit was cancelled before the path ran")
						continue
					}
					r.path(l, p)
				}
				r.cleanup(l)
			})
	}
	if r.total > 0 {
		out.Coverage = append(out.Coverage, integrationaudit.CoverageItem{Name: "smoke_paths", Covered: r.covered, Total: r.total})
	}
	out.Extension = map[string]any{"rag_paths": r.total, "rag_paths_run": r.covered}
	return out, nil
}

func (r *smokeRun) pathUnverified(p ap.PackageTestsSmokePath, reason string) {
	unverify(r.out, integrationaudit.SectionRuntimeSmoke, "smoke_path:"+string(p.ID), reason)
}

func (r *smokeRun) failed(p ap.PackageTestsSmokePath, what string) {
	r.out.Add(newFinding(fdSmokePath, pathTests, "RAG smoke path %s did not complete: %s", quote(string(p.ID)), what))
	record(r.out, integrationaudit.ProbeSmoke, string(p.ID)+" (rag)", "the path completes", what, false)
}

// path runs one RAG smoke path.
func (r *smokeRun) path(l *lab, p ap.PackageTestsSmokePath) {
	principal := l.principals[string(p.Principal)]
	if principal == nil {
		r.pathUnverified(p, "the path names a principal the suite does not define")
		return
	}
	regs := l.regsOf(principal.pair)
	if len(regs) == 0 {
		r.pathUnverified(p, "no source is registered in the principal's scope: the declared binding refuses it, or the core could not build the source")
		return
	}
	reg := regs[0]
	name := string(p.ID) + " (rag)"

	// 1. Retrieval of the principal's own passage.
	res, err := l.ask(r.ctx, principal, reg.doc.marker)
	if err != nil {
		r.pathUnverified(p, "retrieval could not answer ("+codeString(err)+")")
		return
	}
	switch checkEvidence(r.out, l, principal, "smoke path "+string(p.ID), res.Package) {
	case verdictFailed:
		r.failed(p, "the evidence package was not bound to the principal's scope, or held something of another scope")
		return
	case verdictNotShown:
		r.pathUnverified(p, "the evidence package could not be inspected")
		return
	}
	if !foundOwn(res.Package, reg) {
		r.failed(p, "retrieval did not return the passage of the principal's own scope")
		return
	}

	// 2. A grounded answer with citations that resolve.
	l.gen.set(res.Package)
	ans, err := l.answ.Answer(r.ctx, principal.scope, res.Package)
	if err != nil {
		r.pathUnverified(p, "the answer step could not answer ("+codeString(err)+")")
		return
	}
	ga, ok := ans.Answer.Value.(ragc.GroundedAnswer)
	if !ok || len(ga.Citations) == 0 {
		r.failed(p, "the answer step produced no cited answer for the passage retrieval returned")
		return
	}
	for i, c := range ga.Citations {
		if !isRegSource(regs, string(c.SourceID)) {
			r.failed(p, fmt.Sprintf("citation %d is of a source that is not registered in the principal's scope", i+1))
			return
		}
		file, opaque := fileRef(c.OpenRef)
		if !opaque {
			r.failed(p, fmt.Sprintf("citation %d carries a reference that is not an opaque file reference", i+1))
			return
		}
		if _, err := l.resv.Resolve(r.ctx, principal.scope, file); err != nil {
			if outage(r.ctx, err) {
				r.pathUnverified(p, "the resolver could not answer ("+codeString(err)+")")
				return
			}
			r.failed(p, fmt.Sprintf("citation %d does not resolve for the principal (%s)", i+1, codeString(err)))
			return
		}
	}
	record(r.out, integrationaudit.ProbeSmoke, name, "retrieval, a grounded answer with resolvable citations, in the principal's scope",
		fmt.Sprintf("%d citation(s), all resolvable", len(ga.Citations)), true)
	r.covered++

	// 3. The path's own utterance, against what the path expects.
	r.utterance(l, p, principal, regs)

	// 4. What the path expects the application to log.
	for _, ev := range p.Expect.AuditEvents {
		unverify(r.out, integrationaudit.SectionRuntimeSmoke, fmt.Sprintf("audit_event:%s (%s)", ev, p.ID),
			"the event is appended by the dialogue layer, which the audit does not run, and no code of this build's RAG pipeline writes it, so it could not be checked")
	}
}

func isRegSource(regs []*labReg, srcID string) bool {
	for _, r := range regs {
		if r.srcID == srcID {
			return true
		}
	}
	return false
}

// utterance puts the path's own utterance to retrieval and the answer step. The outcome is
// compared with what the path expects of citations, as far as the audit's synthetic corpus
// allows.
func (r *smokeRun) utterance(l *lab, p ap.PackageTestsSmokePath, principal *labPrincipal, regs []*labReg) {
	subject := "smoke_path:" + string(p.ID)
	name := string(p.ID) + " (rag): the path's own utterance"
	if p.Expect.Citations == nil {
		unverify(r.out, integrationaudit.SectionRuntimeSmoke, subject+":utterance", "the path states no expectation about citations")
		return
	}
	wantCitations := *p.Expect.Citations
	res, err := l.ask(r.ctx, principal, p.Utterance)
	if err != nil {
		unverify(r.out, integrationaudit.SectionRuntimeSmoke, subject+":utterance", "retrieval could not answer the utterance ("+codeString(err)+")")
		return
	}
	switch checkEvidence(r.out, l, principal, "the utterance of smoke path "+string(p.ID), res.Package) {
	case verdictFailed:
		r.failed(p, "the evidence package of the utterance was not bound to the principal's scope, or held something of another scope")
		return
	case verdictNotShown:
		unverify(r.out, integrationaudit.SectionRuntimeSmoke, subject+":utterance", "the evidence package of the utterance could not be inspected")
		return
	}
	l.gen.set(res.Package)
	ans, err := l.answ.Answer(r.ctx, principal.scope, res.Package)
	if err != nil {
		unverify(r.out, integrationaudit.SectionRuntimeSmoke, subject+":utterance", "the answer step could not answer the utterance ("+codeString(err)+")")
		return
	}
	switch v := ans.Answer.Value.(type) {
	case ragc.NoSupportedEvidence:
		if wantCitations {
			unverify(r.out, integrationaudit.SectionRuntimeSmoke, subject+":utterance",
				"the path expects a cited answer to its utterance, which the audit's synthetic passages cannot give, so it could not be verified")
			return
		}
		record(r.out, integrationaudit.ProbeSmoke, name, "no supported evidence, no citations", "no_supported_evidence ("+string(v.Reason)+"), no citations", true)
	case ragc.GroundedAnswer:
		for _, c := range v.Citations {
			if !isRegSource(regs, string(c.SourceID)) {
				r.failed(p, "a citation of the utterance's answer is of a source that is not registered in the principal's scope")
				return
			}
		}
		if wantCitations {
			record(r.out, integrationaudit.ProbeSmoke, name, "a cited answer", fmt.Sprintf("%d citation(s)", len(v.Citations)), len(v.Citations) > 0)
			if len(v.Citations) == 0 {
				r.failed(p, "the answer to the utterance carries no citation")
			}
			return
		}
		unverify(r.out, integrationaudit.SectionRuntimeSmoke, subject+":utterance",
			"the path expects no evidence, but the utterance matched a passage of the audit's synthetic corpus, so whether the application's own documents leave it unanswered could not be verified")
	}
}

// cleanup removes the synthetic sources of the lab and requires that nothing of them is
// served afterwards, to anyone.
func (r *smokeRun) cleanup(l *lab) {
	removed := true
	for _, reg := range l.regs {
		if err := l.removeSource(r.ctx, reg); err != nil {
			removed = false
			unverify(r.out, integrationaudit.SectionRuntimeSmoke, "rag_cleanup:"+reg.source, "the index could not remove the synthetic source ("+codeString(err)+")")
		}
	}
	if !removed {
		return
	}
	left := 0
	for _, ref := range l.principalRefs() {
		p := l.principals[ref]
		for _, reg := range l.regsOf(p.pair) {
			res, err := l.ask(r.ctx, p, reg.doc.marker)
			switch {
			case err != nil && outage(r.ctx, err):
				unverify(r.out, integrationaudit.SectionRuntimeSmoke, "rag_cleanup:"+reg.source, "retrieval could not answer after the removal ("+codeString(err)+")")
			case err == nil && (len(res.Package.Items) > 0 || containsAnyText(res.Package, reg.doc.marker)):
				left++
			}
		}
	}
	if left > 0 {
		r.out.Add(newFinding(fdCleanup, pathTests, "%d synthetic source(s) of the audit were still served after the audit removed them", left))
		record(r.out, integrationaudit.ProbeSmoke, "cleanup of the synthetic sources", "nothing of them is served", fmt.Sprintf("%d still served", left), false)
		return
	}
	record(r.out, integrationaudit.ProbeSmoke, "cleanup of the synthetic sources", "nothing of them is served", "nothing served", true)
}
