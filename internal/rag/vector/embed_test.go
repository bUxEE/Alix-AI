package vector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	testModel = "fake-model" // what the fake embedder reports
	embedDim  = 128
)

var bg = context.Background()

// memSource is a ChunkSource over a map, per scope, that records what it was asked.
type memSource struct {
	mu     sync.Mutex
	chunks map[string]map[string]ChunkInput // scope key -> chunk id -> input
	loads  int
	extra  *ChunkInput // returned in addition to what was asked, to test the guard
}

func scopeKey(s contracts.Scope) string {
	return s.ApplicationID + "/" + s.TenantID + "/" + s.WorkspaceID
}

func newMemSource() *memSource { return &memSource{chunks: map[string]map[string]ChunkInput{}} }

func (m *memSource) put(scope contracts.Scope, cs ...ChunkInput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := scopeKey(scope)
	if m.chunks[k] == nil {
		m.chunks[k] = map[string]ChunkInput{}
	}
	for _, c := range cs {
		m.chunks[k][c.ID] = c
	}
}

func (m *memSource) LoadChunks(_ context.Context, scope contracts.Scope, _ string, ids []string) ([]ChunkInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	var out []ChunkInput
	for _, id := range ids {
		if c, ok := m.chunks[scopeKey(scope)][id]; ok {
			out = append(out, c)
		}
	}
	if m.extra != nil {
		out = append(out, *m.extra)
	}
	return out, nil
}

// pipelineFixture is a flat index, a scripted fake embedder and a memSource.
type pipelineFixture struct {
	t     *testing.T
	db    *store.DB
	index *Flat
	emb   *fake.Provider
	src   *memSource
	pipe  *Pipeline
}

func newPipelineFixture(t *testing.T, batch int) *pipelineFixture {
	t.Helper()
	f := &pipelineFixture{t: t, emb: fake.New(fake.WithEmbedDim(embedDim)), src: newMemSource()}
	f.index, f.db = newFlatIndex(t, cfgCache())
	p, err := NewPipeline(PipelineOptions{Index: f.index, Embedder: f.emb, Source: f.src, Model: testModel, BatchSize: batch})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	f.pipe = p
	return f
}

func (f *pipelineFixture) embed(scope contracts.Scope, gen, source string, ids ...string) EmbedStats {
	f.t.Helper()
	st, err := f.pipe.Embed(bg, scope, EmbedRequest{Generation: gen, SourceID: source, ChunkIDs: ids})
	if err != nil {
		f.t.Fatalf("Embed: %v", err)
	}
	return st
}

// embedded returns every text the fake embedder has been asked to embed.
func (f *pipelineFixture) embedded() []string {
	var out []string
	for _, p := range f.emb.Prompts() {
		out = append(out, p.Embed.Inputs...)
	}
	return out
}

func chunkSet(prefix string, n int) []ChunkInput {
	out := make([]ChunkInput, n)
	for i := range out {
		out[i] = ChunkInput{
			ID:   fmt.Sprintf("chk_%s%03d", prefix, i),
			Text: fmt.Sprintf("para%s%d alpha%d bravo%d charlie%d delta%d", prefix, i, i*7919, i*104729, i*1299709, i*15485863),
			Path: []string{"Guide", "Section " + prefix},
		}
	}
	return out
}

