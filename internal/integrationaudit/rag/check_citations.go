package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ProbeRAGCitation is the kind of the probe results of the citation resolver probes.
const ProbeRAGCitation integrationaudit.ProbeKind = "rag_citation"

func citationsCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDCitations, Title: "RAG citations: every citation resolves, permission-checked, to the original",
		Section: integrationaudit.SectionRAGInventory, Stage: integrationaudit.StageRAG, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return runCitations(ctx, o, in)
	})
}

// issued is a citation reference the probes obtained, with the principal it was written for
// and the registration it cites.
type issued struct {
	p      *labPrincipal
	reg    *labReg
	fileID actions.FileID
	// version is the version of the passage when the reference was issued.
	version string
}

// citeRun is the state of one run of the citation check.
type citeRun struct {
	ctx       context.Context
	o         Options
	in        *integrationaudit.Input
	out       *integrationaudit.Outcome
	citations int // citations produced
	resolved  int // of which passed every probe
}

// runCitations is the citation half of master plan 4.5 check 3 ("citation resolver") and
// 12.5 (a permission-checked open/preview reference), run against the core's own answer
// step and resolver over the lab (lab.go).
//
// For every fixture principal and every hosted source registered in its scope, the audit
// retrieves the principal's own passage, has the answer step write a grounded answer from it
// (with the extractive stand-in, not a model), and then holds every citation of the answer to
// what a citation must be:
//
//   - the reference is an opaque file reference, never a path or a URL;
//   - it resolves for the principal it was written for, to the source and version it was
//     issued for, labelled available, with a preview that is a passage of the original
//     document the audit wrote;
//   - it does not resolve for a principal of another tenant or workspace, for another
//     principal of the same scope, or when it never existed, and the three refusals are one
//     and the same typed refusal, so the answer is no oracle for another scope's references;
//   - it stops resolving when the source catalog revokes the source;
//   - it expires.
//
// Then the lifecycle of a source is followed through retrieval and citation: a document the
// source's refresh replaces is served in its new text and not its old, and a source that is
// removed serves nothing and its references resolve no more (the declared refresh and
// deletion behaviour, exercised by the core).
//
// Only the core's own preview resolver can be exercised. A source declared with another kind
// of citation resolver is recorded as unverified by the source check, and its passages are
// checked here all the same, because every answer carries the core's references.
func runCitations(ctx context.Context, o Options, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
	out := &integrationaudit.Outcome{}
	if in.Package == nil {
		noPackage(out, integrationaudit.SectionRAGInventory, "rag_citation:package")
		return out, nil
	}
	pkg := in.Package
	r := &citeRun{ctx: ctx, o: o, in: in, out: out}
	hosted, reasons := labSources(pkg)

	for _, id := range sortedKeys(reasons) {
		unverify(out, integrationaudit.SectionRAGInventory, "rag_citation:source:"+id, reasons[id])
	}
	if len(pkg.Tests) == 0 && len(pkg.Sources) > 0 {
		unverify(out, integrationaudit.SectionRAGInventory, "rag_citation:suite",
			"the package declares no test suite, so it names no tenants and principals to retrieve and cite with")
	}
	if len(hosted) > 0 {
		for _, suite := range pkg.Tests {
			withLab(ctx, o, in, suite, hosted, out,
				func(reason string) {
					unverify(out, integrationaudit.SectionRAGInventory, "rag_citation:suite:"+string(suite.Suite), reason)
				},
				r.suite)
		}
	}
	if r.citations > 0 {
		out.Coverage = append(out.Coverage, integrationaudit.CoverageItem{Name: "rag_citations", Covered: r.resolved, Total: r.citations})
	}
	out.Extension = map[string]any{"hosted_sources": len(hosted), "citations": r.citations}
	return out, nil
}

func (r *citeRun) unverified(subject, reason string) {
	unverify(r.out, integrationaudit.SectionRAGInventory, "rag_citation:probe:"+subject, reason)
}

