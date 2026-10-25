package query

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// testModel is the embedding model of every fixture: the fake embedder reports
// it, the vector generations are built with it and Deps.EmbeddingModel names it.
// A mismatch anywhere would silently turn a hybrid test into an FTS-only one,
// which is why hybrid tests also assert Diagnostics.Mode.
const testModel = "test-embed-32"

// fakeCatalog is the source catalog of a test: the deterministic authority on
// which sources a principal may read. An entry belongs to one tenant, and
// Sources returns only the caller's tenant's entries, so a catalog never answers
// for a source of another scope.
type fakeCatalog struct {
	mu      sync.Mutex
	entries map[string]catEntry
	calls   int
	err     error
	// onCall, when set, runs at the start of every Sources call with its ordinal.
	onCall func(n int)
}

type catEntry struct {
	tenant string
	state  SourceState
}

func newFakeCatalog() *fakeCatalog { return &fakeCatalog{entries: map[string]catEntry{}} }

func (c *fakeCatalog) set(tenant, source string, st SourceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[source] = catEntry{tenant: tenant, state: st}
}

func (c *fakeCatalog) revoke(source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[source]
	e.state.Authorised = false
	c.entries[source] = e
}

func (c *fakeCatalog) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *fakeCatalog) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeCatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]SourceState, error) {
	c.mu.Lock()
	c.calls++
	n, hook := c.calls, c.onCall
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := map[string]SourceState{}
	for _, id := range sourceIDs {
		if e, ok := c.entries[id]; ok && e.tenant == scope.TenantID {
			out[id] = e.state
		}
	}
	return out, nil
}

// mapHits is a HitReader over chunks the fixture built. It stands in for the
// read that internal/rag/index does not export yet: a chunk is returned only if
// the real index's Recheck says the scope can see it in this very read, so it
// applies the same visibility rules (scope, active generation, not tombstoned,
// not hidden) that the production reader must apply.
type mapHits struct {
	ix    *index.Index
	mu    sync.Mutex
	byKey map[string]index.Hit
	calls int
	// rogue, when set, is appended to every answer: a reader that returns a
	// chunk it was not asked for.
	rogue []index.Hit
}

func (m *mapHits) put(h index.Hit) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byKey[candKey(h.GenerationID, h.ChunkID)] = h
}

func (m *mapHits) Hits(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Hit, error) {
	m.mu.Lock()
	m.calls++
	rogue := m.rogue
	m.mu.Unlock()
	verdicts, err := m.ix.Recheck(ctx, scope, refs, includeHidden)
	if err != nil {
		return nil, err
	}
	var out []index.Hit
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range refs {
		if !verdicts[i].Visible {
			continue
		}
		if h, ok := m.byKey[candKey(r.GenerationID, r.ChunkID)]; ok {
			out = append(out, h)
		}
	}
	return append(out, rogue...), nil
}

// env is a real index, a real flat vector index and the fake provider in one
// throwaway database, so authorisation tests run the production code of both.
type env struct {
	t    testing.TB
	db   *store.DB
	ix   *index.Index
	flat *vector.Flat
	clk  *clock.Fake
	chk  *chunk.Chunker
	reg  *providers.Registry
	emb  *fake.Provider
	cat  *fakeCatalog
	hits *mapHits
}

// testChunkConfig makes every paragraph its own child, so a section of several
// paragraphs has several children and a parent worth expanding to.
func testChunkConfig() chunk.Config {
	return chunk.Config{ChildMinToks: 0, ChildMaxToks: 64, ParentMaxToks: 1024}
}

