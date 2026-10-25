package dctx

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// heavyState is the largest state the session store allows: a goal with many long slots
// and eight choices, three goals set aside, eight capsules with facts and records.
func heavyState() sessions.State {
	long := strings.Repeat("a long value that goes on and on ", 20)
	g := &sessions.Goal{CapabilityID: "reservations.create", Phase: "open", Slots: map[string]sessions.Slot{}}
	for i := range 20 {
		g.Slots[fmt.Sprintf("field_%02d", i)] = sessions.Slot{Value: long, Origin: sessions.OriginUserTurn, TurnID: "trn-x"}
	}
	g.Pending = &sessions.Clarification{Field: "pickup_date", Kind: sessions.ClarifyChoice}
	st := sessions.State{Goal: g}
	st.Last = &sessions.DirectiveSummary{TurnID: "trn-x", Act: "disambiguate", Question: &sessions.Question{Field: "title", AnswerKind: "choice"}}
	for i := range 8 {
		st.Last.Question.Choices = append(st.Last.Question.Choices, sessions.ChoiceSummary{ID: fmt.Sprintf("ch_%d", i), Label: long})
	}
	for i, id := range []string{"loans.extend", "account.hours", "catalog.find_title"} {
		sg := &sessions.Goal{ID: fmt.Sprintf("goal_s%d", i), CapabilityID: id, Slots: map[string]sessions.Slot{}, SuspendReason: sessions.SuspendedTopicSwitch}
		for j := range 6 {
			sg.Slots[fmt.Sprintf("field_%d", j)] = sessions.Slot{Value: "v"}
		}
		st.Suspended = append(st.Suspended, sg)
	}
	for i := range 8 {
		st.Capsules = append(st.Capsules, sessions.Capsule{
			GoalID: fmt.Sprintf("goal_c%d", i), CapabilityID: catalogue[i%len(catalogue)].id, Outcome: sessions.OutcomeSuccess,
			Fields: []string{"field_00", "field_01", "field_02"}, ReceiptID: fmt.Sprintf("rcpt_%d", i),
			Facts:    map[string]string{"field_00": long, "field_01": long, "field_02": long},
			Entities: []sessions.ResolvedEntity{{Field: "field_00", EntityType: "title", EntityID: fmt.Sprintf("t_%d", i)}},
		})
	}
	return st
}

// heavyTurns is a full window of long rows.
func heavyTurns(n int) []sessions.Turn {
	body := strings.Repeat("gardening reservation pickup details that run long ", 30)
	var out []sessions.Turn
	for i := 1; i <= n; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		out = append(out, sessions.Turn{TurnID: fmt.Sprintf("trn-%d", (i+1)/2), N: int64((i + 1) / 2), Role: role, Body: body})
	}
	return out
}

// withNewestMark tags the newest row so a test can tell whether it was replayed.
func withNewestMark(turns []sessions.Turn) []sessions.Turn {
	turns[len(turns)-1].Body = "NEWESTROWMARK " + turns[len(turns)-1].Body
	return turns
}

// independentCheck re-checks a built context against the profile's budget with the
// runtime's own check, without trusting anything the builder reports.
func independentCheck(t *testing.T, b *Builder, c Context) {
	t.Helper()
	req := c.ChatRequest()
	if _, err := b.Budget().CheckChat(req); err != nil {
		t.Fatalf("the runtime budget rejects what the builder returned: %v", err)
	}
	if got := b.Budget().EstimateChat(req); got != c.Report.EstTokens {
		t.Fatalf("report says %d estimated tokens, the budget estimates %d", c.Report.EstTokens, got)
	}
	if got := b.Budget().EstimateChat(req); got > b.Profile().MaxPromptTokens {
		t.Fatalf("estimated prompt %d is over the profile cap %d", got, b.Profile().MaxPromptTokens)
	}
	if c.Report.HistoryToks > b.Profile().MaxHistoryTokens {
		t.Fatalf("history is %d estimated tokens, over its own cap %d", c.Report.HistoryToks, b.Profile().MaxHistoryTokens)
	}
}