// suite runs the citation probes of one suite's lab.
func (r *citeRun) suite(l *lab) {
	for _, src := range sortedKeys(l.unhosted) {
		unverify(r.out, integrationaudit.SectionRAGInventory, "rag_citation:source:"+src, l.unhosted[src])
	}
	var refs []issued
	for _, ref := range l.principalRefs() {
		p := l.principals[ref]
		for _, reg := range l.regsOf(p.pair) {
			if r.ctx.Err() != nil {
				r.unverified(p.ref+" cites "+reg.source, "the audit was cancelled before the probe ran")
				continue
			}
			refs = append(refs, r.answerAndCite(l, p, reg)...)
		}
	}
	if len(refs) == 0 {
		return
	}
	r.lifecycle(l, refs[0])
	r.expiry(l, refs)
}

// answerAndCite retrieves a principal's own passage of a source, has the answer step write a
// grounded answer from it and holds each citation to account. It returns the references the
// answer issued.
func (r *citeRun) answerAndCite(l *lab, p *labPrincipal, reg *labReg) []issued {
	subject := fmt.Sprintf("%s cites %s", p.ref, reg.source)
	res, err := l.ask(r.ctx, p, reg.doc.marker)
	if err != nil {
		r.unverified(subject, "retrieval could not produce the evidence ("+codeString(err)+")")
		return nil
	}
	if !samePackageScope(res.Package, p.scope) || !foundOwn(res.Package, reg) {
		// The scope check owns this finding; without evidence there is no citation to hold.
		r.unverified(subject, "retrieval did not return the principal's own passage, so no citation could be produced (see the scope check)")
		return nil
	}
	l.gen.set(res.Package)
	ans, err := l.answ.Answer(r.ctx, p.scope, res.Package)
	if err != nil {
		r.unverified(subject, "the answer step could not answer ("+codeString(err)+")")
		return nil
	}
	ga, ok := ans.Answer.Value.(ragc.GroundedAnswer)
	if !ok {
		reason := "no_supported_evidence"
		if nse, isNSE := ans.Answer.Value.(ragc.NoSupportedEvidence); isNSE {
			reason += ": " + string(nse.Reason)
		}
		r.out.Add(newFinding(fdCitationMissing, pathTests,
			"the answer step produced no cited answer for the passage of source %s that retrieval had just returned to %s (%s)", quote(reg.source), quote(p.ref), reason))
		record(r.out, ProbeRAGCitation, subject, "a grounded answer with a citation", "no grounded answer ("+reason+")", false)
		return nil
	}
	if !r.wellFormed(ga, p, reg, subject) {
		return nil
	}

	var out []issued
	for i, c := range ga.Citations {
		r.citations++
		file, opaque := fileRef(c.OpenRef)
		if !opaque {
			r.out.Add(newFinding(fdCitationOpaque, pathTests,
				"citation %d of the answer for %s on source %s carries a reference that is not an opaque file reference", i+1, quote(p.ref), quote(reg.source)))
			record(r.out, ProbeRAGCitation, subject, "an opaque file reference", "not an opaque file reference", false)
			continue
		}
		it := issued{p: p, reg: reg, fileID: file, version: reg.doc.version}
		if r.holdToAccount(l, it, c, fmt.Sprintf("%s (citation %d)", subject, i+1)) {
			r.resolved++
		}
		out = append(out, it)
	}
	return out
}

// wellFormed checks the shape of the answer: at least one citation, and every claim cites at
// least one of them.
func (r *citeRun) wellFormed(ga ragc.GroundedAnswer, p *labPrincipal, reg *labReg, subject string) bool {
	bad := len(ga.Citations) == 0 || len(ga.Claims) == 0
	for _, c := range ga.Claims {
		if len(c.CitationIndexes) == 0 {
			bad = true
		}
		for _, ix := range c.CitationIndexes {
			if ix < 0 || int(ix) >= len(ga.Citations) {
				bad = true
			}
		}
	}
	if !bad {
		return true
	}
	r.out.Add(newFinding(fdCitationMissing, pathTests,
		"a grounded answer for %s on source %s carries a claim with no citation, or no citation at all", quote(p.ref), quote(reg.source)))
	record(r.out, ProbeRAGCitation, subject, "every claim cites a citation", "a claim without a citation", false)
	return false
}

