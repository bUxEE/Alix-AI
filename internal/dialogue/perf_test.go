package dialogue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
)

func noLimit() envOpt {
	return withDeps(func(d *Deps) {
		d.Limiter = NewWindowLimiter(clock.Real{}, RateConfig{MaxTurns: 1 << 30, Window: time.Hour, MaxKeys: 16})
	})
}

// realisticCatalogue is five shortlisted capabilities of a real-looking package: the
// first two are the titles and purposes of examples/packages/valid-minimal, the rest
// are written in the same style (a field or two each, as such packages have).
func realisticCatalogue(lang verify.Language) []promptCap {
	f := func(n, typ string, req bool) fieldInfo { return fieldInfo{Name: n, Type: typ, Required: req} }
	caps := []promptCap{
		{capSchemaInfo: capSchemaInfo{ID: "catalog.find_title", Fields: []fieldInfo{f("query", "string", true), f("available_only", "boolean", false), f("max_results", "integer", false)}},
			Title: "Find a title", Purpose: "Search the catalogue of the user's branch for titles that match some words and report which of them can be borrowed now",
			Terms: []string{"title = book, work, volume", "search = find, look up", "availability = available, in stock"}},
		{capSchemaInfo: capSchemaInfo{ID: "reservations.create", Fields: []fieldInfo{f("title", "entity_ref", true), f("pickup_date", "date", true), f("notes", "string", false)}},
			Title: "Reserve a title", Purpose: "Reserve a title for pickup at the user's branch on a chosen date",
			Terms: []string{"reservation = reserve, hold, book"}},
		{capSchemaInfo: capSchemaInfo{ID: "loans.extend", Fields: []fieldInfo{f("loan", "entity_ref", true), f("weeks", "integer", true)}},
			Title: "Extend a loan", Purpose: "Extend the due date of a borrowed title by a number of weeks",
			Terms: []string{"loan = borrowed, checked out", "extension = extend, renew"}},
		{capSchemaInfo: capSchemaInfo{ID: "reservations.cancel", Fields: []fieldInfo{f("reservation", "entity_ref", true)}},
			Title: "Cancel a reservation", Purpose: "Cancel a reservation that has not been collected yet"},
		{capSchemaInfo: capSchemaInfo{ID: "account.hours", Fields: []fieldInfo{f("branch", "enum", false)}},
			Title: "Opening hours", Purpose: "Report the opening hours of a branch"},
	}
	if lang == verify.LangIT {
		caps[0].Title, caps[0].Purpose = "Cerca un titolo", "Cerca nel catalogo della sede dell'utente i titoli che corrispondono ad alcune parole e indica quali sono disponibili al prestito"
		caps[0].Terms = []string{"titolo = libro, opera, volume", "ricerca = cerca, trova", "disponibilità = disponibile, in sede"}
		caps[1].Title, caps[1].Purpose = "Prenota un titolo", "Prenota un titolo per il ritiro nella sede dell'utente in una data scelta"
		caps[1].Terms = []string{"prenotazione = prenota, riserva"}
		caps[2].Title, caps[2].Purpose = "Prolunga un prestito", "Prolunga la scadenza di un titolo in prestito di un certo numero di settimane"
		caps[2].Terms = []string{"prestito = in prestito", "proroga = prorogare, rinnova"}
		caps[3].Title, caps[3].Purpose = "Annulla una prenotazione", "Annulla una prenotazione non ancora ritirata"
		caps[4].Title, caps[4].Purpose = "Orari di apertura", "Indica gli orari di apertura di una sede"
	}
	return caps
}

func typicalTurns() []sessions.Turn {
	return []sessions.Turn{
		{TurnID: "trn_1", N: 1, Role: "user", Body: "Do you have any books about gardening?"},
		{TurnID: "trn_1", N: 1, Role: "assistant", Body: "Here is what I found:\n- Titles found: 3\n- The Well-Tempered Garden, available"},
		{TurnID: "trn_2", N: 2, Role: "user", Body: "Reserve the first one for me"},
		{TurnID: "trn_2", N: 2, Role: "assistant", Body: "Which date will you collect it? What is the pickup date?"},
	}
}

