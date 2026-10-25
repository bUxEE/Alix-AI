package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// chunkReader is the vector.ChunkSource of the embedding job: it reads the text
// of child chunks back from the full-text index's own content table, inside the
// job's scope.
//
// This is the one place this package reads another module's table
// (ragindex_chunk): the index keeps the only copy of chunk text and offers no
// read-by-id, and a second copy here would double the storage of every
// document. The query repeats the visibility rules of the index (scope, the
// generation named, not tombstoned, not hidden) and asks for exactly the ids
// requested; a chunk that was withdrawn since the job was queued is simply not
// returned, which the pipeline counts as Missing. Hidden text (SEC-012) is
// never embedded.
type chunkReader struct{ db *store.DB }

var _ vector.ChunkSource = (*chunkReader)(nil)

// LoadChunks implements vector.ChunkSource.
func (c *chunkReader) LoadChunks(ctx context.Context, scope contracts.Scope, generation string, ids []string) ([]vector.ChunkInput, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > vector.MaxJobChunks {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "ingest: %d chunk ids is over the limit of %d", len(ids), vector.MaxJobChunks)
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, generation)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := store.QueryScoped(ctx, c.db, scope, store.ScopedStmt{
		SQL: `SELECT c.chunk_id, c.text, c.header, c.path FROM ragindex_chunk AS c`, Alias: "c",
		Where: `c.generation_id = ? AND c.tombstoned_at IS NULL AND c.hidden = 0 AND c.chunk_id IN (` + placeholders(len(ids)) + `)
 AND EXISTS (SELECT 1 FROM ragindex_generation AS g WHERE g.generation_id = c.generation_id
   AND g.application_id = c.application_id AND g.tenant_id = c.tenant_id AND g.workspace_id = c.workspace_id
   AND g.state IN ('building', 'validating', 'active'))`,
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
				return nil, contracts.Wrap(contracts.ErrInternal, fmt.Errorf("chunk %s: %w", in.ID, err), "ingest: a stored heading path is not valid JSON")
			}
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
