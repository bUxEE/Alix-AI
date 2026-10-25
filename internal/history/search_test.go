package history

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func turnIDs(page SearchPage) []string {
	var out []string
	for _, h := range page.Hits {
		out = append(out, h.TurnID)
	}
	sort.Strings(out)
	return out
}

// SEC-026
//
// Search is the caller's own history and nothing else. The index is one table
// shared by every scope; a hit is read back under the application, tenant and
// workspace of the token and the principal of the caller, so the same word in
// another tenant (with the very same session id and principal id), another
// workspace, another application, another principal or a hostile query's
// operators can never widen what comes back.
func TestHistorySearchScoped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")

	// What the caller may find: two sessions of her own.
	own1 := e.say(me, "the turbine is vibrating", "check the turbine bearings first")
	own2 := e.say(scopeOf(testApp, tenA, ws1, alice, "ses_2"), "turbine maintenance schedule", "")
	// What she may not: the same word everywhere else.
	foreign := map[string]string{}
	foreign["another principal"] = e.say(scopeOf(testApp, tenA, ws1, bob, "ses_3"), "turbine zebrabob", "turbine zebrabob")
	foreign["another tenant, same principal and session id"] = e.say(scopeOf(testApp, tenB, ws1, alice, "ses_1"), "turbine zebratenant", "")
	foreign["another workspace"] = e.say(scopeOf(testApp, tenA, ws2, alice, "ses_1"), "turbine zebraworkspace", "")
	foreign["another application"] = e.say(scopeOf(otherApp, tenA, ws1, alice, "ses_1"), "turbine zebraapp", "")

	search := func(t *testing.T, sc contracts.Scope, q SearchQuery) SearchPage {
		t.Helper()
		page, err := e.svc.Search(ctx, sc, q)
		noErr(t, err)
		return page
	}

	t.Run("the caller finds her own turns and only those", func(t *testing.T) {
		page := search(t, me, SearchQuery{Text: "turbine", Limit: 50})
		// own1 is one user row and one assistant row with the same turn id.
		got := map[string]int{}
		for _, h := range page.Hits {
			got[h.TurnID]++
			if !strings.Contains(strings.ToLower(h.Snippet), "turbine") {
				t.Errorf("snippet %q does not show the match", h.Snippet)
			}
		}
		if diff := cmp.Diff(map[string]int{own1: 2, own2: 1}, got); diff != "" {
			t.Errorf("hits by turn (-want +got):\n%s", diff)
		}
		for why, id := range foreign {
			if got[id] != 0 {
				t.Errorf("a hit from %s: %s", why, id)
			}
		}
	})

	t.Run("a word that only others have finds nothing", func(t *testing.T) {
		for _, w := range []string{"zebrabob", "zebratenant", "zebraworkspace", "zebraapp"} {
			if page := search(t, me, SearchQuery{Text: w}); len(page.Hits) != 0 {
				t.Errorf("%q found %d rows of another scope", w, len(page.Hits))
			}
		}
	})

	t.Run("the others find only their own", func(t *testing.T) {
		got := turnIDs(search(t, scopeOf(testApp, tenA, ws1, bob, "ses_3"), SearchQuery{Text: "turbine"}))
		if len(got) != 2 || got[0] != foreign["another principal"] || got[1] != foreign["another principal"] {
			t.Errorf("bob's hits = %v", got)
		}
		got = turnIDs(search(t, scopeOf(testApp, tenB, ws1, alice, "ses_1"), SearchQuery{Text: "turbine"}))
		if len(got) != 1 || got[0] != foreign["another tenant, same principal and session id"] {
			t.Errorf("tenant B's hits = %v", got)
		}
	})

	t.Run("a session filter cannot reach another principal's session", func(t *testing.T) {
		if page := search(t, me, SearchQuery{Text: "turbine", SessionID: "ses_3"}); len(page.Hits) != 0 {
			t.Errorf("filtering on bob's session returned %d rows", len(page.Hits))
		}
		page := search(t, me, SearchQuery{Text: "turbine", SessionID: "ses_2"})
		if len(page.Hits) != 1 || page.Hits[0].SessionID != "ses_2" || page.Hits[0].TurnID != own2 {
			t.Errorf("filtering on her second session = %+v", page.Hits)
		}
	})

	t.Run("query text is words, never FTS5 syntax", func(t *testing.T) {
		for _, q := range []string{
			`body:turbine`, `turbine OR zebrabob`, `turbine NOT bearings`, `"turbine`, `turbine*`, `(turbine`, `NEAR(turbine zebrabob)`,
			`application_id:app_other turbine`, `tenant_id:ten_b`, `{body}: turbine`, `^turbine`, `turbine"" ""`, `turbine; DROP TABLE history_fts`, `'turbine'`,
		} {
			page, err := e.svc.Search(ctx, me, SearchQuery{Text: q, Limit: 50})
			if err != nil {
				t.Errorf("query %q failed: %v", q, err)
				continue
			}
			for _, h := range page.Hits {
				if h.TurnID != own1 && h.TurnID != own2 {
					t.Errorf("query %q returned a row of another scope: %+v", q, h)
				}
			}
		}
		// OR and NOT are words: "turbine OR zebrabob" needs all three in one row.
		if page := search(t, me, SearchQuery{Text: "turbine OR zebrabob"}); len(page.Hits) != 0 {
			t.Errorf("an OR widened the search: %+v", page.Hits)
		}
		// The words are what is matched, in any order and any case.
		if page := search(t, me, SearchQuery{Text: "BEARINGS, Turbine!"}); len(page.Hits) != 1 {
			t.Errorf("a plain two word query found %d rows, want 1", len(page.Hits))
		}
		// An accent is not a barrier.
		e.say(me, "la turbina è già pronta", "")
		if page := search(t, me, SearchQuery{Text: "gia"}); len(page.Hits) != 1 {
			t.Errorf("diacritics: found %d rows, want 1", len(page.Hits))
		}
	})

	t.Run("a query that is not words is refused", func(t *testing.T) {
		for _, q := range []string{"", "   ", `* () "" ^ :`, "\xff\xfe"} {
			_, err := e.svc.Search(ctx, me, SearchQuery{Text: q})
			wantCode(t, err, contracts.ErrValidationFailed)
		}
		_, err := e.svc.Search(ctx, me, SearchQuery{Text: strings.Repeat("a", MaxQueryBytes+1)})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Limit: MaxSearchLimit + 1})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Offset: MaxSearchOffset + 1})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = e.svc.Search(ctx, me, SearchQuery{Text: "turbine", SessionID: "ses 1"})
		wantCode(t, err, contracts.ErrValidationFailed)
	})

	t.Run("without the capability", func(t *testing.T) {
		_, err := e.svc.Search(ctx, with(me, CapRead), SearchQuery{Text: "turbine"})
		wantCode(t, err, contracts.ErrPermissionDenied)
	})

	t.Run("pages chain through the offset", func(t *testing.T) {
		var got []string
		offset := 0
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("the offset does not advance")
			}
			page := search(t, me, SearchQuery{Text: "turbine", Limit: 2, Offset: offset})
			for _, h := range page.Hits {
				got = append(got, h.TurnID)
			}
			if page.NextOffset == 0 {
				break
			}
			offset = page.NextOffset
		}
		sort.Strings(got)
		want := []string{own1, own1, own2}
		sort.Strings(want)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("the pages do not add up (-want +got):\n%s", diff)
		}
	})
}

