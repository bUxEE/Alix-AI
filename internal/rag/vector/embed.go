package vector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// MaxJobChunks is the most chunk ids one embedding job carries. A job payload
// holds references only (internal/jobs: payloads are clear text and capped), so
// the ids of a batch must fit it comfortably.
const MaxJobChunks = 128

// DefaultEmbedBatch is how many chunk inputs go to the embedder per call. It
// stays under providers.DefaultBudget().MaxBatchItems.
const DefaultEmbedBatch = 32

// ChunkInput is what the embedding input of one chunk is made of.
type ChunkInput struct {
	ID string
	// Text is the chunk's text, verbatim.
	Text string
	// Header is the header row of a table row's table, and Path the heading
	// breadcrumb: context that is not part of the span-mapped Text but that a
	// reader needs to understand it ("Yes, within 30 days" under "Refunds").
	Header string
	Path   []string
}

// InputFromChunk maps a chunk of internal/rag/chunk to its embedding input.
func InputFromChunk(c chunk.Chunk) ChunkInput {
	return ChunkInput{ID: c.ID, Text: c.Text, Header: c.Header, Path: c.Path}
}

// EmbeddingText is the exact string sent to the embedder for a chunk: the
// heading path, the table header and the text, one per line, each only when
// present. It is THE embedding input, and the reuse key hashes this string,
// not chunk.ContentHash (which covers Text alone): a change to the path or the
// header changes the input, so it changes the key and the chunk is embedded
// again. The format is part of the stored data; changing it re-embeds
// everything, which is correct.
func EmbeddingText(c ChunkInput) string {
	var b strings.Builder
	if len(c.Path) > 0 {
		b.WriteString(joinPath(c.Path))
		b.WriteByte('\n')
	}
	if c.Header != "" {
		b.WriteString(c.Header)
		b.WriteByte('\n')
	}
	b.WriteString(c.Text)
	return b.String()
}

// ChunkSource supplies the chunks an embedding job refers to. The job carries
// ids, never text; the source (ingestion, which owns chunk storage) reads the
// text back, inside the job's scope and only for chunks that still exist and
// that may be indexed (a hidden or withdrawn chunk is simply not returned).
// It must return no chunk that was not asked for.
type ChunkSource interface {
	LoadChunks(ctx context.Context, scope contracts.Scope, generation string, ids []string) ([]ChunkInput, error)
}

// PipelineOptions is what NewPipeline needs.
type PipelineOptions struct {
	Index    VectorIndexProvider
	Embedder providers.Embedder
	Source   ChunkSource
	// Model is the embedding model's component identifier. It is part of every
	// reuse key and of every generation, and the embedder must report the same
	// model: a runtime that answers with another model than configured is
	// refused rather than recorded under the wrong id.
	Model string
	// BatchSize is how many inputs go to the embedder per call; 0 means
	// DefaultEmbedBatch.
	BatchSize int
	// Profile is an optional model-profile hint passed to the embedder.
	Profile string
	// Logger receives counts and ids, never chunk text or vectors.
	Logger *slog.Logger
}

// Pipeline turns chunks into stored vectors: reuse what the scope already
// holds, embed the rest, store. It never holds a database transaction across
// a model call (the index methods each take their own short transaction), so a
// slow embedder cannot stall the store's single writer.
type Pipeline struct {
	index    VectorIndexProvider
	embedder providers.Embedder
	source   ChunkSource
	model    string
	batch    int
	profile  string
	log      *slog.Logger
}

