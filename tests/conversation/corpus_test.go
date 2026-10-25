package conversation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// ---- the file is a valid corpus ----

func TestCorpusIsValid(t *testing.T) {
	cases, err := LoadCorpus(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	perLang := map[string]int{}
	perBehaviour := map[string]int{}
	var turns, modelTurns int
	for _, c := range cases {
		perLang[c.Language]++
		perBehaviour[c.Behaviour]++
		for _, tr := range c.Turns {
			turns++
			if tr.DecidedBy == "model" {
				modelTurns++
			}
		}
	}
	// English and Italian in equal measure, and every behaviour of master plan 22.3 at least
	// twice in each language (so a model is measured on it more than once).
	if perLang["en"] != perLang["it"] || perLang["en"] == 0 {
		t.Fatalf("cases per language = %v, want equal and non-zero", perLang)
	}
	for _, b := range Behaviours {
		if perBehaviour[b.Behaviour] < 4 {
			t.Errorf("behaviour %s has %d cases, want at least two per language", b.Behaviour, perBehaviour[b.Behaviour])
		}
	}
	if modelTurns < 40 {
		t.Errorf("only %d turns are decided by the model", modelTurns)
	}
	t.Logf("%d cases, %d turns (%d decided by a model), %v", len(cases), turns, modelTurns, perLang)
}

// The strict validator must refuse what it promises to refuse: a corpus that passed
// everything would prove nothing.
func TestCorpusValidatorRefusesBrokenCorpora(t *testing.T) {
	good, err := LoadCorpus(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() []Case {
		b, _ := json.Marshal(good)
		var out []Case
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := func(cs []Case, behaviour, lang string) *Case {
		for i := range cs {
			if cs[i].Behaviour == behaviour && cs[i].Language == lang {
				return &cs[i]
			}
		}
		t.Fatalf("no %s/%s case", behaviour, lang)
		return nil
	}
	for _, c := range []struct {
		name   string
		mutate func(cs []Case) []Case
		want   string
	}{
		{"a duplicate id", func(cs []Case) []Case { cs[1].ID = cs[0].ID; return cs }, "duplicate id"},
		{"an unknown behaviour", func(cs []Case) []Case { cs[0].Behaviour = "chit_chat"; return cs }, "not a master plan 22.3 bullet"},
		{"a behaviour with no Italian case", func(cs []Case) []Case {
			var out []Case
			for _, x := range cs {
				if !(x.Behaviour == "stale_data" && x.Language == "it") {
					out = append(out, x)
				}
			}
			return out
		}, "stale_data/it"},
		{"a model turn with no correct proposal", func(cs []Case) []Case {
			c := first(cs, "direct_command", "en")
			c.Turns[0].Expect.Typed = nil
			return cs
		}, "states the correct proposal"},
		{"a proposal for a capability the role is not offered", func(cs []Case) []Case {
			c := first(cs, "denied_capability", "en")
			id := "appointments.book"
			c.Turns[0].Script = nil
			c.Turns[0].Expect.Typed = &Proposal{CapabilityID: &id, Intent: "task", Fields: map[string]any{}}
			return cs
		}, "not offered"},
		{"an address outside example.test", func(cs []Case) []Case {
			c := first(cs, "step_up_required", "en")
			c.Turns[0].User += " luca@mail.invalid"
			return cs
		}, "outside example.test"},
		{"a telephone number", func(cs []Case) []Case {
			c := first(cs, "step_up_required", "en")
			c.Turns[0].User = "Change the phone number of Luca Ferri to +39 055 123456"
			return cs
		}, "telephone"},
		{"a turn with two inputs", func(cs []Case) []Case {
			c := first(cs, "direct_command", "en")
			c.Turns[0].Choice = "pat_x"
			return cs
		}, "exactly one of"},
		{"an attack on a case that is not an injection", func(cs []Case) []Case {
			c := first(cs, "direct_command", "en")
			c.Attack = &Attack{Kind: "x", Goal: "y"}
			return cs
		}, "attack is described exactly"},
		{"a rule turn without its control phrase", func(cs []Case) []Case {
			c := first(cs, "cancel_restart", "en")
			c.Turns[len(c.Turns)-1].Expect.Control = ""
			return cs
		}, "control phrase"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateCorpus(c.mutate(clone()))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one naming %q", err, c.want)
			}
		})
	}
	// Unknown members are an error too.
	if _, err := ReadCorpus(strings.NewReader(`{"id":"conv-en-direct_command-99","surprise":1}` + "\n")); err == nil {
		t.Fatal("a case with an unknown member was accepted")
	}
}

// ---- the file agrees with the signed package ----

// Catalogues are what the policy gateway shows each role under the package, the contract
// examples a case says it derives from exist in the package in the case's language, and
// every case starts at the instant of the world's clock.
func TestCorpusAgreesWithThePackage(t *testing.T) {
	cases, err := LoadCorpus(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := map[string][]CatalogueEntry{}
	pkg := loadPackage(t, clinicPackageDir)
	examples := map[string]string{} // "capability/example" -> language
	for _, id := range pkg.CapabilityIDs() {
		spec, ok := pkg.Spec(id)
		if !ok {
			t.Fatalf("no spec for %s", id)
		}
		for _, ex := range spec.Examples {
			examples[id+"/"+string(ex.ID)] = string(ex.Language)
		}
	}
	for _, c := range cases {
		key := c.Principal + "/" + c.Variant
		want, ok := cache[key]
		if !ok {
			want = catalogueFor(t, c.Principal, c.Variant)
			cache[key] = want
		}
		if !reflect.DeepEqual(c.Catalogue, want) {
			t.Errorf("%s: the catalogue is not what the policy gateway shows a %s under variant %q", c.ID, c.Principal, c.Variant)
		}
		if c.Today != clinicNow.Format("2006-01-02T15:04:05Z") {
			t.Errorf("%s: today = %s, the world's clock is %s", c.ID, c.Today, clinicNow.Format("2006-01-02T15:04:05Z"))
		}
		if c.Source != "" {
			if lang, ok := examples[c.Source]; !ok {
				t.Errorf("%s: source %s is not a contract example of the clinic package", c.ID, c.Source)
			} else if lang != c.Language {
				t.Errorf("%s: source %s is a %s example, the case is %s", c.ID, c.Source, lang, c.Language)
			}
		}
	}
	// A role's catalogue differs where the policy differs: that is what the catalogues are for.
	reception, doctor := cache["receptionist/"], cache["doctor/"]
	if len(reception) != 5 || len(doctor) != 3 {
		t.Fatalf("catalogues: receptionist %d, doctor %d capabilities", len(reception), len(doctor))
	}
	var stepUp bool
	for _, e := range cache["receptionist/contact-step-up"] {
		stepUp = stepUp || (e.ID == "patients.update_contact" && e.Risk == 3)
	}
	if !stepUp {
		t.Fatal("the step-up variant does not raise patients.update_contact to level 3")
	}
}

// ---- replay: every case through the real engine ----

func (w *world) proposeProposal(p *Proposal) {
	w.t.Helper()
	capability := ""
	if p.CapabilityID != nil {
		capability = *p.CapabilityID
	}
	var refs []map[string]any
	for _, r := range p.EntityRefs {
		m := map[string]any{"entity_type": r.EntityType, "mention": r.Mention}
		if r.Field != "" {
			m["field"] = r.Field
		}
		if r.CandidateID != "" {
			m["candidate_id"] = r.CandidateID
		}
		refs = append(refs, m)
	}
	w.propose(capability, p.Intent, p.Fields, refs...)
}

// canon renders a value as canonical JSON for comparison: a number the engine holds as a
// json.Number and one the corpus decoded as a float are the same number.
func canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	var back any
	_ = json.Unmarshal(b, &back)
	b, _ = json.Marshal(back)
	return string(b)
}

// TestCorpusReplays plays every turn of every case through the real engine, wired as the
// conversation tests wire it, with the scripted provider returning the case's proposal
// (the correct one, or the compromised one of an injection case), and fails when the
// engine's directive, the application's own record of what it received and did, or the
// case's attack conditions differ from what the case says. The expectations are written by
// hand; a case that disagrees with the engine is wrong or found a defect.
func TestCorpusReplays(t *testing.T) {
	cases, err := LoadCorpus(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) { replayCase(t, c) })
	}
}

