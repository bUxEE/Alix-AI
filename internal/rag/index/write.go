package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Bounds on what one write may carry, so a single call cannot hold the writer
// connection for long or exhaust memory.
const (
	maxIDLen = 256
	// MaxChunksPerDocument bounds the children of one document put into the
	// index in one transaction.
	MaxChunksPerDocument = 50_000
	// MaxChunkBytes bounds the text of one chunk (and of one parent).
	MaxChunkBytes = 1 << 20
)

// Document is one source item's chunks, ready to index.
type Document struct {
	// ID identifies the item inside its source. It must be unique per item
	// within the source and should be the value the chunker was given as
	// chunk.Document.SourceID, because chunk IDs hash it: two items that share
	// a chunker source id would share chunk IDs for identical text.
	ID string
	// Version is the opaque source version or content hash the chunks were cut
	// from. It is returned with every hit so a citation names the version.
	Version string
	// Chunks is the chunker's output for the item.
	Chunks chunk.Result
}

// PutResult says what PutDocument did.
type PutResult struct {
	// Unchanged is true when the item was already indexed at this version with
	// the same chunks; nothing was written.
	Unchanged bool
	// Children and Parents are the chunk counts now held for the item.
	Children int
	Parents  int
}

// pathJSON encodes a heading breadcrumb without HTML escaping, so a heading
// like "R&D" is indexed as the tokens r and d, not r and u0026d.
func pathJSON(path []string) (string, error) {
	if len(path) == 0 {
		return "[]", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(path); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

func parsePath(s string) []string {
	var p []string
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// validateDocument refuses a malformed document instead of repairing it.
func validateDocument(d Document) error {
	if err := checkID("document id", d.ID, maxIDLen); err != nil {
		return err
	}
	if err := checkID("document version", d.Version, maxIDLen); err != nil {
		return err
	}
	if len(d.Chunks.Children) > MaxChunksPerDocument {
		return contracts.Newf(contracts.ErrResourceExhausted, "index: document has %d chunks, over the limit of %d", len(d.Chunks.Children), MaxChunksPerDocument)
	}
	parents := make(map[string]bool, len(d.Chunks.Parents))
	for _, p := range d.Chunks.Parents {
		if err := checkChunk("parent", p); err != nil {
			return err
		}
		if parents[p.ID] {
			return invalidArg("duplicate parent chunk id %s", p.ID)
		}
		parents[p.ID] = true
	}
	seen := make(map[string]bool, len(d.Chunks.Children))
	for _, c := range d.Chunks.Children {
		if err := checkChunk("child", c); err != nil {
			return err
		}
		if seen[c.ID] {
			return invalidArg("duplicate child chunk id %s", c.ID)
		}
		seen[c.ID] = true
		if !parents[c.ParentID] {
			return invalidArg("child chunk %s names parent %q, which is not in the document", c.ID, c.ParentID)
		}
	}
	return nil
}

func checkChunk(role string, c chunk.Chunk) error {
	if err := checkID(role+" chunk id", c.ID, maxIDLen); err != nil {
		return err
	}
	if len(c.Text) > MaxChunkBytes {
		return contracts.Newf(contracts.ErrResourceExhausted, "index: %s chunk %s has %d bytes of text, over the limit of %d", role, c.ID, len(c.Text), MaxChunkBytes)
	}
	for _, s := range []string{c.Text, c.Section, c.Header, c.ContentHash} {
		if !utf8.ValidString(s) {
			return invalidArg("%s chunk %s holds text that is not valid UTF-8", role, c.ID)
		}
	}
	for _, s := range c.Path {
		if !utf8.ValidString(s) {
			return invalidArg("%s chunk %s holds a heading that is not valid UTF-8", role, c.ID)
		}
	}
	if c.Span.Start < 0 || c.Span.End < c.Span.Start {
		return invalidArg("%s chunk %s has an invalid span", role, c.ID)
	}
	return nil
}

// PutDocument indexes one item into a generation, replacing any earlier copy of
// the item in that generation, in one transaction: a query sees the old chunks
// or the new ones, never a mixture and never none. It is allowed while the
// generation is building (initial build) and while it is active (an
// incremental source update); a generation that is validating, superseded,
// failed or invalidated takes no more writes, so what was validated is what is
// activated.
//
// Re-putting an item at the same version with the same chunks writes nothing
// and reports Unchanged, so a rerun of ingestion is idempotent.
func (ix *Index) PutDocument(ctx context.Context, scope contracts.Scope, generationID string, doc Document) (PutResult, error) {
	if err := validateScope(scope); err != nil {
		return PutResult{}, err
	}
	if err := validateDocument(doc); err != nil {
		return PutResult{}, err
	}
	// Encode outside the transaction: the work is pure and a busy retry reruns
	// the transaction body.
	rows, err := encodeChildren(doc)
	if err != nil {
		return PutResult{}, err
	}
	prow, err := encodeParents(doc)
	if err != nil {
		return PutResult{}, err
	}
	var res PutResult
	err = ix.db.Tx(ctx, func(tx *store.Tx) error {
		res = PutResult{}
		g, err := loadGeneration(ctx, tx, scope, generationID)
		if err != nil {
			return err
		}
		if g.state != rag.IndexGenerationStateBuilding && g.state != rag.IndexGenerationStateActive {
			return contracts.Newf(contracts.ErrConflict, "index: generation %s is %s and takes no more writes", generationID, g.state)
		}
		sc, err := ix.ensureScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		oldVersion, oldChunks, hadDoc, err := existingDocument(ctx, tx, scope, generationID, doc.ID)
		if err != nil {
			return err
		}
		if hadDoc && oldVersion == doc.Version {
			same, err := sameChunks(ctx, tx, scope, generationID, doc.ID, rows, prow)
			if err != nil {
				return err
			}
			if same {
				res = PutResult{Unchanged: true, Children: len(rows), Parents: len(prow)}
				return nil
			}
		}
		if err := ix.deleteDocumentRows(ctx, tx, scope, g.sourceID, generationID, doc.ID); err != nil {
			return err
		}
		first, err := allocRids(ctx, tx, scope, sc, len(rows))
		if err != nil {
			return err
		}
		if err := ix.insertChildren(ctx, tx, scope, g.sourceID, generationID, doc, rows, first); err != nil {
			return err
		}
		if err := ix.insertParents(ctx, tx, scope, g.sourceID, generationID, doc.ID, prow); err != nil {
			return err
		}
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `DELETE FROM ragindex_document`,
			Where:     `generation_id = ? AND document_id = ?`,
			WhereArgs: []any{generationID, doc.ID},
		}); err != nil {
			return err
		}
		if _, err := store.InsertScoped(ctx, tx, scope, "ragindex_document",
			[]string{"generation_id", "source_id", "document_id", "version", "chunks", "created_at"},
			generationID, g.sourceID, doc.ID, doc.Version, len(rows), ix.now()); err != nil {
			return err
		}
		docDelta := int64(1)
		if hadDoc {
			docDelta = 0
		}
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `UPDATE ragindex_generation SET documents = documents + ?, chunks = chunks + ?, updated_at = ?`,
			Args:      []any{docDelta, int64(len(rows)) - oldChunks, ix.now()},
			Where:     `generation_id = ?`,
			WhereArgs: []any{generationID},
		}); err != nil {
			return err
		}
		if g.state == rag.IndexGenerationStateActive {
			if err := bumpEpoch(ctx, tx, scope, sc); err != nil {
				return err
			}
		}
		res = PutResult{Children: len(rows), Parents: len(prow)}
		return nil
	})
	if err != nil {
		return PutResult{}, err
	}
	return res, nil
}

