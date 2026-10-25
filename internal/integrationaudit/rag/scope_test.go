package rag

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// leakyRetriever is a retrieval with a fault: it answers a principal's question for the
// principal's scope, and also for every other scope it has been asked in, merging what the
// other scopes find into the principal's evidence package. It is the cross-scope leak the
// probes exist to find.
type leakyRetriever struct {
	inner Retriever
	mu    sync.Mutex
	seen  []contracts.Scope
}

func (r *leakyRetriever) Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error) {
	res, err := r.inner.Query(ctx, scope, req)
	if err != nil {
		return res, err
	}
	r.mu.Lock()
	others := append([]contracts.Scope(nil), r.seen...)
	known := false
	for _, s := range r.seen {
		known = known || keyOf(s) == keyOf(scope)
	}
	if !known {
		r.seen = append(r.seen, scope)
	}
	r.mu.Unlock()
	for _, o := range others {
		if keyOf(o) == keyOf(scope) {
			continue
		}
		if more, err := r.inner.Query(ctx, o, req); err == nil {
			res.Package.Items = append(res.Package.Items, more.Package.Items...)
		}
	}
	return res, nil
}

// wrongScopeRetriever answers with an evidence package bound to another tenant.
type wrongScopeRetriever struct{ inner Retriever }

func (r wrongScopeRetriever) Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error) {
	res, err := r.inner.Query(ctx, scope, req)
	res.Package.Scope.TenantID = "tnt_somebody_else"
	return res, err
}

// withProbes adds scope probes to the lab package's suite.
func withProbes(pkg *apppkg.Package, probes ...ap.PackageTestsScopeProbe) {
	pkg.Tests[0].ScopeProbes = append(pkg.Tests[0].ScopeProbes, probes...)
}

func retrieveProbe(id, principal, source string, dim ap.PackageTestsScopeProbeDimension, expect ap.PackageTestsScopeProbeExpect) ap.PackageTestsScopeProbe {
	src := ap.Slug(source)
	p := ap.PackageTestsScopeProbe{
		ID: ap.Slug(id), Dimension: dim, Operation: ap.PackageTestsScopeProbeOperationRetrieve,
		Principal: ap.Slug(principal), TargetSource: &src, Expect: expect,
	}
	if expect == ap.PackageTestsScopeProbeExpectDeny {
		code := ap.ErrorCode("permission_denied")
		p.ErrorCode = &code
	}
	return p
}