// NewPipeline validates the options.
func NewPipeline(opts PipelineOptions) (*Pipeline, error) {
	switch {
	case opts.Index == nil:
		return nil, invalid("pipeline has no index")
	case opts.Embedder == nil:
		return nil, invalid("pipeline has no embedder")
	case opts.Source == nil:
		return nil, invalid("pipeline has no chunk source")
	}
	if err := CheckID("model", opts.Model); err != nil {
		return nil, err
	}
	batch := opts.BatchSize
	if batch == 0 {
		batch = DefaultEmbedBatch
	}
	if batch < 0 || batch > MaxBatch {
		return nil, invalid("embedding batch size %d is outside 1 to %d", batch, MaxBatch)
	}
	p := &Pipeline{
		index: opts.Index, embedder: opts.Embedder, source: opts.Source,
		model: opts.Model, batch: batch, profile: opts.Profile, log: opts.Logger,
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p, nil
}

// Model returns the embedding model id the pipeline records.
func (p *Pipeline) Model() string { return p.model }

// EmbedRequest names the chunks of one generation to embed.
type EmbedRequest struct {
	Generation string   `json:"generation"`
	SourceID   string   `json:"source_id"`
	ChunkIDs   []string `json:"chunk_ids"`
}

// Validate checks the request's shape.
func (r EmbedRequest) Validate() error {
	if err := CheckID("generation", r.Generation); err != nil {
		return err
	}
	if err := CheckID("source id", r.SourceID); err != nil {
		return err
	}
	if len(r.ChunkIDs) == 0 {
		return invalid("embed request has no chunk ids")
	}
	if len(r.ChunkIDs) > MaxJobChunks {
		return invalid("embed request has %d chunk ids, over the limit of %d", len(r.ChunkIDs), MaxJobChunks)
	}
	for _, id := range r.ChunkIDs {
		if err := CheckID("chunk id", id); err != nil {
			return err
		}
	}
	return nil
}

// EmbedStats says what a run did. Reused + Embedded + Empty + Missing equals
// the number of distinct chunk ids requested.
type EmbedStats struct {
	// Missing chunks were not returned by the source (deleted or withdrawn
	// since the job was queued).
	Missing int
	// Empty chunks have no text to embed and get no vector.
	Empty int
	// Reused chunks got their vector without a model call: it was already in
	// the index for this scope, model and exact input.
	Reused int
	// Embedded chunks cost a model call.
	Embedded int
	// Calls is the number of embedder calls; InputToks is its estimated input
	// size, for the resource accounting.
	Calls     int
	InputToks int
}

// Embed runs one embedding job body. It is idempotent: a repeat finds what the
// first run stored by reuse and embeds only what is still missing, so a crash
// between batches, a retry and an at-least-once redelivery all converge.
func (p *Pipeline) Embed(ctx context.Context, scope contracts.Scope, req EmbedRequest) (EmbedStats, error) {
	var st EmbedStats
	if err := scope.Validate(); err != nil {
		return st, err
	}
	if err := req.Validate(); err != nil {
		return st, err
	}
	ids := Dedupe(req.ChunkIDs)
	asked := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		asked[id] = struct{}{}
	}

	got, err := p.source.LoadChunks(ctx, scope, req.Generation, ids)
	if err != nil {
		return st, err
	}
	byID := make(map[string]ChunkInput, len(got))
	for _, c := range got {
		if _, ok := asked[c.ID]; !ok {
			// A source that returns more than was asked for would make this job
			// embed (and store under this scope) content nobody authorised.
			return st, contracts.Newf(contracts.ErrInternal, "vector: the chunk source returned chunk %q, which was not requested", c.ID)
		}
		byID[c.ID] = c
	}

	type work struct {
		id   string
		text string
		key  InputKey
	}
	var todo []work
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			st.Missing++
			continue
		}
		text := EmbeddingText(c)
		if strings.TrimSpace(text) == "" {
			st.Empty++
			continue
		}
		key, err := NewInputKey(scope, p.model, text)
		if err != nil {
			return st, err
		}
		todo = append(todo, work{id: id, text: text, key: key})
	}

	if len(todo) > 0 && p.index.Info().Reuses {
		refs := make([]ReuseRef, len(todo))
		for i, w := range todo {
			refs[i] = ReuseRef{ChunkID: w.id, Key: w.key}
		}
		res, err := p.index.Reuse(ctx, scope, ReuseRequest{Generation: req.Generation, SourceID: req.SourceID, Model: p.model, Refs: refs})
		if err != nil {
			return st, err
		}
		reused := make(map[string]struct{}, len(res.Reused))
		for _, id := range res.Reused {
			reused[id] = struct{}{}
		}
		rest := todo[:0]
		for _, w := range todo {
			if _, ok := reused[w.id]; ok {
				st.Reused++
				continue
			}
			rest = append(rest, w)
		}
		todo = rest
	}

	for start := 0; start < len(todo); start += p.batch {
		if err := ctx.Err(); err != nil {
			return st, providers.ContextError(err)
		}
		batch := todo[start:min(start+p.batch, len(todo))]
		inputs := make([]string, len(batch))
		for i, w := range batch {
			inputs[i] = w.text
		}
		ereq := providers.EmbedRequest{Inputs: inputs, Purpose: providers.EmbedDocument, Profile: p.profile}
		res, err := p.embedder.Embed(ctx, scope, ereq)
		if err != nil {
			return st, err
		}
		if err := res.CheckResult(ereq); err != nil {
			return st, err
		}
		if res.Model != "" && res.Model != p.model {
			return st, contracts.Newf(contracts.ErrConflict,
				"vector: the embedding runtime answered with model %q but the index is configured for %q; refusing to store vectors under the wrong model id", res.Model, p.model)
		}
		st.Calls++
		st.InputToks += res.Usage.InputTokens
		items := make([]Item, len(batch))
		for i, w := range batch {
			items[i] = Item{ChunkID: w.id, Key: w.key, Vector: res.Vectors[i]}
		}
		if _, err := p.index.Upsert(ctx, scope, UpsertBatch{Generation: req.Generation, SourceID: req.SourceID, Model: p.model, Items: items}); err != nil {
			return st, err
		}
		st.Embedded += len(batch)
	}
	p.log.Debug("embedded chunks", "generation", req.Generation, "source", req.SourceID,
		"n_reused", st.Reused, "n_embedded", st.Embedded, "n_missing", st.Missing, "n_empty", st.Empty, "tok_in", st.InputToks)
	return st, nil
}

// String summarises the stats for logs.
func (s EmbedStats) String() string {
	return fmt.Sprintf("reused=%d embedded=%d missing=%d empty=%d calls=%d", s.Reused, s.Embedded, s.Missing, s.Empty, s.Calls)
}
