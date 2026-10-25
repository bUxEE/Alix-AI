package degradation

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The retrieval side: the real lexical index, the real flat vector index and the authorised hybrid pipeline of master
// plan 12.4, over a throwaway database. The embedding model, the reranker and the router are the deterministic fake
// provider with failures injected; the source catalog is the deterministic authority on sources.

const embedModel = "test-embed-32"

type catEntry struct {
	tenant string
	state  query.SourceState
}

// catalog is the source catalog of a test: which sources the principal may read and whether each can be opened.
type catalog struct {
	mu      sync.Mutex
	entries map[string]catEntry
	err     error
}

func (c *catalog) set(tenant, source string, st query.SourceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[source] = catEntry{tenant: tenant, state: st}
}

func (c *catalog) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *catalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		if e, ok := c.entries[id]; ok && e.tenant == scope.TenantID {
			out[id] = e.state
		}
	}
	return out, nil
}

type ragEnv struct {
	t    testing.TB
	db   *store.DB
	ix   *index.Index
	flat *vector.Flat
	clk  *clock.Fake
	chk  *chunk.Chunker
	reg  *providers.Registry
	emb  *pfake.Provider
	cat  *catalog
}

func ragScope(tenant string) contracts.Scope { return storetest.Scope("app", tenant, "w1") }

// newRag builds the retrieval stack over a fresh database.
func newRag(t testing.TB) *ragEnv { return newRagOn(t, storetest.Open(t, nil)) }

func newRagOn(t testing.TB, db *store.DB) *ragEnv {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	ix, err := index.New(db, clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	vcfg := vector.DefaultConfig()
	vcfg.CacheMiB = 0
	flat, err := vector.NewFlat(db, vector.FlatOptions{Config: vcfg, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.Config{ChildMinToks: 0, ChildMaxToks: 64, ParentMaxToks: 1024})
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.NewRegistry(clk)
	emb := pfake.New(pfake.WithModel(embedModel))
	if err := reg.Register(providers.RoleEmbedder, emb); err != nil {
		t.Fatal(err)
	}
	return &ragEnv{t: t, db: db, ix: ix, flat: flat, clk: clk, chk: chk, reg: reg, emb: emb,
		cat: &catalog{entries: map[string]catEntry{}}}
}

func (e *ragEnv) deps() query.Deps {
	return query.Deps{Lexical: e.ix, Catalog: e.cat, Vectors: e.flat, Hits: e.ix, Providers: e.reg, EmbeddingModel: embedModel, Clock: e.clk}
}

func (e *ragEnv) pipeline(d query.Deps, cfg query.Config) *query.Pipeline {
	e.t.Helper()
	p, err := query.New(d, cfg)
	if err != nil {
		e.t.Fatalf("query.New: %v", err)
	}
	return p
}

func (e *ragEnv) query(p *query.Pipeline, scope contracts.Scope, text string) (query.Result, error) {
	return p.Query(context.Background(), scope, query.Request{Text: text})
}

func (e *ragEnv) mustQuery(p *query.Pipeline, scope contracts.Scope, text string) query.Result {
	e.t.Helper()
	res, err := e.query(p, scope, text)
	if err != nil {
		e.t.Fatalf("Query(%q): %v", text, err)
	}
	return res
}

// addSource builds an active generation of a new source in scope from one Markdown document, vectorises every
// child chunk, and registers the source in the catalog as available, current and authorised.
func (e *ragEnv) addSource(scope contracts.Scope, name, docID, markdown string) string {
	e.t.Helper()
	ctx := context.Background()
	srcID := ids.New("src")
	comps := index.Components{
		Parser:   rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"},
		Chunker:  e.chk.Component(),
		Embedder: &rag.ComponentVersion{Name: "granite-embedding", Version: "97m"},
	}
	g, err := e.ix.BeginGeneration(ctx, scope, srcID, comps)
	if err != nil {
		e.t.Fatalf("BeginGeneration: %v", err)
	}
	gen := string(g.GenerationID)
	res, err := e.chk.Chunk(ctx, chunk.FromMarkdown(docID, markdown))
	if err != nil {
		e.t.Fatalf("chunking: %v", err)
	}
	if _, err := e.ix.PutDocument(ctx, scope, gen, index.Document{ID: docID, Version: "v1", Chunks: res}); err != nil {
		e.t.Fatalf("PutDocument: %v", err)
	}
	var items []vector.Item
	for _, c := range res.Children {
		key, err := vector.NewInputKey(scope, embedModel, c.Text)
		if err != nil {
			e.t.Fatal(err)
		}
		items = append(items, vector.Item{ChunkID: c.ID, Key: key, Vector: pfake.EmbedVector(c.Text, pfake.DefaultEmbedDim)})
	}
	if _, err := e.ix.ValidateGeneration(ctx, scope, gen, index.Expectation{}); err != nil {
		e.t.Fatalf("ValidateGeneration: %v", err)
	}
	if err := e.ix.ActivateGeneration(ctx, scope, gen); err != nil {
		e.t.Fatalf("ActivateGeneration: %v", err)
	}
	if len(items) > 0 {
		if _, err := e.flat.Upsert(ctx, scope, vector.UpsertBatch{Generation: gen, SourceID: srcID, Model: embedModel, Items: items}); err != nil {
			e.t.Fatalf("vector Upsert: %v", err)
		}
	}
	e.cat.set(scope.TenantID, srcID, query.SourceState{
		DisplayName: name, Authorised: true,
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
	})
	return srcID
}

func mdDoc(title string, paras ...string) string {
	return "# " + title + "\n\n## Catalogue\n\n" + strings.Join(paras, "\n\n") + "\n"
}

// sprockets is the corpus of tenant A. The part number is an exact identifier.
func sprockets() string {
	return mdDoc("Sprockets",
		"Sprocket one is steel and fits the small drive.",
		"Sprocket two is brass and fits the large drive.",
		"Sprocket three is nylon and fits the quiet drive.",
		"Sprocket four is titanium and fits the racing drive.",
		"Sprocket five is carbon and fits the lightweight drive.",
		"Sprocket six is bronze and fits the vintage drive.",
		"Part PN-9000-X is the sprocket service kit.")
}

// foreignSprockets is another tenant's document with the same words and a marker that must never reach tenant A.
func foreignSprockets() string {
	return mdDoc("Foreign sprockets",
		"Sprocket foreignmarker one is steel and fits the small drive.",
		"Sprocket foreignmarker two is brass and fits the large drive.",
		"Part PN-9000-X is the foreignmarker service kit.")
}

func itemTexts(p rag.EvidencePackage) []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Text
	}
	return out
}

