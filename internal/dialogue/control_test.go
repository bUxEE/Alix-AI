package dialogue

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// ---- the rule detectors ----

func TestRuleDetectorsAreExactAndNeverMatchInsideASentence(t *testing.T) {
	d := RuleDetectors()
	ctx := context.Background()
	type row struct {
		text string
		want []string // the detectors that match, among help cancel restart resume reject confirm correct
	}
	run := func(text string) []string {
		req := ControlRequest{Text: text, Language: verify.LangEN}
		var got []string
		for _, c := range []struct {
			name string
			d    ControlDetector
		}{{"help", d.Help}, {"cancel", d.Cancel}, {"restart", d.Restart}, {"resume", d.Resume}, {"reject", d.Reject}, {"confirm", d.Confirm}, {"correct", d.Correct}} {
			if c.d.Detect(ctx, req).Matched {
				got = append(got, c.name)
			}
		}
		return got
	}
	for _, r := range []row{
		{"help", []string{"help"}},
		{"What can you do?", []string{"help"}},
		{"  AIUTO!! ", []string{"help"}},
		{"cancel", []string{"cancel"}},
		{"Cancel that.", []string{"cancel"}},
		{"never mind", []string{"cancel"}},
		{"annulla", []string{"cancel"}},
		{"cancel everything", []string{"cancel"}},
		{"start over", []string{"restart"}},
		{"Let's start over", []string{"restart"}},
		{"ricomincia da capo", []string{"restart"}},
		{"resume", []string{"resume"}},
		{"go back to the item lookup", []string{"resume"}},
		{"continue", []string{"resume"}},
		{"no", []string{"reject"}},
		{"No thanks", []string{"reject"}},
		{"yes", []string{"confirm"}},
		{"Yes, please!", []string{"confirm"}},
		{"ok", []string{"confirm"}},
		{"sì", []string{"confirm"}},
		// Not exact: a phrase inside a sentence is a sentence for the controller.
		{"no, make it 5", []string{"correct"}},
		{"yes, I confirm, go ahead and do it", nil},
		{"cancel the appointment with Dr Rossi", nil},
		{"I would like help with my invoice", nil},
		{"please don't cancel", nil},
		{"actually the name is gadget", []string{"correct"}},
		{"make it 5 instead", []string{"correct"}},
		{"cambia la quantità", []string{"correct"}},
		{"stop the presses at five", nil},
		{"", nil},
	} {
		if diff := cmp.Diff(r.want, run(r.text)); diff != "" {
			t.Errorf("%q: detectors (-want +got):\n%s", r.text, diff)
		}
	}

	if got := d.Cancel.Detect(ctx, ControlRequest{Text: "annulla tutto"}); !got.Matched || !got.All {
		t.Errorf("cancel all = %+v", got)
	}
	if got := d.Resume.Detect(ctx, ControlRequest{Text: "go back"}); !got.Matched || !got.Strong || got.Target != "" {
		t.Errorf("go back = %+v", got)
	}
	if got := d.Resume.Detect(ctx, ControlRequest{Text: "continue"}); !got.Matched || got.Strong {
		t.Errorf("continue = %+v", got)
	}
	if got := d.Resume.Detect(ctx, ControlRequest{Text: "Go back to the create item!"}); !got.Matched || !got.Strong || got.Target != "create item" {
		t.Errorf("go back to ... = %+v", got)
	}
}

// ---- the seam ----

