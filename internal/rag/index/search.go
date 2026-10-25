package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// BM25 column weights, in the order of the FTS5 columns text, section, path,
// header. A term in a heading or a table header is stronger evidence than the
// same term in a body paragraph; the heading path repeats the section title,
// so the section carries the larger weight.
const bm25Weights = "1.0, 3.0, 1.5, 2.0"

// Search limits.
const (
	// maxIdentMatches is the most chunks of a scope an identifier may be held by
	// and still count as an exact match; beyond it the identifier is not selective.
	maxIdentMatches = 100
	// maxIdentOnlyScores bounds the exact BM25 lookups for identifier matches
	// the full-text candidates did not already carry.
	maxIdentOnlyScores = 32
)

// Query is a lexical retrieval request. The scope is a separate argument and is
// never part of the query: nothing in a Query can widen it.
type Query struct {
	// Text is the user's question or keywords. It is reduced to quoted terms
	// (see parseQuery); no FTS5 syntax in it has any effect.
	Text string
	// Limit is the number of hits wanted; zero means Config.DefaultLimit and
	// more than Config.MaxLimit is refused.
	Limit int
	// MatchAll requires every term; the default is any term, ranked by BM25.
	MatchAll bool
	// SourceIDs restricts the search to these sources (a deterministic filter,
	// master plan 12.4). Empty means every source in the scope.
	SourceIDs []string
	// IncludeHidden returns chunks the parser flagged hidden (SEC-012). The
	// default is to leave them out: hidden text is a prompt-injection vector.
	IncludeHidden bool
}

// Material returns the query as canonical strings for a cache key: everything
// that changes what Search returns, and nothing else.
func (q Query) Material() []string {
	src := append([]string(nil), q.SourceIDs...)
	sort.Strings(src)
	return []string{
		"q=" + q.Text,
		fmt.Sprintf("limit=%d", q.Limit),
		fmt.Sprintf("all=%t", q.MatchAll),
		fmt.Sprintf("hidden=%t", q.IncludeHidden),
		"sources=" + strings.Join(src, ","),
	}
}

// Hit is one retrieved child chunk, with everything a citation and an evidence
// package need. It is built only from rows that passed the scope and
// visibility predicates.
type Hit struct {
	// Rank is the position in the result, from 1.
	Rank int
	// ChunkID, GenerationID, SourceID, DocumentID and DocumentVersion identify
	// the chunk and the exact version of the source it came from.
	ChunkID         string
	GenerationID    string
	SourceID        string
	DocumentID      string
	DocumentVersion string
	// ParentID is the section the chunk sits under, for parent expansion.
	ParentID string
	Kind     chunk.Kind
	Ordinal  int
	// Text is the chunk text, verbatim from the source.
	Text string
	// Section, Path and Header are the heading context the chunk is indexed with.
	Section string
	Path    []string
	Header  string
	// Span is the extent of Text in the extracted text, in Unicode code points
	// (the unit of a citation span).
	Span    chunk.Span
	Page    int
	PageEnd int
	// ContentHash is the SHA-256 of Text (no scope in it).
	ContentHash string
	EstToks     int
	Hidden      bool
	Split       bool
	// Score orders the result (higher is better): BM25 plus the exact-identifier
	// boost. It is relative to this result and depends on corpus statistics
	// shared across every scope; use Rank, and never show it to a client or
	// persist it.
	Score float64
	// BM25 is the text-match component of Score, zero for a chunk found only by
	// identifier.
	BM25 float64
	// ExactIdentifiers are the identifiers of the query this chunk holds
	// exactly, as written in the query.
	ExactIdentifiers []string
}

// Locator is where the chunk sits in its source, in the form a citation needs.
// It mirrors chunk.Chunk.Locator: the span is in code points.
func (h Hit) Locator() conversation.CitationLocator {
	loc := conversation.CitationLocator{
		Span: &conversation.CitationSpan{Start: int64(h.Span.Start), End: int64(h.Span.End)},
	}
	if h.Page > 0 {
		p := int64(h.Page)
		loc.Page = &p
	}
	if h.Section != "" {
		s := h.Section
		if r := []rune(s); len(r) > 256 {
			s = string(r[:256])
		}
		loc.Section = &s
	}
	return loc
}

// Result is the outcome of a search.
type Result struct {
	Hits []Hit
	// Version is the scope's version as of the snapshot the search read:
	// results and version always belong together, so a cache keyed by it
	// (CacheKey) never stores new results under an old version or the reverse.
	Version Version
}