func idsOf(cs []ChunkInput) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// Master plan 12.3: unchanged chunks reuse embeddings. A second generation built
// from the same chunks (a parser or chunker version bump that leaves the text
// alone) costs no model call, and the reused vectors answer searches.
func TestEmbeddingReuseUnchangedChunk(t *testing.T) {
	f := newPipelineFixture(t, 32)
	scope := storetest.Scope("app", "tenA", "ws1")
	cs := chunkSet("a", 40)
	f.src.put(scope, cs...)

	first := f.embed(scope, "gen_1", "src_1", idsOf(cs)...)
	if first.Embedded != 40 || first.Reused != 0 || first.Calls != 2 {
		t.Fatalf("first run = %v, want 40 embedded in 2 calls", first)
	}
	callsAfterFirst := f.emb.Calls()

	// A repeat of the same job (at-least-once delivery) converges without a model call.
	again := f.embed(scope, "gen_1", "src_1", idsOf(cs)...)
	if again.Embedded != 0 || again.Reused != 40 || f.emb.Calls() != callsAfterFirst {
		t.Fatalf("repeat run = %v with %d new embedder calls, want everything reused", again, f.emb.Calls()-callsAfterFirst)
	}

	// A NEW generation of the same chunks: still no model call.
	second := f.embed(scope, "gen_2", "src_1", idsOf(cs)...)
	if second.Embedded != 0 || second.Reused != 40 || f.emb.Calls() != callsAfterFirst {
		t.Fatalf("second generation = %v with %d new embedder calls, want everything reused", second, f.emb.Calls()-callsAfterFirst)
	}
	st, err := f.index.Stats(bg, scope)
	if err != nil || len(st.Generations) != 2 || st.Generations[0].Vectors != 40 || st.Generations[1].Vectors != 40 || st.Vectors != 80 {
		t.Fatalf("stats = %+v (%v), want two generations of 40", st, err)
	}

	// The copied vectors are real: querying with a chunk's own embedding finds it in gen_2.
	q := fake.EmbedVector(EmbeddingText(cs[7]), embedDim)
	res, err := f.index.Search(bg, scope, Query{Vector: q, Model: testModel, K: 1, Generations: []string{"gen_2"}})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].ChunkID != cs[7].ID || res.Hits[0].Score < 0.98 {
		t.Fatalf("search in the reused generation = %+v (%v), want %s", res, err, cs[7].ID)
	}

	// Only the changed chunks are embedded again: one paragraph edited, one chunk
	// whose heading path changed (the embedding input changed although the Text
	// did not), the rest reused.
	edited := cs[3]
	edited.Text += " and an added sentence"
	renamed := cs[9]
	renamed.Path = []string{"Guide", "Renamed section"}
	f.src.put(scope, edited, renamed)
	before := len(f.embedded())
	third := f.embed(scope, "gen_3", "src_1", idsOf(cs)...)
	if third.Embedded != 2 || third.Reused != 38 {
		t.Fatalf("after one edit and one rename = %v, want 2 embedded and 38 reused", third)
	}
	fresh := f.embedded()[before:]
	joined := strings.Join(fresh, "\n")
	if len(fresh) != 2 || !strings.Contains(joined, "added sentence") || !strings.Contains(joined, "Renamed section") {
		t.Fatalf("the embedder was sent %q, want exactly the edited and the renamed chunk", fresh)
	}
}