// A detector another decision procedure supplies decides what an utterance IS; what to
// do about it stays the engine's: its transitions check the state and reach no commit.
func TestDetectorsAreASeamNotAnAuthority(t *testing.T) {
	sc := memberScope()
	always := func(m ControlDetection) ControlDetector {
		return ControlDetectorFunc(func(context.Context, ControlRequest) ControlDetection { return m })
	}

	t.Run("a replaced detector decides for its decision", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withDeps(func(d *Deps) {
			d.Detectors = Detectors{Cancel: ControlDetectorFunc(func(_ context.Context, r ControlRequest) ControlDetection {
				return ControlDetection{Matched: r.Text == "scrap the whole thing"}
			})}
		}))
		e.prepareCreate(sc)
		res := e.say(sc, "scrap the whole thing")
		wantAct(t, res, "acknowledge_cancel")
		if g := e.goal(sc); g != nil {
			t.Fatalf("goal = %+v", g)
		}
		// The members it did not replace are the rules.
		e2 := newEnv(t, withDeps(func(d *Deps) { d.Detectors = Detectors{Cancel: always(ControlDetection{})} }))
		wantAct(t, e2.say(sc, "help"), "offer_help")
	})

	t.Run("a detector that says confirm cannot commit", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withDeps(func(d *Deps) { d.Detectors = Detectors{Confirm: always(ControlDetection{Matched: true})} }))
		prep := e.prepareCreate(sc)
		res := e.say(sc, "whatever you say")
		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil || res.Prepared.PreparationID != prep.Prepared.PreparationID {
			t.Fatalf("prepared = %+v", res.Prepared)
		}
		e.noCommits()
	})

	t.Run("a detection the state cannot take is ignored", func(t *testing.T) {
		everything := Detectors{
			Reject: always(ControlDetection{Matched: true}), Confirm: always(ControlDetection{Matched: true}),
			Resume: always(ControlDetection{Matched: true, Strong: true}), Restart: always(ControlDetection{Matched: true}),
		}
		// No goal, nothing prepared, nothing set aside: none of them has anything to act on.
		e := newEnv(t, withDeps(func(d *Deps) { d.Detectors = everything }))
		res := e.say(sc, "hello")
		wantAct(t, res, "refuse")
		if len(res.Trace.Transitions) != 0 {
			t.Fatalf("transitions = %v", res.Trace.Transitions)
		}
		// A goal that is not prepared cannot be rejected or reconfirmed.
		e2 := newEnv(t, withLevel(level2), withDeps(func(d *Deps) {
			d.Detectors = Detectors{Reject: always(ControlDetection{Matched: true}), Confirm: always(ControlDetection{Matched: true})}
		}))
		e2.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		e2.say(sc, "create an item named widget")
		e2.proposeOnce(capCreate, "task", map[string]any{"quantity": 2})
		out := e2.say(sc, "2")
		wantAct(t, out, "request_confirmation")
		if g := e2.goal(sc); g == nil || g.Slots["name"].Value != "widget" {
			t.Fatalf("goal = %+v", g)
		}
	})

	t.Run("a detector that panics matches nothing", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withDeps(func(d *Deps) {
			d.Detectors = Detectors{Cancel: ControlDetectorFunc(func(context.Context, ControlRequest) ControlDetection { panic("boom") })}
		}))
		e.prepareCreate(sc)
		e.proposeOnce(capCreate, "task", map[string]any{"quantity": 4})
		res := e.say(sc, "cancel")
		if res.Directive.Act == "acknowledge_cancel" {
			t.Fatal("a detector that panicked cancelled the goal")
		}
	})

	t.Run("the state a detector sees holds no value", func(t *testing.T) {
		var seen []ControlRequest
		e := newEnv(t, withLevel(level2), withDeps(func(d *Deps) {
			d.Detectors = Detectors{Help: ControlDetectorFunc(func(_ context.Context, r ControlRequest) ControlDetection {
				seen = append(seen, r)
				return ControlDetection{}
			})}
		}))
		e.prepareCreate(sc)
		if len(seen) == 0 {
			t.Fatal("the detector was not asked")
		}
		want := ControlState{HasGoal: false}
		if diff := cmp.Diff(want, seen[0].State); diff != "" {
			t.Fatalf("state of the first turn (-want +got):\n%s", diff)
		}
		e.say(sc, "yes")
		if last := seen[len(seen)-1].State; !last.HasGoal || !last.Prepared || last.Suspended != 0 {
			t.Fatalf("state = %+v", last)
		}
	})
}

// ---- references ----