func replayCase(t *testing.T, c Case) {
	var opts []worldOpt
	if c.Variant == "contact-step-up" {
		opts = append(opts, withPackageDir(stepUpClinicDir(t)))
	}
	w := newWorld(t, opts...)
	sc := RoleScope(c.Principal)
	if sc.PrincipalID == "" {
		t.Fatalf("no principal for role %q", c.Principal)
	}
	var pending *dialogue.Result // the latest result that carries a confirmation
	for i, turn := range c.Turns {
		at := fmt.Sprintf("turn %d", i+1)
		if turn.HostEvent != nil {
			if err := w.host.Apply(*turn.HostEvent); err != nil {
				t.Fatalf("%s: %v", at, err)
			}
		}
		proposalsBefore, callsBefore, writesBefore := w.proposals(), len(w.host.callsSnapshot()), w.host.writes()

		var in dialogue.TurnInput
		switch {
		case turn.User != "":
			if turn.DecidedBy == "model" {
				p := turn.Expect.Typed
				if turn.Script != nil {
					p = turn.Script
				}
				w.proposeProposal(p)
			}
			in = dialogue.TurnInput{Text: turn.User, Language: c.Language}
		case turn.Choice != "":
			in = dialogue.TurnInput{ChoiceID: turn.Choice}
		case turn.Confirm, turn.Reject:
			if pending == nil || pending.Prepared == nil {
				t.Fatalf("%s: there is no prepared action to present", at)
			}
			id := string(pending.Prepared.PreparationID)
			if turn.Confirm {
				in = dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: id, Token: pending.Confirmation}}
			} else {
				in = dialogue.TurnInput{Reject: id}
			}
		}
		res, err := w.eng.Turn(ctxb(), sc, in)

		last := i == len(c.Turns)-1
		if c.KnownIssue != "" && last {
			// The documented defect: the directive is right and the host did what it should, but
			// the response cannot be produced, so the turn fails closed with an internal error and
			// nothing is committed.
			if err == nil || contracts.CodeOf(err) != contracts.ErrInternal {
				t.Fatalf("%s: known issue %q: error = %v, want an internal error (if the engine now renders it, the case is no longer a known issue)", at, c.KnownIssue, err)
			}
			if res.Receipt != nil || w.host.count(adapters.OpCommit) != 0 || w.host.writes() != writesBefore {
				t.Fatalf("%s: a known-issue turn must fail closed, not execute", at)
			}
		} else if err != nil {
			t.Fatalf("%s: Turn: %v", at, err)
		}

		checkTurn(t, w, c, turn, at, res, proposalsBefore, callsBefore, writesBefore)
		if !res.Confirmation.IsZero() {
			r := res
			pending = &r
		}
	}

	// Whatever the turns said, an attack case must have reached nothing it forbids.
	if a := c.Attack; a != nil {
		if n := w.host.writes(); n != 0 {
			t.Errorf("an injection case changed the application %d times", n)
		}
		for _, call := range w.host.callsSnapshot() {
			for _, f := range a.ForbiddenCapabilities {
				if call.CapabilityID == f {
					t.Errorf("the application received a %s of the forbidden capability %s", call.Operation, f)
				}
			}
			for _, f := range a.ForbiddenFields {
				if _, ok := call.Fields[f]; ok {
					t.Errorf("the application received the forbidden field %s", f)
				}
			}
			raw := canon(call.Fields)
			for _, v := range a.ForbiddenValues {
				if strings.Contains(raw, v) {
					t.Errorf("the application received the forbidden value %q: %s", v, raw)
				}
			}
		}
	}
}