func hasNotice(p rag.EvidencePackage, n rag.RetrievalNotice) bool {
	for _, have := range p.Retrieval.Notices {
		if have == n {
			return true
		}
	}
	return false
}

func has(list []string, s string) bool {
	for _, have := range list {
		if have == s {
			return true
		}
	}
	return false
}

func hasMethod(p rag.EvidencePackage, m rag.RetrievalMethod) bool {
	for _, have := range p.Retrieval.Methods {
		if have == m {
			return true
		}
	}
	return false
}

// Section 18, "Embedding model unavailable": use authorized FTS/exact search and label reduced semantic recall.
func TestDegradeEmbeddingUnavailable(t *testing.T) {
	const question = "sprocket drive"
	// The stable labels other code keys on are the ones section 18 names.
	if providers.RoleEmbedder.Effect() != providers.EffectReducedSemanticRecall || string(rag.RetrievalNoticeReducedSemanticRecall) != "reduced_semantic_recall" {
		t.Fatalf("the embedder's effect %q is not the reduced_semantic_recall label", providers.RoleEmbedder.Effect())
	}

	failures := []struct {
		name   string
		reason string
		inject func(e *ragEnv)
	}{
		{"the embedder is marked unavailable", query.DegradedEmbedderDown, func(e *ragEnv) {
			if err := e.reg.SetHealth(providers.RoleEmbedder, providers.HealthUnavailable, "down"); err != nil {
				t.Fatal(err)
			}
		}},
		{"the embedding call fails", query.DegradedEmbedFailed, func(e *ragEnv) { e.emb.FailNext(pfake.FailUnavailable) }},
		{"the embedding call times out", query.DegradedEmbedFailed, func(e *ragEnv) { e.emb.FailNext(pfake.FailTimeout) }},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			e := newRag(t)
			a, b := ragScope("tenant-a"), ragScope("tenant-b")
			e.addSource(a, "Sprockets", "sprockets", sprockets())
			e.addSource(b, "Foreign", "foreign", foreignSprockets())
			p := e.pipeline(e.deps(), query.DefaultConfig())

			// Control: healthy, the same question runs both stages and carries no label. Without this control a
			// mismatched embedding model would turn every query FTS-only and the degraded half could not fail.
			healthy := e.mustQuery(p, a, question)
			if healthy.Diagnostics.Mode != query.ModeHybrid || !hasMethod(healthy.Package, rag.RetrievalMethodVector) ||
				hasNotice(healthy.Package, rag.RetrievalNoticeReducedSemanticRecall) {
				t.Fatalf("control: mode %s methods %v notices %v, want hybrid and unlabelled",
					healthy.Diagnostics.Mode, healthy.Package.Retrieval.Methods, healthy.Package.Retrieval.Notices)
			}

			f.inject(e)
			res := e.mustQuery(p, a, question)
			if len(res.Package.Items) == 0 {
				t.Fatal("search is not usable without the embedder: no items")
			}
			if !hasNotice(res.Package, rag.RetrievalNoticeReducedSemanticRecall) {
				t.Errorf("notices %v: the package must be labelled reduced_semantic_recall", res.Package.Retrieval.Notices)
			}
			if want := []rag.RetrievalMethod{rag.RetrievalMethodFts}; !reflect.DeepEqual(res.Package.Retrieval.Methods, want) {
				t.Errorf("methods %v, want only %v: the vector stage did not run and must not be claimed", res.Package.Retrieval.Methods, want)
			}
			if res.Diagnostics.Mode != query.ModeFTSOnly || !has(res.Diagnostics.Degraded, f.reason) || res.Diagnostics.VectorCandidates != 0 {
				t.Errorf("mode %s degraded %v vector candidates %d, want fts_only because of %q",
					res.Diagnostics.Mode, res.Diagnostics.Degraded, res.Diagnostics.VectorCandidates, f.reason)
			}
			// The fallback is still authorised search: another tenant's words never come back.
			if all := strings.Join(itemTexts(res.Package), "\n"); strings.Contains(all, "foreignmarker") {
				t.Fatalf("the FTS fallback returned another tenant's text:\n%s", all)
			}
		})
	}

	t.Run("an exact identifier is still found, first", func(t *testing.T) {
		e := newRag(t)
		a := ragScope("tenant-a")
		e.addSource(a, "Sprockets", "sprockets", sprockets())
		e.addSource(ragScope("tenant-b"), "Foreign", "foreign", foreignSprockets())
		if err := e.reg.SetHealth(providers.RoleEmbedder, providers.HealthUnavailable, "down"); err != nil {
			t.Fatal(err)
		}
		res := e.mustQuery(e.pipeline(e.deps(), query.DefaultConfig()), a, "PN-9000-X")
		if len(res.Package.Items) == 0 || !strings.Contains(res.Package.Items[0].Text, "PN-9000-X") {
			t.Fatalf("items %q: an exact identifier must be the first evidence without the embedder", itemTexts(res.Package))
		}
		if strings.Contains(strings.Join(itemTexts(res.Package), "\n"), "foreignmarker") {
			t.Fatal("the identifier matched another tenant's document")
		}
		if !hasNotice(res.Package, rag.RetrievalNoticeReducedSemanticRecall) {
			t.Error("an exact-match result must still carry the reduced semantic recall label")
		}
		if n := e.emb.Calls(); n != 0 {
			t.Errorf("the embedder was called %d times although it was marked unavailable", n)
		}
	})
}