// The next offset a page offers is one the caller may ask for: never beyond
// MaxSearchOffset, however many more hits there are.
func TestHistorySearchNextOffsetStaysInBounds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	if _, err := e.db.ExecContext(ctx, `INSERT INTO transcript_turns
 (application_id, tenant_id, workspace_id, session_id, principal_id, turn_id, n, role, act, body, created_at)
 WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ?)
 SELECT ?, ?, ?, 'ses_1', ?, 'trn_' || i, i, 'user', '', 'turbine', ? FROM c`,
		MaxSearchOffset+2*MaxSearchLimit, testApp, tenA, ws1, alice, t0.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ offset, limit, want int }{
		{MaxSearchOffset - 10, 10, MaxSearchOffset},
		{MaxSearchOffset - 10, MaxSearchLimit, 0},
		{MaxSearchOffset, 1, 0},
	} {
		page, err := e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Limit: tc.limit, Offset: tc.offset})
		noErr(t, err)
		if len(page.Hits) != tc.limit || page.NextOffset != tc.want {
			t.Errorf("offset %d limit %d: %d hits, next offset %d; want %d hits, next offset %d", tc.offset, tc.limit, len(page.Hits), page.NextOffset, tc.limit, tc.want)
		}
		if page.NextOffset != 0 {
			if _, err := e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Limit: tc.limit, Offset: page.NextOffset}); err != nil {
				t.Errorf("the offered next offset %d is refused: %v", page.NextOffset, err)
			}
		}
	}
}

