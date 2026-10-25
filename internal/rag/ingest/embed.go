package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// vectorBatch is how many chunk ids one embedding job carries.
const vectorBatch = vector.MaxJobChunks

// embedRow is a row of ragingest_embed.
type embedRow struct {
	VersionID string
	Batch     int
	Count     int
	JobID     string
	Requeues  int
}

// pumpEmbeds queues the embedding jobs of indexed versions: the waiting
// batches of one version, or of every version of the source that still has
// some. The embedding job is the one of internal/rag/vector (class 5): it
// reuses the embeddings the scope already holds for the same chunk id and exact
// input and embeds only the rest, in bounded batches, and each batch is its own
// job, so a model that is down delays embeddings and nothing else. With dead
// set it queues a batch again when its job ended failed (at most maxRequeues
// times; then the batch is given up and those chunks stay full-text only).
func (s *Service) pumpEmbeds(ctx context.Context, scope contracts.Scope, serviceIdentity, sourceID, versionID string, dead bool) error {
	where, args := `done_ns = 0 AND version_id IN (SELECT version_id FROM ragingest_version WHERE source_id = ? AND state = 'indexed' AND embed_state = 'pending')`, []any{sourceID}
	if versionID != "" {
		where, args = `done_ns = 0 AND version_id = ?`, []any{versionID}
	}
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT version_id, batch_no, chunk_count, job_id, requeues FROM ragingest_embed`, Where: where, WhereArgs: args,
		Tail: `ORDER BY version_id, batch_no LIMIT 512`,
	})
	if err != nil {
		return err
	}
	var todo []embedRow
	for rows.Next() {
		var r embedRow
		if err := rows.Scan(&r.VersionID, &r.Batch, &r.Count, &r.JobID, &r.Requeues); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	versions := map[string]versionRow{}
	for _, r := range todo {
		if r.JobID != "" {
			if !dead {
				continue
			}
			alive, err := s.jobAlive(ctx, scope, r.JobID)
			if err != nil {
				return err
			}
			if alive {
				continue
			}
			if r.Requeues >= maxRequeues {
				if _, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
					SQL: `UPDATE ragingest_embed SET done_ns = -1`, Where: `version_id = ? AND batch_no = ? AND done_ns = 0`, WhereArgs: []any{r.VersionID, r.Batch},
				}); err != nil {
					return err
				}
				if err := s.embedSettled(ctx, scope, r.VersionID); err != nil {
					return err
				}
				continue
			}
			r.Requeues++
		}
		v, ok := versions[r.VersionID]
		if !ok {
			var err error
			if v, err = getVersion(ctx, s.db, scope, r.VersionID); err != nil {
				if isGone(err) {
					continue
				}
				return err
			}
			versions[r.VersionID] = v
		}
		lo, hi := r.Batch*vectorBatch, min((r.Batch+1)*vectorBatch, len(v.ChunkIDs))
		if lo >= hi {
			continue
		}
		res, err := s.enqueue(ctx, scope, serviceIdentity, vector.EmbedJobType,
			vector.EmbedRequest{Generation: v.GenerationID, SourceID: v.SourceID, ChunkIDs: v.ChunkIDs[lo:hi]},
			embedKey(v.ID, r.Batch, r.Requeues), time.Time{})
		if err != nil {
			return err
		}
		if _, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
			SQL: `UPDATE ragingest_embed SET job_id = ?, requeues = ?`, Args: []any{res.Job.ID, r.Requeues},
			Where: `version_id = ? AND batch_no = ? AND done_ns = 0`, WhereArgs: []any{r.VersionID, r.Batch},
		}); err != nil {
			return err
		}
	}
	return nil
}

// embedHandler is the handler of vector.EmbedJobType: the embedding job of the
// vector pipeline, run under the job's own scope, with the batch bookkeeping
// that makes "this generation is fully embedded" a count.
func (s *Service) embedHandler(ctx context.Context, job jobs.Job) error {
	var req vector.EmbedRequest
	dec := json.NewDecoder(bytes.NewReader(job.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return jobs.Permanent(contracts.Wrap(contracts.ErrValidationFailed, err, "ingest: embed job payload is malformed"))
	}
	if err := req.Validate(); err != nil {
		return jobs.Permanent(err)
	}
	if src, ok, err := loadSource(ctx, s.db, job.Scope, req.SourceID); err != nil {
		return err
	} else if !ok || src.Availability == availRemoved {
		return nil
	}
	if err := s.checkPause(ctx); err != nil {
		return err
	}
	st, err := s.pipe.Embed(ctx, job.Scope, req)
	if err != nil {
		return err
	}
	s.log.Info("embedding job done", "job", job.ID, "generation", req.Generation, "n_reused", st.Reused, "n_embedded", st.Embedded, "n_missing", st.Missing)
	vid, batch, ok := parseEmbedKey(job.IdempotencyKey)
	if !ok {
		return nil
	}
	if err := s.embedBatchDone(ctx, job.Scope, vid, batch, req, st); err != nil {
		return err
	}
	// A finished embedding job frees a place in the queue: waiting versions of
	// the source are queued into it. Housekeeping: the batch is done either way.
	if _, err := s.pump(ctx, job.Scope, job.ServiceIdentity, req.SourceID, s.cfg.EnqueueBatch, false); err != nil {
		s.log.Warn("the ingestion window could not be topped up", "source", req.SourceID, "err", err.Error())
	}
	return nil
}

// parseEmbedKey reads the version and batch out of the idempotency key
// embedKey made.
func parseEmbedKey(key string) (versionID string, batch int, ok bool) {
	rest, found := strings.CutPrefix(key, "ie:")
	if !found {
		return "", 0, false
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return parts[0], n, true
}

// embedBatchDone records a finished batch; recording it twice (a job delivered
// twice) counts it once. A batch with no row left to record was either
// recorded already or its version was superseded while the model ran; see
// dropLateVectors.
func (s *Service) embedBatchDone(ctx context.Context, scope contracts.Scope, versionID string, batch int, req vector.EmbedRequest, st vector.EmbedStats) error {
	changed := false
	if err := s.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `UPDATE ragingest_embed SET embedded = ?, done_ns = ?`, Args: []any{st.Reused + st.Embedded, nsOf(s.clk.Now())},
			Where: `version_id = ? AND batch_no = ? AND done_ns = 0`, WhereArgs: []any{versionID, batch},
		})
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		changed = n > 0
		return err
	}); err != nil {
		return err
	}
	if !changed {
		return s.dropLateVectors(ctx, scope, versionID, req)
	}
	return s.embedSettled(ctx, scope, versionID)
}

// dropLateVectors removes the vectors a job stored for a version that was
// superseded (replaced or deleted) while the model ran. Superseding a version
// deletes its batch rows in the same transaction, so a batch that still had its
// row was stored before that and cleanupVectors, which follows it, removed it.
// One without its row may have been stored after the cleanup, and nothing else
// would ever remove it. The vector index writes in its own transactions, so
// the check and the delete run under the source's write lock instead: no
// successor version becomes indexed in between. Chunks a live version of the
// item also holds are kept, as cleanupVectors keeps them.
func (s *Service) dropLateVectors(ctx context.Context, scope contracts.Scope, versionID string, req vector.EmbedRequest) error {
	if s.vec == nil {
		return nil
	}
	unlock := s.lock(scope, req.SourceID)
	defer unlock()
	keep := map[string]bool{}
	v, err := getVersion(ctx, s.db, scope, versionID)
	switch {
	case isGone(err):
		// The source or the generation was removed with its versions.
	case err != nil:
		return err
	case v.State != vSuperseded:
		return nil // a repeat delivery of a batch already recorded
	default:
		live, err := queryVersions(ctx, s.db, scope, `generation_id = ? AND item_key = ? AND state = 'indexed' AND vec_owed = 0`, []any{req.Generation, v.Key}, "")
		if err != nil {
			return err
		}
		for _, l := range live {
			for _, id := range l.ChunkIDs {
				keep[id] = true
			}
		}
	}
	var late []string
	for _, id := range req.ChunkIDs {
		if !keep[id] {
			late = append(late, id)
		}
	}
	if len(late) == 0 {
		return nil
	}
	_, err = s.vec.DeleteChunks(ctx, scope, req.Generation, late)
	return err
}

// embedSettled runs after a batch reached a final state: when the version has
// no batch left it is marked embedded, the generation's embedded count is
// refreshed, and once the serving generation is fully embedded the vectors of
// the generations it replaced are released.
func (s *Service) embedSettled(ctx context.Context, scope contracts.Scope, versionID string) error {
	v, err := getVersion(ctx, s.db, scope, versionID)
	if err != nil {
		if isGone(err) {
			return nil
		}
		return err
	}
	remaining, err := s.countEmbedRows(ctx, scope, versionID, `done_ns = 0`)
	if err != nil {
		return err
	}
	if remaining == 0 {
		if _, err := setVersion(ctx, s.db, scope, v.ID, `embed_state = 'done', updated_ns = ?`, []any{nsOf(s.clk.Now())}, vIndexed); err != nil {
			return err
		}
	}
	if err := s.refreshEmbedded(ctx, scope, v.GenerationID); err != nil {
		return err
	}
	if remaining == 0 {
		return s.dropRetiredVectors(ctx, scope, v.SourceID)
	}
	return nil
}

func (s *Service) countEmbedRows(ctx context.Context, scope contracts.Scope, versionID, where string) (int, error) {
	q, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT count(*) FROM ragingest_embed`, Where: `version_id = ? AND ` + where, WhereArgs: []any{versionID},
	})
	if err != nil {
		return 0, err
	}
	var n int
	return n, q.Scan(&n)
}

