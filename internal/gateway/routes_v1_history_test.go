package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of the history area run the real stack of newAPIEnv: the real
// session store writes the transcript, the real history service reads, searches,
// deletes and exports it, and the real audit log records what leaves or goes.
// Only the area's service is built here, exactly the way the composition root
// builds it.

func init() {
	registerTestServices("v1_history", func(t testing.TB, e *apiEnv, s *Services) {
		svc, err := history.New(history.Deps{DB: e.db, Transcripts: e.sess, Audit: e.alog, Clock: e.clk})
		if err != nil {
			t.Fatal(err)
		}
		if err := Provide(s, svc); err != nil {
			t.Fatal(err)
		}
	})
}

const (
	historyRead    = history.CapRead
	historySearch  = history.CapSearch
	historyClear   = history.CapClear
	historyExport  = history.CapExport
	historyCleanup = history.CapCleanup
)

// historySay stores one turn of the session of sc the way the dialogue engine
// does, through the session store.
func historySay(t testing.TB, e *apiEnv, sc contracts.Scope, user, assistant string) {
	t.Helper()
	ctx := context.Background()
	sess, err := e.sess.Open(ctx, sc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := e.sess.Save(ctx, sess, &sessions.TurnRecord{TurnID: fmt.Sprintf("trn_%d", sess.TurnCount+1), User: user, Act: "answer", Assistant: assistant}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	e.clk.Advance(time.Minute)
}

func historyPath(suffix string) string { return "/v1/history" + suffix }

func historySegmentsPath(session, query string) string {
	p := historyPath("/sessions/" + session + "/segments")
	if query != "" {
		p += "?" + query
	}
	return p
}

type historyPageOut struct {
	SessionID  string            `json:"session_id"`
	Turns      []HistoryTurnBody `json:"turns"`
	NextCursor *string           `json:"next_cursor"`
	Total      int64             `json:"total"`
}

// historyLines splits an export into its lines.
func historyLines(t testing.TB, rec interface{ String() string }) []string {
	t.Helper()
	body := rec.String()
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("the export does not end with a newline: %q", body)
	}
	return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
}

// A client reads the transcript in pages, finds words in it, exports it and
// clears it, each under its own capability; every body is what the OpenAPI
// document says it is.
func TestHistoryRoutesEndToEnd(t *testing.T) {
	e := newAPIEnv(t)
	sc := e.scopeOf()
	for i := 1; i <= 5; i++ {
		historySay(t, e, sc, fmt.Sprintf("question %d about the turbine", i), fmt.Sprintf("answer %d", i))
	}

	t.Run("segments are paged by whole turns", func(t *testing.T) {
		var got []string
		query := "limit=2"
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("the cursor does not advance")
			}
			rec := e.call(http.MethodGet, historySegmentsPath(apiSess, query), nil, withScopes(historyRead))
			wantStatus(t, rec, http.StatusOK)
			validateSpec(t, "HistorySegmentPage", rec.Body.Bytes())
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q: private history must not be cached", cc)
			}
			page := decodeBody[historyPageOut](t, rec)
			if page.SessionID != apiSess || page.Total != 10 {
				t.Fatalf("page = %+v", page)
			}
			for _, turn := range page.Turns {
				got = append(got, fmt.Sprintf("%d/%s", turn.N, turn.Role))
			}
			if page.NextCursor == nil {
				break
			}
			query = "limit=2&cursor=" + *page.NextCursor
		}
		var want []string
		for n := 1; n <= 5; n++ {
			want = append(want, fmt.Sprintf("%d/user", n), fmt.Sprintf("%d/assistant", n))
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("the pages do not add up (-want +got):\n%s", diff)
		}
	})

	t.Run("segments are authorised and checked", func(t *testing.T) {
		// No capability, and a capability that is another operation's.
		wantError(t, e.call(http.MethodGet, historySegmentsPath(apiSess, ""), nil, withScopes(historySearch, historyExport)), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.call(http.MethodGet, historySegmentsPath(apiSess, ""), nil), http.StatusForbidden, contracts.ErrPermissionDenied)
		// A path that is not the token's session.
		wantError(t, e.call(http.MethodGet, historySegmentsPath("ses_other_01", ""), nil, withScopes(historyRead)), http.StatusForbidden, contracts.ErrScopeViolation)
		// Another principal on the same session.
		wantError(t, e.call(http.MethodGet, historySegmentsPath(apiSess, ""), nil, withScopes(historyRead), withPrincipal("usr_mallory")), http.StatusForbidden, contracts.ErrScopeViolation)
		// Another tenant's token for the same session id sees an empty history.
		rec := e.call(http.MethodGet, historySegmentsPath(apiSess, ""), nil, withScopes(historyRead), withTenant(apiOther))
		wantStatus(t, rec, http.StatusOK)
		if page := decodeBody[historyPageOut](t, rec); len(page.Turns) != 0 || page.Total != 0 {
			t.Errorf("another tenant read %+v", page)
		}
		for _, q := range []string{"limit=0", "limit=101", "limit=-1", "limit=x", "limit=1&limit=2", "cursor=abc", "cursor=-1", "cursor=" + strings.Repeat("9", 19), "cursor="} {
			wantError(t, e.call(http.MethodGet, historySegmentsPath(apiSess, q), nil, withScopes(historyRead)), http.StatusBadRequest, contracts.ErrValidationFailed)
		}
		wantError(t, e.call(http.MethodGet, historySegmentsPath("bad%20id", ""), nil, withScopes(historyRead)), http.StatusBadRequest, contracts.ErrValidationFailed)
	})

	t.Run("search", func(t *testing.T) {
		rec := e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "TURBINE, question 3", Limit: 10}, withScopes(historySearch))
		wantStatus(t, rec, http.StatusOK)
		validateSpec(t, "HistorySearchPage", rec.Body.Bytes())
		body := decodeBody[HistorySearchBody](t, rec)
		if len(body.Hits) != 1 || body.Hits[0].N != 3 || body.Hits[0].Role != "user" || body.Hits[0].SessionID != apiSess || body.NextOffset != nil {
			t.Fatalf("hits = %+v", body)
		}
		// Pages chain through next_offset.
		rec = e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "turbine", Limit: 2}, withScopes(historySearch))
		wantStatus(t, rec, http.StatusOK)
		validateSpec(t, "HistorySearchPage", rec.Body.Bytes())
		body = decodeBody[HistorySearchBody](t, rec)
		if len(body.Hits) != 2 || body.NextOffset == nil || *body.NextOffset != 2 {
			t.Fatalf("first page = %+v", body)
		}
		// Capability, body and principal.
		wantError(t, e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "turbine"}, withScopes(historyRead)), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "* ( )"}, withScopes(historySearch)), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.call(http.MethodPost, historyPath("/search"), `{"text":"turbine","tenant_id":"ten_globex"}`, withScopes(historySearch)), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "turbine", Limit: 51}, withScopes(historySearch)), http.StatusBadRequest, contracts.ErrValidationFailed)
		rec = e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "turbine"}, withScopes(historySearch), withPrincipal("usr_mallory"))
		if body := decodeBody[HistorySearchBody](t, rec); len(body.Hits) != 0 {
			t.Errorf("another principal found %d rows", len(body.Hits))
		}
		// What a person searches for is theirs: it is in the body, never in a log.
		e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "zebrawarning"}, withScopes(historySearch))
		if strings.Contains(e.logs.String(), "zebrawarning") {
			t.Error("the text of a search reached the log")
		}
		// The search is not a GET: its text would be in the URL.
		if rec := e.call(http.MethodGet, historyPath("/search?text=turbine"), nil, withScopes(historySearch)); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("GET /v1/history/search = %d", rec.Code)
		}
	})

	t.Run("export", func(t *testing.T) {
		rec := e.call(http.MethodPost, historyPath("/export"), nil, withScopes(historyExport))
		wantStatus(t, rec, http.StatusOK)
		if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q", cc)
		}
		lines := historyLines(t, rec.Body)
		if len(lines) != 11 {
			t.Fatalf("%d lines, want 10 rows and a trailer", len(lines))
		}
		for _, l := range lines[:10] {
			validateSpec(t, "HistoryExportRow", []byte(l))
		}
		validateSpec(t, "HistoryExportTrailer", []byte(lines[10]))
		var trailer HistoryExportTrailer
		if err := json.Unmarshal([]byte(lines[10]), &trailer); err != nil || !trailer.Complete || trailer.Rows != 10 {
			t.Errorf("trailer = %q", lines[10])
		}
		// A window and a session, with a body.
		rec = e.call(http.MethodPost, historyPath("/export"), HistoryExportRequest{SessionID: apiSess, Since: apiNow.Add(time.Minute), Until: apiNow.Add(3 * time.Minute)}, withScopes(historyExport))
		wantStatus(t, rec, http.StatusOK)
		if lines := historyLines(t, rec.Body); len(lines) != 5 { // turns 2 and 3, and the trailer
			t.Errorf("windowed export has %d lines, want 5: %v", len(lines), lines)
		}
		// Refusals are ordinary errors: nothing was streamed.
		bad := e.call(http.MethodPost, historyPath("/export"), nil, withScopes(historyRead, historySearch, historyClear))
		wantError(t, bad, http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.call(http.MethodPost, historyPath("/export"), HistoryExportRequest{SessionID: "bad id"}, withScopes(historyExport)), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.call(http.MethodPost, historyPath("/export"), HistoryExportRequest{SessionID: apiSess}, withScopes(historyExport), withPrincipal("usr_mallory")), http.StatusForbidden, contracts.ErrScopeViolation)
	})

	t.Run("an empty history exports only its trailer", func(t *testing.T) {
		rec := e.call(http.MethodPost, historyPath("/export"), nil, withScopes(historyExport), withTenant(apiOther))
		wantStatus(t, rec, http.StatusOK)
		if lines := historyLines(t, rec.Body); len(lines) != 1 || lines[0] != `{"complete":true,"rows":0}` {
			t.Errorf("empty export = %v", lines)
		}
	})

	t.Run("cleanup needs its own capability and reaches the workspace", func(t *testing.T) {
		other := e.scopeOf(withPrincipal("usr_bob"), withSession("ses_bob_01"))
		historySay(t, e, other, "bob's turn", "")
		cutoff := e.clk.Now()
		wantError(t, e.call(http.MethodPost, historyPath("/cleanup"), HistoryCleanupRequest{Before: cutoff}, withScopes(historyClear, historyExport, historyRead, historySearch)), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.call(http.MethodPost, historyPath("/cleanup"), HistoryCleanupRequest{Before: cutoff.Add(time.Hour)}, withScopes(historyCleanup)), http.StatusBadRequest, contracts.ErrValidationFailed)
		wantError(t, e.call(http.MethodPost, historyPath("/cleanup"), `{}`, withScopes(historyCleanup)), http.StatusBadRequest, contracts.ErrValidationFailed)
		if n := e.historyCount(`SELECT COUNT(*) FROM transcript_turns`); n != 11 {
			t.Fatalf("a refused cleanup left %d rows, want 11", n)
		}
		rec := e.call(http.MethodPost, historyPath("/cleanup"), HistoryCleanupRequest{Before: cutoff}, withScopes(historyCleanup))
		wantStatus(t, rec, http.StatusOK)
		validateSpec(t, "HistoryCleanupResult", rec.Body.Bytes())
		if got := decodeBody[HistoryCleanupBody](t, rec); got.RowsDeleted != 11 {
			t.Errorf("cleanup deleted %d rows, want 11 (every principal's rows of the workspace)", got.RowsDeleted)
		}
		if n := e.historyCount(`SELECT COUNT(*) FROM transcript_turns`); n != 0 {
			t.Errorf("%d rows left", n)
		}
		// The sessions and the receipts' tables are what they were.
		if e.sessionRows() != 2 {
			t.Errorf("%d session rows, want the 2 sessions to survive their transcript", e.sessionRows())
		}
	})
}