// fileRef returns the file reference of an open reference, and whether it is an opaque one:
// a file reference whose identifier is a bare identifier, with no path or URL in it.
func fileRef(ref conversation.CitationOpenRef) (actions.FileID, bool) {
	f, ok := ref.Value.(conversation.CitationOpenRefFile)
	if !ok {
		return "", false
	}
	id := string(f.FileID)
	if !strings.HasPrefix(id, "fil_") || strings.ContainsAny(id, "/\\:.?#% ") {
		return "", false
	}
	return f.FileID, true
}

// holdToAccount runs every probe of one citation and reports whether all of them passed.
func (r *citeRun) holdToAccount(l *lab, it issued, c conversation.Citation, subject string) bool {
	good := true
	fail := func() { good = false }

	// It resolves for its owner, to the original.
	got, err := l.resv.Resolve(r.ctx, it.p.scope, it.fileID)
	switch {
	case err != nil && outage(r.ctx, err):
		r.unverified(subject, "the resolver could not answer ("+codeString(err)+")")
		return false
	case err != nil:
		r.out.Add(newFinding(fdCitationUnresolved, pathTests,
			"a citation for %s on source %s does not resolve for the principal it was written for (%s)", quote(it.p.ref), quote(it.reg.source), codeString(err)))
		record(r.out, ProbeRAGCitation, subject, "resolves for the principal it was written for", "refused ("+codeString(err)+")", false)
		return false
	}
	if reason := originalMismatch(got, c, it.reg); reason != "" {
		r.out.Add(newFinding(fdCitationOriginal, pathTests,
			"a citation for %s on source %s resolves to something other than the original: %s", quote(it.p.ref), quote(it.reg.source), reason))
		record(r.out, ProbeRAGCitation, subject, "resolves to the source, version and passage it cites", reason, false)
		fail()
	} else {
		record(r.out, ProbeRAGCitation, subject, "resolves to the source, version and passage it cites", "resolves to the original", true)
	}

	// It resolves for nobody else, and the refusals are one refusal.
	refused := map[string]error{}
	try := func(what string, scope contracts.Scope, id actions.FileID) {
		_, err := l.resv.Resolve(r.ctx, scope, id)
		switch {
		case err == nil:
			r.out.Add(newFinding(fdCitationForeign, pathTests,
				"a citation reference written for %s on source %s resolved for %s", quote(it.p.ref), quote(it.reg.source), what))
			record(r.out, ProbeRAGCitation, subject, "does not resolve for "+what, "resolved", false)
			fail()
		case outage(r.ctx, err):
			r.unverified(subject+" for "+what, "the resolver could not answer ("+codeString(err)+")")
		case !contracts.HasCode(err, contracts.ErrPermissionDenied):
			r.out.Add(newFinding(fdCitationForeign, pathTests,
				"a citation reference written for %s on source %s was refused for %s with %s, not permission_denied", quote(it.p.ref), quote(it.reg.source), what, codeString(err)))
			record(r.out, ProbeRAGCitation, subject, "does not resolve for "+what+", refused as permission_denied", "refused ("+codeString(err)+")", false)
			fail()
		default:
			record(r.out, ProbeRAGCitation, subject, "does not resolve for "+what, "refused (permission_denied)", true)
			refused[what] = err
		}
	}
	if q := otherPrincipal(l, it.p, false); q != nil {
		try("a principal of another tenant or workspace", q.scope, it.fileID)
	} else {
		r.unverified(subject+" for another scope", "the suite's fixtures hold no principal of another scope")
	}
	if q := otherPrincipal(l, it.p, true); q != nil {
		try("another principal of the same scope", q.scope, it.fileID)
	}
	try("a reference that never existed", it.p.scope, actions.FileID(ids.New("fil")))
	if foreignErr, ok := refused["a principal of another tenant or workspace"]; ok {
		if unknownErr, ok := refused["a reference that never existed"]; ok &&
			(contracts.CodeOf(foreignErr) != contracts.CodeOf(unknownErr) || foreignErr.Error() != unknownErr.Error()) {
			r.out.Add(newFinding(fdCitationOracle, pathTests,
				"the refusal of a reference of another scope differs from the refusal of a reference that never existed (source %s)", quote(it.reg.source)))
			record(r.out, ProbeRAGCitation, subject, "one and the same refusal for a foreign and an unknown reference", "the refusals differ", false)
			fail()
		}
	}

	// It stops resolving when the source is revoked.
	l.cat.revoke(it.reg)
	_, err = l.resv.Resolve(r.ctx, it.p.scope, it.fileID)
	l.cat.restore(it.reg)
	switch {
	case err == nil:
		r.out.Add(newFinding(fdCitationRevoked, pathTests,
			"a citation reference for %s on source %s still resolved after the source was revoked", quote(it.p.ref), quote(it.reg.source)))
		record(r.out, ProbeRAGCitation, subject, "does not resolve once the source is revoked", "resolved", false)
		fail()
	case outage(r.ctx, err):
		r.unverified(subject+" after revocation", "the resolver could not answer ("+codeString(err)+")")
	default:
		record(r.out, ProbeRAGCitation, subject, "does not resolve once the source is revoked", "refused ("+codeString(err)+")", true)
	}
	return good
}