// childRow is a chunk ready to insert.
type childRow struct {
	c      chunk.Chunk
	path   string
	idents []string
}

type parentRow struct {
	c    chunk.Chunk
	path string
}

func encodeChildren(doc Document) ([]childRow, error) {
	out := make([]childRow, len(doc.Chunks.Children))
	for i, c := range doc.Chunks.Children {
		p, err := pathJSON(c.Path)
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrInternal, err, "index: cannot encode a heading path")
		}
		out[i] = childRow{c: c, path: p, idents: chunkIdentKeys(c.Text, c.Section, c.Header)}
	}
	return out, nil
}

func encodeParents(doc Document) ([]parentRow, error) {
	out := make([]parentRow, len(doc.Chunks.Parents))
	for i, c := range doc.Chunks.Parents {
		p, err := pathJSON(c.Path)
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrInternal, err, "index: cannot encode a heading path")
		}
		out[i] = parentRow{c: c, path: p}
	}
	return out, nil
}

// existingDocument reads a document's row in a generation. Tombstoned rows do
// not count: a deleted item that comes back is a fresh put.
func existingDocument(ctx context.Context, tx *store.Tx, scope contracts.Scope, generationID, documentID string) (version string, chunks int64, found bool, err error) {
	row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `SELECT version, chunks FROM ragindex_document`,
		Where:     `generation_id = ? AND document_id = ? AND tombstoned_at IS NULL`,
		WhereArgs: []any{generationID, documentID},
	})
	if err != nil {
		return "", 0, false, err
	}
	switch err := row.Scan(&version, &chunks); {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, false, nil
	case err != nil:
		return "", 0, false, err
	}
	return version, chunks, true, nil
}