// TestCrossScopeChunkProbe: the chunk-level cross-scope probes run against the core's own
// index and query pipeline over synthetic passages in the suite's tenants and workspaces. A
// correct core passes every probe; a retrieval that lets a chunk of another scope into a
// principal's evidence package is found and is critical, so is one that binds the package to
// another scope, and the suite's own retrieve probes are executed (allow and deny).
func TestCrossScopeChunkProbe(t *testing.T) {
	t.Run("a correct core passes every probe", func(t *testing.T) {
		pkg := labPackage(t)
		withProbes(pkg, retrieveProbe("visitor-denied-network-a", "visitor-b1", "loan-policies", ap.PackageTestsScopeProbeDimensionTenant, ap.PackageTestsScopeProbeExpectDeny))
		out := runCheck(t, checkOf(t, Options{}, IDScope), auditInput(pkg))
		if len(out.Findings) > 0 || len(out.Unverified) > 0 {
			t.Fatalf("a correct core drew findings or unverified items:\n%s", dump(out))
		}
		if bad := failedProbes(out); len(bad) > 0 {
			t.Fatalf("failed probes: %+v", bad)
		}
		// three fixture principals in three scopes: each reads its own passage and tries the
		// other two scopes' passages three ways; the suite's allow probe adds a positive one, and
		// its deny probe tries the source's two other scopes two ways and adds its own verdict
		var positive, negative int
		for _, p := range out.Probes {
			switch p.Kind {
			case integrationaudit.ProbePositiveScope:
				positive++
			case integrationaudit.ProbeNegativeScope:
				negative++
			}
		}
		if positive != 3+1 || negative != 3*2*3+2*2+1 {
			t.Errorf("positive = %d, negative = %d, want 4 and 23", positive, negative)
		}
		// another workspace of the same tenant is another scope
		if probeNamed(out, "member-a1 asks for the passage of network-a/branch-2") == nil {
			t.Errorf("the other workspace of the same tenant was not probed")
		}
		if probeNamed(out, "member-retrieves-policies: member-a1 retrieves from loan-policies") == nil ||
			probeNamed(out, "visitor-denied-network-a: visitor-b1 retrieves from loan-policies") == nil {
			t.Errorf("the suite's own retrieve probes were not executed:\n%s", dump(out))
		}
		if len(out.Coverage) != 1 || out.Coverage[0].Name != "rag_scope_probes" || out.Coverage[0].Covered != 2 || out.Coverage[0].Total != 2 {
			t.Errorf("coverage = %+v", out.Coverage)
		}
	})

	t.Run("a chunk of another scope reaches a package", func(t *testing.T) {
		pkg := labPackage(t)
		withProbes(pkg, retrieveProbe("visitor-denied-network-a", "visitor-b1", "loan-policies", ap.PackageTestsScopeProbeDimensionTenant, ap.PackageTestsScopeProbeExpectDeny))
		o := Options{Hooks: Hooks{Retrieve: func(in Retriever) Retriever { return &leakyRetriever{inner: in} }}}
		out := runCheck(t, checkOf(t, o, IDScope), auditInput(pkg))
		if !hasFinding(out, fdChunkLeak.id) || !hasCritical(out) {
			t.Fatalf("a cross-scope leak went unnoticed:\n%s", dump(out))
		}
		var failedNegative int
		for _, p := range failedProbes(out) {
			if p.Kind == integrationaudit.ProbeNegativeScope {
				failedNegative++
			}
		}
		if failedNegative == 0 {
			t.Errorf("no negative probe is recorded as failed:\n%s", dump(out))
		}
		// what leaked is never quoted: no marker, no generated identifier
		for _, f := range out.Findings {
			for _, bad := range []string{"mk", "src_", "gen_", "tnt_", "usr_"} {
				if strings.Contains(f.Message, bad) {
					t.Errorf("finding %q carries %q", f.Message, bad)
				}
			}
		}
		// the suite's deny probe is among those that failed
		if p := probeNamed(out, "visitor-denied-network-a"); p == nil || p.Passed {
			t.Errorf("the deny probe = %+v, want a failed one", p)
		}
	})

	t.Run("a package bound to another scope", func(t *testing.T) {
		pkg := labPackage(t)
		o := Options{Hooks: Hooks{Retrieve: func(in Retriever) Retriever { return wrongScopeRetriever{inner: in} }}}
		out := runCheck(t, checkOf(t, o, IDScope), auditInput(pkg))
		if !hasFinding(out, fdPackageScope.id) || !hasCritical(out) {
			t.Fatalf("a package of another scope went unnoticed:\n%s", dump(out))
		}
	})

	t.Run("the principal's own passage must be found", func(t *testing.T) {
		pkg := labPackage(t)
		o := Options{Hooks: Hooks{Retrieve: func(in Retriever) Retriever { return emptyRetriever{inner: in} }}}
		out := runCheck(t, checkOf(t, o, IDScope), auditInput(pkg))
		if !hasFinding(out, fdOwnChunkMissing.id) || !hasCritical(out) {
			t.Fatalf("a retrieval that finds nothing proves no isolation, and was passed:\n%s", dump(out))
		}
	})

	t.Run("a probe that cannot be run is unverified, never passed", func(t *testing.T) {
		pkg := labPackage(t)
		withProbes(pkg,
			retrieveProbe("role-probe", "member-a1", "loan-policies", ap.PackageTestsScopeProbeDimensionRole, ap.PackageTestsScopeProbeExpectDeny),
			retrieveProbe("document-probe", "member-a1", "loan-policies", ap.PackageTestsScopeProbeDimensionDocument, ap.PackageTestsScopeProbeExpectDeny),
			retrieveProbe("stranger-probe", "nobody", "loan-policies", ap.PackageTestsScopeProbeDimensionSource, ap.PackageTestsScopeProbeExpectAllow),
			retrieveProbe("ghost-probe", "member-a1", "no-such-source", ap.PackageTestsScopeProbeDimensionSource, ap.PackageTestsScopeProbeExpectAllow),
		)
		out := runCheck(t, checkOf(t, Options{}, IDScope), auditInput(pkg))
		if len(out.Findings) > 0 {
			t.Fatalf("probes that could not run drew findings:\n%s", dump(out))
		}
		for _, id := range []string{"role-probe", "document-probe", "stranger-probe", "ghost-probe"} {
			if !hasUnverified(out, "scope_probe:"+id) {
				t.Errorf("unverified = %v, want scope_probe:%s", unverifiedSubjects(out), id)
			}
		}
		if c := out.Coverage[0]; c.Covered != 1 || c.Total != 5 {
			t.Errorf("coverage = %+v, want 1 of 5", c)
		}
	})

	t.Run("a source bound to a fixed tenant is not something a disposable tenant can be", func(t *testing.T) {
		pkg := labPackage(t)
		root := folderRoot(pkg)
		pkg.Sources["loan-policies"] = folderSource(t, root, bindFixed("network-a"), bindShared)
		out := runCheck(t, checkOf(t, Options{}, IDScope), auditInput(pkg))
		if len(out.Findings) > 0 {
			t.Fatalf("%s", dump(out))
		}
		if !hasUnverified(out, "scope_probe:member-retrieves-policies") || !hasUnverified(out, "rag_scope:source:loan-policies") {
			t.Errorf("unverified = %v", unverifiedSubjects(out))
		}
	})

	t.Run("the sweep is bounded, and what is cut is unverified", func(t *testing.T) {
		pkg := labPackage(t)
		out := runCheck(t, checkOf(t, Options{MaxSweep: 4}, IDScope), auditInput(pkg))
		if !hasUnverified(out, "rag_scope:sweep") {
			t.Fatalf("unverified = %v", unverifiedSubjects(out))
		}
		if len(out.Findings) > 0 {
			t.Fatalf("%s", dump(out))
		}
	})

	t.Run("the audit does not leave its store behind", func(t *testing.T) {
		scratch := t.TempDir()
		pkg := labPackage(t)
		runCheck(t, checkOf(t, Options{ScratchDir: scratch}, IDScope), auditInput(pkg))
		entries, err := readDirNames(scratch)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("the scratch directory still holds %v", entries)
		}
	})
}

// emptyRetriever finds nothing for anybody.
type emptyRetriever struct{ inner Retriever }

func (r emptyRetriever) Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error) {
	res, err := r.inner.Query(ctx, scope, req)
	res.Package.Items = nil
	return res, err
}
