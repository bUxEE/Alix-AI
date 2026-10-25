package upgrade

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The vector index is released by the process that has the embedder, not by an
// upgrade: `alixai serve` calls ReleaseVector with the VectorRebuild its
// composition built when an embedder role is registered. Two rules shape it.
//
//   - It touches the vector record and no other. RebuildQuarantined and
//     RepairDerived release every record they were not handed an index for (they
//     read it as an index a migration dropped), so a serve that handed them only
//     the vector index would release the full-text records too, and retrieval and
//     history search would answer from emptied indexes without saying so.
//   - The record goes only after a rebuild that returned, an index that passes its
//     check and nothing else: an index that was rebuilt in part, or not at all, is
//     the "found nothing" the quarantine exists to prevent.

// VectorGeneration is one generation the vector tables still hold: a generation
// that was isolated keeps its row (it says which source, model and dimension a
// re-embedding has to reproduce) with its vectors dropped.
type VectorGeneration struct {
	ApplicationID, TenantID, WorkspaceID string
	GenerationID, SourceID, Model        string
	// Vectors is how many vectors the generation holds now.
	Vectors int
}

// VectorGenerations lists the live generations of every scope, which is what a
// re-embedding of the whole index walks. The statement is deliberately not
// scoped: it is deployment-wide maintenance, and each generation is then handled
// under its own scope, never one derived from another.
func VectorGenerations(ctx context.Context, db *store.DB) ([]VectorGeneration, error) {
	ok, err := vectorIndex{db: db}.present(ctx)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the vector generations cannot be listed")
	}
	if !ok {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT application_id, tenant_id, workspace_id, generation_id, source_id, model_id, vectors
		FROM vector_generations WHERE state = 'live' ORDER BY application_id, tenant_id, workspace_id, generation_id`)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the vector generations cannot be listed")
	}
	defer rows.Close()
	var out []VectorGeneration
	for rows.Next() {
		var g VectorGeneration
		if err := rows.Scan(&g.ApplicationID, &g.TenantID, &g.WorkspaceID, &g.GenerationID, &g.SourceID, &g.Model, &g.Vectors); err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the vector generations cannot be listed")
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the vector generations cannot be listed")
	}
	return out, nil
}

// ReleaseVector re-embeds the isolated vector index through rebuild, checks it
// and releases its quarantine record. It reports released only when the record is
// gone; a vector index that is not isolated is left alone (false, nil).
//
// Without a rebuild (no embedder here) the index stays isolated and the error
// wraps ErrReembedRequired. A rebuild that fails, or an index that fails its
// check afterwards, leaves the record where it is and returns the error, so the
// caller can try again: retrieval keeps labelling its results reduced recall
// meanwhile. Every other record of the quarantine is untouched.
func ReleaseVector(ctx context.Context, q *Quarantine, db *store.DB, rebuild VectorRebuild) (released bool, err error) {
	if !q.Has(VectorIndexName) {
		return false, nil
	}
	ix := vectorIndex{db: db, rebuild: rebuild}
	if err := ix.Rebuild(ctx); err != nil {
		return false, err
	}
	if err := ix.Check(ctx); err != nil {
		if errors.Is(err, ErrDamaged) {
			return false, structuralError(VectorIndexName, err)
		}
		return false, err
	}
	if err := q.Clear(VectorIndexName); err != nil {
		return false, err
	}
	return true, nil
}