// SEC-023: reuse must not become a cross-tenant existence oracle. Identical
// content in another tenant, workspace or application is embedded afresh (the
// embedder is called exactly as if the first scope did not exist), shares no
// row, and the keys are unrelated.
func TestSecEmbeddingReuseDoesNotCrossTenants(t *testing.T) {
	f := newPipelineFixture(t, 32)
	a := storetest.Scope("app", "tenA", "ws1")
	cs := chunkSet("same", 6)
	f.src.put(a, cs...)
	f.embed(a, "gen_a", "src_1", idsOf(cs)...)
	calls := f.emb.Calls()

	for name, other := range map[string]contracts.Scope{
		"another tenant":      storetest.Scope("app", "tenB", "ws1"),
		"another workspace":   storetest.Scope("app", "tenA", "ws2"),
		"another application": storetest.Scope("appX", "tenA", "ws1"),
	} {
		f.src.put(other, cs...) // byte-identical content, identical chunk ids
		st := f.embed(other, "gen_a", "src_1", idsOf(cs)...)
		if st.Reused != 0 || st.Embedded != len(cs) {
			t.Fatalf("%s: %v, want every chunk embedded afresh and none reused", name, st)
		}
		if f.emb.Calls() <= calls {
			t.Fatalf("%s: the embedder was not called for identical content: reuse crossed a scope boundary", name)
		}
		calls = f.emb.Calls()

		// The same observable shape as a scope that never met the content: the
		// stats of each scope count only its own vectors.
		stats, err := f.index.Stats(bg, other)
		if err != nil || stats.Vectors != len(cs) {
			t.Fatalf("%s: stats = %+v (%v)", name, stats, err)
		}
	}

	// The raw rows: no two scopes hold rows under the same input key.
	rows, err := f.db.QueryContext(bg,
		`SELECT input_key FROM vector_rows GROUP BY input_key HAVING count(DISTINCT application_id || '/' || tenant_id || '/' || workspace_id) > 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("two scopes hold rows under the same input key: the key does not include the scope")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedFailsClosed(t *testing.T) {
	scope := storetest.Scope("app", "tenA", "ws1")

	t.Run("a model that differs from the configured one is refused", func(t *testing.T) {
		f := newPipelineFixture(t, 8)
		f.pipe.model = "configured-model-v2"
		cs := chunkSet("m", 3)
		f.src.put(scope, cs...)
		_, err := f.pipe.Embed(bg, scope, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(cs)})
		if contracts.CodeOf(err) != contracts.ErrConflict {
			t.Fatalf("err = %v, want conflict", err)
		}
		if st, _ := f.index.Stats(bg, scope); st.Vectors != 0 {
			t.Fatalf("vectors were stored under the wrong model: %+v", st)
		}
	})

	t.Run("a source that returns an unrequested chunk is refused", func(t *testing.T) {
		f := newPipelineFixture(t, 8)
		cs := chunkSet("x", 3)
		f.src.put(scope, cs...)
		f.src.extra = &ChunkInput{ID: "chk_smuggled", Text: "content nobody asked for"}
		_, err := f.pipe.Embed(bg, scope, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(cs)})
		if contracts.CodeOf(err) != contracts.ErrInternal {
			t.Fatalf("err = %v, want internal", err)
		}
		if f.emb.Calls() != 0 {
			t.Fatal("the embedder was called after the source misbehaved")
		}
	})

	t.Run("missing and empty chunks are counted, not embedded", func(t *testing.T) {
		f := newPipelineFixture(t, 8)
		cs := chunkSet("e", 2)
		f.src.put(scope, cs...)
		f.src.put(scope, ChunkInput{ID: "chk_blank", Text: "   \n"})
		st := f.embed(scope, "gen_1", "src_1", append(idsOf(cs), "chk_blank", "chk_gone")...)
		if st.Embedded != 2 || st.Empty != 1 || st.Missing != 1 {
			t.Fatalf("stats = %v, want 2 embedded, 1 empty, 1 missing", st)
		}
	})

	t.Run("an invalid scope is refused before anything runs", func(t *testing.T) {
		f := newPipelineFixture(t, 8)
		_, err := f.pipe.Embed(bg, contracts.Scope{}, EmbedRequest{Generation: "g", SourceID: "s", ChunkIDs: []string{"c"}})
		if c := contracts.CodeOf(err); c != contracts.ErrScopeMissing {
			t.Fatalf("err = %v, want scope_missing", err)
		}
		if f.src.loads != 0 {
			t.Fatal("the source was read without a scope")
		}
	})

	t.Run("an embedder failure keeps its retryable code and stores nothing", func(t *testing.T) {
		f := newPipelineFixture(t, 8)
		cs := chunkSet("u", 3)
		f.src.put(scope, cs...)
		f.emb.FailNext(fake.FailUnavailable)
		_, err := f.pipe.Embed(bg, scope, EmbedRequest{Generation: "gen_1", SourceID: "src_1", ChunkIDs: idsOf(cs)})
		if c := contracts.CodeOf(err); c != contracts.ErrUnavailable {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if st, _ := f.index.Stats(bg, scope); st.Vectors != 0 {
			t.Fatalf("a failed embed stored %+v", st)
		}
		// The retry succeeds.
		if st := f.embed(scope, "gen_1", "src_1", idsOf(cs)...); st.Embedded != 3 {
			t.Fatalf("retry = %v", st)
		}
	})
}

func TestEmbeddingTextIsTheExactInput(t *testing.T) {
	c := ChunkInput{ID: "c", Text: "Yes, within 30 days.", Header: "Question | Answer", Path: []string{"Policies", "Refunds"}}
	want := "Policies > Refunds\nQuestion | Answer\nYes, within 30 days."
	if got := EmbeddingText(c); got != want {
		t.Fatalf("EmbeddingText = %q, want %q", got, want)
	}
	if got := EmbeddingText(ChunkInput{Text: "plain"}); got != "plain" {
		t.Fatalf("a chunk with no context embeds its text unchanged, got %q", got)
	}
	// The reuse key follows the composed input, not the bare text.
	scope := storetest.Scope("app", "tenA", "ws1")
	k1, _ := NewInputKey(scope, "m", EmbeddingText(c))
	c.Path = []string{"Policies", "Returns"}
	k2, _ := NewInputKey(scope, "m", EmbeddingText(c))
	if k1 == k2 {
		t.Fatal("a changed heading path left the reuse key unchanged: the key does not cover the actual input")
	}
}