// visibility is the predicate every read of chunk content goes through. It is
// built once, in one place, so no read path can forget a part of it:
//
//   - the scope's three columns, on the chunk and on its generation;
//   - the generation is the active one of its source;
//   - the chunk is not tombstoned;
//   - the chunk is not hidden, unless the caller asked for hidden chunks;
//   - optionally, the source is one of an explicit list.
//
// It refers to the aliases c (ragindex_chunk) and g (ragindex_generation).
func visibility(scope contracts.Scope, includeHidden bool, sourceIDs []string) (string, []any, error) {
	cp, cargs, err := store.ScopedWhereAs(scope, "c")
	if err != nil {
		return "", nil, err
	}
	gp, gargs, err := store.ScopedWhereAs(scope, "g")
	if err != nil {
		return "", nil, err
	}
	hidden := 0
	if includeHidden {
		hidden = 1
	}
	pred := cp + ` AND ` + gp + ` AND g.state = 'active' AND c.tombstoned_at IS NULL AND c.hidden <= ?`
	args := append(append(cargs, gargs...), hidden)
	if len(sourceIDs) > 0 {
		// One bound JSON array, however many sources: the filter is applied inside
		// the search, before ranking and the limit, never after them.
		list, err := json.Marshal(sourceIDs)
		if err != nil {
			return "", nil, contracts.Wrap(contracts.ErrInternal, err, "index: cannot encode the source filter")
		}
		pred += ` AND c.source_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(list))
	}
	return pred, args, nil
}

func (ix *Index) checkQuery(q Query) (limit int, err error) {
	limit = q.Limit
	switch {
	case limit < 0:
		return 0, invalidArg("limit %d is negative", limit)
	case limit == 0:
		limit = ix.cfg.DefaultLimit
	case limit > ix.cfg.MaxLimit:
		return 0, invalidArg("limit %d is over the maximum of %d", limit, ix.cfg.MaxLimit)
	}
	for _, s := range q.SourceIDs {
		if err := checkID("source id", s, maxIDLen); err != nil {
			return 0, err
		}
	}
	return limit, nil
}

// Search runs BM25 retrieval, with exact-identifier matches boosted, inside
// the scope. The scope is applied in SQL, before ranking and before any text
// is read:
//
//  1. the full-text scan itself is confined to the scope's rowid range, so
//     other scopes' rows are never matched, scored or sorted;
//  2. every candidate is joined to its chunk and generation rows under the
//     scope predicate, the active-generation check and the tombstone check
//     (see visibility), all before ORDER BY and LIMIT;
//  3. the chunk text is read in a second statement that repeats the whole
//     predicate for exactly the chosen rows, so text is returned only from a
//     row that passed it, and every returned row is checked once more in Go.
//
// A scope that never indexed anything has no rowid range and returns nothing.
// All of it runs in one read snapshot together with the version stamp.
func (ix *Index) Search(ctx context.Context, scope contracts.Scope, q Query) (Result, error) {
	if err := validateScope(scope); err != nil {
		return Result{}, err
	}
	limit, err := ix.checkQuery(q)
	if err != nil {
		return Result{}, err
	}
	pq, err := parseQuery(q.Text, q.MatchAll)
	if err != nil {
		return Result{}, err
	}
	var res Result
	err = ix.db.View(ctx, func(tx *store.Tx) error {
		res = Result{}
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if res.Version, err = readVersion(ctx, tx, scope, sc); err != nil {
			return err
		}
		if pq.match == "" && len(pq.idents) == 0 {
			return nil
		}
		return ix.search(ctx, tx, scope, sc, q, pq, limit, &res)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// candidate is a chunk that matched, before its text is read.
type candidate struct {
	rid     int64
	bm25    float64
	idents  map[int]bool // indexes into parsedQuery.idents
	scoreOK bool
}

func (ix *Index) search(ctx context.Context, tx *store.Tx, scope contracts.Scope, sc scopeRow, q Query, pq parsedQuery, limit int, res *Result) error {
	vis, visArgs, err := visibility(scope, q.IncludeHidden, q.SourceIDs)
	if err != nil {
		return err
	}
	lo, hi := ridRange(sc.no)
	cands := map[int64]*candidate{}

	// 1. BM25 over the scope's rowid range.
	if pq.match != "" {
		candLimit := min(max(limit*3, 30), ix.cfg.MaxLimit*3)
		query := `SELECT c.rid, bm25(ragindex_fts, ` + bm25Weights + `) AS score
FROM ragindex_fts
CROSS JOIN ragindex_chunk AS c ON c.rid = ragindex_fts.rowid
CROSS JOIN ragindex_generation AS g ON g.generation_id = c.generation_id
WHERE ragindex_fts MATCH ? AND ragindex_fts.rowid >= ? AND ragindex_fts.rowid <= ?
  AND ` + vis + `
ORDER BY score, c.rid
LIMIT ?`
		args := append([]any{pq.match, lo, hi}, visArgs...)
		args = append(args, candLimit)
		rows, err := ix.traced(ctx, tx, query, args...)
		if err != nil {
			return ftsError(err)
		}
		for rows.Next() {
			var rid int64
			var score float64
			if err := rows.Scan(&rid, &score); err != nil {
				rows.Close()
				return err
			}
			cands[rid] = &candidate{rid: rid, bm25: -score, scoreOK: true}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return ftsError(err)
		}
		rows.Close()
	}

	// 2. Exact identifiers.
	if len(pq.idents) > 0 && ix.cfg.ExactIdentifierBoost > 0 {
		if err := ix.identCandidates(ctx, tx, scope, pq, vis, visArgs, lo, hi, cands); err != nil {
			return err
		}
	}
	if len(cands) == 0 {
		return nil
	}

	// 3. Exact BM25 for identifier-only matches, a bounded number of them.
	if pq.match != "" {
		var missing []int64
		for rid, c := range cands {
			if !c.scoreOK {
				missing = append(missing, rid)
			}
		}
		sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
		if len(missing) > maxIdentOnlyScores {
			missing = missing[:maxIdentOnlyScores]
		}
		for _, rid := range missing {
			var score float64
			err := tx.QueryRowContext(ctx,
				`SELECT bm25(ragindex_fts, `+bm25Weights+`) FROM ragindex_fts WHERE ragindex_fts MATCH ? AND rowid = ?`,
				pq.match, rid).Scan(&score)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// The chunk holds the identifier but not the text terms.
			case err != nil:
				return ftsError(err)
			default:
				cands[rid].bm25 = -score
			}
			cands[rid].scoreOK = true
		}
	}

	// 4. Rank, cut, then read the text of exactly the chosen rows.
	boost := ix.cfg.ExactIdentifierBoost
	type ranked struct {
		rid   int64
		score float64
	}
	order := make([]ranked, 0, len(cands))
	for rid, c := range cands {
		order = append(order, ranked{rid, c.bm25 + boost*float64(len(c.idents))})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].score != order[j].score {
			return order[i].score > order[j].score
		}
		return order[i].rid < order[j].rid
	})
	if len(order) > limit {
		order = order[:limit]
	}
	rids := make([]int64, len(order))
	for i, o := range order {
		rids[i] = o.rid
	}
	rowsByRid, err := ix.readHits(ctx, tx, scope, sc, q, rids)
	if err != nil {
		return err
	}
	for _, o := range order {
		h, ok := rowsByRid[o.rid]
		if !ok {
			// Not visible any more in this very snapshot cannot happen; if the
			// second statement refuses a row the first one chose, drop it.
			continue
		}
		c := cands[o.rid]
		h.Score, h.BM25 = o.score, c.bm25
		for i := range pq.idents {
			if c.idents[i] {
				h.ExactIdentifiers = append(h.ExactIdentifiers, pq.idents[i].Raw)
			}
		}
		h.Rank = len(res.Hits) + 1
		res.Hits = append(res.Hits, h)
	}
	return nil
}

// identCandidates adds the chunks that hold one of the query's identifiers
// exactly. Each key is a B-tree range scan inside the scope and goes through the
// same visibility predicate as the full-text path. An identifier held by more
// than maxIdentMatches chunks of the scope does not identify anything (it is
// closer to a common word), so it is left to BM25 and boosts nothing.
func (ix *Index) identCandidates(ctx context.Context, tx *store.Tx, scope contracts.Scope, pq parsedQuery, vis string, visArgs []any, lo, hi int64, cands map[int64]*candidate) error {
	ip, iargs, err := store.ScopedWhereAs(scope, "i")
	if err != nil {
		return err
	}
	query := `SELECT i.rid
FROM ragindex_ident AS i
CROSS JOIN ragindex_chunk AS c ON c.rid = i.rid
CROSS JOIN ragindex_generation AS g ON g.generation_id = c.generation_id
WHERE ` + ip + ` AND i.ident = ? AND i.rid >= ? AND i.rid <= ?
  AND ` + vis + `
LIMIT ?`
	// owners maps a chunk to the query identifiers it holds, one lookup per key.
	for i, id := range pq.idents {
		matched := map[int64]bool{}
		for _, key := range id.Keys {
			args := append([]any(nil), iargs...)
			args = append(args, key, lo, hi)
			args = append(args, visArgs...)
			args = append(args, maxIdentMatches+1)
			rows, err := ix.traced(ctx, tx, query, args...)
			if err != nil {
				return err
			}
			var got []int64
			for rows.Next() {
				var rid int64
				if err := rows.Scan(&rid); err != nil {
					rows.Close()
					return err
				}
				got = append(got, rid)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			if len(got) > maxIdentMatches {
				continue // not selective
			}
			for _, rid := range got {
				matched[rid] = true
			}
		}
		for rid := range matched {
			c := cands[rid]
			if c == nil {
				c = &candidate{rid: rid}
				cands[rid] = c
			}
			if c.idents == nil {
				c.idents = map[int]bool{}
			}
			c.idents[i] = true
		}
	}
	return nil
}

// readHits reads the full rows of the chosen chunks. The statement repeats the
// scope, generation, tombstone and hidden predicates; a row that fails them is
// simply absent, and a row that comes back outside the scope's rowid range or
// carrying another scope's columns is a hard error.
func (ix *Index) readHits(ctx context.Context, tx *store.Tx, scope contracts.Scope, sc scopeRow, q Query, rids []int64) (map[int64]Hit, error) {
	out := make(map[int64]Hit, len(rids))
	if len(rids) == 0 {
		return out, nil
	}
	vis, visArgs, err := visibility(scope, q.IncludeHidden, q.SourceIDs)
	if err != nil {
		return nil, err
	}
	dp, dargs, err := store.ScopedWhereAs(scope, "d")
	if err != nil {
		return nil, err
	}
	lo, hi := ridRange(sc.no)
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(rids)), ", ")
	query := `SELECT c.rid, c.application_id, c.tenant_id, c.workspace_id,
       c.chunk_id, c.generation_id, c.source_id, c.document_id, d.version, c.parent_id, c.kind, c.ordinal,
       c.text, c.section, c.path, c.header, c.span_start, c.span_end, c.page, c.page_end,
       c.est_toks, c.content_hash, c.hidden, c.split
FROM ragindex_chunk AS c
CROSS JOIN ragindex_generation AS g ON g.generation_id = c.generation_id
CROSS JOIN ragindex_document AS d ON d.generation_id = c.generation_id AND d.document_id = c.document_id
WHERE c.rid IN (` + marks + `) AND c.rid >= ? AND c.rid <= ?
  AND ` + vis + `
  AND ` + dp + ` AND d.tombstoned_at IS NULL`
	args := make([]any, 0, len(rids)+2+len(visArgs)+len(dargs))
	for _, r := range rids {
		args = append(args, r)
	}
	args = append(args, lo, hi)
	args = append(args, visArgs...)
	args = append(args, dargs...)
	rows, err := ix.traced(ctx, tx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			h               Hit
			rid             int64
			app, tenant, ws string
			kind, pathText  string
			hidden, split   int
		)
		if err := rows.Scan(&rid, &app, &tenant, &ws,
			&h.ChunkID, &h.GenerationID, &h.SourceID, &h.DocumentID, &h.DocumentVersion, &h.ParentID, &kind, &h.Ordinal,
			&h.Text, &h.Section, &pathText, &h.Header, &h.Span.Start, &h.Span.End, &h.Page, &h.PageEnd,
			&h.EstToks, &h.ContentHash, &hidden, &split); err != nil {
			return nil, err
		}
		if rid < lo || rid > hi || app != scope.ApplicationID || tenant != scope.TenantID || ws != scope.WorkspaceID {
			return nil, contracts.New(contracts.ErrScopeViolation, "index: a row outside the caller's scope reached the result; refusing the whole result")
		}
		h.Kind = chunk.Kind(kind)
		h.Path = parsePath(pathText)
		h.Hidden, h.Split = hidden == 1, split == 1
		out[rid] = h
	}
	return out, rows.Err()
}

// ftsError classifies an FTS5 failure. The match expression is built only from
// quoted terms, so a syntax error is a bug, not user input.
func ftsError(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrInternal, err, "index: full-text query failed")
}