// TestPromptSizesForTheEfficiencyReview records the size of the prompts of typical
// turns (docs/efficiency/M1-14.md) and guards them: a prompt that grows past the Lite
// budget is a regression. Tokens are estimates (providers.EstimateTokens: one token
// per three bytes, which overestimates English and Italian by about a third).
func TestPromptSizesForTheEfficiencyReview(t *testing.T) {
	// The Lite profile with the total cap lifted: it measures what the prompt weighs
	// before the cap cuts anything. History keeps its own, realistic cap.
	builder := ctxBuilderFor(t, ctxLiteWith(func(p *dctx.Profile) { p.MaxPromptTokens = 1 << 20 }))
	build := func(in proposalInput) (builtPrompt, error) {
		return buildProposal(context.Background(), builder, memberScope(), in)
	}
	size := func(name string, b builtPrompt, bound int) {
		t.Logf("%-58s est_toks=%4d bytes=%5d (~%d tokens at 4 bytes/token)", name, b.EstTokens, b.Bytes, b.Bytes/4)
		if bound > 0 && b.EstTokens > bound {
			t.Errorf("%s: %d estimated tokens, over the bound %d", name, b.EstTokens, bound)
		}
	}
	for _, lang := range []verify.Language{verify.LangEN, verify.LangIT} {
		caps := realisticCatalogue(lang)
		in := proposalInput{App: "demo-library", Now: t0, Lang: lang, Utterance: "Please reserve The Leopard for me, I will collect it on 2026-11-13", Caps: caps}
		b, err := build(in)
		if err != nil {
			t.Fatal(err)
		}
		size("proposal, 5 capabilities, no history, "+string(lang), b, ctxLiteCap)

		withHistory := in
		withHistory.Turns = typicalTurns()
		withHistory.State = sessions.State{
			Goal: &sessions.Goal{CapabilityID: "reservations.create", Slots: map[string]sessions.Slot{"title": {Value: "t_77", Origin: sessions.OriginEntityResolution}}, Pending: &sessions.Clarification{Field: "pickup_date", Kind: sessions.ClarifyText}},
			Last: &sessions.DirectiveSummary{Act: "ask_missing", MissingFields: []string{"pickup_date"}, Question: &sessions.Question{Field: "pickup_date", AnswerKind: "text"}},
		}
		withHistory.Visible = func(capabilityID, field string) bool {
			return capabilityID == "reservations.create" && (field == "" || field == "title" || field == "pickup_date")
		}
		b, err = build(withHistory)
		if err != nil {
			t.Fatal(err)
		}
		size("proposal, 5 capabilities, goal + 4 turn rows, "+string(lang), b, ctxLiteCap)

		one := in
		one.Caps = caps[:2]
		b, err = build(one)
		if err != nil {
			t.Fatal(err)
		}
		size("proposal, 2 capabilities (decision stage narrowed), "+string(lang), b, 1200)
	}

	// The response prompts, from the real engine on the demo application.
	e := newEnv(t, withLevel(level2))
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	e.model.On(pfake.Contains("show me")).Reply(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
	e.model.On(pfake.Contains("create an item")).Reply(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
	e.onAct("request_confirmation", "Please confirm: create an item named widget.")
	e.onAct("report_outcome", "Done. The item widget was created.")
	read := e.say(memberScope(), sayShow)
	write := e.say(memberScope(), sayCreate)
	out := e.turn(memberScope(), confirmInput(write))
	for name, res := range map[string]Result{"read": read, "write (prepare)": write, "confirm (commit)": out} {
		for _, c := range res.Trace.ModelCalls {
			t.Logf("demo app, %-18s %-9s est_toks=%4d bytes=%5d out_toks=%d", name, c.Purpose, c.PromptToks, c.PromptBytes, c.TokOut)
			if c.PromptToks > ctxLiteCap {
				t.Errorf("%s %s: %d estimated tokens", name, c.Purpose, c.PromptToks)
			}
		}
	}
}

func benchEnv(b *testing.B) *env {
	b.Helper()
	e := newEnv(b, withLevel(level2), noLimit())
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	e.model.On(pfake.Contains("show me")).Reply(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
	e.model.On(pfake.Contains("create an item")).Reply(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
	e.onAct("request_confirmation", "Please confirm: create an item named widget.")
	e.onAct("report_outcome", "Done. The item widget was created.")
	return e
}

// BenchmarkTurnRead measures the pipeline of a read turn with a model that answers
// instantly: everything except inference (routing with the policy gateway, the
// session store, the coordinator, the host call, the verifier).
func BenchmarkTurnRead(b *testing.B) {
	e := benchEnv(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.eng.Turn(ctx, memberScope(), TurnInput{Text: sayShow, Language: "en"}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTurnWriteAndConfirm measures a prepare turn and its confirmation turn.
func BenchmarkTurnWriteAndConfirm(b *testing.B) {
	e := benchEnv(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := e.eng.Turn(ctx, memberScope(), TurnInput{Text: sayCreate, Language: "en"})
		if err != nil || res.Prepared == nil {
			b.Fatalf("prepare: %v %+v", err, res.Directive.Act)
		}
		if _, err := e.eng.Turn(ctx, memberScope(), confirmInput(res)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildProposal measures building the proposal prompt of five capabilities
// and a goal, the part of a turn that scales with the catalogue.
func BenchmarkBuildProposal(b *testing.B) {
	in := proposalInput{App: "demo-library", Now: t0, Lang: verify.LangEN, Utterance: strings.Repeat("reserve a book ", 8), Caps: realisticCatalogue(verify.LangEN), Turns: typicalTurns()}
	builder := ctxBuilderFor(b, dctx.Lite())
	ctx, scope := context.Background(), memberScope()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildProposal(ctx, builder, scope, in); err != nil {
			b.Fatal(err)
		}
	}
}