func (e *apiEnv) historyCount(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// Clearing a session deletes its transcript and its search entries and nothing
// else, and says how much.
func TestHistoryClearRoute(t *testing.T) {
	e := newAPIEnv(t)
	sc := e.scopeOf()
	historySay(t, e, sc, "remember the zebra", "noted")
	historySay(t, e, sc, "and the giraffe", "noted too")
	if rec := e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "zebra"}, withScopes(historySearch)); len(decodeBody[HistorySearchBody](t, rec).Hits) != 1 {
		t.Fatal("the fixture is not searchable")
	}

	path := historyPath("/sessions/" + apiSess)
	wantError(t, e.call(http.MethodDelete, path, nil, withScopes(historyRead, historyExport)), http.StatusForbidden, contracts.ErrPermissionDenied)
	wantError(t, e.call(http.MethodDelete, historyPath("/sessions/ses_other_01"), nil, withScopes(historyClear)), http.StatusForbidden, contracts.ErrScopeViolation)
	wantError(t, e.call(http.MethodDelete, path, nil, withScopes(historyClear), withPrincipal("usr_mallory")), http.StatusForbidden, contracts.ErrScopeViolation)
	if n := e.historyCount(`SELECT COUNT(*) FROM transcript_turns`); n != 4 {
		t.Fatalf("a refused clear left %d rows, want 4", n)
	}

	rec := e.call(http.MethodDelete, path, nil, withScopes(historyClear))
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "HistorySessionCleared", rec.Body.Bytes())
	if got := decodeBody[HistorySessionClearedBody](t, rec); got.SessionID != apiSess || got.RowsDeleted != 4 {
		t.Errorf("clear = %+v", got)
	}
	if rec := e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "zebra"}, withScopes(historySearch)); len(decodeBody[HistorySearchBody](t, rec).Hits) != 0 {
		t.Error("a cleared turn is still found by a search")
	}
	rec = e.call(http.MethodGet, historySegmentsPath(apiSess, ""), nil, withScopes(historyRead))
	if page := decodeBody[historyPageOut](t, rec); len(page.Turns) != 0 || page.NextCursor != nil || page.Total != 0 {
		t.Errorf("the transcript after clearing = %+v", page)
	}
	// The session itself is still there, and clearing it again is not an error.
	if e.sessionRows() != 1 {
		t.Errorf("%d session rows after clearing the transcript, want 1", e.sessionRows())
	}
	rec = e.call(http.MethodDelete, path, nil, withScopes(historyClear))
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[HistorySessionClearedBody](t, rec); got.RowsDeleted != 0 {
		t.Errorf("clearing an empty transcript deleted %d rows", got.RowsDeleted)
	}
	// The deletion was recorded, and carries no text.
	var data string
	if err := e.db.QueryRowContext(context.Background(), `SELECT data FROM audit_events WHERE event_type = 'deletion.history_cleared'`).Scan(&data); err != nil {
		t.Fatalf("the deletion is not in the audit log: %v", err)
	}
	if strings.Contains(data, "zebra") || !strings.Contains(data, `"rows":4`) {
		t.Errorf("audit data = %s", data)
	}
}

