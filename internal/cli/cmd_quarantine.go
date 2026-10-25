package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The quarantine of isolated derived indexes in `alixai serve` (master plan 18 "SQLite/index
// corruption", 19; M5-20 library, M5-24 composition).
//
// `alixai upgrade apply|recover` isolates a damaged derived index: it records the index in
// <data dir>/upgrade/quarantine and empties it, so nothing is served from damaged bytes. An
// emptied index answers "nothing matched" to every question, so every reader of one has to say
// that it is isolated, and the readers live in this process. Two halves, both here:
//
//   - The labels. serve hands the quarantine of its data directory (quarantineOf) to retrieval
//     (query.Deps.Quarantine, in ragCompose) and to history (history.Deps.Quarantine, in
//     historyProvide). An isolated full-text index is not searched and retrieval carries the
//     reduced_semantic_recall notice and Diagnostics.Isolated; an isolated vector index leaves
//     retrieval full-text only with the same labels; history search, whose page has no field a
//     label could ride in, is unavailable until its index is rebuilt. A quarantine that cannot
//     be read reads as isolated (upgrade.Quarantine.Has fails closed).
//
//   - The release of the vector index. A full-text index is rebuilt by SQLite from its content
//     table, which the upgrade does itself. A vector is the output of the embedding model, so the
//     index can only be rebuilt where the embedder is: here. When an embedder role is registered
//     and the "rag" section names its model, serve runs a loop (quarantineStart) that, while the
//     vector index is isolated, re-embeds every chunk of every generation the vector tables still
//     name (quarantineReembed) and then releases the record (upgrade.ReleaseVector: rebuild, check,
//     clear, and no other record is touched). Without an embedder nothing can rebuild the index:
//     it stays isolated, retrieval stays labelled, and serve says so once at start.
//
// # What the re-embedding may and may not do
//
// It embeds only text the ingestion pipeline itself would embed (quarantineChunks repeats the
// rules of the ingestion's chunk reader: the scope, not tombstoned, not hidden (SEC-012), a
// generation that is building, validating or active) and only for a source this process serves
// right now: the scope it works under is the registration's own, and ragSources.Resolve decides
// whether the source is served. A generation whose source is not registered at all (removed) is
// skipped, ingestion's orphan sweep drops its vectors; a source that is registered and not served
// (its package is not active, it is being re-bound) holds the release back, and so does a
// generation built with another embedding model than the configured one: the index stays isolated
// and labelled rather than released with a hole in it. It yields to live work between batches (the
// resource governor) and stops while ingestion is paused for disk pressure. A failed pass is
// retried with a growing delay and never ends the loop; stopping serve cancels it and waits.

// quarantineProviderName is the name of the serve provider. Providers run in name order and this
// one needs the RAG stack, which "rag" builds.
const quarantineProviderName = "rag_quarantine"

// quarantineMaxDelayFactor bounds the retry delay of a failing pass to this many periods.
const quarantineMaxDelayFactor = 10

// Test seams, replaced only by tests.
var (
	// quarantineEvery is the period of the release loop, and the first retry delay.
	quarantineEvery = time.Minute
	// quarantineOnPass sees the result of every pass of the release loop.
	quarantineOnPass func(released bool, err error)
)

func init() { registerServeProvider(quarantineProviderName, quarantineProvide) }

// quarantineOf is the quarantine of the data directory serve runs on. Nil only for a serveDeps
// built by hand with no data directory, where nothing is isolated.
func quarantineOf(dataDir string, clk clock.Clock) *upgrade.Quarantine {
	if dataDir == "" {
		return nil
	}
	return upgrade.NewQuarantine(dataDir, clk)
}

// quarantineQueryOf is q as the quarantine retrieval consults; a missing one stays a nil
// interface, which query reads as "nothing is isolated".
func quarantineQueryOf(q *upgrade.Quarantine) query.Quarantine {
	if q == nil {
		return nil
	}
	return q
}

// quarantineHistoryOf is q as the quarantine history search consults.
func quarantineHistoryOf(q *upgrade.Quarantine) history.Quarantine {
	if q == nil {
		return nil
	}
	return q
}

// quarantineProvide starts the release loop when an embedder can re-embed the vector index. It
// builds no service of its own and a process with no RAG stack has no vector index to release.
func quarantineProvide(_ context.Context, d serveDeps) error {
	st, ok := gateway.Lookup[*ragStack](d.Services)
	if !ok {
		return nil
	}
	if st.quarantineVectorRebuild() == nil {
		if st.input.Quarantine.Has(upgrade.VectorIndexName) && d.Log != nil {
			d.Log.Warn("rag: the vector index is isolated and no embedder is configured to rebuild it: retrieval is full-text only and labelled reduced_semantic_recall until an embedder role and rag.embedding.model are configured and the chunks are embedded again")
		}
		return nil
	}
	if d.AddService != nil {
		d.AddService(st.quarantineStart)
	}
	return nil
}

// quarantineVectorRebuild is the upgrade.VectorRebuild of this process: it re-embeds the chunks
// with the embedder the ingestion pipeline uses. Nil while ingestion is text-only, because without
// an embedder there is nothing that can rebuild a vector.
func (s *ragStack) quarantineVectorRebuild() upgrade.VectorRebuild {
	if s.embedding == nil || s.vectors == nil || s.model == "" {
		return nil
	}
	return s.quarantineReembed
}

// quarantineStart is the service of the process (serveDeps.AddService): it runs a pass at once and
// then after every period until ctx ends. The function it returns stops the loop and waits for it
// (and for the embedding call in flight, which the cancellation ends) before the store closes.
func (s *ragStack) quarantineStart(ctx context.Context) (stop func()) {
	rebuild := s.quarantineVectorRebuild()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		delay := quarantineEvery
		for {
			if _, err := s.quarantineRelease(rctx, rebuild); err != nil {
				delay = min(delay*2, quarantineMaxDelayFactor*quarantineEvery)
			} else {
				delay = quarantineEvery
			}
			t := time.NewTimer(delay)
			select {
			case <-rctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// quarantineRelease is one pass: when the vector index is isolated, re-embed it and release the
// record. A pass over an index that is not isolated costs one lookup of the record.
func (s *ragStack) quarantineRelease(ctx context.Context, rebuild upgrade.VectorRebuild) (bool, error) {
	released, err := upgrade.ReleaseVector(ctx, s.input.Quarantine, s.input.DB, rebuild)
	switch {
	case released:
		s.log.Info("rag: the vector index was embedded again and released from the quarantine: retrieval is hybrid again")
	case err != nil && ctx.Err() == nil:
		s.log.Warn("rag: the isolated vector index could not be rebuilt yet; retrieval stays full-text only and labelled, and the pass is retried",
			slog.String("code", string(contracts.CodeOf(err))), slog.String("err_text", err.Error()))
	}
	if quarantineOnPass != nil {
		quarantineOnPass(released, err)
	}
	return released, err
}

// quarantineReembed embeds every chunk of every generation the vector tables name again. It
// returns nil only when no generation is left that it could not embed, which is what lets
// upgrade.ReleaseVector release the record: the first problem is returned after the others were
// tried, so one source that is down does not hold the rest back from being embedded.
func (s *ragStack) quarantineReembed(ctx context.Context, db *store.DB) error {
	emb := s.embedding
	pipe, err := vector.NewPipeline(vector.PipelineOptions{
		Index: s.vectors, Embedder: emb.Embedder, Source: quarantineChunks{db: db}, Model: emb.Model,
		BatchSize: emb.BatchSize, Profile: emb.Profile, Logger: s.log,
	})
	if err != nil {
		return err
	}
	gens, err := upgrade.VectorGenerations(ctx, db)
	if err != nil {
		return err
	}
	var first error
	for _, g := range gens {
		if err := s.quarantineGeneration(ctx, db, pipe, g); err != nil {
			if ctx.Err() != nil {
				return err
			}
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// quarantineGeneration embeds the chunks of one generation, in batches of the size an embedding
// job carries, under the scope its source was registered with.
func (s *ragStack) quarantineGeneration(ctx context.Context, db *store.DB, pipe *vector.Pipeline, g upgrade.VectorGeneration) error {
	e := s.sources.lookup(contracts.Scope{ApplicationID: g.ApplicationID, TenantID: g.TenantID, WorkspaceID: g.WorkspaceID}, g.SourceID)
	if e == nil {
		return nil // no source of this process: removed. Nothing serves it, and the orphan sweep drops its vectors.
	}
	scope := e.scope
	if g.Model != s.model {
		return contracts.Newf(contracts.ErrConflict,
			"rag: generation %s was built with embedding model %q and the configured one is %q; it cannot be re-embedded here, and the index stays isolated until ingestion replaces it",
			g.GenerationID, g.Model, s.model)
	}
	if _, err := s.sources.Resolve(ctx, scope, g.SourceID); err != nil {
		if errors.Is(err, ingest.ErrSourceGone) {
			return contracts.Wrap(contracts.ErrUnavailable, err,
				"rag: source "+g.SourceID+" is registered but not served now (its package is not active), so its vectors cannot be embedded again yet")
		}
		return err
	}
	after := ""
	for {
		ids, err := quarantineChunkIDs(ctx, db, scope, g.GenerationID, after, vector.MaxJobChunks)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := s.quarantineGate(ctx); err != nil {
			return err
		}
		if _, err := pipe.Embed(ctx, scope, vector.EmbedRequest{Generation: g.GenerationID, SourceID: g.SourceID, ChunkIDs: ids}); err != nil {
			return err
		}
		after = ids[len(ids)-1]
	}
}

// quarantineGate is the pause between two batches: ingestion paused for disk pressure holds the
// re-embedding too, and live work is yielded to (the resource governor, when serve has one).
func (s *ragStack) quarantineGate(ctx context.Context) error {
	if v := s.svc.Paused(ctx); v.Paused {
		return contracts.New(contracts.ErrUnavailable, "rag: ingestion is paused ("+v.Reason+"), so the vector index is not rebuilt now")
	}
	if r := s.input.Resources; r != nil && r.Gov != nil {
		return r.Gov.Yield(ctx, jobs.ClassIngest)
	}
	return nil
}

// quarantineVisible is the condition under which a chunk may be embedded: the rules of
// ingest's chunk reader.
const quarantineVisible = `c.generation_id = ? AND c.tombstoned_at IS NULL AND c.hidden = 0
 AND EXISTS (SELECT 1 FROM ragindex_generation AS g WHERE g.generation_id = c.generation_id
   AND g.application_id = c.application_id AND g.tenant_id = c.tenant_id AND g.workspace_id = c.workspace_id
   AND g.state IN ('building', 'validating', 'active'))`

// quarantineChunkIDs is the next page of the ids of the chunks of a generation that may be
// embedded, after the id `after`, inside the scope.
func quarantineChunkIDs(ctx context.Context, db *store.DB, scope contracts.Scope, generation, after string, limit int) ([]string, error) {
	rows, err := store.QueryScoped(ctx, db, scope, store.ScopedStmt{
		SQL: `SELECT DISTINCT c.chunk_id FROM ragindex_chunk AS c`, Alias: "c",
		Where: quarantineVisible + ` AND c.chunk_id > ?`, WhereArgs: []any{generation, after},
		Tail: `ORDER BY c.chunk_id LIMIT ?`, TailArgs: []any{limit},
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// quarantineChunks is the vector.ChunkSource of the re-embedding: the text of the chunks of one
// generation, by id, with the visibility rules of the ingestion's chunk reader (it asks for
// exactly the ids it is given and returns nothing hidden or withdrawn).
type quarantineChunks struct{ db *store.DB }

var _ vector.ChunkSource = quarantineChunks{}

// LoadChunks implements vector.ChunkSource.
func (c quarantineChunks) LoadChunks(ctx context.Context, scope contracts.Scope, generation string, ids []string) ([]vector.ChunkInput, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > vector.MaxJobChunks {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "rag: %d chunk ids is over the limit of %d", len(ids), vector.MaxJobChunks)
	}
	args := []any{generation}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := store.QueryScoped(ctx, c.db, scope, store.ScopedStmt{
		SQL: `SELECT c.chunk_id, c.text, c.header, c.path FROM ragindex_chunk AS c`, Alias: "c",
		Where:     quarantineVisible + ` AND c.chunk_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`,
		WhereArgs: args,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []vector.ChunkInput
	for rows.Next() {
		var in vector.ChunkInput
		var path string
		if err := rows.Scan(&in.ID, &in.Text, &in.Header, &path); err != nil {
			return nil, err
		}
		if path != "" && path != "[]" {
			if err := json.Unmarshal([]byte(path), &in.Path); err != nil {
				return nil, contracts.Wrap(contracts.ErrInternal, fmt.Errorf("chunk %s: %w", in.ID, err), "rag: a stored heading path is not valid JSON")
			}
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
