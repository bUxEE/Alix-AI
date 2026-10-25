package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Stack is the retrieval side of the product in one database: the scoped
// full-text index, the flat vector index and the chunker, built the way the
// ingestion pipeline builds them (chunk, index, embed through the vector
// pipeline, validate, activate) but without the job queue and without the
// sandboxed parser, which a Markdown corpus does not need. What it measures is
// therefore chunk + index + embed throughput; the parser is not in it.
type Stack struct {
	DB    *store.DB
	Ix    *index.Index
	Flat  *vector.Flat // nil when no embedder: full-text only
	Chk   *chunk.Chunker
	Model string

	pipe *vector.Pipeline
	src  *memChunkSource
	comp rag.ComponentVersion
	path string
}

// StackOptions configures OpenStack.
type StackOptions struct {
	// Path is the database file.
	Path string
	// Embedder, when set, builds the vector index; Model is the id the index
	// records for it. With none the stack is full-text only.
	Embedder providers.Embedder
	Model    string
	// Chunk and Vector size the chunker and the vector index; zero values take
	// the shipped defaults.
	Chunk  chunk.Config
	Vector vector.Config
	// VectorCeilingScale raises the vector ceilings for the synthetic scale
	// corpora, which hold more than a Lite workspace is meant to.
	VectorCeilingScale bool
	// ReaderCacheKiB is the page cache of each reader connection, in KiB; 0 is
	// the store's default (2000, the Lite configuration).
	ReaderCacheKiB int
}

// OpenStack opens (creating it if need be) the database and the indexes over it.
func OpenStack(ctx context.Context, o StackOptions) (*Stack, error) {
	db, err := store.Open(ctx, store.Options{Path: o.Path, Config: store.Config{ReaderCacheKiB: o.ReaderCacheKiB}})
	if err != nil {
		return nil, fmt.Errorf("opening the store: %w", err)
	}
	s := &Stack{DB: db, Model: o.Model, path: o.Path, src: &memChunkSource{byGen: map[string]map[string]vector.ChunkInput{}}}
	fail := func(err error) (*Stack, error) {
		_ = db.Close()
		return nil, err
	}
	if s.Ix, err = index.New(db, nil, index.Config{}); err != nil {
		return fail(err)
	}
	cc := o.Chunk
	if cc == (chunk.Config{}) {
		cc = chunk.DefaultConfig()
	}
	if s.Chk, err = chunk.New(cc); err != nil {
		return fail(err)
	}
	if o.Embedder == nil {
		return s, nil
	}
	vc := o.Vector
	if vc == (vector.Config{}) {
		vc = vector.DefaultConfig()
	}
	if o.VectorCeilingScale {
		vc.MaxVectorsPerWorkspace, vc.MaxVectorsPerDeployment = 2_000_000, 4_000_000
	}
	if s.Flat, err = vector.NewFlat(db, vector.FlatOptions{Config: vc}); err != nil {
		return fail(err)
	}
	if s.pipe, err = vector.NewPipeline(vector.PipelineOptions{Index: s.Flat, Embedder: o.Embedder, Source: s.src, Model: o.Model}); err != nil {
		return fail(err)
	}
	s.comp = rag.ComponentVersion{Name: componentName(o.Model), Version: "1"}
	return s, nil
}

var componentUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// componentName turns a model id into a component name of an index generation.
func componentName(model string) string {
	n := strings.Trim(componentUnsafe.ReplaceAllString(strings.ToLower(model), "-"), "-._")
	if n == "" {
		n = "embedder"
	}
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// Close closes the database.
func (s *Stack) Close() error { return s.DB.Close() }

// DBPath is the database file.
func (s *Stack) DBPath() string { return s.path }

// StoreSize is what the database holds, read from SQLite's own page counts so
// that no checkpoint (which cannot run inside the store's write transaction) is
// needed: Used is the pages in use, Disk the files on disk (main file and
// write-ahead log), which is larger while the log has not been folded back.
// Used is the index size of the corpus: text, full-text index, parents,
// vectors and the bookkeeping tables around them.
type StoreSize struct {
	Used int64
	Disk int64
}

func (s *Stack) StoreSize(ctx context.Context) (StoreSize, error) {
	var pages, free, pageSize int64
	for _, q := range []struct {
		sql string
		dst *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &pageSize}} {
		if err := s.DB.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return StoreSize{}, fmt.Errorf("%s: %w", q.sql, err)
		}
	}
	sz := StoreSize{Used: (pages - free) * pageSize}
	for _, suffix := range []string{"", "-wal"} {
		st, err := os.Stat(s.path + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return StoreSize{}, err
		}
		sz.Disk += st.Size()
	}
	return sz, nil
}

