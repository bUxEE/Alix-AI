package ext

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The local search of the host application: the real lexical (FTS5) index and the authorised retrieval pipeline
// over the world's own database. No model of any kind is registered: search must not need one, local or external.

type searchEnv struct {
	t   testing.TB
	ix  *index.Index
	chk *chunk.Chunker
	cat *sourceCatalog
	pl  *query.Pipeline
}

// sourceCatalog is the deterministic authority on which sources the principal may read.
type sourceCatalog struct{ states map[string]query.SourceState }

func (c *sourceCatalog) Sources(_ context.Context, _ contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		if st, ok := c.states[id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

func newSearch(t testing.TB, db *store.DB) *searchEnv {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	ix, err := index.New(db, clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.Config{ChildMinToks: 0, ChildMaxToks: 64, ParentMaxToks: 1024})
	if err != nil {
		t.Fatal(err)
	}
	cat := &sourceCatalog{states: map[string]query.SourceState{}}
	pl, err := query.New(query.Deps{Lexical: ix, Catalog: cat, Hits: ix, Clock: clk}, query.DefaultConfig())
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	return &searchEnv{t: t, ix: ix, chk: chk, cat: cat, pl: pl}
}

// addSource indexes one Markdown document as an active, authorised, current source of scope.
func (e *searchEnv) addSource(scope contracts.Scope, name, markdown string) {
	e.t.Helper()
	ctx := context.Background()
	srcID := ids.New("src")
	g, err := e.ix.BeginGeneration(ctx, scope, srcID, index.Components{
		Parser:  rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"},
		Chunker: e.chk.Component(),
	})
	if err != nil {
		e.t.Fatalf("BeginGeneration: %v", err)
	}
	gen := string(g.GenerationID)
	res, err := e.chk.Chunk(ctx, chunk.FromMarkdown("doc-1", markdown))
	if err != nil {
		e.t.Fatalf("chunking: %v", err)
	}
	if _, err := e.ix.PutDocument(ctx, scope, gen, index.Document{ID: "doc-1", Version: "v1", Chunks: res}); err != nil {
		e.t.Fatalf("PutDocument: %v", err)
	}
	if _, err := e.ix.ValidateGeneration(ctx, scope, gen, index.Expectation{}); err != nil {
		e.t.Fatalf("ValidateGeneration: %v", err)
	}
	if err := e.ix.ActivateGeneration(ctx, scope, gen); err != nil {
		e.t.Fatalf("ActivateGeneration: %v", err)
	}
	e.cat.states[srcID] = query.SourceState{
		DisplayName: name, Authorised: true,
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
	}
}

func (e *searchEnv) search(scope contracts.Scope, text string) (query.Result, error) {
	return e.pl.Query(context.Background(), scope, query.Request{Text: text})
}