// A quarantined application serves no history, and every route of the area
// answers the same.
func TestHistoryRoutesRefuseQuarantinedApplication(t *testing.T) {
	e := newAPIEnv(t)
	historySay(t, e, e.scopeOf(), "hello", "hi")
	e.fapps.quarantine(true)
	all := []string{historyRead, historySearch, historyClear, historyExport, historyCleanup}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, historySegmentsPath(apiSess, ""), nil},
		{http.MethodDelete, historyPath("/sessions/" + apiSess), nil},
		{http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "hello"}},
		{http.MethodPost, historyPath("/export"), nil},
		{http.MethodPost, historyPath("/cleanup"), HistoryCleanupRequest{Before: e.clk.Now()}},
	} {
		wantError(t, e.call(c.method, c.path, c.body, withScopes(all...)), http.StatusForbidden, contracts.ErrQuarantined)
	}
	if n := e.historyCount(`SELECT COUNT(*) FROM transcript_turns`); n != 2 {
		t.Errorf("a quarantined application's history was changed: %d rows", n)
	}
}

// The history area is on the document and mounted: five operations.
func TestHistoryAreaRoutes(t *testing.T) {
	e := newAPIEnv(t)
	want := []string{
		"DELETE /v1/history/sessions/{session_id}",
		"GET /v1/history/sessions/{session_id}/segments",
		"POST /v1/history/cleanup",
		"POST /v1/history/export",
		"POST /v1/history/search",
	}
	if diff := cmp.Diff(want, e.srv.AreaRoutes("v1_history")); diff != "" {
		t.Errorf("history routes (-want +got):\n%s", diff)
	}
}