// sameChunks reports whether the live chunk and parent ids and content hashes
// of the item already equal the incoming ones.
func sameChunks(ctx context.Context, tx *store.Tx, scope contracts.Scope, generationID, documentID string, kids []childRow, parents []parentRow) (bool, error) {
	have := map[string]string{}
	rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `SELECT chunk_id, content_hash FROM ragindex_chunk`,
		Where:     `generation_id = ? AND document_id = ? AND tombstoned_at IS NULL`,
		WhereArgs: []any{generationID, documentID},
	})
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return false, err
		}
		have[id] = h
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if len(have) != len(kids) {
		return false, nil
	}
	for _, k := range kids {
		if h, ok := have[k.c.ID]; !ok || h != k.c.ContentHash {
			return false, nil
		}
	}
	var n int
	prow, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `SELECT count(*) FROM ragindex_parent`,
		Where:     `generation_id = ? AND document_id = ? AND tombstoned_at IS NULL`,
		WhereArgs: []any{generationID, documentID},
	})
	if err != nil {
		return false, err
	}
	if err := prow.Scan(&n); err != nil {
		return false, err
	}
	return n == len(parents), nil
}

// deleteDocumentRows physically removes every row of the item in one
// generation (live or tombstoned). It is bounded by the item's size, so it is
// done inline when an item is replaced; deletions of whole sources are
// tombstoned and compacted later.
func (ix *Index) deleteDocumentRows(ctx context.Context, tx *store.Tx, scope contracts.Scope, sourceID, generationID, documentID string) error {
	if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `DELETE FROM ragindex_chunk`,
		Where:     `source_id = ? AND document_id = ? AND generation_id = ?`,
		WhereArgs: []any{sourceID, documentID, generationID},
	}); err != nil {
		return err
	}
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `DELETE FROM ragindex_parent`,
		Where:     `source_id = ? AND document_id = ? AND generation_id = ?`,
		WhereArgs: []any{sourceID, documentID, generationID},
	})
	return err
}

