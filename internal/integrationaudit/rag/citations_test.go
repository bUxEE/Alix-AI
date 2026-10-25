package rag

import (
	"context"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// resolverFault is a citation resolver with one chosen fault.
type resolverFault struct {
	inner Resolver
	mode  string

	mu     sync.Mutex
	issuer map[actions.FileID]contracts.Scope
	last   map[string]answer.Resolution
}

func newResolverFault(mode string) func(Resolver) Resolver {
	return func(in Resolver) Resolver {
		return &resolverFault{inner: in, mode: mode, issuer: map[actions.FileID]contracts.Scope{}, last: map[string]answer.Resolution{}}
	}
}

func (r *resolverFault) Resolve(ctx context.Context, scope contracts.Scope, id actions.FileID) (answer.Resolution, error) {
	got, err := r.inner.Resolve(ctx, scope, id)
	r.mu.Lock()
	owner, issued := r.issuer[id]
	if err == nil && !issued {
		r.issuer[id] = scope
	}
	key := scope.PrincipalID + "|" + string(id)
	cached, hadCache := r.last[key]
	if err == nil {
		r.last[key] = got
	}
	r.mu.Unlock()

	switch r.mode {
	case "answers-anybody": // a reference resolves for whoever presents it
		if err != nil && issued {
			return r.inner.Resolve(ctx, owner, id)
		}
	case "oracle": // a reference of another scope is refused differently from an unknown one
		if err != nil && issued && owner.PrincipalID != scope.PrincipalID && contracts.HasCode(err, contracts.ErrPermissionDenied) {
			return answer.Resolution{}, contracts.New(contracts.ErrPermissionDenied, "this reference belongs to another principal")
		}
	case "never-expires":
		if err != nil && hadCache && contracts.HasCode(err, contracts.ErrExpired) {
			return cached, nil
		}
	case "survives-revocation":
		if err != nil && hadCache && contracts.HasCode(err, contracts.ErrPermissionDenied) {
			return cached, nil
		}
	case "wrong-version":
		if err == nil {
			got.Version = "sha256:not-the-original"
		}
	case "wrong-preview":
		if err == nil {
			got.Preview = "text that is nowhere in the original document"
		}
	case "refuses-everybody":
		return answer.Resolution{}, contracts.New(contracts.ErrPermissionDenied, "refused")
	case "outage":
		return answer.Resolution{}, contracts.New(contracts.ErrUnavailable, "the catalog is down at /srv/secret/catalog.db")
	}
	return got, err
}

// answererFault is an answer step with one chosen fault.
type answererFault struct {
	inner Answerer
	mode  string
}

func newAnswererFault(mode string) func(Answerer) Answerer {
	return func(in Answerer) Answerer { return answererFault{inner: in, mode: mode} }
}

func (a answererFault) Answer(ctx context.Context, scope contracts.Scope, pkg ragc.EvidencePackage) (answer.Result, error) {
	res, err := a.inner.Answer(ctx, scope, pkg)
	if err != nil {
		return res, err
	}
	ga, grounded := res.Answer.Value.(ragc.GroundedAnswer)
	switch a.mode {
	case "uncited":
		if grounded {
			ga.Citations = nil
			for i := range ga.Claims {
				ga.Claims[i].CitationIndexes = nil
			}
			res.Answer = ragc.AnswerResult{Value: ga}
		}
	case "no-evidence":
		res.Answer = ragc.AnswerResult{Value: ragc.NoSupportedEvidence{Outcome: "no_supported_evidence", Reason: ragc.NoSupportedEvidenceReasonClaimsUnsupported}}
	case "path-reference":
		if grounded {
			for i := range ga.Citations {
				ga.Citations[i].OpenRef = conversation.CitationOpenRef{Value: conversation.CitationOpenRefFile{Kind: "file", FileID: "fil_../../etc/passwd"}}
			}
			res.Answer = ragc.AnswerResult{Value: ga}
		}
	case "source-reference":
		if grounded {
			for i := range ga.Citations {
				ga.Citations[i].OpenRef = conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}}
			}
			res.Answer = ragc.AnswerResult{Value: ga}
		}
	}
	return res, nil
}

