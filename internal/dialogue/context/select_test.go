package dctx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/sessions"
)

// scoreRow reads a row without allocating; it must agree with the plain definition: the
// number of distinct query terms among the words of the row.
func TestScoreRowMatchesTheDefinition(t *testing.T) {
	want := termSet{}
	for _, w := range words("Reserve gardening book disponibilità pickup 2026 Francesco "+strings.Repeat("z", 70), maxTerms) {
		want[w] = len(want)
	}
	rows := []string{
		"",
		"nothing to see here at all",
		"I want to reserve a gardening book",
		"RESERVE reserve Reserve",
		"la disponibilità del libro, Disponibilità!",
		"pickup on 2026-11-13 for francesco",
		"Gardening-book/pickup;2026",
		strings.Repeat("z", 70) + " reserve",
		"abc xyz de",
		"reserve​gardening", // a format character splits nothing visible: it is removed before this point; here it is a separator
	}
	for _, body := range rows {
		got := scoreRow(body, want)
		exp := 0
		seen := map[string]bool{}
		for _, w := range words(body, 0) {
			if _, ok := want[w]; ok && !seen[w] && len(w) <= 64 {
				seen[w] = true
				exp++
			}
		}
		if got != exp {
			t.Errorf("scoreRow(%q) = %d, want %d", body, got, exp)
		}
	}
	if scoreRow("reserve", termSet{}) != 0 {
		t.Error("an empty term set scored")
	}
}

func TestRankingOrderAndBounds(t *testing.T) {
	p := Lite()
	var window []sessions.Turn
	for i := 1; i <= 30; i++ {
		window = append(window, sessions.Turn{TurnID: fmt.Sprintf("trn-%d", i), N: int64(i), Role: "user", Body: fmt.Sprintf("row %d about sourdough starter and bread baking", i)})
	}
	want := termSet{"sourdough": 0, "baking": 1}
	got := selectRows(p, window, map[string]bool{"trn-3": true}, want)
	if len(got) != p.MaxTurnRows {
		t.Fatalf("%d rows selected, want the cap %d", len(got), p.MaxTurnRows)
	}
	// Only the newest CandidateRows rows are looked at: row 3 of 30 is outside the 24.
	for _, c := range got {
		if c.turn.TurnID == "trn-3" {
			t.Error("a row older than the candidate window was selected")
		}
	}
	// The newest RecentRows come first, newest first, then the lexical matches, best then newest.
	if got[0].turn.TurnID != "trn-30" || got[1].turn.TurnID != "trn-29" || got[0].class != classRecent || got[2].class != classLexical {
		t.Errorf("unexpected order: %v %v %v", got[0].turn.TurnID, got[1].turn.TurnID, got[2].turn.TurnID)
	}
	for i := 3; i < len(got); i++ {
		if got[i].score == got[i-1].score && got[i].idx > got[i-1].idx {
			t.Errorf("ties are not newest first: %d after %d", got[i].idx, got[i-1].idx)
		}
	}
	// The same input always ranks the same way.
	again := selectRows(p, window, map[string]bool{"trn-3": true}, want)
	for i := range got {
		if got[i].turn.TurnID != again[i].turn.TurnID {
			t.Fatal("ranking is not deterministic")
		}
	}
	// inOrder returns conversation order.
	ord := inOrder(got, 4)
	for i := 1; i < len(ord); i++ {
		if ord[i].idx < ord[i-1].idx {
			t.Fatal("inOrder is not oldest first")
		}
	}
}