func newEnv(t testing.TB, embOpts ...fake.Option) *env {
	t.Helper()
	db := storetest.Open(t, nil) // store.Default holds the migrations of every imported module
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	ix, err := index.New(db, clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	flat, err := vector.NewFlat(db, vector.FlatOptions{Config: vectorTestConfig(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(testChunkConfig())
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.NewRegistry(clk)
	emb := fake.New(append([]fake.Option{fake.WithModel(testModel)}, embOpts...)...)
	if err := reg.Register(providers.RoleEmbedder, emb); err != nil {
		t.Fatal(err)
	}
	return &env{
		t: t, db: db, ix: ix, flat: flat, clk: clk, chk: chk, reg: reg, emb: emb,
		cat:  newFakeCatalog(),
		hits: &mapHits{ix: ix, byKey: map[string]index.Hit{}},
	}
}

func vectorTestConfig() vector.Config {
	c := vector.DefaultConfig()
	c.CacheMiB = 0
	return c
}

// deps is the full set of collaborators of the pipeline under test.
func (e *env) deps() Deps {
	return Deps{
		Lexical: e.ix, Catalog: e.cat, Vectors: e.flat, Hits: e.hits,
		Providers: e.reg, EmbeddingModel: testModel, Clock: e.clk,
	}
}

func (e *env) pipeline(d Deps, cfg Config) *Pipeline {
	e.t.Helper()
	p, err := New(d, cfg)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	return p
}

// docSpec is one document of a source: its ID and its Markdown.
type docSpec struct {
	id, md string
	// cd, when set, is the chunker input used instead of parsing md (a document
	// with hidden blocks, which Markdown cannot express).
	cd *chunk.Document
}

// source is a built source: one active generation with its documents indexed
// and vectorised.
type source struct {
	id, gen  string
	name     string
	scope    contracts.Scope
	children []chunk.Chunk
	docOf    map[string]string // chunk id -> document id
}

// addSource builds an active generation of a new source in scope from docs, puts
// a vector per child into the flat index and registers the source in the catalog
// as available, current and authorised (egress and classification unstated).
func (e *env) addSource(scope contracts.Scope, name string, docs ...docSpec) *source {
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
	genID := string(g.GenerationID)
	s := &source{id: srcID, gen: genID, name: name, scope: scope, docOf: map[string]string{}}
	var items []vector.Item
	for _, d := range docs {
		cdoc := chunk.FromMarkdown(d.id, d.md)
		if d.cd != nil {
			cdoc = *d.cd
		}
		res, err := e.chk.Chunk(ctx, cdoc)
		if err != nil {
			e.t.Fatalf("chunking %s: %v", d.id, err)
		}
		if _, err := e.ix.PutDocument(ctx, scope, genID, index.Document{ID: d.id, Version: "v1", Chunks: res}); err != nil {
			e.t.Fatalf("PutDocument %s: %v", d.id, err)
		}
		for _, c := range res.Children {
			s.children = append(s.children, c)
			s.docOf[c.ID] = d.id
			e.hits.put(index.Hit{
				ChunkID: c.ID, GenerationID: genID, SourceID: srcID, DocumentID: d.id, DocumentVersion: "v1",
				ParentID: c.ParentID, Kind: c.Kind, Ordinal: c.Ordinal, Text: c.Text, Section: c.Section, Path: c.Path,
				Header: c.Header, Span: c.Span, Page: c.Page, PageEnd: c.PageEnd, ContentHash: c.ContentHash,
				EstToks: c.EstToks, Hidden: c.Hidden, Split: c.Split,
			})
			key, err := vector.NewInputKey(scope, testModel, c.Text)
			if err != nil {
				e.t.Fatal(err)
			}
			items = append(items, vector.Item{ChunkID: c.ID, Key: key, Vector: fake.EmbedVector(c.Text, fake.DefaultEmbedDim)})
		}
	}
	if _, err := e.ix.ValidateGeneration(ctx, scope, genID, index.Expectation{}); err != nil {
		e.t.Fatalf("ValidateGeneration: %v", err)
	}
	if err := e.ix.ActivateGeneration(ctx, scope, genID); err != nil {
		e.t.Fatalf("ActivateGeneration: %v", err)
	}
	if len(items) > 0 {
		if _, err := e.flat.Upsert(ctx, scope, vector.UpsertBatch{Generation: genID, SourceID: srcID, Model: testModel, Items: items}); err != nil {
			e.t.Fatalf("vector Upsert: %v", err)
		}
	}
	e.cat.set(scope.TenantID, srcID, SourceState{
		DisplayName:  name,
		Authorised:   true,
		Availability: conversation.CitationAvailabilityAvailable,
		Staleness:    conversation.CitationStalenessCurrent,
	})
	return s
}

func (s *source) chunkWith(t testing.TB, substr string) chunk.Chunk {
	t.Helper()
	for _, c := range s.children {
		if strings.Contains(c.Text, substr) {
			return c
		}
	}
	t.Fatalf("no chunk of source %s holds %q", s.name, substr)
	return chunk.Chunk{}
}

// scopeOf is a valid scope of a tenant in the test application.
func scopeOf(tenant string) contracts.Scope { return storetest.Scope("app", tenant, "w1") }

// section renders one Markdown section: a heading and one paragraph per text.
func mdSection(title string, paras ...string) string {
	return "## " + title + "\n\n" + strings.Join(paras, "\n\n") + "\n\n"
}

func doc(title string, sections ...string) string {
	return "# " + title + "\n\n" + strings.Join(sections, "")
}

// texts returns the evidence texts of a package in order.
func texts(p rag.EvidencePackage) []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Text
	}
	return out
}

func allText(p rag.EvidencePackage) string { return strings.Join(texts(p), "\n") }

func hasNotice(p rag.EvidencePackage, n rag.RetrievalNotice) bool {
	for _, have := range p.Retrieval.Notices {
		if have == n {
			return true
		}
	}
	return false
}

func hasString(list []string, s string) bool {
	for _, have := range list {
		if have == s {
			return true
		}
	}
	return false
}

func mustQuery(t testing.TB, p *Pipeline, scope contracts.Scope, req Request) Result {
	t.Helper()
	res, err := p.Query(context.Background(), scope, req)
	if err != nil {
		t.Fatalf("Query(%q): %v", req.Text, err)
	}
	return res
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// replaceGeneration builds and activates a new generation of s holding one
// document at the given version: the old generation stops serving the source,
// as when a source is re-indexed while a query is running.
func (e *env) replaceGeneration(s *source, d docSpec, version string) string {
	e.t.Helper()
	ctx := context.Background()
	comps := index.Components{
		Parser:   rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"},
		Chunker:  e.chk.Component(),
		Embedder: &rag.ComponentVersion{Name: "granite-embedding", Version: "97m"},
	}
	g, err := e.ix.BeginGeneration(ctx, s.scope, s.id, comps)
	if err != nil {
		e.t.Fatalf("BeginGeneration: %v", err)
	}
	genID := string(g.GenerationID)
	res, err := e.chk.Chunk(ctx, chunk.FromMarkdown(d.id, d.md))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.ix.PutDocument(ctx, s.scope, genID, index.Document{ID: d.id, Version: version, Chunks: res}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.ix.ValidateGeneration(ctx, s.scope, genID, index.Expectation{}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.ix.ActivateGeneration(ctx, s.scope, genID); err != nil {
		e.t.Fatal(err)
	}
	return genID
}