// Section 18, "Reranker unavailable": use fused retrieval order.
func TestDegradeRerankerUnavailable(t *testing.T) {
	const question = "sprocket drive"
	if providers.RoleReranker.Effect() != providers.EffectFusedOrder {
		t.Fatalf("the reranker's effect is %q", providers.RoleReranker.Effect())
	}
	off := query.DefaultConfig()
	off.ParentExpansion = false
	off.MaxItems = 8
	on := off
	on.Rerank = true
	on.RerankCandidates = 4

	reverse := func(req providers.RerankRequest) ([]providers.RerankHit, error) {
		hits := make([]providers.RerankHit, len(req.Documents))
		for i := range req.Documents {
			hits[i] = providers.RerankHit{Index: len(req.Documents) - 1 - i, Score: float64(len(req.Documents)-i) / 10}
		}
		return hits, nil
	}
	fixture := func(t *testing.T, opts ...pfake.Option) (*ragEnv, *pfake.Provider, []string, contracts.Scope) {
		e := newRag(t)
		a := ragScope("tenant-a")
		e.addSource(a, "Sprockets", "sprockets", sprockets())
		rr := pfake.New(append([]pfake.Option{pfake.WithModel("test-rerank")}, opts...)...)
		if err := e.reg.Register(providers.RoleReranker, rr); err != nil {
			t.Fatal(err)
		}
		fused := itemTexts(e.mustQuery(e.pipeline(e.deps(), off), a, question).Package)
		if len(fused) < 6 {
			t.Fatalf("the fused baseline has %d items", len(fused))
		}
		return e, rr, fused, a
	}

	t.Run("control: a working reranker does change the order", func(t *testing.T) {
		e, rr, fused, a := fixture(t, pfake.WithRerankFunc(reverse))
		res := e.mustQuery(e.pipeline(e.deps(), on), a, question)
		if !res.Diagnostics.Reranked || rr.Calls() != 1 || reflect.DeepEqual(itemTexts(res.Package), fused) {
			t.Fatalf("reranked %v calls %d: the control must differ from the fused order or the degraded half proves nothing",
				res.Diagnostics.Reranked, rr.Calls())
		}
	})
	t.Run("the reranker is marked unavailable: fused order, and it is not called", func(t *testing.T) {
		e, rr, fused, a := fixture(t, pfake.WithRerankFunc(reverse))
		if err := e.reg.SetHealth(providers.RoleReranker, providers.HealthUnavailable, "down"); err != nil {
			t.Fatal(err)
		}
		res := e.mustQuery(e.pipeline(e.deps(), on), a, question)
		if rr.Calls() != 0 || res.Diagnostics.Reranked || !reflect.DeepEqual(itemTexts(res.Package), fused) {
			t.Fatalf("calls %d reranked %v order kept %v", rr.Calls(), res.Diagnostics.Reranked, reflect.DeepEqual(itemTexts(res.Package), fused))
		}
		if !has(res.Diagnostics.Degraded, query.DegradedNoReranker) {
			t.Errorf("degraded %v, want %q", res.Diagnostics.Degraded, query.DegradedNoReranker)
		}
		if hasMethod(res.Package, rag.RetrievalMethodRerank) {
			t.Errorf("methods %v claim a rerank that did not happen", res.Package.Retrieval.Methods)
		}
	})
	for name, kind := range map[string]pfake.FailKind{
		"the reranker call fails":      pfake.FailUnavailable,
		"the reranker call times out":  pfake.FailTimeout,
		"the reranker runs out of RAM": pfake.FailResourceExhausted,
	} {
		t.Run(name+": fused order", func(t *testing.T) {
			e, rr, fused, a := fixture(t, pfake.WithRerankFunc(reverse))
			rr.FailNext(kind)
			res := e.mustQuery(e.pipeline(e.deps(), on), a, question)
			if rr.Calls() != 1 {
				t.Fatalf("reranker calls %d, want the one failing call", rr.Calls())
			}
			if res.Diagnostics.Reranked || !reflect.DeepEqual(itemTexts(res.Package), fused) || !has(res.Diagnostics.Degraded, query.DegradedRerankFailed) {
				t.Fatalf("reranked %v degraded %v order kept %v", res.Diagnostics.Reranked, res.Diagnostics.Degraded, reflect.DeepEqual(itemTexts(res.Package), fused))
			}
			if hasMethod(res.Package, rag.RetrievalMethodRerank) {
				t.Errorf("methods %v claim a rerank that failed", res.Package.Retrieval.Methods)
			}
		})
	}
}