// A cleared turn cannot be found any more, whatever deleted it, and the index
// stays consistent with the transcript table.
func TestHistorySearchFollowsDeletion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := scopeOf(testApp, tenA, ws1, alice, "ses_1", adminCaps...)
	e.say(admin, "quarterly forecast review", "the forecast is attached")
	e.say(scopeOf(testApp, tenA, ws1, alice, "ses_2"), "forecast again", "")
	e.say(scopeOf(testApp, tenB, ws1, alice, "ses_1"), "forecast of tenant b", "")
	e.checkIndex()
	if got := e.ftsMatches("forecast"); got != 4 {
		t.Fatalf("the index holds %d rows for the word, want 4", got)
	}

	res, err := e.svc.ClearSession(ctx, admin, "ses_1")
	noErr(t, err)
	if res.Rows != 2 {
		t.Fatalf("cleared %d rows, want 2", res.Rows)
	}
	e.checkIndex()
	page, err := e.svc.Search(ctx, admin, SearchQuery{Text: "forecast", Limit: 50})
	noErr(t, err)
	if len(page.Hits) != 1 || page.Hits[0].SessionID != "ses_2" {
		t.Errorf("after clearing ses_1 the hits are %+v, want only ses_2", page.Hits)
	}
	if got := e.ftsMatches("forecast"); got != 2 {
		t.Errorf("the index still holds %d entries for the word, want 2 (a cleared turn must leave the index)", got)
	}

	e.clk.Advance(time.Hour)
	res, err = e.svc.Cleanup(ctx, admin, e.clk.Now())
	noErr(t, err)
	if res.Rows != 1 {
		t.Fatalf("cleanup removed %d rows, want 1 (the workspace's remaining turn)", res.Rows)
	}
	e.checkIndex()
	page, err = e.svc.Search(ctx, admin, SearchQuery{Text: "forecast"})
	noErr(t, err)
	if len(page.Hits) != 0 {
		t.Errorf("after cleanup the hits are %+v", page.Hits)
	}
	// The other tenant's turn is still found by its owner.
	other := scopeOf(testApp, tenB, ws1, alice, "ses_1")
	if page, err = e.svc.Search(ctx, other, SearchQuery{Text: "forecast"}); err != nil || len(page.Hits) != 1 {
		t.Errorf("tenant B's search after tenant A's cleanup = %+v, %v", page.Hits, err)
	}

	// A rebuild of the index from the table gives the same answer.
	if _, err := e.db.ExecContext(ctx, `INSERT INTO history_fts (history_fts) VALUES ('rebuild')`); err != nil {
		t.Fatal(err)
	}
	e.checkIndex()
	if got := e.ftsMatches("forecast"); got != 1 {
		t.Errorf("after a rebuild the index holds %d entries, want 1", got)
	}
}

