package isolationsec

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

var bg = context.Background()

// callerScope is the verified scope of a caller. Everything but the tenant is the same between the
// two tenants of a test, which is the shape in which a key that forgets the tenant would show.
func callerScope(app, tenant, workspace, principal string) contracts.Scope {
	s := storetest.Scope(app, tenant, workspace)
	s.PrincipalID = principal
	s.SessionID = "ses_shared"
	return s
}

// ---------------------------------------------------------------- a cache keyed by the production key

// resultCache is the cache a layer above the index would keep: entries keyed by index.CacheKey, with
// the counters a test reads. It is the only part of this file that is the test's own; the key is the
// production function, and the version is the one the real index reports.
type resultCache struct {
	mu            sync.Mutex
	entries       map[string]string
	hits, misses  int
	lastHitOrigin string
}

func newResultCache() *resultCache { return &resultCache{entries: map[string]string{}} }

func (c *resultCache) lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return v, ok
}

func (c *resultCache) store(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = value
}

// ---------------------------------------------------------------- an index with documents

type indexRig struct {
	t   *testing.T
	ix  *index.Index
	chk *chunk.Chunker
	clk *clock.Fake
}

func newIndexRig(t *testing.T) *indexRig {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil)
	ix, err := index.New(db, clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return &indexRig{t: t, ix: ix, chk: chk, clk: clk}
}

func (r *indexRig) doc(id, version, md string) index.Document {
	r.t.Helper()
	res, err := r.chk.Chunk(bg, chunk.FromMarkdown(id, md))
	if err != nil {
		r.t.Fatalf("chunking %s: %v", id, err)
	}
	return index.Document{ID: id, Version: version, Chunks: res}
}