// memChunkSource serves the chunks of the generation being embedded.
type memChunkSource struct {
	mu    sync.Mutex
	byGen map[string]map[string]vector.ChunkInput
}

func (m *memChunkSource) put(gen string, in []vector.ChunkInput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.byGen[gen]
	if g == nil {
		g = make(map[string]vector.ChunkInput, len(in))
		m.byGen[gen] = g
	}
	for _, c := range in {
		g[c.ID] = c
	}
}

func (m *memChunkSource) drop(gen string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byGen, gen)
}

func (m *memChunkSource) LoadChunks(_ context.Context, _ contracts.Scope, gen string, want []string) ([]vector.ChunkInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.byGen[gen]
	out := make([]vector.ChunkInput, 0, len(want))
	for _, id := range want {
		if c, ok := g[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// Doc is one document to index: its id in the source and its Markdown.
type Doc struct {
	ID   string
	Text string
}

// IngestResult says what building one generation cost.
type IngestResult struct {
	GenerationID string
	Docs         int
	Parents      int
	Children     int
	Bytes        int64
	Chunking     time.Duration
	Indexing     time.Duration
	Embedding    time.Duration
	Finalizing   time.Duration
	Embedded     int
	Reused       int
	EmbedCalls   int
}

// Total is the wall time of the build.
func (r IngestResult) Total() time.Duration {
	return r.Chunking + r.Indexing + r.Embedding + r.Finalizing
}

// Add accumulates another build.
func (r *IngestResult) Add(o IngestResult) {
	r.Docs += o.Docs
	r.Parents += o.Parents
	r.Children += o.Children
	r.Bytes += o.Bytes
	r.Chunking += o.Chunking
	r.Indexing += o.Indexing
	r.Embedding += o.Embedding
	r.Finalizing += o.Finalizing
	r.Embedded += o.Embedded
	r.Reused += o.Reused
	r.EmbedCalls += o.EmbedCalls
}

// IngestHooks lets a caller observe what a build produced.
type IngestHooks struct {
	// OnChunks is called with every document's chunking, before it is indexed.
	OnChunks func(sourceID string, doc Doc, res chunk.Result)
}

// Ingest builds one generation of a source from docs and activates it: chunk
// every document, index it, embed its children (when the stack has an
// embedder), validate the generation against the counts the build knows it
// wrote, and flip it active. The generation it replaces stops serving at that
// moment.
func (s *Stack) Ingest(ctx context.Context, scope contracts.Scope, sourceID, version string, docs []Doc, hooks IngestHooks) (IngestResult, error) {
	var chunking time.Duration
	var bytesIn int64
	indexed := make([]index.Document, 0, len(docs))
	for _, d := range docs {
		t0 := startWatch()
		res, err := s.Chk.Chunk(ctx, chunk.FromMarkdown(d.ID, d.Text))
		if err != nil {
			return IngestResult{}, fmt.Errorf("chunking %s: %w", d.ID, err)
		}
		chunking += t0.elapsed()
		if hooks.OnChunks != nil {
			hooks.OnChunks(sourceID, d, res)
		}
		bytesIn += int64(len(d.Text))
		indexed = append(indexed, index.Document{ID: d.ID, Version: version, Chunks: res})
	}
	r, err := s.IngestChunked(ctx, scope, sourceID, indexed)
	r.Chunking, r.Bytes = chunking, bytesIn
	return r, err
}

// IngestChunked is Ingest for documents that are already chunked (the
// synthetic scale corpus builds exact chunk counts by hand). Chunking is the
// caller's to time.
func (s *Stack) IngestChunked(ctx context.Context, scope contracts.Scope, sourceID string, docs []index.Document) (IngestResult, error) {
	var r IngestResult
	comps := index.Components{
		Parser:  rag.ComponentVersion{Name: "rageval-markdown", Version: "1"},
		Chunker: s.Chk.Component(),
	}
	if s.pipe != nil {
		c := s.comp
		comps.Embedder = &c
	}
	g, err := s.Ix.BeginGeneration(ctx, scope, sourceID, comps)
	if err != nil {
		return r, fmt.Errorf("beginning a generation of %s: %w", sourceID, err)
	}
	r.GenerationID = string(g.GenerationID)

	var inputs []vector.ChunkInput
	for _, d := range docs {
		t1 := startWatch()
		if _, err := s.Ix.PutDocument(ctx, scope, r.GenerationID, d); err != nil {
			return r, fmt.Errorf("indexing %s: %w", d.ID, err)
		}
		r.Indexing += t1.elapsed()
		r.Docs++
		r.Parents += len(d.Chunks.Parents)
		r.Children += len(d.Chunks.Children)
		if s.pipe != nil {
			for _, c := range d.Chunks.Children {
				inputs = append(inputs, vector.InputFromChunk(c))
			}
		}
	}
	if s.pipe != nil {
		t0 := startWatch()
		if err := s.embed(ctx, scope, sourceID, r.GenerationID, inputs, &r); err != nil {
			return r, err
		}
		r.Embedding = t0.elapsed()
	}

	t0 := startWatch()
	docsN, chunksN := r.Docs, r.Children
	if rep, err := s.Ix.ValidateGeneration(ctx, scope, r.GenerationID, index.Expectation{Documents: &docsN, Chunks: &chunksN}); err != nil {
		return r, fmt.Errorf("validating the generation of %s: %w (%v)", sourceID, err, rep.Problems)
	}
	if s.pipe != nil {
		if err := s.Ix.SetEmbeddedChunks(ctx, scope, r.GenerationID, r.Embedded+r.Reused); err != nil && contracts.CodeOf(err) != contracts.ErrValidationFailed {
			return r, err
		}
	}
	if err := s.Ix.ActivateGeneration(ctx, scope, r.GenerationID); err != nil {
		return r, fmt.Errorf("activating the generation of %s: %w", sourceID, err)
	}
	r.Finalizing = t0.elapsed()
	return r, nil
}

// embed runs the vector pipeline over the children in jobs of at most
// vector.MaxJobChunks, as the queue would.
func (s *Stack) embed(ctx context.Context, scope contracts.Scope, sourceID, gen string, inputs []vector.ChunkInput, r *IngestResult) error {
	s.src.put(gen, inputs)
	defer s.src.drop(gen)
	for start := 0; start < len(inputs); start += vector.MaxJobChunks {
		batch := inputs[start:min(start+vector.MaxJobChunks, len(inputs))]
		idsIn := make([]string, len(batch))
		for i, c := range batch {
			idsIn[i] = c.ID
		}
		st, err := s.pipe.Embed(ctx, scope, vector.EmbedRequest{Generation: gen, SourceID: sourceID, ChunkIDs: idsIn})
		if err != nil {
			return fmt.Errorf("embedding generation %s: %w", gen, err)
		}
		if st.Missing > 0 {
			return fmt.Errorf("embedding generation %s: %d chunks were missing from the chunk source", gen, st.Missing)
		}
		r.Embedded += st.Embedded
		r.Reused += st.Reused
		r.EmbedCalls += st.Calls
	}
	return nil
}

// newTempDB returns a path for a database in a fresh temporary directory and a
// cleanup that removes it. The directory is 0700.
func newTempDB(dir string) (path string, cleanup func(), err error) {
	d, err := os.MkdirTemp(dir, "rageval-*")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(d, "rag.db"), func() { _ = os.RemoveAll(d) }, nil
}

// srcID makes a source id.
func newSourceID() string { return ids.New("src") }