func TestMatchExpressionIsOnlyQuotedWords(t *testing.T) {
	for in, want := range map[string]string{
		"turbine":           `"turbine"`,
		"Turbine  BEARINGS": `"turbine" AND "bearings"`,
		`a" OR "b`:          `"a" AND "or" AND "b"`,
		"body:x":            `"body" AND "x"`,
		"x x X":             `"x"`,
		"è già pronta":      `"è" AND "già" AND "pronta"`,
		"one two three four five six seven eight nine ten eleven twelve thirteen": `"one" AND "two" AND "three" AND "four" AND "five" AND "six" AND "seven" AND "eight" AND "nine" AND "ten" AND "eleven" AND "twelve"`,
	} {
		got, err := matchExpression(in)
		if err != nil || got != want {
			t.Errorf("matchExpression(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// insertTurns writes n user rows saying body into one session of a scope,
// straight into the transcript table.
func insertTurns(t *testing.T, db *store.DB, app, tenant, ws, principal string, n int, body string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO transcript_turns
 (application_id, tenant_id, workspace_id, session_id, principal_id, turn_id, n, role, act, body, created_at)
 WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ?)
 SELECT ?, ?, ?, 'ses_bulk', ?, 'trn_bulk_' || i, i, 'user', '', ?, ? FROM c`,
		n, app, tenant, ws, principal, body, t0.UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// The scope is part of the MATCH: a search visits, and BM25 scores, only the rows
// of its own application, tenant and workspace, however much history the other
// scopes hold in the shared index. (Before migration 0002 the MATCH yielded every
// scope's rows and the join dropped the foreign ones.)
func TestHistorySearchMatchesOnlyItsScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	own := e.say(me, "turbine bearings", "")
	e.say(scopeOf(testApp, tenA, ws1, bob, "ses_2"), "turbine of bob", "")
	insertTurns(t, e.db, testApp, tenB, ws1, alice, 300, "turbine of the noisy neighbour")
	e.say(scopeOf(testApp, tenA, ws2, alice, "ses_1"), "turbine", "")
	e.say(scopeOf(otherApp, tenA, ws1, alice, "ses_1"), "turbine", "")
	if got := e.ftsMatches("turbine"); got != 304 {
		t.Fatalf("the index holds %d rows for the word, want 304", got)
	}

	match, err := scopedMatch(me, "turbine")
	noErr(t, err)
	// What the search's MATCH yields is what it visits and scores: the two rows of
	// its scope (bob's too: the principal is the SQL predicate's job), nothing else.
	if got := e.count(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH ?`, match); got != 2 {
		t.Errorf("the search's MATCH yields %d rows, want its scope's 2", got)
	}
	if got := e.count(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH ?
 AND (application_id, tenant_id, workspace_id) != (?, ?, ?)`, match, testApp, tenA, ws1); got != 0 {
		t.Errorf("the search's MATCH yields %d rows of another scope", got)
	}
	page, err := e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Limit: 50})
	noErr(t, err)
	if len(page.Hits) != 1 || page.Hits[0].TurnID != own {
		t.Errorf("hits = %+v, want only %s", page.Hits, own)
	}

	// And Search's own MATCH carries the scope: her row, indexed under another
	// tenant's token, is no longer a candidate, although the SQL predicate alone
	// would let it through. (The index is out of step on purpose from here.)
	if page, err = e.svc.Search(ctx, me, SearchQuery{Text: "bearings"}); err != nil || len(page.Hits) != 1 {
		t.Fatalf("before: %d hits, %v; want 1", len(page.Hits), err)
	}
	for _, stmt := range []struct{ sql, token string }{
		{`INSERT INTO history_fts (history_fts, rowid, body, scope, application_id, tenant_id, workspace_id)
 SELECT 'delete', seq, body, ?, application_id, tenant_id, workspace_id FROM transcript_turns WHERE turn_id = ?`, scopeToken(me)},
		{`INSERT INTO history_fts (rowid, body, scope, application_id, tenant_id, workspace_id)
 SELECT seq, body, ?, application_id, tenant_id, workspace_id FROM transcript_turns WHERE turn_id = ?`, scopeToken(scopeOf(testApp, tenB, ws1, alice, "ses_1"))},
	} {
		if _, err := e.db.ExecContext(ctx, stmt.sql, stmt.token, own); err != nil {
			t.Fatal(err)
		}
	}
	if page, err = e.svc.Search(ctx, me, SearchQuery{Text: "bearings"}); err != nil || len(page.Hits) != 0 {
		t.Errorf("a row indexed under another scope's token: %d hits, %v; want 0", len(page.Hits), err)
	}
}

// The token Search puts in the MATCH is the one the index holds for the row, for
// any valid scope value (non-ASCII and the longest included), and two scopes whose
// joined values read the same still have different tokens.
func TestHistoryScopeTokenIsTheIndexedOne(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("\U0001F600", contracts.MaxScopeValueLen)
	for i, sc := range []contracts.Scope{
		scopeOf(testApp, tenA, ws1, alice, "ses_1"),
		scopeOf(testApp, "tenant_è_ü", ws1, alice, "ses_1"),
		scopeOf(long, long, long, alice, "ses_1"),
		scopeOf("app_ab", "c", ws1, alice, "ses_1"),
		scopeOf("app_a", "bc", ws1, alice, "ses_1"),
	} {
		noErr(t, sc.Validate())
		insertTurns(t, e.db, sc.ApplicationID, sc.TenantID, sc.WorkspaceID, alice, 1, fmt.Sprintf("marker%d shared", i))
		var indexed string
		if err := e.db.QueryRowContext(context.Background(), `SELECT scope FROM history_search_src
 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ?`, sc.ApplicationID, sc.TenantID, sc.WorkspaceID).Scan(&indexed); err != nil {
			t.Fatal(err)
		}
		if tok := scopeToken(sc); tok != indexed {
			t.Errorf("scope %d: token %q, the index derives %q", i, tok, indexed)
		}
		for _, word := range []string{fmt.Sprintf("marker%d", i), "shared"} {
			match, err := scopedMatch(sc, word)
			noErr(t, err)
			if got := e.count(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH ?`, match); got != 1 {
				t.Errorf("scope %d: %q yields %d rows, want its own 1", i, word, got)
			}
		}
	}
	e.checkIndex()
}