// A display route must not hand a client what the live stream would have
// stripped: assistant Markdown in history (the segments, a search snippet, the
// export) is sanitised the way the token events of a turn are, the user's own
// text is not touched, and a text the sanitiser refuses is withheld.
func TestHistoryRoutesSanitiseAssistantText(t *testing.T) {
	e := newAPIEnv(t)
	sc := e.scopeOf()
	const hostile = "Here you go <script>alert(1)</script> [open](javascript:alert(2)) ![pixel](http://tracker.example/p.png) <img src=x onerror=alert(3)> done"
	historySay(t, e, sc, "my words <b>as typed</b>", hostile)
	historySay(t, e, sc, "again", "zero\u200bwidth, which the live stream refuses")

	clean := func(t *testing.T, what, s string) {
		t.Helper()
		live := strings.ReplaceAll(strings.ToLower(s), "\\<", "") // an escaped angle bracket is literal text
		for _, bad := range []string{"<script", "<img", "javascript:", "tracker.example", "onerror="} {
			if strings.Contains(live, bad) {
				t.Errorf("%s carries %q: %s", what, bad, s)
			}
		}
	}

	rec := e.call(http.MethodGet, historySegmentsPath(apiSess, ""), nil, withScopes(historyRead))
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "HistorySegmentPage", rec.Body.Bytes())
	page := decodeBody[historyPageOut](t, rec)
	for _, turn := range page.Turns {
		clean(t, "a segment", turn.Body) // decoded: the JSON form escapes every angle bracket
	}
	if len(page.Turns) != 4 || page.Turns[0].Body != "my words <b>as typed</b>" || !page.Turns[3].Withheld || page.Turns[3].Body != "" {
		t.Fatalf("turns = %+v", page.Turns)
	}
	if !strings.Contains(page.Turns[1].Body, "Here you go") {
		t.Errorf("the sanitised text lost its words: %q", page.Turns[1].Body)
	}

	rec = e.call(http.MethodPost, historyPath("/search"), HistorySearchRequest{Text: "here you go"}, withScopes(historySearch))
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "HistorySearchPage", rec.Body.Bytes())
	for _, hit := range decodeBody[HistorySearchBody](t, rec).Hits {
		clean(t, "a search hit", hit.Snippet)
	}

	rec = e.call(http.MethodPost, historyPath("/export"), nil, withScopes(historyExport))
	wantStatus(t, rec, http.StatusOK)
	lines := historyLines(t, rec.Body)
	for _, l := range lines[:len(lines)-1] {
		validateSpec(t, "HistoryExportRow", []byte(l))
		var row struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatal(err)
		}
		clean(t, "an exported row", row.Body)
	}
}