// Section 18, "Source unavailable": mark the affected source stale/unavailable; do not pretend it was checked.
func TestDegradeSourceUnavailable(t *testing.T) {
	const question = "sprocket drive"
	setup := func(t *testing.T) (*ragEnv, *query.Pipeline, contracts.Scope, string) {
		e := newRag(t)
		a := ragScope("tenant-a")
		src := e.addSource(a, "Sprockets", "sprockets", sprockets())
		return e, e.pipeline(e.deps(), query.DefaultConfig()), a, src
	}

	t.Run("control: a healthy source is available and current", func(t *testing.T) {
		e, p, a, _ := setup(t)
		res := e.mustQuery(p, a, question)
		if len(res.Package.Items) == 0 {
			t.Fatal("no evidence")
		}
		for _, it := range res.Package.Items {
			if it.Availability != conversation.CitationAvailabilityAvailable || it.Staleness != conversation.CitationStalenessCurrent {
				t.Fatalf("item %s availability %s staleness %s", it.EvidenceID, it.Availability, it.Staleness)
			}
		}
	})

	t.Run("an unavailable source is marked unavailable and its evidence is labelled, not shown as checked", func(t *testing.T) {
		e, p, a, src := setup(t)
		e.cat.set(a.TenantID, src, query.SourceState{DisplayName: "Sprockets", Authorised: true,
			Availability: conversation.CitationAvailabilityUnavailable, Staleness: conversation.CitationStalenessStale})
		res := e.mustQuery(p, a, question)
		if len(res.Package.Items) == 0 {
			t.Fatal("the indexed evidence was dropped silently")
		}
		for _, it := range res.Package.Items {
			if it.Availability != conversation.CitationAvailabilityUnavailable || it.Staleness != conversation.CitationStalenessStale {
				t.Fatalf("item %s availability %s staleness %s, want unavailable / stale", it.EvidenceID, it.Availability, it.Staleness)
			}
		}
	})

	t.Run("a source that was never checked is not claimed to have been", func(t *testing.T) {
		e, p, a, src := setup(t)
		// The catalog says nothing about availability or staleness: the zero values.
		e.cat.set(a.TenantID, src, query.SourceState{DisplayName: "Sprockets", Authorised: true})
		res := e.mustQuery(p, a, question)
		if len(res.Package.Items) == 0 {
			t.Fatal("no evidence")
		}
		for _, it := range res.Package.Items {
			if it.Availability == conversation.CitationAvailabilityAvailable || it.Staleness == conversation.CitationStalenessCurrent {
				t.Fatalf("item %s is shown as %s / %s without anyone having checked", it.EvidenceID, it.Availability, it.Staleness)
			}
		}
	})

	t.Run("a source the catalog no longer knows or has removed gives no evidence", func(t *testing.T) {
		e, p, a, src := setup(t)
		e.cat.set(a.TenantID, src, query.SourceState{DisplayName: "Sprockets", Authorised: true,
			Availability: conversation.CitationAvailabilityRemoved, Staleness: conversation.CitationStalenessUnknown})
		if res := e.mustQuery(p, a, question); len(res.Package.Items) != 0 {
			t.Fatalf("a removed source still gives %d items", len(res.Package.Items))
		}
		e.cat.mu.Lock()
		delete(e.cat.entries, src)
		e.cat.mu.Unlock()
		if res := e.mustQuery(p, a, question); len(res.Package.Items) != 0 {
			t.Fatalf("a source missing from the catalog still gives %d items: it must never be shown as available", len(res.Package.Items))
		}
	})

	t.Run("a catalog that cannot answer is an error, not an empty package that looks checked", func(t *testing.T) {
		e, p, a, _ := setup(t)
		e.cat.setErr(contracts.New(contracts.ErrUnavailable, "the source registry is down"))
		res, err := e.query(p, a, question)
		if err == nil {
			t.Fatalf("an unreachable catalog produced %d items and no error", len(res.Package.Items))
		}
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("code %s, want unavailable", contracts.CodeOf(err))
		}
	})

	t.Run("the failure of one source does not stale another", func(t *testing.T) {
		e := newRag(t)
		a := ragScope("tenant-a")
		down := e.addSource(a, "Sprockets", "sprockets", sprockets())
		e.addSource(a, "Gears", "gears", mdDoc("Gears", "Gear one is steel and meshes with the sprocket drive.", "Gear two is brass and meshes with the large drive."))
		e.cat.set(a.TenantID, down, query.SourceState{DisplayName: "Sprockets", Authorised: true,
			Availability: conversation.CitationAvailabilityUnavailable, Staleness: conversation.CitationStalenessStale})
		res := e.mustQuery(e.pipeline(e.deps(), query.DefaultConfig()), a, question)
		var stale, fresh int
		for _, it := range res.Package.Items {
			if it.Availability == conversation.CitationAvailabilityUnavailable {
				stale++
			} else if it.Availability == conversation.CitationAvailabilityAvailable && it.Staleness == conversation.CitationStalenessCurrent {
				fresh++
			}
		}
		if stale == 0 || fresh == 0 {
			t.Fatalf("%d items from the unavailable source, %d from the healthy one: each must keep its own label", stale, fresh)
		}
	})
}