func (ix *Index) insertChildren(ctx context.Context, tx *store.Tx, scope contracts.Scope, sourceID, generationID string, doc Document, rows []childRow, first int64) error {
	if len(rows) == 0 {
		return nil
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO ragindex_chunk
		(rid, application_id, tenant_id, workspace_id, generation_id, tombstoned_at, hidden, split,
		 source_id, document_id, chunk_id, parent_id, kind, ordinal, span_start, span_end, page, page_end,
		 est_toks, content_hash, section, header, path, text)
		VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	insIdent, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO ragindex_ident (application_id, tenant_id, workspace_id, ident, rid) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insIdent.Close()
	for i, r := range rows {
		c := r.c
		rid := first + int64(i)
		if _, err := ins.ExecContext(ctx, rid, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, generationID,
			b2i(c.Hidden), b2i(c.Split), sourceID, doc.ID, c.ID, c.ParentID, string(c.Kind), c.Ordinal,
			c.Span.Start, c.Span.End, c.Page, c.PageEnd, c.EstToks, c.ContentHash, c.Section, c.Header, r.path, c.Text); err != nil {
			if store.IsConstraint(err) {
				return contracts.Wrap(contracts.ErrConflict, err, "index: a chunk id already exists in this generation under another document; give each item its own chunker source id")
			}
			return err
		}
		for _, k := range r.idents {
			if _, err := insIdent.ExecContext(ctx, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, k, rid); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ix *Index) insertParents(ctx context.Context, tx *store.Tx, scope contracts.Scope, sourceID, generationID, documentID string, rows []parentRow) error {
	if len(rows) == 0 {
		return nil
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO ragindex_parent
		(application_id, tenant_id, workspace_id, generation_id, source_id, document_id, chunk_id, kind, ordinal, part,
		 text, section, path, span_start, span_end, page, page_end, est_toks, hidden, tombstoned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, r := range rows {
		c := r.c
		if _, err := ins.ExecContext(ctx, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, generationID, sourceID, documentID,
			c.ID, string(c.Kind), c.Ordinal, c.Part, c.Text, c.Section, r.path, c.Span.Start, c.Span.End, c.Page, c.PageEnd,
			c.EstToks, b2i(c.Hidden)); err != nil {
			if store.IsConstraint(err) {
				return contracts.Wrap(contracts.ErrConflict, err, "index: a parent chunk id already exists in this generation under another document")
			}
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- deletion

// DeleteDocument hides every copy of an item, in every generation of its
// source, immediately: the chunks are tombstoned in one transaction and no
// query, recheck or parent expansion returns them from the next snapshot on.
// The rows and their index entries stay until Compact removes them. It reports
// how many chunks were hidden.
func (ix *Index) DeleteDocument(ctx context.Context, scope contracts.Scope, sourceID, documentID string) (int, error) {
	if err := validateScope(scope); err != nil {
		return 0, err
	}
	if err := checkID("source id", sourceID, maxIDLen); err != nil {
		return 0, err
	}
	if err := checkID("document id", documentID, maxIDLen); err != nil {
		return 0, err
	}
	hidden := 0
	err := ix.db.Tx(ctx, func(tx *store.Tx) error {
		hidden = 0
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil || !found {
			return err
		}
		now := ix.now()
		// Adjust each affected generation's counts from the document rows
		// before they are tombstoned.
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `SELECT generation_id, chunks FROM ragindex_document`,
			Where:     `source_id = ? AND document_id = ? AND tombstoned_at IS NULL`,
			WhereArgs: []any{sourceID, documentID},
		})
		if err != nil {
			return err
		}
		type adj struct {
			gen    string
			chunks int64
		}
		var adjs []adj
		for rows.Next() {
			var a adj
			if err := rows.Scan(&a.gen, &a.chunks); err != nil {
				rows.Close()
				return err
			}
			adjs = append(adjs, a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, a := range adjs {
			if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       `UPDATE ragindex_generation SET documents = max(documents - 1, 0), chunks = max(chunks - ?, 0), updated_at = ?`,
				Args:      []any{a.chunks, now},
				Where:     `generation_id = ?`,
				WhereArgs: []any{a.gen},
			}); err != nil {
				return err
			}
		}
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `UPDATE ragindex_chunk SET tombstoned_at = ?`,
			Args:      []any{now},
			Where:     `source_id = ? AND document_id = ? AND tombstoned_at IS NULL`,
			WhereArgs: []any{sourceID, documentID},
		})
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		hidden = int(n)
		for _, table := range []string{"ragindex_parent", "ragindex_document"} {
			if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       `UPDATE ` + table + ` SET tombstoned_at = ?`,
				Args:      []any{now},
				Where:     `source_id = ? AND document_id = ? AND tombstoned_at IS NULL`,
				WhereArgs: []any{sourceID, documentID},
			}); err != nil {
				return err
			}
		}
		return bumpEpoch(ctx, tx, scope, sc)
	})
	return hidden, err
}

// DeleteSource hides everything a source holds, in every generation,
// immediately, and invalidates its generations so none can be activated again.
// Physical removal is Compact's job. It reports how many chunks were hidden.
func (ix *Index) DeleteSource(ctx context.Context, scope contracts.Scope, sourceID string) (int, error) {
	if err := validateScope(scope); err != nil {
		return 0, err
	}
	if err := checkID("source id", sourceID, maxIDLen); err != nil {
		return 0, err
	}
	hidden := 0
	err := ix.db.Tx(ctx, func(tx *store.Tx) error {
		hidden = 0
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil || !found {
			return err
		}
		now := ix.now()
		for _, table := range []string{"ragindex_chunk", "ragindex_parent", "ragindex_document"} {
			res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       `UPDATE ` + table + ` SET tombstoned_at = ?`,
				Args:      []any{now},
				Where:     `source_id = ? AND tombstoned_at IS NULL`,
				WhereArgs: []any{sourceID},
			})
			if err != nil {
				return err
			}
			if table == "ragindex_chunk" {
				n, err := res.RowsAffected()
				if err != nil {
					return err
				}
				hidden = int(n)
			}
		}
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `UPDATE ragindex_generation SET state = 'invalidated', retired_at = ?`,
			Args:      []any{now},
			Where:     `source_id = ? AND state IN ('building', 'validating', 'active', 'superseded')`,
			WhereArgs: []any{sourceID},
		}); err != nil {
			return err
		}
		return bumpEpoch(ctx, tx, scope, sc)
	})
	return hidden, err
}