// originalMismatch says how a resolved citation differs from the original it cites, or "".
func originalMismatch(got answer.Resolution, c conversation.Citation, reg *labReg) string {
	switch {
	case string(got.SourceID) != reg.srcID || string(c.SourceID) != reg.srcID:
		return "another source"
	case string(got.Version) != reg.doc.version || string(c.Version) != reg.doc.version:
		return "another version"
	case got.Availability != conversation.CitationAvailabilityAvailable:
		return "a source that is not labelled available"
	case strings.TrimSpace(got.Preview) == "":
		return "no preview of the cited passage"
	case !strings.Contains(collapse(reg.doc.text), collapse(got.Preview)):
		return "a preview that is not a passage of the original"
	}
	return ""
}

// otherPrincipal returns a fixture principal other than p: with sameScope, one of the same
// tenant and workspace; without, one of another tenant or workspace.
func otherPrincipal(l *lab, p *labPrincipal, sameScope bool) *labPrincipal {
	for _, ref := range l.principalRefs() {
		q := l.principals[ref]
		if q.ref == p.ref {
			continue
		}
		if (q.pair == p.pair) == sameScope {
			return q
		}
	}
	return nil
}

// lifecycle follows a source's life through retrieval and citation: its document is replaced
// (the refresh), and then the source is removed (the deletion).
func (r *citeRun) lifecycle(l *lab, it issued) {
	reg := it.reg
	subject := fmt.Sprintf("%s: source %s changes and is removed", it.p.ref, reg.source)
	old := reg.doc
	fresh := synthetic(markerOf(l.suffix, reg.source+"|refreshed", reg.pair))

	if err := l.replace(r.ctx, reg, fresh); err != nil {
		r.unverified(subject, "the index could not take the replacement ("+codeString(err)+")")
		return
	}
	newRes, newErr := l.ask(r.ctx, it.p, fresh.marker)
	oldRes, oldErr := l.ask(r.ctx, it.p, old.marker)
	if newErr != nil || oldErr != nil {
		r.unverified(subject, "retrieval could not answer after the change")
		return
	}
	switch {
	case !foundOwn(newRes.Package, reg):
		r.out.Add(newFinding(fdSourceChange, pathTests, "after the document of source %s was replaced, retrieval did not serve the new text", quote(reg.source)))
		record(r.out, ProbeRAGCitation, subject, "the replaced document is served in its new text", "the new text was not served", false)
	case containsAnyText(oldRes.Package, old.marker):
		r.out.Add(newFinding(fdSourceChange, pathTests, "after the document of source %s was replaced, retrieval still served the old text", quote(reg.source)))
		record(r.out, ProbeRAGCitation, subject, "the replaced document's old text is no longer served", "the old text was still served", false)
	default:
		record(r.out, ProbeRAGCitation, subject, "the replaced document is served in its new text only", "new text served, old text gone", true)
	}
	// A reference issued before the change still names the version that was cited.
	if got, err := l.resv.Resolve(r.ctx, it.p.scope, it.fileID); err == nil && string(got.Version) != it.version {
		r.out.Add(newFinding(fdCitationOriginal, pathTests,
			"after the document of source %s was replaced, an earlier citation resolved to another version than the one it cited", quote(reg.source)))
		record(r.out, ProbeRAGCitation, subject, "an earlier citation keeps naming the version it cited", "it named another version", false)
	}

	if err := l.removeSource(r.ctx, reg); err != nil {
		r.unverified(subject, "the index could not remove the source ("+codeString(err)+")")
		return
	}
	after, aerr := l.ask(r.ctx, it.p, fresh.marker)
	_, rerr := l.resv.Resolve(r.ctx, it.p.scope, it.fileID)
	switch {
	case aerr != nil:
		r.unverified(subject, "retrieval could not answer after the removal")
	case foundOwn(after.Package, reg) || containsAnyText(after.Package, fresh.marker):
		r.out.Add(newFinding(fdSourceChange, pathTests, "after source %s was removed, retrieval still served its content", quote(reg.source)))
		record(r.out, ProbeRAGCitation, subject, "a removed source serves nothing", "its content was still served", false)
	case rerr == nil:
		r.out.Add(newFinding(fdSourceChange, pathTests, "after source %s was removed, an earlier citation of it still resolved", quote(reg.source)))
		record(r.out, ProbeRAGCitation, subject, "a removed source's references resolve no more", "an earlier citation still resolved", false)
	default:
		record(r.out, ProbeRAGCitation, subject, "a removed source serves nothing and its references resolve no more", "nothing served, reference refused", true)
	}
}