func (r *indexRig) comps() index.Components {
	emb := rag.ComponentVersion{Name: "granite-embedding", Version: "97m"}
	return index.Components{Parser: rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"}, Chunker: r.chk.Component(), Embedder: &emb}
}

// serve puts documents into a new generation of a new source and makes it the one that serves. It
// returns the source and the generation.
func (r *indexRig) serve(scope contracts.Scope, docs map[string]string) (source, generation string) {
	r.t.Helper()
	source = ids.New("src")
	g, err := r.ix.BeginGeneration(bg, scope, source, r.comps())
	if err != nil {
		r.t.Fatal(err)
	}
	generation = string(g.GenerationID)
	for _, id := range sortedKeys(docs) {
		if _, err := r.ix.PutDocument(bg, scope, generation, r.doc(id, "v1", docs[id])); err != nil {
			r.t.Fatal(err)
		}
	}
	if _, err := r.ix.ValidateGeneration(bg, scope, generation, index.Expectation{}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.ix.ActivateGeneration(bg, scope, generation); err != nil {
		r.t.Fatal(err)
	}
	return source, generation
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (r *indexRig) search(scope contracts.Scope, q index.Query) index.Result {
	r.t.Helper()
	res, err := r.ix.Search(bg, scope, q)
	if err != nil {
		r.t.Fatalf("Search(%q): %v", q.Text, err)
	}
	return res
}

// ---------------------------------------------------------------- the vector index

func unit(dim, hot int) []float32 {
	v := make([]float32, dim)
	v[hot] = 1
	return v
}

// ---------------------------------------------------------------- the catalog of the resolver

// grantCatalog authorises a source for the scopes it was granted to, and for those only.
type grantCatalog struct {
	mu     sync.Mutex
	grants map[string]bool // tenant + "/" + workspace + "/" + principal + "/" + source
	down   bool
}

func (c *grantCatalog) grant(s contracts.Scope, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.grants == nil {
		c.grants = map[string]bool{}
	}
	c.grants[grantKey(s, source)] = true
}

func (c *grantCatalog) revoke(s contracts.Scope, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.grants, grantKey(s, source))
}

func grantKey(s contracts.Scope, source string) string {
	return s.ApplicationID + "/" + s.TenantID + "/" + s.WorkspaceID + "/" + s.PrincipalID + "/" + source
}

func (c *grantCatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, errors.New("catalog down")
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		out[id] = query.SourceState{
			DisplayName: "Source " + id, Authorised: c.grants[grantKey(scope, id)],
			Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
		}
	}
	return out, nil
}

// SEC-022
//
// Cached results (retrieval, evidence, embeddings of queries, citation previews, prompt assembly) are
// not served across tenants, principals or source versions. Every cache key includes the
// application, tenant, workspace, principal scope and source version, the same query under another
// scope or after a source update never hits the earlier entry, and a preview reference is bound to
// its scope and expires. See the package comment for which caches exist.
func TestSecNoSharedCacheAcrossScopes(t *testing.T) {
	t.Run("the key of every result cache binds scope, version and query", func(t *testing.T) {
		r := newIndexRig(t)
		base := callerScope("app_clinic", "ten_a", "wks_1", "usr_1")
		other := callerScope("app_clinic", "ten_b", "wks_1", "usr_1")
		const text = "Notes about the harbour crane."
		docs := map[string]string{"crane": "# Crane\n\n" + text, "quay": "# Quay\n\nNotes about the quay."}
		src, gen := r.serve(base, docs)
		// The other tenant holds the same documents: the same questions have answers there too.
		r.serve(other, docs)
		q := index.Query{Text: "harbour crane", Limit: 5}

		for _, ns := range []string{"retrieval", "evidence", "query-embedding", "citation-preview", "prompt-assembly"} {
			t.Run(ns, func(t *testing.T) {
				cache := newResultCache()
				key := func(s contracts.Scope, v index.Version, material ...string) string {
					t.Helper()
					k, err := index.CacheKey(s, ns, v, material...)
					if err != nil {
						t.Fatalf("CacheKey: %v", err)
					}
					return k
				}
				res := r.search(base, q)
				if len(res.Hits) == 0 {
					t.Fatal("the query found nothing: the test did not exercise the cache")
				}
				cache.store(key(base, res.Version, q.Material()...), "answer for the base scope")

				// Control: the same scope, the same version, the same query hits.
				again := r.search(base, q)
				if v, ok := cache.lookup(key(base, again.Version, q.Material()...)); !ok || v != "answer for the base scope" {
					t.Fatalf("the cache missed for an unchanged index and an unchanged scope (%q, %v)", v, ok)
				}
				if cache.hits != 1 || cache.misses != 0 {
					t.Fatalf("hits = %d, misses = %d after the control", cache.hits, cache.misses)
				}
				// The session carries no authority: another session of the same principal hits the same entry.
				sess := base
				sess.SessionID = "ses_another"
				if _, ok := cache.lookup(key(sess, again.Version, q.Material()...)); !ok {
					t.Error("the key depends on the session id")
				}
				hitsBefore := cache.hits

				// The same query under any other part of the scope never hits it, even where that scope holds the
				// same documents and so has an answer of its own to give.
				for name, mutate := range map[string]func(*contracts.Scope){
					"another application":       func(s *contracts.Scope) { s.ApplicationID = "app_other" },
					"another tenant":            func(s *contracts.Scope) { *s = other },
					"another workspace":         func(s *contracts.Scope) { s.WorkspaceID = "wks_2" },
					"another principal":         func(s *contracts.Scope) { s.PrincipalID = "usr_2" },
					"a role added":              func(s *contracts.Scope) { s.Roles = []string{"member", "admin"} },
					"a role changed":            func(s *contracts.Scope) { s.Roles = []string{"viewer"} },
					"a capability scope added":  func(s *contracts.Scope) { s.CapabilityScopes = []string{"test.read", "test.write"} },
					"a capability scope lost":   func(s *contracts.Scope) { s.CapabilityScopes = []string{"test.other"} },
					"all of tenant and session": func(s *contracts.Scope) { s.TenantID, s.SessionID = "ten_c", "ses_c" },
				} {
					s := base
					s.Roles = slices.Clone(base.Roles)
					s.CapabilityScopes = slices.Clone(base.CapabilityScopes)
					mutate(&s)
					// The version is the one the real index reports for that scope when it has one, and the base
					// version otherwise: the key must differ either way.
					v := res.Version
					if name == "another tenant" {
						v = r.search(other, q).Version
					}
					if _, ok := cache.lookup(key(s, v, q.Material()...)); ok {
						t.Errorf("%s: the query hit the entry of another scope", name)
					}
					// And with the very version of the base scope, so only the scope differs.
					if _, ok := cache.lookup(key(s, res.Version, q.Material()...)); ok {
						t.Errorf("%s (same version): the query hit the entry of another scope", name)
					}
				}
				if cache.hits != hitsBefore {
					t.Errorf("hits moved from %d to %d during the scope variations", hitsBefore, cache.hits)
				}

				// Another namespace and another query are other entries.
				otherNS, _ := index.CacheKey(base, ns+"-x", res.Version, q.Material()...)
				if _, ok := cache.lookup(otherNS); ok {
					t.Error("another namespace hit the entry")
				}
				if _, ok := cache.lookup(key(base, res.Version, index.Query{Text: "quay", Limit: 5}.Material()...)); ok {
					t.Error("another query hit the entry")
				}

				// After a source update the same query, scope and namespace never hit the earlier entry. Each kind
				// of update is applied in turn, and the entry is stored again at the new version.
				update := func(name string, mutate func()) {
					t.Helper()
					before := r.search(base, q).Version
					cache.store(key(base, before, q.Material()...), name+" before")
					mutate()
					after := r.search(base, q).Version
					if _, ok := cache.lookup(key(base, after, q.Material()...)); ok {
						t.Errorf("%s: the earlier entry was served after the update", name)
					}
					// The earlier entry is still in the cache: it is the new key that does not reach it, so this
					// is not a cache that was simply emptied.
					if v, ok := cache.lookup(key(base, before, q.Material()...)); !ok || v != name+" before" {
						t.Errorf("%s: the entry of the earlier version was lost (%q, %v)", name, v, ok)
					}
					cache.store(key(base, after, q.Material()...), name+" after")
					if v, ok := cache.lookup(key(base, r.search(base, q).Version, q.Material()...)); !ok || v != name+" after" {
						t.Errorf("%s: the cache missed at the new version (%q, %v)", name, v, ok)
					}
				}
				update("a document updated", func() {
					if _, err := r.ix.PutDocument(bg, base, gen, r.doc("crane", "v2", "# Crane\n\n"+text+" Revised after the inspection.")); err != nil {
						t.Fatal(err)
					}
				})
				update("a document deleted", func() {
					if _, err := r.ix.DeleteDocument(bg, base, src, "quay"); err != nil {
						t.Fatal(err)
					}
				})
				update("a new generation activated", func() {
					g2, err := r.ix.BeginGeneration(bg, base, src, r.comps())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := r.ix.PutDocument(bg, base, string(g2.GenerationID), r.doc("crane", "v3", "# Crane\n\n"+text+" Rebuilt.")); err != nil {
						t.Fatal(err)
					}
					if _, err := r.ix.ValidateGeneration(bg, base, string(g2.GenerationID), index.Expectation{}); err != nil {
						t.Fatal(err)
					}
					if err := r.ix.ActivateGeneration(bg, base, string(g2.GenerationID)); err != nil {
						t.Fatal(err)
					}
					gen = string(g2.GenerationID)
				})
				// A change in another scope does not touch this scope's version: no needless miss, and nothing
				// learned about the other scope.
				v0 := r.search(base, q).Version
				r.serve(other, map[string]string{"extra": "# Extra\n\nMore notes about the harbour."})
				if v1 := r.search(base, q).Version; v1.Epoch != v0.Epoch || !slices.Equal(v1.Active, v0.Active) {
					t.Errorf("a change in another tenant moved this tenant's version: %+v -> %+v", v0, v1)
				}
			})
		}
	})

	t.Run("the resident cache of the vector index never serves one scope's vectors to another", func(t *testing.T) {
		const dim = 8
		model := "fake-embed"
		cfgOn := vector.DefaultConfig()
		cfgOn.CacheMiB = 8
		cfgOff := vector.DefaultConfig()
		cfgOff.CacheMiB = 0
		// The same sequence runs against a flat index with the cache on and one with it off: the cache is a
		// latency dial and never a source of truth, so every answer must be the same.
		run := func(cfg vector.Config) []string {
			db := storetest.Open(t, nil)
			flat, err := vector.NewFlat(db, vector.FlatOptions{Config: cfg, Clock: clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))})
			if err != nil {
				t.Fatal(err)
			}
			scopes := map[string]contracts.Scope{
				"A": callerScope("app_clinic", "ten_a", "wks_1", "usr_1"),
				"B": callerScope("app_clinic", "ten_b", "wks_1", "usr_1"),
				"W": callerScope("app_clinic", "ten_a", "wks_2", "usr_1"),
				"X": callerScope("app_other", "ten_a", "wks_1", "usr_1"),
			}
			// Every scope holds a generation of the SAME id with its own chunks: a cache keyed by the generation
			// alone would give one scope's vectors to the next.
			const generation = "gen_shared"
			put := func(name string, hot int, chunks ...string) {
				t.Helper()
				var items []vector.Item
				for _, c := range chunks {
					key, err := vector.NewInputKey(scopes[name], model, c)
					if err != nil {
						t.Fatal(err)
					}
					items = append(items, vector.Item{ChunkID: c, Key: key, Vector: unit(dim, hot)})
				}
				if _, err := flat.Upsert(bg, scopes[name], vector.UpsertBatch{Generation: generation, SourceID: "src_1", Model: model, Items: items}); err != nil {
					t.Fatalf("Upsert %s: %v", name, err)
				}
			}
			put("A", 0, "chk_a1", "chk_a2")
			put("B", 0, "chk_b1", "chk_b2")
			put("W", 0, "chk_w1")
			put("X", 0, "chk_x1")

			var log []string
			search := func(name string, gens ...string) []string {
				t.Helper()
				res, err := flat.Search(bg, scopes[name], vector.Query{Vector: unit(dim, 0), Model: model, K: 10, Generations: gens})
				if err != nil {
					t.Fatalf("Search %s: %v", name, err)
				}
				var got []string
				for _, h := range res.Hits {
					got = append(got, h.ChunkID)
				}
				slices.Sort(got)
				for _, s := range res.Skipped {
					got = append(got, "skipped:"+s.Generation+":"+string(s.Reason))
				}
				log = append(log, name+"="+join(got))
				return got
			}
			want := map[string][]string{"A": {"chk_a1", "chk_a2"}, "B": {"chk_b1", "chk_b2"}, "W": {"chk_w1"}, "X": {"chk_x1"}}
			// B warms the cache first, then every other scope asks for the same generation id, twice.
			for _, name := range []string{"B", "A", "W", "X", "A", "B", "X", "W"} {
				if got := search(name, generation); !slices.Equal(got, want[name]) {
					t.Fatalf("scope %s got %v, want only its own %v", name, got, want[name])
				}
			}
			// A generation that exists in no scope of the caller is not found, whoever holds it elsewhere.
			if _, err := flat.Upsert(bg, scopes["B"], vector.UpsertBatch{Generation: "gen_only_b", SourceID: "src_2", Model: model, Items: []vector.Item{{
				ChunkID: "chk_secret", Key: mustKey(t, scopes["B"], model, "secret"), Vector: unit(dim, 0)}}}); err != nil {
				t.Fatal(err)
			}
			if got := search("B", "gen_only_b"); !slices.Equal(got, []string{"chk_secret"}) {
				t.Fatalf("B did not find its own generation: %v", got)
			}
			for _, name := range []string{"A", "W", "X"} {
				if got := search(name, "gen_only_b"); !slices.Equal(got, []string{"skipped:gen_only_b:unknown_generation"}) {
					t.Fatalf("scope %s asking for another scope's generation got %v", name, got)
				}
			}
			// A source update in A: a1 now points elsewhere. A's next search sees the update, never the cached
			// vectors; and nothing changes for B.
			if _, err := flat.Upsert(bg, scopes["A"], vector.UpsertBatch{Generation: generation, SourceID: "src_1", Model: model, Items: []vector.Item{{
				ChunkID: "chk_a1", Key: mustKey(t, scopes["A"], model, "chk_a1 v2"), Vector: unit(dim, 3)}}}); err != nil {
				t.Fatal(err)
			}
			res, err := flat.Search(bg, scopes["A"], vector.Query{Vector: unit(dim, 3), Model: model, K: 1, Generations: []string{generation}})
			if err != nil || len(res.Hits) != 1 || res.Hits[0].ChunkID != "chk_a1" || res.Hits[0].Score < 0.99 {
				t.Fatalf("A's search after its own update = %+v, %v: the stale vectors were served", res, err)
			}
			log = append(log, "A-after-update="+res.Hits[0].ChunkID)
			if got := search("B", generation); !slices.Equal(got, want["B"]) {
				t.Fatalf("B after A's update got %v", got)
			}
			// Dropping A's generation takes nothing from the others, and A finds nothing.
			if n, err := flat.DeleteGeneration(bg, scopes["A"], generation); err != nil || n != 2 {
				t.Fatalf("DeleteGeneration = %d, %v", n, err)
			}
			if got := search("A", generation); !slices.Equal(got, []string{"skipped:gen_shared:unknown_generation"}) {
				t.Fatalf("A after dropping its generation got %v", got)
			}
			for _, name := range []string{"B", "W", "X"} {
				if got := search(name, generation); !slices.Equal(got, want[name]) {
					t.Fatalf("scope %s after A's delete got %v", name, got)
				}
			}
			return log
		}
		on, off := run(cfgOn), run(cfgOff)
		if !slices.Equal(on, off) {
			t.Fatalf("the cache changed an answer:\n  with the cache:    %v\n  without the cache: %v", on, off)
		}
	})

	t.Run("a citation reference is bound to its scope and expires", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
		cat := &grantCatalog{}
		cfg := answer.DefaultConfig()
		res, err := answer.NewResolver(cat, clk, cfg)
		if err != nil {
			t.Fatal(err)
		}
		a := callerScope("app_clinic", "ten_a", "wks_1", "usr_1")
		const source = "src_policies"
		cat.grant(a, source)
		// The same principal id in another tenant, workspace and application, and another principal in the
		// same tenant: each is granted the source too, so only the reference's binding can refuse them.
		others := map[string]contracts.Scope{
			"another tenant":      callerScope("app_clinic", "ten_b", "wks_1", "usr_1"),
			"another workspace":   callerScope("app_clinic", "ten_a", "wks_2", "usr_1"),
			"another application": callerScope("app_other", "ten_a", "wks_1", "usr_1"),
			"another principal":   callerScope("app_clinic", "ten_a", "wks_1", "usr_2"),
		}
		for _, s := range others {
			cat.grant(s, source)
		}
		ref, expires, err := res.Issue(a, answer.RefSpec{SourceID: actions.SourceID(source), Version: "v1", Preview: "the passage that was cited"})
		if err != nil {
			t.Fatal(err)
		}
		fid := fileIDOf(t, ref)

		// Control: the scope the reference was issued for resolves it, with the preview that was cited.
		got, err := res.Resolve(bg, a, fid)
		if err != nil || got.Preview != "the passage that was cited" || string(got.SourceID) != source || !got.ExpiresAt.Equal(expires) {
			t.Fatalf("the owner's reference = %+v, %v", got, err)
		}

		// Every other scope gets one and the same refusal, the one of a reference that was never issued.
		_, never := res.Resolve(bg, a, "fil_neverissued")
		for name, s := range others {
			_, err := res.Resolve(bg, s, fid)
			if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
				t.Errorf("%s: %v, want permission_denied", name, err)
			}
			if err == nil || never == nil || err.Error() != never.Error() {
				t.Errorf("%s: the refusal (%v) differs from the refusal of a reference that does not exist (%v)", name, err, never)
			}
		}

		// The catalog stays the authority: a revoked source is refused to its owner at once.
		cat.revoke(a, source)
		if _, err := res.Resolve(bg, a, fid); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("a revoked source resolved: %v", err)
		}
		cat.grant(a, source)
		if _, err := res.Resolve(bg, a, fid); err != nil {
			t.Fatalf("the reference did not resolve after the grant came back: %v", err)
		}

		// A reference expires: to its owner it says so, to anyone else it is the same refusal as ever.
		clk.Advance(cfg.RefTTL - time.Second)
		if _, err := res.Resolve(bg, a, fid); err != nil {
			t.Fatalf("the reference expired early: %v", err)
		}
		clk.Advance(2 * time.Second)
		if _, err := res.Resolve(bg, a, fid); contracts.CodeOf(err) != contracts.ErrExpired {
			t.Errorf("an expired reference: %v, want expired", err)
		}
		for name, s := range others {
			if _, err := res.Resolve(bg, s, fid); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
				t.Errorf("%s, after expiry: %v, want permission_denied (the expiry is not told to another scope)", name, err)
			}
		}
	})
}

func mustKey(t testing.TB, s contracts.Scope, model, input string) vector.InputKey {
	t.Helper()
	k, err := vector.NewInputKey(s, model, input)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func fileIDOf(t testing.TB, ref conversation.CitationOpenRef) actions.FileID {
	t.Helper()
	f, ok := ref.Value.(conversation.CitationOpenRefFile)
	if !ok || f.FileID == "" {
		t.Fatalf("the reference is not a file reference: %+v", ref)
	}
	return f.FileID
}
