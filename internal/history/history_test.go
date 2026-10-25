package history

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// rowKey identifies a transcript row inside a session.
type rowKey struct {
	N    int64
	Role string
}

func keys(turns []Turn) []rowKey {
	out := make([]rowKey, 0, len(turns))
	for _, t := range turns {
		out = append(out, rowKey{t.N, t.Role})
	}
	return out
}

// seedSession stores seven turns in the session of sc: turn 3 has an assistant
// row only (a confirmation carries no user text) and turn 5 a user row only (a
// turn that failed before it answered), so a turn is not always two rows.
func seedSession(e *env, sc contracts.Scope) {
	for i := 1; i <= 7; i++ {
		user, assistant := fmt.Sprintf("question %d", i), fmt.Sprintf("answer %d", i)
		switch i {
		case 3:
			user = ""
		case 5:
			assistant = ""
		}
		e.say(sc, user, assistant)
		e.clk.Advance(time.Minute)
	}
}

// Pages are made of whole turns and chain through their cursor: walking them
// returns every row of the transcript once and in order, whatever the page size,
// even or odd, and a page never splits the user's row from the assistant's.
func TestHistoryPaginated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	seedSession(e, sc)

	all, err := e.sess.Transcript(ctx, sc, 0, 500)
	noErr(t, err)
	var want []rowKey
	for _, r := range all {
		want = append(want, rowKey{r.N, r.Role})
	}
	if len(want) != 12 {
		t.Fatalf("seeded %d rows, want 12", len(want))
	}

	for limit := 1; limit <= 8; limit++ {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			var got []rowKey
			cursor, pages := int64(0), 0
			for {
				page, err := e.svc.Segments(ctx, sc, "ses_1", cursor, limit)
				noErr(t, err)
				pages++
				if page.Total != 12 || page.SessionID != "ses_1" {
					t.Fatalf("page = total %d session %q, want 12 and ses_1", page.Total, page.SessionID)
				}
				turns := map[int64]bool{}
				for _, k := range keys(page.Turns) {
					turns[k.N] = true
				}
				if len(turns) > limit {
					t.Fatalf("a page of limit %d holds %d turns", limit, len(turns))
				}
				if page.NextCursor != 0 {
					// The page ends on a turn boundary: its last turn is complete.
					last := page.Turns[len(page.Turns)-1].N
					if page.NextCursor != last {
						t.Fatalf("next cursor = %d, want the last turn %d", page.NextCursor, last)
					}
					if len(turns) != limit {
						t.Fatalf("a page with a successor holds %d turns, want %d", len(turns), limit)
					}
				}
				got = append(got, keys(page.Turns)...)
				if page.NextCursor == 0 {
					break
				}
				cursor = page.NextCursor
				if pages > 20 {
					t.Fatal("the cursor does not advance")
				}
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("the pages do not add up to the transcript (-want +got):\n%s", diff)
			}
			if wantPages := (7 + limit - 1) / limit; pages != wantPages {
				t.Errorf("%d pages, want %d", pages, wantPages)
			}
		})
	}

	t.Run("the rows are the stored ones", func(t *testing.T) {
		page, err := e.svc.Segments(ctx, sc, "ses_1", 0, 2)
		noErr(t, err)
		if len(page.Turns) != 4 || page.Turns[0].Body != "question 1" || page.Turns[1].Role != "assistant" || page.Turns[1].Act != "answer" {
			t.Fatalf("first page = %+v", page.Turns)
		}
		if !page.Turns[0].CreatedAt.Equal(t0) || page.Turns[0].TurnID == "" {
			t.Errorf("first row = %+v", page.Turns[0])
		}
		page, err = e.svc.Segments(ctx, sc, "ses_1", 2, 2) // turns 3 (assistant only) and 4
		noErr(t, err)
		if got := keys(page.Turns); len(got) != 3 || got[0] != (rowKey{3, "assistant"}) {
			t.Errorf("rows of turns 3 and 4 = %v", got)
		}
	})

	t.Run("past the end is an empty page", func(t *testing.T) {
		page, err := e.svc.Segments(ctx, sc, "ses_1", 7, 5)
		noErr(t, err)
		if len(page.Turns) != 0 || page.NextCursor != 0 {
			t.Errorf("page = %+v", page)
		}
		page, err = e.svc.Segments(ctx, sc, "ses_unknown", 0, 5)
		noErr(t, err)
		if len(page.Turns) != 0 || page.Total != 0 {
			t.Errorf("a session with no transcript = %+v", page)
		}
	})

	t.Run("the default page", func(t *testing.T) {
		page, err := e.svc.Segments(ctx, sc, "ses_1", 0, 0)
		noErr(t, err)
		if len(page.Turns) != 12 || page.NextCursor != 0 {
			t.Errorf("default page holds %d rows, next %d", len(page.Turns), page.NextCursor)
		}
	})

	t.Run("a long transcript is paged by the bound", func(t *testing.T) {
		long := scopeOf(testApp, tenA, ws1, alice, "ses_long")
		for i := 0; i < DefaultPage+3; i++ {
			e.say(long, fmt.Sprintf("u%d", i), fmt.Sprintf("a%d", i))
		}
		page, err := e.svc.Segments(ctx, long, "ses_long", 0, 0)
		noErr(t, err)
		if len(page.Turns) != 2*DefaultPage || page.NextCursor != DefaultPage {
			t.Errorf("default page = %d rows, next %d; want %d rows, next %d", len(page.Turns), page.NextCursor, 2*DefaultPage, DefaultPage)
		}
	})
}

