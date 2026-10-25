package index

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// fixture is an index over a throwaway database, a fake clock and a chunker.
type fixture struct {
	t   *testing.T
	db  *store.DB
	ix  *Index
	clk *clock.Fake
	chk *chunk.Chunker
}

func newFixture(t *testing.T, cfg ...Config) *fixture {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module, Migrations()); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t, reg)
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	c := Config{}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	ix, err := New(db, clk, c)
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: db, ix: ix, clk: clk, chk: chk}
}

func (f *fixture) scope(app, tenant, ws string) contracts.Scope {
	return storetest.Scope(app, tenant, ws)
}

func (f *fixture) source() string { return ids.New("src") }

func (f *fixture) comps() Components {
	emb := rag.ComponentVersion{Name: "granite-embedding", Version: "97m"}
	return Components{
		Parser:   rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"},
		Chunker:  f.chk.Component(),
		Embedder: &emb,
	}
}

// doc chunks Markdown text into a Document whose id is also the chunker's
// source id, as the Document comment advises.
func (f *fixture) doc(id, version, md string) Document {
	f.t.Helper()
	res, err := f.chk.Chunk(context.Background(), chunk.FromMarkdown(id, md))
	if err != nil {
		f.t.Fatalf("chunking %s: %v", id, err)
	}
	return Document{ID: id, Version: version, Chunks: res}
}

// spec is a document to build: its id and its Markdown.
type spec struct{ id, md string }

// begin starts a generation and puts the documents into it.
func (f *fixture) begin(scope contracts.Scope, source string, docs ...spec) Generation {
	f.t.Helper()
	ctx := context.Background()
	g, err := f.ix.BeginGeneration(ctx, scope, source, f.comps())
	if err != nil {
		f.t.Fatalf("BeginGeneration: %v", err)
	}
	for _, d := range docs {
		if _, err := f.ix.PutDocument(ctx, scope, string(g.GenerationID), f.doc(d.id, "v1", d.md)); err != nil {
			f.t.Fatalf("PutDocument %s: %v", d.id, err)
		}
	}
	return g
}

// validated is begin plus a passing validation.
func (f *fixture) validated(scope contracts.Scope, source string, docs ...spec) Generation {
	f.t.Helper()
	g := f.begin(scope, source, docs...)
	if _, err := f.ix.ValidateGeneration(context.Background(), scope, string(g.GenerationID), Expectation{}); err != nil {
		f.t.Fatalf("ValidateGeneration: %v", err)
	}
	return g
}

// serving is validated plus activation: the source is queryable.
func (f *fixture) serving(scope contracts.Scope, source string, docs ...spec) Generation {
	f.t.Helper()
	g := f.validated(scope, source, docs...)
	if err := f.ix.ActivateGeneration(context.Background(), scope, string(g.GenerationID)); err != nil {
		f.t.Fatalf("ActivateGeneration: %v", err)
	}
	return g
}

func (f *fixture) search(scope contracts.Scope, q Query) Result {
	f.t.Helper()
	res, err := f.ix.Search(context.Background(), scope, q)
	if err != nil {
		f.t.Fatalf("Search(%q): %v", q.Text, err)
	}
	return res
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// docIDs returns the document ids of the hits, in rank order.
func docIDs(res Result) []string {
	out := make([]string, len(res.Hits))
	for i, h := range res.Hits {
		out[i] = h.DocumentID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinHits(res Result) string {
	var b strings.Builder
	for _, h := range res.Hits {
		fmt.Fprintf(&b, "[%d %s score=%.3f] ", h.Rank, h.DocumentID, h.Score)
	}
	return b.String()
}