// TestCitationResolverProbe: every citation of a grounded answer is held to what 12.5
// requires: an opaque reference that resolves, permission-checked, for the principal it was
// written for, to the source, version and passage of the original; that refuses everybody
// else with one and the same refusal; that stops when the source is revoked or removed; and
// that expires. A correct core passes, and each fault of the resolver or the answer step is
// found by the probe that exists for it.
func TestCitationResolverProbe(t *testing.T) {
	t.Run("a correct core passes every probe", func(t *testing.T) {
		pkg := labPackage(t)
		out := runCheck(t, checkOf(t, Options{}, IDCitations), auditInput(pkg))
		if len(out.Findings) > 0 || len(out.Unverified) > 0 {
			t.Fatalf("a correct core drew findings or unverified items:\n%s", dump(out))
		}
		if bad := failedProbes(out); len(bad) > 0 {
			t.Fatalf("failed probes: %+v", bad)
		}
		// three principals cite their own passage: each citation is held to the original,
		// refused for another scope and for a reference that never existed, and refused once
		// the source is revoked; one source's life is followed, and every reference expires
		if len(out.Coverage) != 1 || out.Coverage[0].Name != "rag_citations" || out.Coverage[0].Covered != 3 || out.Coverage[0].Total != 3 {
			t.Errorf("coverage = %+v", out.Coverage)
		}
		for _, want := range []string{
			"member-a1 cites loan-policies (citation 1)", "member-a1: source loan-policies changes and is removed",
			"visitor-b1 cites loan-policies: after the reference's life",
		} {
			if probeNamed(out, want) == nil {
				t.Errorf("no probe %q:\n%s", want, dump(out))
			}
		}
	})

	faults := []struct {
		name     string
		hooks    Hooks
		finding  string
		critical bool
	}{
		{"a reference resolves for anybody", Hooks{Resolve: newResolverFault("answers-anybody")}, fdCitationForeign.id, true},
		{"a foreign reference is refused differently from an unknown one", Hooks{Resolve: newResolverFault("oracle")}, fdCitationOracle.id, false},
		{"a reference never expires", Hooks{Resolve: newResolverFault("never-expires")}, fdCitationExpiry.id, true},
		{"a reference survives the revocation of its source", Hooks{Resolve: newResolverFault("survives-revocation")}, fdCitationRevoked.id, true},
		{"a reference resolves to another version", Hooks{Resolve: newResolverFault("wrong-version")}, fdCitationOriginal.id, true},
		{"a reference resolves to a passage that is not in the original", Hooks{Resolve: newResolverFault("wrong-preview")}, fdCitationOriginal.id, true},
		{"claims without citations", Hooks{Answer: newAnswererFault("uncited")}, fdCitationMissing.id, true},
		{"no cited answer for a passage retrieval just returned", Hooks{Answer: newAnswererFault("no-evidence")}, fdCitationMissing.id, true},
		{"a reference that is a path", Hooks{Answer: newAnswererFault("path-reference")}, fdCitationOpaque.id, true},
		{"a reference that is not a file reference", Hooks{Answer: newAnswererFault("source-reference")}, fdCitationOpaque.id, true},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			pkg := labPackage(t)
			out := runCheck(t, checkOf(t, Options{Hooks: tc.hooks}, IDCitations), auditInput(pkg))
			if !hasFinding(out, tc.finding) {
				t.Fatalf("finding %s not raised:\n%s", tc.finding, dump(out))
			}
			if tc.critical && !hasCritical(out) {
				t.Errorf("the fault must block activation:\n%s", dump(out))
			}
			if !tc.critical && hasCritical(out) {
				t.Errorf("the fault is a warning:\n%s", dump(out))
			}
			if len(failedProbes(out)) == 0 && tc.finding != fdCitationOpaque.id {
				t.Errorf("no probe is recorded as failed:\n%s", dump(out))
			}
		})
	}

	t.Run("a resolver that cannot answer is unverified, never a pass or a finding", func(t *testing.T) {
		pkg := labPackage(t)
		out := runCheck(t, checkOf(t, Options{Hooks: Hooks{Resolve: newResolverFault("outage")}}, IDCitations), auditInput(pkg))
		if len(out.Findings) > 0 {
			t.Fatalf("an outage is not a finding:\n%s", dump(out))
		}
		if len(out.Unverified) == 0 {
			t.Fatalf("nothing is recorded as unverified:\n%s", dump(out))
		}
		if len(out.Coverage) > 0 && out.Coverage[0].Covered != 0 {
			t.Errorf("citations were counted as resolved: %+v", out.Coverage)
		}
	})

	t.Run("sources the audit cannot host are unverified", func(t *testing.T) {
		pkg := loadRef(t, validMinimalDir) // its source is shipped inside the package
		out := runCheck(t, checkOf(t, Options{}, IDCitations), auditInput(pkg))
		if len(out.Findings) > 0 || len(out.Probes) > 0 {
			t.Fatalf("nothing could be probed, so nothing can be found or passed:\n%s", dump(out))
		}
		if !hasUnverified(out, "rag_citation:source:loan-policies") {
			t.Errorf("unverified = %v", unverifiedSubjects(out))
		}
	})
}
