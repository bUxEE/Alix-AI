package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// staleRetriever remembers every non-empty answer and, when the real retrieval finds nothing,
// answers with what it remembers: a retrieval that goes on serving what was removed.
type staleRetriever struct {
	inner Retriever
	mu    sync.Mutex
	seen  map[string]query.Result
}

func newStaleRetriever(in Retriever) Retriever {
	return &staleRetriever{inner: in, seen: map[string]query.Result{}}
}

func (r *staleRetriever) Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error) {
	res, err := r.inner.Query(ctx, scope, req)
	key := scope.PrincipalID + "|" + req.Text
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err == nil && len(res.Package.Items) > 0:
		r.seen[key] = res
	case err == nil:
		if old, ok := r.seen[key]; ok {
			return old, nil
		}
	}
	return res, err
}

// ragPath builds a RAG smoke path for the lab package's suite.
func ragPath(id, principal, utterance string, citations bool, events ...string) ap.PackageTestsSmokePath {
	exp := ap.PackageTestsSmokePathExpect{Citations: &citations}
	for _, e := range events {
		exp.AuditEvents = append(exp.AuditEvents, ap.DottedID(e))
	}
	return ap.PackageTestsSmokePath{ID: ap.Slug(id), Path: ap.PackageTestsSmokePathPathRAG, Principal: ap.Slug(principal), Utterance: utterance, Expect: exp}
}

func withPaths(pkg *apppkg.Package, paths ...ap.PackageTestsSmokePath) {
	pkg.Tests[0].Smoke.Paths = append(pkg.Tests[0].Smoke.Paths, paths...)
}