// DeleteScope hides everything the scope's workspace holds, immediately (a
// workspace or tenant deletion, master plan 19), and invalidates every
// generation in it. It reports how many chunks were hidden.
func (ix *Index) DeleteScope(ctx context.Context, scope contracts.Scope) (int, error) {
	if err := validateScope(scope); err != nil {
		return 0, err
	}
	hidden := 0
	err := ix.db.Tx(ctx, func(tx *store.Tx) error {
		var err error
		hidden, err = ix.tombstoneWorkspace(ctx, tx, scope)
		return err
	})
	return hidden, err
}

// DeleteTenant is DeleteScope for every workspace of the scope's tenant. The
// tenant comes from the scope, never from an argument, so a caller can only
// delete its own tenant's data.
func (ix *Index) DeleteTenant(ctx context.Context, scope contracts.Scope) (int, error) {
	if err := validateScope(scope); err != nil {
		return 0, err
	}
	hidden := 0
	err := ix.db.Tx(ctx, func(tx *store.Tx) error {
		hidden = 0
		rows, err := tx.QueryContext(ctx,
			`SELECT workspace_id FROM ragindex_scope WHERE application_id = ? AND tenant_id = ? ORDER BY workspace_id`,
			scope.ApplicationID, scope.TenantID)
		if err != nil {
			return err
		}
		var workspaces []string
		for rows.Next() {
			var w string
			if err := rows.Scan(&w); err != nil {
				rows.Close()
				return err
			}
			workspaces = append(workspaces, w)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, w := range workspaces {
			ws := scope
			ws.WorkspaceID = w
			n, err := ix.tombstoneWorkspace(ctx, tx, ws)
			if err != nil {
				return err
			}
			hidden += n
		}
		return nil
	})
	return hidden, err
}

func (ix *Index) tombstoneWorkspace(ctx context.Context, tx *store.Tx, scope contracts.Scope) (int, error) {
	sc, found, err := lookupScope(ctx, tx, scope)
	if err != nil || !found {
		return 0, err
	}
	now := ix.now()
	hidden := 0
	for _, table := range []string{"ragindex_chunk", "ragindex_parent", "ragindex_document"} {
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:   `UPDATE ` + table + ` SET tombstoned_at = ?`,
			Args:  []any{now},
			Where: `tombstoned_at IS NULL`,
		})
		if err != nil {
			return 0, err
		}
		if table == "ragindex_chunk" {
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			hidden = int(n)
		}
	}
	if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:   `UPDATE ragindex_generation SET state = 'invalidated', retired_at = ?`,
		Args:  []any{now},
		Where: `state IN ('building', 'validating', 'active', 'superseded')`,
	}); err != nil {
		return 0, err
	}
	return hidden, bumpEpoch(ctx, tx, scope, sc)
}

// ---------------------------------------------------------------- validation

// Expectation states what the builder knows a finished generation must hold.
// A nil field is not checked.
type Expectation struct {
	Documents *int
	Chunks    *int
}

// Report is what Validate found.
type Report struct {
	Documents      int
	Parents        int
	Chunks         int
	ProbesRun      int
	ProbesFailed   int
	OrphanChildren int
	Problems       []string
}