// The caller's words are confined to the body column: a word that is a scope
// token (her own, which every row of her scope carries, or another tenant's) or
// the column's name finds nothing.
func TestHistorySearchWordsNeverMatchTheScopeColumn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	other := scopeOf(testApp, tenB, ws1, alice, "ses_1")
	e.say(me, "turbine", "turbine checked")
	e.say(other, "turbine", "")
	mine := scopeToken(me)
	// Unconfined, the token would match every row of the scope.
	if got := e.ftsMatches(mine); got != 2 {
		t.Fatalf("the scope token is indexed for %d rows, want 2", got)
	}
	for _, q := range []string{mine, strings.ToLower(mine), scopeToken(other), "scope", "scope " + mine, "scope:" + mine, "{scope}: " + mine} {
		page, err := e.svc.Search(ctx, me, SearchQuery{Text: q, Limit: 50})
		noErr(t, err)
		if len(page.Hits) != 0 {
			t.Errorf("query %q matched the scope column: %+v", q, page.Hits)
		}
	}
}

// The index follows an update of a row's body and of its scope columns, which
// are indexed now.
func TestHistorySearchIndexFollowsUpdates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	moved := scopeOf(testApp, tenB, ws1, alice, "ses_1")
	e.say(me, "turbine", "")
	yields := func(sc contracts.Scope, word string) int {
		t.Helper()
		match, err := scopedMatch(sc, word)
		noErr(t, err)
		return e.count(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH ?`, match)
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE transcript_turns SET body = 'compressor'`); err != nil {
		t.Fatal(err)
	}
	e.checkIndex()
	if yields(me, "compressor") != 1 || yields(me, "turbine") != 0 {
		t.Error("the index did not follow the new body")
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE transcript_turns SET tenant_id = ?`, tenB); err != nil {
		t.Fatal(err)
	}
	e.checkIndex()
	if yields(me, "compressor") != 0 || yields(moved, "compressor") != 1 {
		t.Error("the index did not follow the new scope")
	}
}

// A database written before migration 0002 (the index without the scope token)
// is migrated in place: the turns it holds are indexed with their tokens, found
// by their own scope only, and new turns are indexed as they are written.
func TestHistorySearchIndexUpgradesInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre.db")
	v1, err := fs.ReadFile(migrations, "migrations/0001_history.sql")
	noErr(t, err)
	reg := store.NewRegistry()
	noErr(t, reg.Register("transcript", os.DirFS("../sessions/transcript")))
	noErr(t, reg.Register(Module, fstest.MapFS{"migrations/0001_history.sql": {Data: v1}}))
	old, err := store.Open(ctx, store.Options{Path: path, Migrations: reg})
	noErr(t, err)
	insertTurns(t, old, testApp, tenA, ws1, alice, 3, "turbine before the upgrade")
	insertTurns(t, old, testApp, tenB, ws1, alice, 5, "turbine of tenant b")
	var cols int
	noErr(t, old.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('history_fts') WHERE name = 'scope'`).Scan(&cols))
	if cols != 0 {
		t.Fatal("the pre-upgrade database already has the scope column")
	}
	noErr(t, old.Close())

	e := newEnv(t, withPath(path))
	e.checkIndex()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_bulk")
	match, err := scopedMatch(me, "turbine")
	noErr(t, err)
	if got := e.count(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH ?`, match); got != 3 {
		t.Errorf("after the upgrade the search's MATCH yields %d rows, want its scope's 3", got)
	}
	page, err := e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Limit: 50})
	noErr(t, err)
	if len(page.Hits) != 3 {
		t.Errorf("after the upgrade the search finds %d rows, want 3", len(page.Hits))
	}
	for _, h := range page.Hits {
		if h.SessionID != "ses_bulk" || !strings.Contains(h.Snippet, "before the upgrade") {
			t.Errorf("a hit that is not the scope's own pre-upgrade turn: %+v", h)
		}
	}
	e.say(scopeOf(testApp, tenA, ws1, alice, "ses_1"), "turbine after the upgrade", "")
	if page, err = e.svc.Search(ctx, me, SearchQuery{Text: "turbine", Limit: 50}); err != nil || len(page.Hits) != 4 {
		t.Errorf("a turn written after the upgrade: %d hits, %v; want 4", len(page.Hits), err)
	}
	e.checkIndex()
}