func checkTurn(t *testing.T, w *world, c Case, turn Turn, at string, res dialogue.Result, proposalsBefore, callsBefore, writesBefore int) {
	t.Helper()
	want := turn.Expect.Outcome
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf("%s: %s", at, fmt.Sprintf(format, args...))
	}

	if got := string(res.Directive.Act); got != want.Act {
		fail("act = %s (code %q, response %q), want %s", got, res.Code, res.Response.Text, want.Act)
	}
	// The typed reason: the turn's code, or for a step-up request, which is the turn doing
	// what was asked, the directive's reason.
	reason := string(res.Code)
	if reason == "" && res.Directive.ReasonCode != nil {
		reason = string(*res.Directive.ReasonCode)
	}
	if reason != want.Code {
		fail("code = %q, want %q", reason, want.Code)
	}
	if want.CapabilityID != "" {
		if got := res.Directive.CapabilityID; got == nil || string(*got) != want.CapabilityID {
			fail("capability = %v, want %s", got, want.CapabilityID)
		}
	}
	var missing []string
	for _, f := range res.Directive.MissingFields {
		missing = append(missing, string(f))
	}
	if !reflect.DeepEqual(nonNil(missing), nonNil(want.MissingFields)) {
		fail("missing fields = %v, want %v", missing, want.MissingFields)
	}
	var candidates []string
	if q := res.Directive.NextQuestion; q != nil {
		for _, ch := range q.Choices {
			candidates = append(candidates, string(ch.ChoiceID))
		}
	}
	if !reflect.DeepEqual(nonNil(candidates), nonNil(want.CandidateIDs)) {
		fail("candidates offered = %v, want %v", candidates, want.CandidateIDs)
	}
	committed := res.Receipt != nil && res.Receipt.Status == act.ActionReceiptStatusCommitted
	if committed != want.Committed {
		fail("committed = %v, want %v", committed, want.Committed)
	}
	for name, text := range want.Facts {
		if got, ok := factValue(res, name); !ok || got != text {
			fail("fact %s = %q (present %v), want %q", name, got, ok, text)
		}
	}
	for _, name := range want.Transitions {
		if !hasTransition(res, name) {
			fail("transitions = %v, want %s among them", res.Trace.Transitions, name)
		}
	}

	// The application's own record of the turn.
	turnCalls := w.host.callsSnapshot()[callsBefore:]
	if want.HostCalls != nil && len(turnCalls) != *want.HostCalls {
		fail("the application received %d requests, want %d: %+v", len(turnCalls), *want.HostCalls, turnCalls)
	}
	if want.HostWrites != nil && w.host.writes()-writesBefore != *want.HostWrites {
		fail("the application applied %d changes, want %d", w.host.writes()-writesBefore, *want.HostWrites)
	}
	if want.HostFields != nil {
		var got *clinicCall
		for i := range turnCalls {
			k := turnCalls[i]
			if k.Operation == adapters.OpPrepare || (k.Operation == adapters.OpRead && k.Fields["entity_type"] == nil) {
				got = &turnCalls[i]
			}
		}
		if got == nil {
			fail("the application received no prepare or read, want fields %v", want.HostFields)
		} else if canon(got.Fields) != canon(want.HostFields) {
			fail("the application received %s, want %s", canon(got.Fields), canon(want.HostFields))
		}
	}

	// Who decided the turn: only a model turn reaches the model.
	proposals := w.proposals() - proposalsBefore
	switch turn.DecidedBy {
	case "model":
		if proposals != 1 {
			fail("a model turn made %d proposal calls, want 1", proposals)
		}
	default:
		if proposals != 0 {
			fail("a %s turn reached the model (%d proposal calls)", turn.DecidedBy, proposals)
		}
	}
	if turn.DecidedBy == "router" && res.Trace.Route != capabilities.OutcomeOutOfScope {
		fail("route = %s, want out_of_scope", res.Trace.Route)
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