// OK reports whether the generation passed.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// ValidateGeneration checks a building (or already validating) generation and,
// when it passes, moves it to validating, the only state ActivateGeneration
// accepts. The checks are cheap SQL ones:
//
//   - the recorded counts equal the rows actually held, and match the
//     Expectation when one is given;
//   - every child has its parent in the generation, and every row carries the
//     generation's own scope;
//   - a sample of children (Config.ValidateProbes) is found again through the
//     full-text index itself, proving the index holds what the content table
//     holds.
//
// A generation that fails is marked failed and returns contracts.ErrValidation
// Failed together with the Report; it never serves. (The FTS5 integrity-check
// command is deliberately not run here: M0-05 found false corruption reports on
// long-lived pooled connections; the sample probe is the cheap check, and the
// store's IntegrityCheck on a fresh connection is the thorough one.)
func (ix *Index) ValidateGeneration(ctx context.Context, scope contracts.Scope, generationID string, exp Expectation) (Report, error) {
	if err := validateScope(scope); err != nil {
		return Report{}, err
	}
	var rep Report
	err := ix.db.Tx(ctx, func(tx *store.Tx) error {
		rep = Report{}
		g, err := loadGeneration(ctx, tx, scope, generationID)
		if err != nil {
			return err
		}
		if g.state != rag.IndexGenerationStateBuilding && g.state != rag.IndexGenerationStateValidating {
			return contracts.Newf(contracts.ErrConflict, "index: generation %s is %s; only a building generation can be validated", generationID, g.state)
		}
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if !found {
			return errNotFound("generation")
		}
		if err := ix.inspect(ctx, tx, scope, sc, g, exp, &rep); err != nil {
			return err
		}
		if !rep.OK() {
			// Mark failed in the same transaction as the findings so a failed
			// generation is never left looking like a building one.
			if err := ix.setState(ctx, tx, scope, generationID, rag.IndexGenerationStateFailed,
				rag.IndexGenerationStateBuilding, rag.IndexGenerationStateValidating); err != nil {
				return err
			}
			return setError(ctx, tx, scope, generationID, string(contracts.ErrValidationFailed), firstProblems(rep.Problems))
		}
		if g.state == rag.IndexGenerationStateBuilding {
			return ix.setState(ctx, tx, scope, generationID, rag.IndexGenerationStateValidating, rag.IndexGenerationStateBuilding)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	if !rep.OK() {
		return rep, contracts.Newf(contracts.ErrValidationFailed, "index: generation %s failed validation: %s", generationID, firstProblems(rep.Problems)).
			WithDetails("problems", rep.Problems)
	}
	return rep, nil
}

func firstProblems(p []string) string {
	const keep = 3
	if len(p) > keep {
		return joinProblems(p[:keep]) + "; and more"
	}
	return joinProblems(p)
}

func joinProblems(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

func (ix *Index) inspect(ctx context.Context, tx *store.Tx, scope contracts.Scope, sc scopeRow, g genRow, exp Expectation, rep *Report) error {
	problem := func(format string, args ...any) {
		rep.Problems = append(rep.Problems, fmt.Sprintf(format, args...))
	}
	count := func(query string, args ...any) (int, error) {
		row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: query, Where: `generation_id = ? AND tombstoned_at IS NULL`, WhereArgs: append([]any{g.id}, args...),
		})
		if err != nil {
			return 0, err
		}
		var n int
		return n, row.Scan(&n)
	}
	var err error
	if rep.Chunks, err = count(`SELECT count(*) FROM ragindex_chunk`); err != nil {
		return err
	}
	if rep.Parents, err = count(`SELECT count(*) FROM ragindex_parent`); err != nil {
		return err
	}
	if rep.Documents, err = count(`SELECT count(*) FROM ragindex_document`); err != nil {
		return err
	}
	if int64(rep.Chunks) != g.chunks {
		problem("recorded %d chunks but the generation holds %d", g.chunks, rep.Chunks)
	}
	if int64(rep.Documents) != g.documents {
		problem("recorded %d documents but the generation holds %d", g.documents, rep.Documents)
	}
	if exp.Chunks != nil && *exp.Chunks != rep.Chunks {
		problem("expected %d chunks, found %d", *exp.Chunks, rep.Chunks)
	}
	if exp.Documents != nil && *exp.Documents != rep.Documents {
		problem("expected %d documents, found %d", *exp.Documents, rep.Documents)
	}

	// Orphans: a child whose parent is not in the generation.
	orow, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `SELECT count(*) FROM ragindex_chunk AS c`, Alias: "c",
		Where: `c.generation_id = ? AND c.tombstoned_at IS NULL AND NOT EXISTS (
			SELECT 1 FROM ragindex_parent AS p
			WHERE p.generation_id = c.generation_id AND p.document_id = c.document_id
			  AND p.chunk_id = c.parent_id AND p.tombstoned_at IS NULL)`,
		WhereArgs: []any{g.id},
	})
	if err != nil {
		return err
	}
	if err := orow.Scan(&rep.OrphanChildren); err != nil {
		return err
	}
	if rep.OrphanChildren > 0 {
		problem("%d chunks have no parent in the generation", rep.OrphanChildren)
	}

	// Scope: every row of the generation must be inside the scope's rowid range.
	lo, hi := ridRange(sc.no)
	var stray int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM ragindex_chunk WHERE generation_id = ? AND (rid < ? OR rid > ?
			OR application_id <> ? OR tenant_id <> ? OR workspace_id <> ?)`,
		g.id, lo, hi, scope.ApplicationID, scope.TenantID, scope.WorkspaceID).Scan(&stray); err != nil {
		return err
	}
	if stray > 0 {
		problem("%d chunks lie outside the generation's scope", stray)
	}

	return ix.probeIndex(ctx, tx, scope, sc, g, rep)
}

// probeIndex looks up a sample of the generation's children through the
// full-text index, by a phrase from their own text, restricted to their rowid.
func (ix *Index) probeIndex(ctx context.Context, tx *store.Tx, scope contracts.Scope, sc scopeRow, g genRow, rep *Report) error {
	n := ix.cfg.ValidateProbes
	if rep.Chunks == 0 || n == 0 {
		return nil
	}
	picks, err := ix.probeRids(ctx, tx, scope, g, n)
	if err != nil {
		return err
	}
	lo, hi := ridRange(sc.no)
	for _, rid := range picks {
		var text string
		row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `SELECT text FROM ragindex_chunk`, Where: `rid = ?`, WhereArgs: []any{rid},
		})
		if err != nil {
			return err
		}
		if err := row.Scan(&text); err != nil {
			return err
		}
		words := probeWords(text)
		if len(words) == 0 {
			continue // nothing the tokenizer would index
		}
		if len(words) > 3 {
			words = words[:3]
		}
		rep.ProbesRun++
		var hit int64
		err = tx.QueryRowContext(ctx,
			`SELECT rowid FROM ragindex_fts WHERE ragindex_fts MATCH ? AND rowid >= ? AND rowid <= ? AND rowid = ?`,
			quoteTerm(strings.Join(words, " ")), lo, hi, rid).Scan(&hit)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			rep.ProbesFailed++
		case err != nil:
			return err
		}
	}
	if rep.ProbesFailed > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d of %d sampled chunks are missing from the full-text index", rep.ProbesFailed, rep.ProbesRun))
	}
	return nil
}

// probeRids picks up to n of the generation's live children, evenly spaced by
// rid: the first, the last and those between. One ordered scan reads the rids
// (8 bytes each): reading each pick by its position instead would sort the
// source's chunks once per pick, under the single writer.
func (ix *Index) probeRids(ctx context.Context, tx *store.Tx, scope contracts.Scope, g genRow, n int) ([]int64, error) {
	rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `SELECT rid FROM ragindex_chunk`,
		Where:     `source_id = ? AND generation_id = ? AND tombstoned_at IS NULL`,
		WhereArgs: []any{g.sourceID, g.id},
		Tail:      `ORDER BY rid`,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, err
		}
		all = append(all, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) <= n {
		return all, nil
	}
	if n == 1 {
		return all[:1], nil
	}
	picks := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		picks = append(picks, all[i*(len(all)-1)/(n-1)])
	}
	return picks, nil
}
