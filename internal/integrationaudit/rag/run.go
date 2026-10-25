package rag

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// retrieveProbes lists the retrieval scope probes of a suite, in the suite's order.
func retrieveProbes(suite ap.PackageTests) []ap.PackageTestsScopeProbe {
	var out []ap.PackageTestsScopeProbe
	for _, p := range suite.ScopeProbes {
		if p.Operation == ap.PackageTestsScopeProbeOperationRetrieve {
			out = append(out, p)
		}
	}
	return out
}

// ragSmokePaths lists the RAG smoke paths of a suite, in the suite's order.
func ragSmokePaths(suite ap.PackageTests) []ap.PackageTestsSmokePath {
	var out []ap.PackageTestsSmokePath
	for _, p := range suite.Smoke.Paths {
		if p.Path == ap.PackageTestsSmokePathPathRAG {
			out = append(out, p)
		}
	}
	return out
}

// unverify records something a check of the given section could not look at.
func unverify(out *integrationaudit.Outcome, section int, subject, reason string) {
	out.Unverify(integrationaudit.Unverified{
		Subject: subject, Section: section, Target: integrationaudit.TargetApplication, Reason: reason,
	})
}

// record adds a probe result to an outcome.
func record(out *integrationaudit.Outcome, kind integrationaudit.ProbeKind, subject, expected, observed string, passed bool) {
	out.Probes = append(out.Probes, integrationaudit.ProbeResult{
		Kind: kind, Subject: subject, Expected: expected, Observed: observed, Passed: passed,
	})
}

// withLab builds the lab of a suite, runs body in it and removes it, whatever body does
// (a panic included: the deferred removal runs before the panic moves on). When the lab
// cannot be built the check says so with a warning and calls unavailable with the reason,
// so that every probe it owned is recorded as unverified: a probe that did not run is never
// absent from the report.
func withLab(ctx context.Context, o Options, in *integrationaudit.Input, suite ap.PackageTests, hosted []labSource,
	out *integrationaudit.Outcome, unavailable func(reason string), body func(l *lab)) {
	l, err := newLab(ctx, o, in, suite, hosted)
	if err != nil {
		reason := "the throwaway RAG index of suite " + quote(string(suite.Suite)) + " could not be started: " + err.Error()
		if ctx.Err() != nil {
			reason = "the audit was cancelled before the throwaway RAG index of suite " + quote(string(suite.Suite)) + " was ready"
		} else {
			out.Add(newFinding(fdLabUnavailable, pathTests, "the throwaway RAG index of suite %s could not be started (%s)", quote(string(suite.Suite)), err.Error()))
		}
		unavailable(reason)
		return
	}
	defer func() {
		if err := l.close(); err != nil {
			out.Add(newFinding(fdScratchLeft, pathTests, "the throwaway store of suite %s could not be removed", quote(string(suite.Suite))))
		}
	}()
	body(l)
}

// outage reports whether err says the answer could not be established (the audit was
// cancelled, something timed out, ran out of resources or is unavailable, or failed
// internally) rather than that the request was refused.
func outage(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch contracts.CodeOf(err) {
	case contracts.ErrUnavailable, contracts.ErrTimeout, contracts.ErrResourceExhausted, contracts.ErrInternal, "":
		return true
	}
	return false
}

// refusal reports whether err is the core refusing the request for a reason of scope or
// permission: nothing was disclosed, which is the outcome a probe of another scope wants.
func refusal(err error) bool {
	switch contracts.CodeOf(err) {
	case contracts.ErrScopeViolation, contracts.ErrPermissionDenied, contracts.ErrScopeMissing, contracts.ErrValidationFailed:
		return true
	}
	return false
}

// noHostable describes, in one phrase, why no declared source could be hosted.
func noHostable(declared int, reasons map[string]string) string {
	if declared == 0 {
		return "the package declares no RAG source"
	}
	return "none of the package's declared RAG sources can be hosted by the audit (a source shipped in the package or read through an API needs a provider that is not part of this build)"
}

// verdict is what one retrieval of a probe showed.
type verdict int

const (
	verdictHeld       verdict = iota // nothing of another scope, and the own passage when it was asked for
	verdictFailed                    // a finding was raised
	verdictNotShown                  // the answer could not be established
	verdictOwnMissing                // the own passage was not found
)

// checkEvidence inspects an evidence package a principal's retrieval returned: the package
// must be the principal's, and nothing of another scope may be in it.
func checkEvidence(out *integrationaudit.Outcome, l *lab, p *labPrincipal, what string, pkg ragc.EvidencePackage) verdict {
	if !samePackageScope(pkg, p.scope) {
		out.Add(newFinding(fdPackageScope, pathTests, "the evidence package of %s was bound to another scope than the principal that asked (%s)", quote(p.ref), what))
		return verdictFailed
	}
	leaked, err := leaks(pkg, l.foreign(p))
	if err != nil {
		return verdictNotShown
	}
	if leaked {
		out.Add(newFinding(fdChunkLeak, pathTests, "retrieval for %s leaked a chunk, source or identifier of another scope into the evidence package (%s)", quote(p.ref), what))
		return verdictFailed
	}
	return verdictHeld
}

func samePackageScope(pkg ragc.EvidencePackage, s contracts.Scope) bool {
	ps := pkg.Scope
	return string(ps.ApplicationID) == s.ApplicationID && string(ps.TenantID) == s.TenantID &&
		string(ps.WorkspaceID) == s.WorkspaceID && string(ps.PrincipalID) == s.PrincipalID
}

// foundOwn reports whether the package holds the registration's passage.
func foundOwn(pkg ragc.EvidencePackage, r *labReg) bool {
	for _, it := range pkg.Items {
		if string(it.SourceID) == r.srcID && containsFold(it.Text, r.doc.marker) {
			return true
		}
	}
	return false
}

// noPackage records that a check could not look at anything because no package was loaded.
// The framework does not run a check without a package (it records the check as unverified
// itself); this is for a check called by something that does.
func noPackage(out *integrationaudit.Outcome, section int, subject string) {
	unverify(out, section, subject, "the package could not be loaded and verified, so its RAG sources were not examined")
}