func TestParseRefs(t *testing.T) {
	type want struct {
		kind    refKind
		ordinal int
		noun    string
		days    int
		weekday *time.Weekday
		pure    bool
	}
	fri, mon := time.Friday, time.Monday
	for _, tc := range []struct {
		text string
		lang verify.Language
		want []want
	}{
		{"the second one", verify.LangEN, []want{{kind: refOrdinal, ordinal: 2, pure: true}}},
		{"The last one!", verify.LangEN, []want{{kind: refOrdinal, ordinal: -1, pure: true}}},
		{"third", verify.LangEN, []want{{kind: refOrdinal, ordinal: 3, pure: true}}},
		{"number 2", verify.LangEN, []want{{kind: refOrdinal, ordinal: 2, pure: true}}},
		{"option #4 please", verify.LangEN, []want{{kind: refOrdinal, ordinal: 4, pure: true}}},
		{"show me the first item", verify.LangEN, []want{{kind: refOrdinal, ordinal: 1, pure: true}}},
		{"I'll take the 2nd one", verify.LangEN, []want{{kind: refOrdinal, ordinal: 2, pure: true}}},
		{"my first name is Bob", verify.LangEN, nil},
		{"first, create an item named widget", verify.LangEN, nil},
		{"il secondo", verify.LangIT, []want{{kind: refOrdinal, ordinal: 2, pure: true}}},
		{"la terza", verify.LangIT, []want{{kind: refOrdinal, ordinal: 3, pure: true}}},
		{"l'ultimo", verify.LangIT, nil},
		{"quello ultimo", verify.LangIT, []want{{kind: refOrdinal, ordinal: -1, pure: true}}},
		{"secondo me va bene", verify.LangIT, nil},
		{"prima di domani", verify.LangIT, nil},
		{"that patient", verify.LangEN, []want{{kind: refThat, noun: "patient", pure: true}}},
		{"book that patient again", verify.LangEN, []want{{kind: refThat, noun: "patient", pure: true}}},
		{"that one", verify.LangEN, []want{{kind: refThat, noun: "one", pure: true}}},
		{"the same patient", verify.LangEN, []want{{kind: refThat, noun: "patient", pure: true}}},
		{"quel paziente", verify.LangIT, []want{{kind: refThat, noun: "paziente", pure: true}}},
		{"lo stesso paziente", verify.LangIT, []want{{kind: refThat, noun: "paziente", pure: true}}},
		{"book that patient for a long and detailed checkup", verify.LangEN, []want{{kind: refThat, noun: "patient"}}},
		{"same time tomorrow", verify.LangEN, []want{{kind: refSameTime, days: 1, pure: true}}},
		{"the same time on friday", verify.LangEN, []want{{kind: refSameTime, weekday: &fri, pure: true}}},
		{"same time next monday", verify.LangEN, []want{{kind: refSameTime, weekday: &mon, pure: true}}},
		{"same time in 2 weeks", verify.LangEN, []want{{kind: refSameTime, days: 14, pure: true}}},
		{"tomorrow at the same time", verify.LangEN, []want{{kind: refSameTime, days: 1, pure: true}}},
		{"same time", verify.LangEN, nil},
		{"tomorrow", verify.LangEN, nil},
		{"domani alla stessa ora", verify.LangIT, []want{{kind: refSameTime, days: 1, pure: true}}},
		{"stesso orario fra 3 giorni", verify.LangIT, []want{{kind: refSameTime, days: 3, pure: true}}},
		{"", verify.LangEN, nil},
	} {
		refs, pure := parseRefs(tc.text, tc.lang)
		var got []want
		for _, r := range refs {
			got = append(got, want{kind: r.kind, ordinal: r.ordinal, noun: r.noun, days: r.days, weekday: r.weekday, pure: pure})
		}
		if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(want{})); diff != "" {
			t.Errorf("%q (%s): references (-want +got):\n%s", tc.text, tc.lang, diff)
		}
	}
}

// refTurn is a turn over a hand-built state, for the resolution rules.
func refTurn(t *testing.T, st sessions.State, lang verify.Language) *turn {
	t.Helper()
	return &turn{
		pkg: buildPackage(t, pkgOpts{resolveItems: true}), now: t0, lang: lang,
		sess: sessions.Session{State: st}, userText: "",
	}
}

