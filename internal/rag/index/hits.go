package index

import (
	"context"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// MaxHitRefs bounds one Hits call, as MaxRecheckRefs bounds one Recheck.
const MaxHitRefs = MaxRecheckRefs

// hitRidStatement finds the row of one chunk by its reference: the partial
// unique index ragindex_chunk_live is named for the reason Recheck names it
// (recheckStatement), and the scope predicate on the chunk (cp) is the caller's.
// The generation, the document and the hidden rules are applied by readHits,
// which repeats the whole visibility predicate for the rows this finds.
func hitRidStatement(cp string) string {
	return `SELECT c.rid
FROM ragindex_chunk AS c INDEXED BY ragindex_chunk_live
WHERE ` + cp + ` AND c.generation_id = ? AND c.chunk_id = ? AND c.tombstoned_at IS NULL
LIMIT 1`
}

// Hits reads chunks by reference, under the caller's scope. It is how the
// hybrid pipeline of master plan 12.4 gives a candidate that only the vector
// stage found (the vector index returns chunk, generation and source ids, no
// text) the text, span, document version and parent a citation needs: it
// implements query.HitReader.
//
// For each ref whose chunk is visible to the scope in this very read it returns
// the same Hit that Search returns for that chunk, with Rank, Score and BM25
// zero and no exact identifiers (nothing was ranked). A chunk is visible when it
// is in the scope, not tombstoned, in the generation now serving its source, in
// a document that is not tombstoned, not hidden unless includeHidden, and, when
// the ref names a DocumentVersion, still at that version. Hits come back in the
// order of refs; a chunk named twice comes back once.
//
// Every ref that is not visible is simply absent. A chunk of another scope, a
// hidden one, a deleted one, one of a superseded generation, one of another
// version and one that never existed all give the same answer (no hit, no
// error), so the call is never an oracle for another tenant's ids or for the
// existence of hidden text; Recheck is where a reason is given, and only for
// the caller's own scope. A ref with an empty id names nothing and is absent
// too. All reads run in one snapshot, through the statements Search uses to read
// rows, so a row of another scope reaching the result is an ErrScopeViolation
// that refuses the whole result.
func (ix *Index) Hits(ctx context.Context, scope contracts.Scope, refs []Ref, includeHidden bool) ([]Hit, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	if len(refs) > MaxHitRefs {
		return nil, contracts.Newf(contracts.ErrResourceExhausted, "index: %d refs is over the read-by-reference limit of %d", len(refs), MaxHitRefs)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	var out []Hit
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		out = nil
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil || !found {
			return err
		}
		cp, cargs, err := store.ScopedWhereAs(scope, "c")
		if err != nil {
			return err
		}
		query := hitRidStatement(cp)

		// 1. Each ref to its row, by the live-chunk index.
		rids := make([]int64, len(refs)) // 0: no such chunk in the scope
		var wanted []int64
		seen := map[int64]bool{}
		for i, ref := range refs {
			if ref.ChunkID == "" || ref.GenerationID == "" {
				continue
			}
			args := append(append([]any(nil), cargs...), ref.GenerationID, ref.ChunkID)
			rows, err := ix.traced(ctx, tx, query, args...)
			if err != nil {
				return err
			}
			var rid int64
			ok := rows.Next()
			if ok {
				err = rows.Scan(&rid)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			rids[i] = rid
			if !seen[rid] {
				seen[rid] = true
				wanted = append(wanted, rid)
			}
		}
		if len(wanted) == 0 {
			return nil
		}

		// 2. The rows themselves, under the whole visibility predicate.
		byRid, err := ix.readHits(ctx, tx, scope, sc, Query{IncludeHidden: includeHidden}, wanted)
		if err != nil {
			return err
		}
		emitted := map[int64]bool{}
		for i, ref := range refs {
			rid := rids[i]
			if rid == 0 || emitted[rid] {
				continue
			}
			h, ok := byRid[rid]
			if !ok {
				continue
			}
			if ref.DocumentVersion != "" && h.DocumentVersion != ref.DocumentVersion {
				continue
			}
			emitted[rid] = true
			out = append(out, h)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