// A transcript page is the caller's: the capability, the principal, the
// application, tenant and workspace all have to match, and a request that is
// malformed is refused before anything is read.
func TestHistorySegmentsAuthorised(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	seedSession(e, sc)

	t.Run("without the capability", func(t *testing.T) {
		_, err := e.svc.Segments(ctx, with(sc, CapSearch, CapExport), "ses_1", 0, 5)
		wantCode(t, err, contracts.ErrPermissionDenied)
		_, err = e.svc.Segments(ctx, with(sc, "history.reader", "History.Read"), "ses_1", 0, 5)
		wantCode(t, err, contracts.ErrPermissionDenied) // the match is exact
	})
	t.Run("an invalid scope", func(t *testing.T) {
		bad := sc.Clone()
		bad.TenantID = ""
		_, err := e.svc.Segments(ctx, bad, "ses_1", 0, 5)
		wantCode(t, err, contracts.ErrScopeMissing)
	})
	t.Run("another principal's session", func(t *testing.T) {
		_, err := e.svc.Segments(ctx, scopeOf(testApp, tenA, ws1, bob, "ses_bob"), "ses_1", 0, 5)
		wantCode(t, err, contracts.ErrScopeViolation)
	})
	t.Run("another session of the same principal", func(t *testing.T) {
		// The token names ses_other; the history asked for is ses_1, also alice's.
		page, err := e.svc.Segments(ctx, scopeOf(testApp, tenA, ws1, alice, "ses_other"), "ses_1", 0, 5)
		noErr(t, err)
		if len(page.Turns) == 0 {
			t.Error("the caller's own older session cannot be read")
		}
	})
	t.Run("another tenant, workspace or application sees nothing", func(t *testing.T) {
		for _, other := range []contracts.Scope{
			scopeOf(testApp, tenB, ws1, alice, "ses_1"),
			scopeOf(testApp, tenA, ws2, alice, "ses_1"),
			scopeOf(otherApp, tenA, ws1, alice, "ses_1"),
		} {
			page, err := e.svc.Segments(ctx, other, "ses_1", 0, 5)
			noErr(t, err)
			if len(page.Turns) != 0 || page.Total != 0 {
				t.Errorf("%s/%s/%s read %d rows of another scope", other.ApplicationID, other.TenantID, other.WorkspaceID, len(page.Turns))
			}
		}
	})
	t.Run("malformed input", func(t *testing.T) {
		for name, call := range map[string]func() error{
			"session id":    func() error { _, err := e.svc.Segments(ctx, sc, "ses 1", 0, 5); return err },
			"empty session": func() error { _, err := e.svc.Segments(ctx, sc, "", 0, 5); return err },
			"long session":  func() error { _, err := e.svc.Segments(ctx, sc, strings.Repeat("a", 129), 0, 5); return err },
			"negative page": func() error { _, err := e.svc.Segments(ctx, sc, "ses_1", 0, -1); return err },
			"huge page":     func() error { _, err := e.svc.Segments(ctx, sc, "ses_1", 0, MaxPage+1); return err },
			"cursor":        func() error { _, err := e.svc.Segments(ctx, sc, "ses_1", -1, 5); return err },
		} {
			t.Run(name, func(t *testing.T) { wantCode(t, call(), contracts.ErrValidationFailed) })
		}
	})
	t.Run("the host's extra authoriser can refuse but never grant", func(t *testing.T) {
		var asked []Action
		deny := newEnv(t, withDeps(func(d *Deps) {
			d.Authorizer = func(_ context.Context, _ contracts.Scope, a Action) error {
				asked = append(asked, a)
				return contracts.New(contracts.ErrScopeViolation, "no")
			}
		}))
		own := scopeOf(testApp, tenA, ws1, alice, "ses_1")
		deny.say(own, "hello", "hi")
		_, err := deny.svc.Segments(ctx, own, "ses_1", 0, 5)
		wantCode(t, err, contracts.ErrPermissionDenied) // whatever the hook said
		_, err = deny.svc.Segments(ctx, with(own, CapSearch), "ses_1", 0, 5)
		wantCode(t, err, contracts.ErrPermissionDenied)
		if len(asked) != 1 || asked[0] != ActionRead {
			t.Errorf("the hook was asked %v, want one read (the missing capability is refused before it)", asked)
		}
	})
}