// refreshEmbedded writes the generation's count of embedded chunks.
func (s *Service) refreshEmbedded(ctx context.Context, scope contracts.Scope, genID string) error {
	var sum int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(e.embedded), 0) FROM ragingest_embed AS e
 JOIN ragingest_version AS v ON v.version_id = e.version_id AND v.application_id = e.application_id
   AND v.tenant_id = e.tenant_id AND v.workspace_id = e.workspace_id
 WHERE e.application_id = ? AND e.tenant_id = ? AND e.workspace_id = ? AND v.generation_id = ? AND v.state = 'indexed' AND e.done_ns > 0`,
		scope.ApplicationID, scope.TenantID, scope.WorkspaceID, genID).Scan(&sum); err != nil {
		return err
	}
	ig, err := s.ix.GetGeneration(ctx, scope, genID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	n := min(sum, int(ig.Counts.Chunks))
	if err := s.ix.SetEmbeddedChunks(ctx, scope, genID, n); err != nil && contracts.CodeOf(err) != contracts.ErrValidationFailed {
		return err
	}
	return nil
}

// dropRetiredVectors releases the embeddings of generations that no longer
// serve, once the serving generation holds its own: a generation that was
// replaced is the source of the embeddings the new one reuses (same chunk id,
// same exact input), so they are kept until the new one is complete. A
// rollback to a generation whose vectors were released is full-text only until
// they are rebuilt.
func (s *Service) dropRetiredVectors(ctx context.Context, scope contracts.Scope, sourceID string) error {
	if s.vec == nil {
		return nil
	}
	active, ok, err := s.ix.ActiveGeneration(ctx, scope, sourceID)
	if err != nil || !ok {
		return err
	}
	pending, err := countVersions(ctx, s.db, scope, `generation_id = ? AND state = 'indexed' AND embed_state = 'pending'`, string(active.GenerationID))
	if err != nil || pending > 0 {
		return err
	}
	gens, err := listGens(ctx, s.db, scope, sourceID)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if g.VectorsDropped || g.ID == string(active.GenerationID) {
			continue
		}
		ig, err := s.ix.GetGeneration(ctx, scope, g.ID)
		switch {
		case err != nil && !isNotFound(err):
			return err
		case err == nil && (ig.State == rag.IndexGenerationStateBuilding || ig.State == rag.IndexGenerationStateValidating || ig.State == rag.IndexGenerationStateActive):
			continue
		}
		if err := s.dropGenerationVectors(ctx, scope, g.ID); err != nil {
			return err
		}
	}
	return nil
}

var _ = fmt.Sprint