func TestThatEntityResolutionOrderAndAmbiguity(t *testing.T) {
	ent := func(id string) sessions.ResolvedEntity {
		return sessions.ResolvedEntity{Field: "item_id", EntityType: "item", EntityID: id, Version: "v1"}
	}
	items := func(ids ...string) []sessions.ResultItem {
		var out []sessions.ResultItem
		for _, id := range ids {
			out = append(out, sessions.ResultItem{EntityType: "item", EntityID: id, Version: "v9"})
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		state  sessions.State
		wantID string
		wantOK bool
	}{
		{"the active goal's own record first", sessions.State{
			Goal:    &sessions.Goal{Entities: []sessions.ResolvedEntity{ent("item_goal")}},
			Results: []sessions.ResultSummary{{Items: items("item_res")}},
		}, "item_goal", true},
		{"the latest result when it presented one record", sessions.State{
			Results: []sessions.ResultSummary{{Items: items("item_old")}, {Items: items("item_new")}},
		}, "item_new", true},
		{"a list is ambiguous and does not fall back to an older result", sessions.State{
			Results: []sessions.ResultSummary{{Items: items("item_old")}, {Items: items("item_a", "item_b")}},
		}, "", false},
		{"a record of another type is not it", sessions.State{
			Results: []sessions.ResultSummary{{Items: []sessions.ResultItem{{EntityType: "patient", EntityID: "pat_1"}}}},
		}, "", false},
		{"a goal set aside", sessions.State{
			Suspended: []*sessions.Goal{{Entities: []sessions.ResolvedEntity{ent("item_old")}}, {Entities: []sessions.ResolvedEntity{ent("item_susp")}}},
		}, "item_susp", true},
		{"a completed goal", sessions.State{
			Capsules: []sessions.Capsule{{Entities: []sessions.ResolvedEntity{ent("item_caps")}}},
		}, "item_caps", true},
		{"nothing", sessions.State{}, "", false},
	} {
		tt := refTurn(t, tc.state, verify.LangEN)
		got, ok := tt.thatEntity("item")
		if ok != tc.wantOK || got.EntityID != tc.wantID {
			t.Errorf("%s: thatEntity = %+v, %v; want %q, %v", tc.name, got, ok, tc.wantID, tc.wantOK)
		}
	}
}

func TestOrdinalPoolPrefersTheQuestionJustAsked(t *testing.T) {
	choices := func(ids ...string) []act.Choice {
		var out []act.Choice
		for _, id := range ids {
			out = append(out, act.Choice{ChoiceID: common.Identifier(id), Label: id, Value: identifierValue(id)})
		}
		return out
	}
	st := sessions.State{
		Goal: &sessions.Goal{Pending: &sessions.Clarification{
			Field: "item_id", Kind: sessions.ClarifyChoice, EntityType: "item", Version: "v7", Choices: choices("item_x", "item_y"),
		}},
		Results: []sessions.ResultSummary{{Items: []sessions.ResultItem{{EntityType: "item", EntityID: "item_r1"}, {EntityType: "item", EntityID: "item_r2"}}}},
	}
	tt := refTurn(t, st, verify.LangEN)
	pool := tt.entityPool("item", "item_id")
	if len(pool) != 2 || pool[0].EntityID != "item_x" || pool[0].Version != "v7" {
		t.Fatalf("pool = %+v, want the offered choices with their version", pool)
	}
	if pool := tt.entityPool("item", "other_field"); len(pool) != 2 || pool[0].EntityID != "item_r1" {
		t.Fatalf("pool for another field = %+v, want the latest result", pool)
	}
	if pool := tt.entityPool("patient", ""); len(pool) != 0 {
		t.Fatalf("pool for another type = %+v", pool)
	}
}

func TestTimeOfDayComesFromTheNearestDateAndTime(t *testing.T) {
	at := func(h int) string { return time.Date(2026, 9, 1, h, 15, 0, 0, time.UTC).Format("2006-01-02T15:04:05Z") }
	slot := func(v string) sessions.Slot {
		return sessions.Slot{Value: v, Origin: sessions.OriginUserTurn, Type: "datetime"}
	}
	tod := func(h int) time.Duration { return time.Duration(h)*time.Hour + 15*time.Minute }
	for _, tc := range []struct {
		name  string
		state sessions.State
		field string
		want  time.Duration
		ok    bool
	}{
		{"the field's own value", sessions.State{Goal: &sessions.Goal{Slots: map[string]sessions.Slot{"when": slot(at(8)), "other": slot(at(9))}}}, "when", tod(8), true},
		{"another date and time of the goal", sessions.State{Goal: &sessions.Goal{Slots: map[string]sessions.Slot{"other": slot(at(9))}}}, "when", tod(9), true},
		{"a goal set aside", sessions.State{Suspended: []*sessions.Goal{{Slots: map[string]sessions.Slot{"when": slot(at(10))}}}}, "when", tod(10), true},
		{"a capsule's kept fact", sessions.State{Capsules: []sessions.Capsule{{Facts: map[string]string{"when": at(11)}}}}, "when", tod(11), true},
		{"the newest capsule", sessions.State{Capsules: []sessions.Capsule{{Facts: map[string]string{"when": at(11)}}, {Facts: map[string]string{"when": at(12)}}}}, "when", tod(12), true},
		{"a value that is not a date and time", sessions.State{Goal: &sessions.Goal{Slots: map[string]sessions.Slot{"name": {Value: "widget", Origin: sessions.OriginUserTurn}}}}, "when", 0, false},
		{"nothing", sessions.State{}, "when", 0, false},
	} {
		got, ok := refTurn(t, tc.state, verify.LangEN).timeOfDay(tc.field)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: timeOfDay = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMustStateNamesEachFieldOnce(t *testing.T) {
	label := func(s string) act.Fact { return textFact("label", s) }
	got := mustStateAll([]act.Fact{label("Alpha"), textFact("total", "3"), label("Beta"), label("Gamma")})
	if diff := cmp.Diff([]common.FieldName{"label", "total"}, got); diff != "" {
		t.Fatalf("must_state (-want +got):\n%s", diff)
	}
	if got := mustStateAll(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty must_state = %#v, want a non-nil empty list (the schema requires the array)", got)
	}
}