// containsAnyText reports whether any item of the package holds the marker in its text.
func containsAnyText(pkg ragc.EvidencePackage, marker string) bool {
	for _, it := range pkg.Items {
		if containsFold(it.Text, marker) {
			return true
		}
	}
	return false
}

// expiry moves the lab's clock past the life of a reference and requires every reference
// issued to have expired for the scope it belongs to.
func (r *citeRun) expiry(l *lab, refs []issued) {
	l.clk.Advance(l.cfg.RefTTL + 1)
	for _, it := range refs {
		subject := fmt.Sprintf("%s cites %s: after the reference's life", it.p.ref, it.reg.source)
		_, err := l.resv.Resolve(r.ctx, it.p.scope, it.fileID)
		switch {
		case err == nil:
			r.out.Add(newFinding(fdCitationExpiry, pathTests,
				"a citation reference for %s on source %s still resolved after its time to live", quote(it.p.ref), quote(it.reg.source)))
			record(r.out, ProbeRAGCitation, subject, "the reference has expired", "it still resolved", false)
		case outage(r.ctx, err):
			r.unverified(subject, "the resolver could not answer ("+codeString(err)+")")
		case !contracts.HasCode(err, contracts.ErrExpired):
			r.out.Add(newFinding(fdCitationExpiry, pathTests,
				"a citation reference for %s on source %s was refused after its time to live with %s, not expired", quote(it.p.ref), quote(it.reg.source), codeString(err)))
			record(r.out, ProbeRAGCitation, subject, "the reference has expired, and says so", "refused ("+codeString(err)+")", false)
		default:
			record(r.out, ProbeRAGCitation, subject, "the reference has expired", "expired", true)
		}
	}
}