// SEC-076: prompt, history and tool-schema budgets are enforced before the runtime is called.
func TestContextTokenBudgetEnforced(t *testing.T) {
	t.Run("a typical turn fits every profile", func(t *testing.T) {
		for _, name := range ProfileNames() {
			b := newBuilder(t, name)
			c := mustBuild(t, b, baseInput("please reserve The Leopard for me, I will collect it on 2026-11-13"))
			independentCheck(t, b, c)
			t.Logf("%-16s est_toks=%4d of %4d (frame %d, constraint %d, user %d) shortlist=%d", name, c.Report.EstTokens, b.Profile().MaxPromptTokens,
				c.Report.FrameToks, c.Report.ConstraintToks, c.Report.UserToks, c.Report.Shortlist)
		}
	})

	t.Run("the heaviest state the store allows is cut to fit", func(t *testing.T) {
		for _, name := range ProfileNames() {
			b := newBuilder(t, name)
			in := baseInput("gardening reservation pickup")
			in.State, in.Turns = heavyState(), withNewestMark(heavyTurns(b.Profile().CandidateRows))
			c := mustBuild(t, b, in)
			independentCheck(t, b, c)
			if c.Report.CapsulesFull+c.Report.CapsulesDigested+c.Report.Suspended > 0 && !strings.Contains(content(c), "NEWESTROWMARK") {
				t.Errorf("%s: history survives but the newest row, the answer's question, was cut: %+v", name, c.Report)
			}
			if len(c.Report.Trimmed) == 0 {
				t.Errorf("%s: a heavy state needed no trimming; the fixture is not heavy enough (history %d of cap %d)", name, c.Report.HistoryToks, b.Profile().MaxHistoryTokens)
			}
			if c.Report.TurnRows > b.Profile().MaxTurnRows || c.Report.CapsulesFull > b.Profile().MaxCapsulesFull || c.Report.Suspended > b.Profile().MaxSuspended {
				t.Errorf("%s: section counts exceed the profile: %+v", name, c.Report)
			}
			t.Logf("%-16s est_toks=%4d history=%3d/%d rows=%d capsules=%d+%d suspended=%d trimmed=%v", name, c.Report.EstTokens, c.Report.HistoryToks,
				b.Profile().MaxHistoryTokens, c.Report.TurnRows, c.Report.CapsulesFull, c.Report.CapsulesDigested, c.Report.Suspended, c.Report.Trimmed)
		}
	})

	t.Run("history has its own cap whatever the total is", func(t *testing.T) {
		p := Lite()
		p.MaxPromptTokens, p.MaxHistoryTokens = 100000, 150 // a huge total, a small history
		b, err := New(p)
		if err != nil {
			t.Fatal(err)
		}
		in := baseInput("gardening reservation pickup")
		in.State, in.Turns = heavyState(), heavyTurns(p.CandidateRows)
		c := mustBuild(t, b, in)
		if c.Report.HistoryToks > 150 {
			t.Fatalf("history is %d estimated tokens against a cap of 150", c.Report.HistoryToks)
		}
		if c.Report.EstTokens > 100000 || len(c.Report.Trimmed) == 0 {
			t.Fatalf("the history cap did not trim anything: %+v", c.Report)
		}
	})

	t.Run("an oversize message is rejected, not clipped", func(t *testing.T) {
		b := newBuilder(t, ProfileLite)
		long := strings.Repeat("x", b.Profile().MaxUtteranceRunes+1)
		c, err := b.Build(context.Background(), scopeOf(sessionID), baseInput(long))
		if err == nil {
			t.Fatal("an utterance over the cap was accepted")
		}
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("code = %s, want validation_failed", contracts.CodeOf(err))
		}
		if len(c.Items) != 0 {
			t.Fatal("a context came back with the error")
		}
		// exactly at the cap is fine
		if _, err := b.Build(context.Background(), scopeOf(sessionID), baseInput(strings.Repeat("x", b.Profile().MaxUtteranceRunes))); err != nil {
			t.Fatalf("an utterance at the cap was rejected: %v", err)
		}
	})

	t.Run("a minimum that does not fit is a budget violation and nothing is built", func(t *testing.T) {
		b := newBuilder(t, ProfileLite)
		in := baseInput("hello there")
		in.Fixed = func(n int) Fixed {
			f := frameFor(n)
			f.System += strings.Repeat("padding that no cut can remove. ", 800) // about 8k estimated tokens
			return f
		}
		in.State, in.Turns = heavyState(), heavyTurns(24)
		c, err := b.Build(context.Background(), scopeOf(sessionID), in)
		if err == nil {
			t.Fatalf("an over-budget frame was built (%d estimated tokens)", c.Report.EstTokens)
		}
		which, ok := providers.IsBudgetViolation(err)
		if !ok || which != "max_prompt_tokens" {
			t.Fatalf("error %v is not the max_prompt_tokens budget violation (%q, %v)", err, which, ok)
		}
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("code = %s, want validation_failed (not retryable)", contracts.CodeOf(err))
		}
		if len(c.Items) != 0 {
			t.Fatal("a context came back with the error")
		}
	})

	t.Run("an output cap above the profile is refused", func(t *testing.T) {
		b := newBuilder(t, ProfileLite)
		in := baseInput("hello there")
		in.MaxOutputTokens = b.Profile().MaxOutputTokens + 1
		_, err := b.Build(context.Background(), scopeOf(sessionID), in)
		if which, ok := providers.IsBudgetViolation(err); !ok || which != "max_output_tokens" {
			t.Fatalf("error %v is not the max_output_tokens violation", err)
		}
	})

	t.Run("unusable input fails closed", func(t *testing.T) {
		b := newBuilder(t, ProfileLite)
		bad := map[string]func() (Context, error){
			"no scope": func() (Context, error) {
				return b.Build(context.Background(), contracts.Scope{}, baseInput("hi there"))
			},
			"no frame": func() (Context, error) {
				in := baseInput("hi there")
				in.Fixed = nil
				return b.Build(context.Background(), scopeOf("s"), in)
			},
			"no capability": func() (Context, error) {
				in := baseInput("hi there")
				in.Shortlist = 0
				return b.Build(context.Background(), scopeOf("s"), in)
			},
			"empty message": func() (Context, error) { return b.Build(context.Background(), scopeOf("s"), baseInput("  \n")) },
			"cancelled": func() (Context, error) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return b.Build(ctx, scopeOf("s"), baseInput("hi there"))
			},
		}
		for name, f := range bad {
			if c, err := f(); err == nil || len(c.Items) != 0 {
				t.Errorf("%s: err=%v items=%d, want an error and no context", name, err, len(c.Items))
			}
		}
	})

	t.Run("the cut order is fixed and monotonic as the budget shrinks", func(t *testing.T) {
		order := []string{"turns", "digest", "capsules", "suspended", "recent_turns", "choices", "capabilities"}
		keptRows, keptCaps := 1<<30, 1<<30
		var sawCaps bool
		for cap := 2400; cap >= 600; cap -= 50 {
			p := Lite()
			p.MaxPromptTokens = cap
			b, err := New(p)
			if err != nil {
				t.Fatal(err)
			}
			in := baseInput("gardening reservation pickup")
			in.State, in.Turns = heavyState(), withNewestMark(heavyTurns(p.CandidateRows))
			c, err := b.Build(context.Background(), scopeOf(sessionID), in)
			if err != nil {
				if _, ok := providers.IsBudgetViolation(err); !ok {
					t.Fatalf("cap %d: %v", cap, err)
				}
				continue // the minimum no longer fits: allowed, and checked below
			}
			independentCheck(t, b, c)
			// the trimmed list is a subsequence of the canonical order
			at := -1
			for _, w := range c.Report.Trimmed {
				i := slices.Index(order, w)
				if i <= at {
					t.Fatalf("cap %d: trimmed %v is not in the order %v", cap, c.Report.Trimmed, order)
				}
				at = i
			}
			// a later section is cut only once the earlier ones are gone: no capsule is cut
			// while a row that is not among the newest remains, and the newest rows go after
			// every capsule and every goal set aside
			if slices.Contains(c.Report.Trimmed, "capsules") && c.Report.TurnRows > p.RecentRows {
				t.Fatalf("cap %d: capsules were cut while %d rows remain, of which only %d are the newest", cap, c.Report.TurnRows, p.RecentRows)
			}
			if slices.Contains(c.Report.Trimmed, "recent_turns") && (c.Report.Suspended != 0 || c.Report.CapsulesFull != 0 || c.Report.CapsulesDigested != 0) {
				t.Fatalf("cap %d: the newest rows were cut while history remains: %+v", cap, c.Report)
			}
			if slices.Contains(c.Report.Trimmed, "choices") && (c.Report.Suspended != 0 || c.Report.CapsulesFull != 0 || c.Report.TurnRows != 0) {
				t.Fatalf("cap %d: choices were cut while history remains: %+v", cap, c.Report)
			}
			// the question being answered outlives a completed goal
			if (c.Report.CapsulesFull+c.Report.CapsulesDigested > 0 || c.Report.Suspended > 0) && !strings.Contains(content(c), "NEWESTROWMARK") {
				t.Fatalf("cap %d: a capsule or a goal set aside survives but the newest row was cut: %+v", cap, c.Report)
			}
			if slices.Contains(c.Report.Trimmed, "capabilities") {
				sawCaps = true
				if !strings.Contains(content(c), "Goal: reservations.create") || !strings.Contains(content(c), "gardening reservation pickup") {
					t.Fatalf("cap %d: the goal or the message was cut", cap)
				}
			}
			if c.Report.TurnRows > keptRows || c.Report.Shortlist > keptCaps {
				t.Fatalf("cap %d: a smaller budget kept more (rows %d>%d or shortlist %d>%d)", cap, c.Report.TurnRows, keptRows, c.Report.Shortlist, keptCaps)
			}
			keptRows, keptCaps = c.Report.TurnRows, c.Report.Shortlist
		}
		if !sawCaps {
			t.Fatal("the sweep never reached the point where capabilities are cut")
		}
	})
}

