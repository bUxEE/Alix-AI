package history

import (
	"context"
	"encoding/hex"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Bounds on a query: BM25 scores every row that matches, so the text and the
// number of terms are capped before they reach SQLite.
const (
	// MaxQueryBytes is the longest query text accepted.
	MaxQueryBytes = 512
	// MaxQueryTerms is the most distinct terms one query contributes.
	MaxQueryTerms = 12
	// maxTermRunes clips one term.
	maxTermRunes = 64
)

// SearchQuery is a search of the caller's own transcript.
type SearchQuery struct {
	// Text is the words to find; every one of them must occur in the turn. It is
	// plain text: operators, quotes and wildcards are just words.
	Text string
	// SessionID, when set, restricts the search to one of the caller's sessions.
	SessionID string
	// Limit is the page size (DefaultSearchLimit when zero, at most
	// MaxSearchLimit) and Offset how many hits to skip (at most MaxSearchOffset).
	Limit  int
	Offset int
}

// Hit is one transcript row that matches.
type Hit struct {
	SessionID string
	TurnID    string
	N         int64
	Role      string
	CreatedAt time.Time
	// Snippet is a short excerpt of the row around the match, as plain text.
	Snippet string
}

// SearchPage is one page of hits, best match first.
type SearchPage struct {
	Hits []Hit
	// NextOffset is the offset of the next page, or 0 when there is none or it
	// would be beyond MaxSearchOffset.
	NextOffset int
}

// Search finds the caller's own transcript rows that contain every word of the
// query, best match first (BM25, then insertion order). A row is a hit only if
// its application, tenant and workspace are the token's and its principal is the
// caller's: the index is shared, so the MATCH itself is confined to the scope
// (scopedMatch) and the scope predicate is applied to every hit as well, so no
// query can reach, or be slowed by, another scope's text. Words the caller typed
// are never FTS5 syntax: see matchExpression.
func (s *Service) Search(ctx context.Context, scope contracts.Scope, q SearchQuery) (SearchPage, error) {
	if err := s.authorize(ctx, scope, ActionSearch, CapSearch); err != nil {
		return SearchPage{}, err
	}
	match, err := scopedMatch(scope, q.Text)
	if err != nil {
		return SearchPage{}, err
	}
	if s.quar != nil && s.quar.Has(SearchIndexName) {
		// The index was emptied by the repair that isolated it: searching it would answer
		// "nothing matched" to every question, and an empty page is exactly that claim. The
		// search index is the only way to search, so there is no reduced answer to give:
		// the search is unavailable until the index is rebuilt (master plan 18).
		return SearchPage{}, contracts.New(contracts.ErrUnavailable,
			"history: the search index is being rebuilt; search is unavailable until it is").
			WithDetails("index", SearchIndexName)
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = DefaultSearchLimit
	case limit < 0 || limit > MaxSearchLimit:
		return SearchPage{}, contracts.Newf(contracts.ErrValidationFailed, "history: the page size must be between 1 and %d hits", MaxSearchLimit)
	}
	if q.Offset < 0 || q.Offset > MaxSearchOffset {
		return SearchPage{}, contracts.Newf(contracts.ErrValidationFailed, "history: the offset must be between 0 and %d", MaxSearchOffset)
	}
	where, args := "history_fts MATCH ? AND t.principal_id = ?", []any{match, scope.PrincipalID}
	if q.SessionID != "" {
		if !validID(q.SessionID) {
			return SearchPage{}, badSessionID()
		}
		where += " AND t.session_id = ?"
		args = append(args, q.SessionID)
	}
	// BM25 weighs the scope column (1) at zero: every candidate carries the same token.
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "SELECT t.session_id, t.turn_id, t.n, t.role, t.created_at, snippet(history_fts, 0, '', '', '...', 20) " +
			"FROM history_fts JOIN transcript_turns t ON t.seq = history_fts.rowid",
		Alias:     "t",
		Where:     where,
		WhereArgs: args,
		Tail:      "ORDER BY bm25(history_fts, 1.0, 0.0), t.seq LIMIT ? OFFSET ?",
		TailArgs:  []any{limit + 1, q.Offset},
	})
	if err != nil {
		return SearchPage{}, wrapDB(err)
	}
	defer rows.Close()
	page := SearchPage{Hits: []Hit{}}
	for rows.Next() {
		var h Hit
		var created int64
		if err := rows.Scan(&h.SessionID, &h.TurnID, &h.N, &h.Role, &created, &h.Snippet); err != nil {
			return SearchPage{}, wrapDB(err)
		}
		h.CreatedAt = fromMS(created)
		if h.Role == roleAssistant {
			h.Snippet, _ = s.display(h.Role, h.Snippet)
		}
		page.Hits = append(page.Hits, h)
	}
	if err := rows.Err(); err != nil {
		return SearchPage{}, wrapDB(err)
	}
	if len(page.Hits) > limit {
		page.Hits = page.Hits[:limit]
		// A next page is offered only at an offset the caller may ask for.
		if next := q.Offset + limit; next <= MaxSearchOffset {
			page.NextOffset = next
		}
	}
	return page, nil
}

// scopedMatch is the MATCH expression of a search: the scope's token in the
// scope column AND the caller's words in the body column. FTS5 then yields, and
// BM25 scores, only the rows of the scope, whatever the other scopes hold, and no
// word the caller typed can match a token.
func scopedMatch(scope contracts.Scope, text string) (string, error) {
	words, err := matchExpression(text)
	if err != nil {
		return "", err
	}
	return `scope : "` + scopeToken(scope) + `" AND body : (` + words + `)`, nil
}

// scopeToken is the one token the index holds in the scope column of every row
// of the scope's application, tenant and workspace. Migration 0002 derives it in
// SQL with the same rule: 's', then the hex of each value's UTF-8 bytes, joined
// by 'g' (not a hex digit, so the encoding is unambiguous). It is letters and
// digits only, so it is a single FTS5 token and needs no quoting.
func scopeToken(scope contracts.Scope) string {
	enc := func(v string) string { return strings.ToUpper(hex.EncodeToString([]byte(v))) }
	return "s" + enc(scope.ApplicationID) + "g" + enc(scope.TenantID) + "g" + enc(scope.WorkspaceID)
}

// matchExpression turns free text into an FTS5 expression made only of quoted
// words joined by AND. The words are runs of letters, digits and combining marks;
// everything else separates them. Nothing the caller typed reaches FTS5 as
// syntax, so AND, OR, NOT, NEAR, a column filter ("body:x"), a prefix star, a
// parenthesis or a quote is a plain word or a separator, and a query can neither
// fail on syntax nor name a column. scopedMatch confines it to the body column
// and adds the scope; this only keeps the expression's meaning and cost
// predictable.
func matchExpression(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", contracts.New(contracts.ErrValidationFailed, "history: the query is not valid UTF-8")
	}
	if len(text) > MaxQueryBytes {
		return "", contracts.Newf(contracts.ErrValidationFailed, "history: the query is over %d bytes", MaxQueryBytes)
	}
	var words []string
	seen := map[string]bool{}
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := strings.ToLower(b.String())
		b.Reset()
		if r := []rune(w); len(r) > maxTermRunes {
			w = string(r[:maxTermRunes])
		}
		if !seen[w] && len(words) < MaxQueryTerms {
			seen[w] = true
			words = append(words, w)
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	if len(words) == 0 {
		return "", contracts.New(contracts.ErrValidationFailed, "history: the query has no words to search for")
	}
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, " AND "), nil
}