// Every table of the module carries the scope columns, the search index included,
// and the retention table's sentinel value can never be a real scope value.
func TestHistoryTablesAreScoped(t *testing.T) {
	e := newEnv(t)
	violations, err := store.CheckScopeColumns(context.Background(), e.db)
	noErr(t, err)
	for _, v := range violations {
		if strings.HasPrefix(v.Table, "history_") {
			t.Errorf("table %s breaks the scope rule: %s", v.Table, v.Problem)
		}
	}
	for _, name := range []string{"history_fts", "history_retention"} {
		if e.count(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name) != 1 {
			t.Errorf("table %s does not exist", name)
		}
	}
	// A real scope value can never be the wildcard the retention table uses for
	// "every tenant" and "every workspace".
	if scopeOf(testApp, "*", ws1, alice, "ses_1").Validate() == nil || scopeOf(testApp, tenA, "*", alice, "ses_1").Validate() == nil {
		t.Error("a scope with a wildcard validates, so the retention sentinel could collide with a tenant")
	}
}

// hostile is what a model could write and the verifier lets through: raw HTML, a
// script link and an image. The live stream sanitises it before a client sees it.
const hostile = "Here you go <script>alert(1)</script> [open](javascript:alert(2)) ![pixel](http://tracker.example/p.png) <img src=x onerror=alert(3)> done"

// What a host renders from history is never more than it would have got from
// the live stream: assistant text is sanitised by the same sanitiser on the way
// out, in the segments, the snippet of a search hit and the export, and the stored
// row is left as it is. A text the sanitiser refuses is withheld, never shown raw.
func TestHistoryAssistantTextIsSanitised(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	userText := "my own words <b>stay</b> as typed"
	e.say(sc, userText, hostile)
	e.say(sc, "second", "zero\u200bwidth text that the live stream would refuse")

	unsafe := func(t *testing.T, what, s string) {
		t.Helper()
		// An escaped angle bracket (backslash, then <) is literal text in Markdown, not a tag.
		live := strings.ReplaceAll(strings.ToLower(s), "\\<", "")
		for _, bad := range []string{"<script", "<img", "javascript:", "tracker.example", "onerror"} {
			if strings.Contains(live, bad) {
				t.Errorf("%s still carries %q: %q", what, bad, s)
			}
		}
	}

	t.Run("segments", func(t *testing.T) {
		page, err := e.svc.Segments(ctx, sc, "ses_1", 0, 10)
		noErr(t, err)
		if len(page.Turns) != 4 {
			t.Fatalf("%d rows, want 4", len(page.Turns))
		}
		if page.Turns[0].Body != userText || page.Turns[0].Withheld {
			t.Errorf("the user's own text was changed: %q", page.Turns[0].Body)
		}
		unsafe(t, "the assistant row", page.Turns[1].Body)
		if page.Turns[1].Withheld || !strings.Contains(page.Turns[1].Body, "Here you go") || !strings.Contains(page.Turns[1].Body, "done") {
			t.Errorf("the assistant row = %q: sanitising should keep the text", page.Turns[1].Body)
		}
		want, _, err := e.svc.san.SanitizeResponse(hostile)
		noErr(t, err)
		if page.Turns[1].Body != want {
			t.Errorf("history shows %q, the live stream would show %q", page.Turns[1].Body, want)
		}
		if got := page.Turns[3]; got.Body != "" || !got.Withheld || got.Role != "assistant" {
			t.Errorf("a refused text = %+v, want it withheld and empty", got)
		}
	})
	t.Run("search", func(t *testing.T) {
		page, err := e.svc.Search(ctx, sc, SearchQuery{Text: "here you go", Limit: 10})
		noErr(t, err)
		if len(page.Hits) != 1 {
			t.Fatalf("%d hits", len(page.Hits))
		}
		unsafe(t, "the snippet", page.Hits[0].Snippet)
		// A user row's snippet is the user's words.
		page, err = e.svc.Search(ctx, sc, SearchQuery{Text: "own words typed", Limit: 10})
		noErr(t, err)
		if len(page.Hits) != 1 || !strings.Contains(page.Hits[0].Snippet, "<b>stay</b>") {
			t.Errorf("user snippet = %+v", page.Hits)
		}
	})
	t.Run("export", func(t *testing.T) {
		var out bytes.Buffer
		_, err := e.svc.Export(ctx, with(sc, CapExport), &out, ExportOptions{})
		noErr(t, err)
		rows := decodeExport(t, out.Bytes())
		if len(rows) != 4 || rows[0].Body != userText {
			t.Fatalf("export = %+v", rows)
		}
		unsafe(t, "the exported assistant row", rows[1].Body)
		if rows[3].Body != "" || !rows[3].Withheld {
			t.Errorf("the refused text in the export = %+v", rows[3])
		}
	})
	t.Run("the stored row is untouched", func(t *testing.T) {
		var body string
		err := e.db.QueryRowContext(ctx, `SELECT body FROM transcript_turns WHERE role = 'assistant' ORDER BY seq LIMIT 1`).Scan(&body)
		noErr(t, err)
		if body != hostile {
			t.Errorf("the transcript row was rewritten: %q", body)
		}
	})
}