func TestProfiles(t *testing.T) {
	t.Run("unknown or empty name is an error, never a default", func(t *testing.T) {
		for _, name := range []string{"", "huge", "Lite", "lite ", "performance2"} {
			p, err := ProfileByName(name)
			if err == nil {
				t.Errorf("%q gave %v", name, p)
			}
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("%q: code = %s", name, contracts.CodeOf(err))
			}
		}
	})
	t.Run("every profile is valid and a smaller machine never gets a larger context", func(t *testing.T) {
		order := []string{ProfileExperimental4G, ProfileLite, ProfileBalanced, ProfilePerformance}
		var prev Profile
		for i, name := range order {
			p, err := ProfileByName(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Validate(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if i > 0 && (p.MaxPromptTokens < prev.MaxPromptTokens || p.MaxOutputTokens < prev.MaxOutputTokens || p.MaxShortlist < prev.MaxShortlist || p.MaxHistoryTokens < prev.MaxHistoryTokens) {
				t.Errorf("%s has a smaller budget than %s", name, prev.Name)
			}
			prev = p
		}
		if p := Lite(); p.MaxPromptTokens != 2048 || p.MaxOutputTokens != 256 || p.MaxShortlist != 5 {
			t.Errorf("the Lite profile is %v, want the 2048/256/5 proposed by M1-14", p)
		}
	})
	t.Run("an incoherent profile is refused", func(t *testing.T) {
		mods := map[string]func(*Profile){
			"zero prompt":        func(p *Profile) { p.MaxPromptTokens = 0 },
			"negative rows":      func(p *Profile) { p.MaxTurnRows = -1 },
			"history over total": func(p *Profile) { p.MaxHistoryTokens = p.MaxPromptTokens },
			"rows over window":   func(p *Profile) { p.MaxTurnRows = p.CandidateRows + 1 },
			"recent over rows":   func(p *Profile) { p.RecentRows = p.MaxTurnRows + 1 },
		}
		for name, mod := range mods {
			p := Lite()
			mod(&p)
			if _, err := New(p); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
	t.Run("the request budget is the profile's", func(t *testing.T) {
		for _, name := range ProfileNames() {
			b := newBuilder(t, name)
			if err := b.Budget().Validate(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if b.Budget().MaxPromptTokens != b.Profile().MaxPromptTokens || b.Budget().MaxOutputTokens != b.Profile().MaxOutputTokens {
				t.Errorf("%s: budget %+v differs from profile %v", name, b.Budget(), b.Profile())
			}
		}
	})
}
