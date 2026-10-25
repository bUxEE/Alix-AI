package index

import (
	"context"
	"database/sql"
	"errors"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// MaxRecheckRefs bounds one Recheck call.
const MaxRecheckRefs = 1000

// Ref names a chunk a caller holds: what a hit is reduced to for the final
// authorisation and version recheck of the query pipeline (master plan 12.4).
type Ref struct {
	ChunkID      string
	GenerationID string
	// DocumentVersion, when not empty, must still be the version the chunk
	// belongs to; a document replaced since the search fails the recheck.
	DocumentVersion string
}

// RefOf reduces a hit to its Ref.
func RefOf(h Hit) Ref {
	return Ref{ChunkID: h.ChunkID, GenerationID: h.GenerationID, DocumentVersion: h.DocumentVersion}
}

// Why a chunk failed the recheck.
const (
	// ReasonGone: no live chunk with that id in the scope: it was deleted,
	// tombstoned with its source or document, replaced, or never existed. The
	// answer is the same for all of them, and for another scope's chunk.
	ReasonGone = "gone"
	// ReasonGenerationInactive: its generation no longer serves its source.
	ReasonGenerationInactive = "generation_inactive"
	// ReasonVersionChanged: the document was replaced by another version.
	ReasonVersionChanged = "version_changed"
	// ReasonHidden: the parser flagged the text hidden.
	ReasonHidden = "hidden"
)

// Verdict is the recheck result for one Ref.
type Verdict struct {
	Ref     Ref
	Visible bool
	// Reason is empty when Visible, else one of the Reason constants.
	Reason string
}

// recheckStatement is the one-chunk lookup of Recheck. cp and gp are the scope
// predicates on the chunk (c) and on its generation (g).
//
// It names the partial unique index ragindex_chunk_live (generation_id,
// chunk_id, document_id) WHERE tombstoned_at IS NULL, and it must: without the
// hint SQLite, which has no statistics here, prefers ragindex_chunk_doc, whose
// three scope equalities look more selective, and walks every chunk of the
// caller's workspace to find one row (docs/efficiency/M2-16.md has the
// measurements: 240 ms of a 423 ms query at 50k chunks in one scope). The hint
// also makes the planner's choice a contract: if the index is dropped or the
// predicate stops matching its WHERE clause, the statement fails with "no query
// solution" instead of quietly scanning, and TestRecheckUsesTheLiveChunkIndex
// reads the plan.
//
// The joined generation and document rows are single-row primary-key lookups
// (ragindex_generation by generation_id; ragindex_document by generation_id and
// document_id).
func recheckStatement(cp, gp string) string {
	return `SELECT g.state, d.version, c.hidden
FROM ragindex_chunk AS c INDEXED BY ragindex_chunk_live
JOIN ragindex_generation AS g ON g.generation_id = c.generation_id
LEFT JOIN ragindex_document AS d ON d.generation_id = c.generation_id AND d.document_id = c.document_id AND d.tombstoned_at IS NULL
WHERE ` + cp + ` AND ` + gp + ` AND c.generation_id = ? AND c.chunk_id = ? AND c.tombstoned_at IS NULL
LIMIT 1`
}

// Recheck re-reads, in one fresh snapshot, whether each chunk is still
// retrievable by the scope: it exists in the scope, is not tombstoned, belongs
// to the generation now serving its source, and is still at the version the
// caller saw. Run it just before an evidence package is built, so a source
// deleted or revoked while the query was running vanishes (SEC-028). Hidden
// chunks fail unless includeHidden. Verdicts come back in the order of refs.
func (ix *Index) Recheck(ctx context.Context, scope contracts.Scope, refs []Ref, includeHidden bool) ([]Verdict, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	if len(refs) > MaxRecheckRefs {
		return nil, contracts.Newf(contracts.ErrResourceExhausted, "index: %d refs is over the recheck limit of %d", len(refs), MaxRecheckRefs)
	}
	out := make([]Verdict, len(refs))
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		cp, cargs, err := store.ScopedWhereAs(scope, "c")
		if err != nil {
			return err
		}
		gp, gargs, err := store.ScopedWhereAs(scope, "g")
		if err != nil {
			return err
		}
		query := recheckStatement(cp, gp)
		for i, ref := range refs {
			out[i] = Verdict{Ref: ref, Reason: ReasonGone}
			args := append(append(append([]any(nil), cargs...), gargs...), ref.GenerationID, ref.ChunkID)
			var version sql.NullString
			var state string
			var hidden int
			rows, err := ix.traced(ctx, tx, query, args...)
			if err != nil {
				return err
			}
			found := rows.Next()
			if found {
				err = rows.Scan(&state, &version, &hidden)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			switch {
			case err != nil:
				return err
			case !found:
				continue
			}
			switch {
			case state != string(rag.IndexGenerationStateActive):
				out[i].Reason = ReasonGenerationInactive
			case ref.DocumentVersion != "" && (!version.Valid || version.String != ref.DocumentVersion):
				out[i].Reason = ReasonVersionChanged
			case hidden == 1 && !includeHidden:
				out[i].Reason = ReasonHidden
			default:
				out[i].Visible, out[i].Reason = true, ""
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Parent is a section chunk, the context a child may be expanded to.
type Parent struct {
	ID           string
	GenerationID string
	SourceID     string
	DocumentID   string
	Kind         chunk.Kind
	Ordinal      int
	Part         int
	Text         string
	Section      string
	Path         []string
	Span         chunk.Span
	Page         int
	PageEnd      int
	EstToks      int
	// Hidden is true when some child of the section is hidden text, so the
	// section text contains it.
	Hidden bool
}

// ParentOf returns the section a hit sits under, under the same scope and
// visibility rules as a search: the generation must be the serving one and the
// section must not be tombstoned. A section that contains hidden text is not
// returned unless includeHidden, because expanding to it would hand that text
// to a model; ok is false then, and the caller falls back to the child.
func (ix *Index) ParentOf(ctx context.Context, scope contracts.Scope, h Hit, includeHidden bool) (p Parent, ok bool, err error) {
	if err := validateScope(scope); err != nil {
		return Parent{}, false, err
	}
	err = ix.db.View(ctx, func(tx *store.Tx) error {
		pp, pargs, err := store.ScopedWhereAs(scope, "p")
		if err != nil {
			return err
		}
		gp, gargs, err := store.ScopedWhereAs(scope, "g")
		if err != nil {
			return err
		}
		hidden := 0
		if includeHidden {
			hidden = 1
		}
		args := append(append([]any(nil), pargs...), gargs...)
		args = append(args, h.GenerationID, h.DocumentID, h.ParentID, hidden)
		var kind, pathText string
		var hid int
		err = tx.QueryRowContext(ctx, `SELECT p.chunk_id, p.generation_id, p.source_id, p.document_id, p.kind, p.ordinal, p.part,
       p.text, p.section, p.path, p.span_start, p.span_end, p.page, p.page_end, p.est_toks, p.hidden
FROM ragindex_parent AS p
JOIN ragindex_generation AS g ON g.generation_id = p.generation_id
WHERE `+pp+` AND `+gp+` AND g.state = 'active' AND p.tombstoned_at IS NULL
  AND p.generation_id = ? AND p.document_id = ? AND p.chunk_id = ? AND p.hidden <= ?`, args...).Scan(
			&p.ID, &p.GenerationID, &p.SourceID, &p.DocumentID, &kind, &p.Ordinal, &p.Part,
			&p.Text, &p.Section, &pathText, &p.Span.Start, &p.Span.End, &p.Page, &p.PageEnd, &p.EstToks, &hid)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		p.Kind = chunk.Kind(kind)
		p.Path = parsePath(pathText)
		p.Hidden = hid == 1
		ok = true
		return nil
	})
	if err != nil || !ok {
		return Parent{}, false, err
	}
	return p, true, nil
}
