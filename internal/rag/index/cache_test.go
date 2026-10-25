package index

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-022: every cache key includes the application, tenant, workspace and
// principal scope and the source version. The same query under a different
// scope, or after a source update, never produces the earlier key.
func TestCacheKeysIncludeScopeAndVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base := f.scope("app", "t1", "w1")
	src := f.source()
	gen := f.serving(base, src, spec{"d", "Notes about the harbour crane."}, spec{"e", "Notes about the quay."})
	q := Query{Text: "harbour crane", Limit: 5}

	res := f.search(base, q)
	key := func(scope contracts.Scope, ns string, v Version, material ...string) string {
		t.Helper()
		k, err := CacheKey(scope, ns, v, material...)
		if err != nil {
			t.Fatalf("CacheKey: %v", err)
		}
		return k
	}
	k0 := key(base, "retrieval", res.Version, q.Material()...)
	if !strings.HasPrefix(k0, "rik1_") || len(k0) != len("rik1_")+64 {
		t.Errorf("key shape: %q", k0)
	}
	if k0 != key(base, "retrieval", res.Version, q.Material()...) {
		t.Error("the key is not deterministic")
	}

	// Every part of the authorisation scope is part of the key.
	vary := map[string]func(s *contracts.Scope){
		"application":      func(s *contracts.Scope) { s.ApplicationID = "app2" },
		"tenant":           func(s *contracts.Scope) { s.TenantID = "t2" },
		"workspace":        func(s *contracts.Scope) { s.WorkspaceID = "w2" },
		"principal":        func(s *contracts.Scope) { s.PrincipalID = "usr_other" },
		"role added":       func(s *contracts.Scope) { s.Roles = []string{"member", "admin"} },
		"role changed":     func(s *contracts.Scope) { s.Roles = []string{"viewer"} },
		"capability added": func(s *contracts.Scope) { s.CapabilityScopes = []string{"test.read", "test.write"} },
	}
	seen := map[string]string{k0: "base"}
	for name, mutate := range vary {
		s := base
		s.Roles = append([]string(nil), base.Roles...)
		s.CapabilityScopes = append([]string(nil), base.CapabilityScopes...)
		mutate(&s)
		k := key(s, "retrieval", res.Version, q.Material()...)
		if prev, dup := seen[k]; dup {
			t.Errorf("%s gives the same key as %s", name, prev)
		}
		seen[k] = name
	}
	// The session carries no authority and is not part of the key; the order of
	// roles is not either.
	s := base
	s.SessionID = "ses_another"
	s.Roles = []string{"member"}
	if key(s, "retrieval", res.Version, q.Material()...) != k0 {
		t.Error("the key depends on the session id")
	}
	two := base
	two.Roles = []string{"b", "a"}
	two2 := base
	two2.Roles = []string{"a", "b"}
	if key(two, "ns", res.Version) != key(two2, "ns", res.Version) {
		t.Error("the key depends on the order of roles")
	}

	// The namespace and the material are part of it, and fields cannot be
	// shifted into each other.
	if key(base, "evidence", res.Version, q.Material()...) == k0 {
		t.Error("namespace is not part of the key")
	}
	if key(base, "retrieval", res.Version, Query{Text: "quay", Limit: 5}.Material()...) == k0 {
		t.Error("the query is not part of the key")
	}
	if key(base, "retrieval", res.Version, Query{Text: "harbour crane", Limit: 6}.Material()...) == k0 {
		t.Error("the query options are not part of the key")
	}
	if key(base, "retrieval", res.Version, Query{Text: "harbour crane", Limit: 5, IncludeHidden: true}.Material()...) == k0 {
		t.Error("IncludeHidden is not part of the key")
	}
	if key(base, "n", res.Version, "ab", "c") == key(base, "n", res.Version, "a", "bc") {
		t.Error("material fields can be shifted into each other")
	}
	shifted1, shifted2 := base, base
	shifted1.TenantID, shifted1.WorkspaceID = "a", "bc"
	shifted2.TenantID, shifted2.WorkspaceID = "ab", "c"
	if key(shifted1, "n", res.Version) == key(shifted2, "n", res.Version) {
		t.Error("scope fields can be shifted into each other")
	}
	if _, err := CacheKey(contracts.Scope{}, "retrieval", res.Version); err == nil {
		t.Error("a key was built for an invalid scope")
	}
	if _, err := CacheKey(base, "", res.Version); err == nil {
		t.Error("a key was built without a namespace")
	}

	// The version: any change to what the scope can see gives a new key, so a
	// cache entry made before the change is never hit again.
	var cache = map[string]int{}
	store := func(r Result) { cache[key(base, "retrieval", r.Version, q.Material()...)] = len(r.Hits) }
	lookup := func() (int, bool) {
		v, err := f.ix.Version(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		n, ok := cache[key(base, "retrieval", v, q.Material()...)]
		return n, ok
	}
	store(res)
	if n, ok := lookup(); !ok || n != len(res.Hits) {
		t.Fatalf("the cache missed for an unchanged index (%d, %v)", n, ok)
	}
	stage := func(name string, mutate func()) {
		t.Helper()
		before, err := f.ix.Version(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		mutate()
		after, err := f.ix.Version(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		if key(base, "retrieval", before, q.Material()...) == key(base, "retrieval", after, q.Material()...) {
			t.Errorf("%s did not change the cache key (version %+v -> %+v)", name, before, after)
		}
		if _, ok := lookup(); ok {
			t.Errorf("%s: a stale entry would still be served", name)
		}
		store(f.search(base, q))
	}
	id := string(gen.GenerationID)
	stage("updating a document in the active generation", func() {
		if _, err := f.ix.PutDocument(ctx, base, id, f.doc("d", "v2", "Notes about the harbour crane, revised.")); err != nil {
			t.Fatal(err)
		}
	})
	stage("deleting a document", func() {
		if _, err := f.ix.DeleteDocument(ctx, base, src, "e"); err != nil {
			t.Fatal(err)
		}
	})
	stage("activating a new generation", func() {
		g2 := f.validated(base, src, spec{"d", "Notes about the harbour crane, rebuilt."})
		if err := f.ix.ActivateGeneration(ctx, base, string(g2.GenerationID)); err != nil {
			t.Fatal(err)
		}
	})
	stage("invalidating the generation", func() {
		a, ok, err := f.ix.ActiveGeneration(ctx, base, src)
		if err != nil || !ok {
			t.Fatal("no active generation", err)
		}
		if err := f.ix.InvalidateGeneration(ctx, base, string(a.GenerationID), nil); err != nil {
			t.Fatal(err)
		}
	})
	src2 := f.source()
	stage("a new source going live", func() {
		f.serving(base, src2, spec{"z", "Notes about the harbour crane in the annex."})
	})
	stage("deleting a source", func() {
		if _, err := f.ix.DeleteSource(ctx, base, src2); err != nil {
			t.Fatal(err)
		}
	})

	// A re-put of an unchanged document is not a change.
	kept := f.serving(base, f.source(), spec{"k", "kept as is"})
	vBefore, _ := f.ix.Version(ctx, base)
	if _, err := f.ix.PutDocument(ctx, base, string(kept.GenerationID), f.doc("k", "v1", "kept as is")); err != nil {
		t.Fatal(err)
	}
	if vAfter, _ := f.ix.Version(ctx, base); vAfter.Epoch != vBefore.Epoch {
		t.Error("an unchanged put moved the version")
	}

	// Changes in another scope do not touch this scope's version: no needless
	// invalidation across tenants (and no information about them).
	vBefore, _ = f.ix.Version(ctx, base)
	f.serving(f.scope("app", "t-other", "w1"), f.source(), spec{"o", "somebody else's notes"})
	if vAfter, _ := f.ix.Version(ctx, base); vAfter.Epoch != vBefore.Epoch || len(vAfter.Active) != len(vBefore.Active) {
		t.Errorf("another scope's change moved this scope's version: %+v -> %+v", vBefore, vAfter)
	}

	// The version a search returns is the version of the snapshot it read.
	cur := f.search(base, q)
	if v, _ := f.ix.Version(ctx, base); v.Epoch != cur.Version.Epoch || strings.Join(v.Active, ",") != strings.Join(cur.Version.Active, ",") {
		t.Errorf("Result.Version %+v differs from Version() %+v", cur.Version, v)
	}
}