// TestRAGSmokePath: the RAG path of the runtime smoke test. For every `rag` path of every
// suite the audit retrieves for the path's principal in the disposable scopes, has a grounded
// answer written with citations that resolve for the principal, puts the path's own
// utterance to the same pipeline, checks the audit events the path expects, and removes the
// synthetic sources, proving nothing of them is served afterwards. What it cannot establish is
// unverified, and never appended or assumed.
func TestRAGSmokePath(t *testing.T) {
	t.Run("a correct core completes the paths", func(t *testing.T) {
		pkg := labPackage(t)
		out := runCheck(t, checkOf(t, Options{}, IDSmoke), auditInput(pkg))
		if len(out.Findings) > 0 {
			t.Fatalf("a correct core drew findings:\n%s", dump(out))
		}
		if bad := failedProbes(out); len(bad) > 0 {
			t.Fatalf("failed probes: %+v", bad)
		}
		for _, want := range []string{
			"smoke-rag (rag)", "smoke-rag-no-evidence (rag)", "smoke-rag-no-evidence (rag): the path's own utterance", "cleanup of the synthetic sources",
		} {
			if p := probeNamed(out, want); p == nil || !p.Passed || p.Kind != integrationaudit.ProbeSmoke {
				t.Errorf("probe %q = %+v", want, p)
			}
		}
		// the path that expects no evidence got none: no_supported_evidence, no citations
		if p := probeNamed(out, "smoke-rag-no-evidence (rag): the path's own utterance"); p == nil || !strings.Contains(p.Observed, "no_supported_evidence") {
			t.Errorf("the no-evidence utterance = %+v", p)
		}
		if len(out.Coverage) != 1 || out.Coverage[0].Name != "smoke_paths" || out.Coverage[0].Covered != 2 || out.Coverage[0].Total != 2 {
			t.Errorf("coverage = %+v", out.Coverage)
		}
		// what the audit cannot see is unverified: the events a path expects, and the cited
		// answer to the library's own question, which its synthetic passages cannot give
		for _, want := range []string{
			"audit_event:rag.retrieved (smoke-rag)", "audit_event:turn.completed (smoke-rag)",
			"audit_event:rag.no_supported_evidence (smoke-rag-no-evidence)", "smoke_path:smoke-rag:utterance",
		} {
			if !hasUnverified(out, want) {
				t.Errorf("unverified = %v, want %s", unverifiedSubjects(out), want)
			}
		}
		for _, p := range out.Probes {
			if strings.Contains(p.Subject, "audit event") {
				t.Errorf("an audit event was passed, which no code of this build writes: %+v", p)
			}
		}
	})

	t.Run("the utterance is compared with what the path expects, as far as the corpus allows", func(t *testing.T) {
		pkg := labPackage(t)
		pkg.Tests[0].Smoke.Paths = nil
		withPaths(pkg,
			// a question the synthetic passage answers, and a path that expects a cited answer
			ragPath("answered", "member-a1", "What does the synthetic audit passage state?", true),
			// the same question, where the path expects none: the corpus cannot say whether the
			// application's own documents leave it unanswered, so this is not a failure
			ragPath("expects-nothing", "member-a1", "What does the synthetic audit passage state?", false),
		)
		out := runCheck(t, checkOf(t, Options{}, IDSmoke), auditInput(pkg))
		if len(out.Findings) > 0 {
			t.Fatalf("%s", dump(out))
		}
		if p := probeNamed(out, "answered (rag): the path's own utterance"); p == nil || !p.Passed || !strings.Contains(p.Observed, "citation") {
			t.Errorf("a cited answer to the utterance = %+v", p)
		}
		if !hasUnverified(out, "smoke_path:expects-nothing:utterance") {
			t.Errorf("unverified = %v", unverifiedSubjects(out))
		}
		// no expectation about citations: nothing to compare
		pkg.Tests[0].Smoke.Paths[0].Expect.Citations = nil
		out = runCheck(t, checkOf(t, Options{}, IDSmoke), auditInput(pkg))
		if !hasUnverified(out, "smoke_path:answered:utterance") {
			t.Errorf("unverified = %v", unverifiedSubjects(out))
		}
	})

	faults := []struct {
		name    string
		hooks   Hooks
		finding string
	}{
		{"retrieval binds the package to another scope", Hooks{Retrieve: func(in Retriever) Retriever { return wrongScopeRetriever{inner: in} }}, fdPackageScope.id},
		{"retrieval finds nothing of the principal's own passage", Hooks{Retrieve: func(in Retriever) Retriever { return emptyRetriever{inner: in} }}, fdSmokePath.id},
		{"the answer carries no citation", Hooks{Answer: newAnswererFault("uncited")}, fdSmokePath.id},
		{"the answer step finds no supported evidence", Hooks{Answer: newAnswererFault("no-evidence")}, fdSmokePath.id},
		{"a citation does not resolve for the principal", Hooks{Resolve: newResolverFault("refuses-everybody")}, fdSmokePath.id},
		{"removed sources are still served", Hooks{Retrieve: newStaleRetriever}, fdCleanup.id},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			pkg := labPackage(t)
			out := runCheck(t, checkOf(t, Options{Hooks: tc.hooks}, IDSmoke), auditInput(pkg))
			if !hasFinding(out, tc.finding) || !hasCritical(out) {
				t.Fatalf("finding %s not raised:\n%s", tc.finding, dump(out))
			}
			if len(failedProbes(out)) == 0 && tc.finding != fdPackageScope.id {
				t.Errorf("no probe is recorded as failed:\n%s", dump(out))
			}
		})
	}

	t.Run("a path the audit cannot run is unverified, never passed", func(t *testing.T) {
		pkg := labPackage(t)
		pkg.Tests[0].Smoke.Paths = nil
		withPaths(pkg, ragPath("stranger", "nobody", "Anything?", true))
		out := runCheck(t, checkOf(t, Options{}, IDSmoke), auditInput(pkg))
		if len(out.Findings) > 0 || !hasUnverified(out, "smoke_path:stranger") {
			t.Fatalf("%s", dump(out))
		}
		if len(out.Coverage) != 1 || out.Coverage[0].Covered != 0 || out.Coverage[0].Total != 1 {
			t.Errorf("coverage = %+v", out.Coverage)
		}
	})

	t.Run("sources the audit cannot host leave every path unverified", func(t *testing.T) {
		pkg := loadRef(t, validMinimalDir)
		out := runCheck(t, checkOf(t, Options{}, IDSmoke), auditInput(pkg))
		if len(out.Findings) > 0 || len(out.Probes) > 0 {
			t.Fatalf("nothing could be run:\n%s", dump(out))
		}
		for _, id := range []string{"smoke-rag", "smoke-rag-no-evidence"} {
			if !hasUnverified(out, "smoke_path:"+id) {
				t.Errorf("unverified = %v, want smoke_path:%s", unverifiedSubjects(out), id)
			}
		}
	})

	t.Run("a lab that cannot start is a warning, and every path is unverified", func(t *testing.T) {
		pkg := labPackage(t)
		notADir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := runCheck(t, checkOf(t, Options{ScratchDir: notADir}, IDSmoke), auditInput(pkg))
		if got := findingIDs(out, contracts.SeverityWarning); len(got) != 1 || got[0] != fdLabUnavailable.id || hasCritical(out) {
			t.Fatalf("%s", dump(out))
		}
		for _, id := range []string{"smoke-rag", "smoke-rag-no-evidence"} {
			if !hasUnverified(out, "smoke_path:"+id) {
				t.Errorf("unverified = %v, want smoke_path:%s", unverifiedSubjects(out), id)
			}
		}
	})

	t.Run("the throwaway store is removed, even when a probe panics", func(t *testing.T) {
		scratch := t.TempDir()
		pkg := labPackage(t)
		o := Options{ScratchDir: scratch, Hooks: Hooks{Retrieve: func(in Retriever) Retriever { return panicRetriever{} }}}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("the panic was swallowed")
				}
			}()
			_, _ = checkOf(t, o, IDSmoke).Run(context.Background(), auditInput(pkg))
		}()
		entries, err := readDirNames(scratch)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("the scratch directory still holds %v", entries)
		}
	})
}

type panicRetriever struct{}

func (panicRetriever) Query(context.Context, contracts.Scope, query.Request) (query.Result, error) {
	panic("a retrieval that panics")
}
