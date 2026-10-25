package isolationsec

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// markers are the strings unique to one tenant, one per source a prompt is built from. They are
// generated for this run and none of them is a word of any question.
type markers struct {
	tenant string // the tenant identifier
	// the application's data: the family name of a patient the tenant's host holds, and its identifier
	adapter, patientID string
	// a sentence of the tenant's own document
	rag string
	// what the tenant's user said earlier in the conversation
	history string
	// a value collected for the booking the tenant's conversation is in the middle of
	state string
	// what the tenant's client supplied: a line of readable context, and the description and a parameter
	// name of a UI tool it declared
	client, tool, schema string
}

func newMarkers(tag string) markers {
	return markers{
		tenant:  canary("tnt-" + tag + "-"),
		adapter: canary("fam" + tag), patientID: "pat_" + canary("")[1:13],
		rag:     canary("rag" + tag),
		history: canary("his" + tag), state: canary("sta" + tag),
		client: canary("cli" + tag), tool: canary("too" + tag), schema: canary("sch" + tag),
	}
}

// needles lists everything of the tenant that no other tenant's prompt may carry.
func (m markers) needles() map[string]string {
	return map[string]string{
		"the tenant":                  m.tenant,
		"a record of the application": m.adapter,
		"a record id":                 m.patientID,
		"a sentence of a document":    m.rag,
		"a message of the history":    m.history,
		"a collected value":           m.state,
		"a line of client context":    m.client,
		"a UI tool description":       m.tool,
		"a UI tool parameter":         m.schema,
	}
}

// docFor is the tenant's document: the same wording for every tenant, and one sentence of its own.
func docFor(m markers) map[string]string {
	return map[string]string{"reception.md": "# Reception\n\n" +
		"The reception of the clinic is open from eight in the morning until six in the evening on weekdays.\n" +
		"The reception desk code for visitors of this clinic is " + m.rag + ".\n"}
}

// clientFor is what the tenant's client supplies with a turn.
func clientFor(m markers) client {
	params := fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"}},"required":[]}`, m.schema)
	return client{
		lines: []string{"Open screen: patient list, filter " + m.client},
		tools: []dialogue.UIToolDeclaration{{Name: "open_panel", Description: "Opens the panel " + m.tool, Parameters: json.RawMessage(params)}},
	}
}

const (
	askKnowledge = "What are the opening hours of the reception and which code do visitors need?"
	askFree      = "Which appointments are free on 2026-10-14?"
	askFreeLater = "Which appointments are free on 2026-10-15?"
)

// turnsOf runs the turns of one tenant's conversation: a question for the documents, a search of the
// application's data with a remark the user wants remembered, a booking left waiting for its
// confirmation with a value collected, and a listing. The questions are the same for every tenant; only
// the words the tenant's own user adds, and what its client supplies, are its own.
func (w *world) turnsOf(sc contracts.Scope, m markers, given string) {
	w.t.Helper()
	c := clientFor(m)
	k := w.say(sc, client{}, askKnowledge)
	wantAct(w.t, k, "present_result")
	if len(k.Directive.Sources) == 0 {
		w.t.Fatalf("tenant %s: the question for the documents was answered with no source: %+v", sc.TenantID, k.Directive)
	}
	f := w.say(sc, c, "Find the patient "+given+". Note for later: my locker code is "+m.history)
	wantAct(w.t, f, "present_result")
	if n, _ := factOf(f, "patient_count"); n != "1" {
		w.t.Fatalf("tenant %s: patient_count = %q, want its one patient", sc.TenantID, n)
	}
	b := w.say(sc, client{}, "Book Luca Ferri for 2026-10-14 10:30 reason: "+m.state)
	wantAct(w.t, b, "request_confirmation")
	w.say(sc, c, askFree)
}

// factOf returns the display (or canonical) text of the first fact of the result named field.
func factOf(res dialogue.Result, field string) (string, bool) {
	for _, f := range res.Directive.Facts {
		if string(f.Field) != field {
			continue
		}
		b, _ := json.Marshal(f)
		var m struct {
			Value struct {
				Value any `json:"value"`
			} `json:"value"`
			Display string `json:"display"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Display != "" {
			return m.Display, true
		}
		return fmt.Sprint(m.Value.Value), true
	}
	return "", false
}

// frameOf is the trusted system message of a proposal prompt: the application, the rules and the
// capabilities the model is offered.
func frameOf(c sent) string {
	if len(c.Prompt.Chat.Messages) == 0 {
		return ""
	}
	return c.Prompt.Chat.Messages[0].Content
}

// SEC-029
//
// Text belonging to another tenant reaches no model prompt on any path. Tenant B is seeded with a unique
// marker in every source a prompt is built from, tenant A asks the same questions as B and then some, and
// every prompt sent for A is scanned for every marker of B (and the reverse). B's own prompts carry B's
// markers, which is what proves the scan can see each path, and a marker planted in a source of A is
// found by the same scan.
func TestSecNoForeignTextInAnyPrompt(t *testing.T) {
	mA, mB := newMarkers("a"), newMarkers("b")
	session := canary("ses-")
	w := newWorld(t, []string{mA.tenant, mB.tenant}, withRAG())
	// Tenant B's token does not permit one capability of the application: its shortlist is its own.
	scA, scB := caller(mA.tenant, session), caller(mB.tenant, session, "patients.update_contact")
	if scA.PrincipalID != scB.PrincipalID || scA.WorkspaceID != scB.WorkspaceID || scA.SessionID != scB.SessionID {
		t.Fatal("the two callers must differ in the tenant only")
	}

	// The seeds: the application's data of each tenant, and the documents of each.
	w.plant(mA.tenant, mA.patientID, "Ada", mA.adapter)
	w.plant(mB.tenant, mB.patientID, "Bea", mB.adapter)
	srcA := w.addSource(scA, "docs-a", docFor(mA))
	w.addSource(scB, "docs-b", docFor(mB))

	// The settings of B: a document of its own, and an attempt to put a marker in one.
	tenantSettings := func(sc contracts.Scope) contracts.Scope {
		s := sc.Clone()
		s.CapabilityScopes = append(s.CapabilityScopes, settings.CapabilityTenantRead, settings.CapabilityTenantWrite, settings.CapabilityUserRead)
		return s
	}
	settingsB := tenantSettings(scB)
	rec, err := w.settings.Put(bg, settingsB, settings.LevelTenant, settings.PutRequest{
		Settings: json.RawMessage(`{"default_language":"it","capabilities":{"disabled":["appointments.cancel"]}}`)})
	if err != nil {
		t.Fatalf("tenant B's settings were refused: %v", err)
	}
	settingsDoc := string(rec.Settings)
	for _, free := range []string{
		`{"note":"` + mB.history + `"}`,
		`{"capabilities":{"disabled":["` + mB.history + `"]}}`,
		`{"default_language":"` + mB.history + `"}`,
	} {
		if _, err := w.settings.Put(bg, settingsB, settings.LevelTenant, settings.PutRequest{ExpectedVersion: rec.Version, Settings: json.RawMessage(free)}); err == nil {
			t.Fatalf("a settings document that carries free text was accepted: %s", free)
		}
	}
	if again, err := w.settings.Get(bg, settingsB, settings.LevelTenant); err != nil || again.Version != rec.Version || strings.Contains(string(again.Settings), mB.history) {
		t.Fatalf("tenant B's settings after the refused documents = %+v, %v", again, err)
	}

	// The conversations. B first, then A with the same questions, then B again: a leak through any state
	// shared between the tenants would show in either direction.
	w.turnsOf(scB, mB, "Bea")
	w.turnsOf(scA, mA, "Ada")
	// A asks for B's patient by name, and B for A's: the application answers each from its own data.
	for _, c := range []struct {
		sc    contracts.Scope
		m     markers
		other string
	}{{scA, mA, "Bea"}, {scB, mB, "Ada"}} {
		res := w.say(c.sc, clientFor(c.m), "Find the patient "+c.other)
		wantAct(t, res, "present_result")
		if n, _ := factOf(res, "patient_count"); n != "0" {
			t.Fatalf("a tenant found %q patients named %s in its own data", n, c.other)
		}
	}
	w.say(scB, clientFor(mB), askFreeLater)
	w.say(scA, clientFor(mA), askFreeLater)

	calls := w.sent()
	forA, forB := forTenant(calls, mA.tenant), forTenant(calls, mB.tenant)
	if len(forA) < 12 || len(forB) < 12 {
		t.Fatalf("calls for A: %d, for B: %d: the scenario did not run", len(forA), len(forB))
	}
	for _, c := range calls {
		if c.Tenant != mA.tenant && c.Tenant != mB.tenant {
			t.Fatalf("a call was made under the tenant %q", c.Tenant)
		}
	}

	t.Run("no marker of one tenant is in any prompt sent for the other", func(t *testing.T) {
		needlesB, needlesA := mB.needles(), mA.needles()
		needlesB["tenant B's settings document"] = settingsDoc
		needlesB["the other's patient (given name and record)"] = "Bea " + mB.adapter
		needlesA["the other's patient (given name and record)"] = "Ada " + mA.adapter
		if hits := scan(forA, needlesB); len(hits) != 0 {
			t.Errorf("text of tenant B reached a prompt sent for tenant A:\n  %s", strings.Join(hits, "\n  "))
		}
		if hits := scan(forB, needlesA); len(hits) != 0 {
			t.Errorf("text of tenant A reached a prompt sent for tenant B:\n  %s", strings.Join(hits, "\n  "))
		}
		// Nothing of tenant B's settings reaches a prompt of its own either: settings are not an input to any
		// prompt, and the schemas are closed (see above), so there is no marker to plant in one.
		if hits := scan(calls, map[string]string{"tenant B's settings document": settingsDoc}); len(hits) != 0 {
			t.Errorf("a settings document reached a prompt:\n  %s", strings.Join(hits, "\n  "))
		}
	})

	// The positive controls: the markers of a tenant do reach its own prompts, on every path, so the scan
	// above looked at text that was there to be found.
	t.Run("control: a tenant's own markers reach its own prompts on every path", func(t *testing.T) {
		for _, c := range []struct {
			who   string
			calls []sent
			m     markers
		}{{"A", forA, mA}, {"B", forB, mB}} {
			path := func(what, needle, model string, kinds ...string) {
				t.Helper()
				for _, call := range c.calls {
					if call.Model == model && (len(kinds) == 0 || slices.Contains(kinds, call.Kind)) && scan([]sent{call}, map[string]string{what: needle}) != nil {
						return
					}
				}
				t.Errorf("tenant %s: %s (%q) never reached a prompt of the %s model (%v)", c.who, what, needle, model, kinds)
			}
			path("the application's data", c.m.adapter, "dialogue", "chat/text")
			path("a document", c.m.rag, "rag", "chat/constrained", "chat/text")
			path("a document, at ingestion", c.m.rag, "rag", "embed")
			path("the history", c.m.history, "dialogue", "chat/constrained")
			path("a collected value", c.m.state, "dialogue", "chat/constrained")
			path("client context", c.m.client, "dialogue", "chat/constrained")
			path("a UI tool description", c.m.tool, "dialogue", "chat/constrained")
			path("a UI tool parameter", c.m.schema, "dialogue", "chat/constrained")
		}
	})

	// The capability shortlist: each tenant's frame is its own, and A's is the one a world with no other
	// tenant gives.
	t.Run("the capability shortlist of a tenant is its own", func(t *testing.T) {
		const update = "\n- patients.update_contact:"
		var framesA, framesB []string
		for _, c := range forA {
			if c.Kind == "chat/constrained" && c.Model == "dialogue" {
				framesA = append(framesA, frameOf(c))
			}
		}
		for _, c := range forB {
			if c.Kind == "chat/constrained" && c.Model == "dialogue" {
				framesB = append(framesB, frameOf(c))
			}
		}
		if len(framesA) == 0 || len(framesB) == 0 {
			t.Fatalf("frames for A: %d, for B: %d", len(framesA), len(framesB))
		}
		offered := func(frames []string) bool {
			for _, f := range frames {
				if strings.Contains(f, update) {
					return true
				}
			}
			return false
		}
		if !offered(framesA) {
			t.Error("tenant A, whose token permits every capability, was never offered patients.update_contact")
		}
		if offered(framesB) {
			t.Error("tenant B, whose token does not permit patients.update_contact, was offered it")
		}
		// The frame A gets beside B is the frame A gets alone.
		alone := newWorld(t, []string{mA.tenant}, withRAG())
		alone.plant(mA.tenant, mA.patientID, "Ada", mA.adapter)
		alone.say(scA, clientFor(mA), "Find the patient Ada. Note for later: my locker code is "+mA.history)
		var ref string
		for _, c := range forTenant(alone.sent(), mA.tenant) {
			if c.Kind == "chat/constrained" && c.Model == "dialogue" {
				ref = frameOf(c)
			}
		}
		if ref == "" {
			t.Fatal("no frame in the reference world")
		}
		if !slices.Contains(framesA, ref) {
			t.Errorf("none of tenant A's frames is the frame of a world in which tenant B does not exist:\n%s", ref)
		}
		if slices.Contains(framesB, ref) {
			t.Error("the comparison cannot tell tenant B's frames from tenant A's")
		}
	})

	// A marker planted into one of A's sources is found by the same scan, on each path.
	t.Run("control: a marker planted in a source of A is detected", func(t *testing.T) {
		before := w.mark()
		fresh := func() []sent { return w.since(before, mA.tenant) }

		// The application's data of A holds a patient with B's marker as a family name.
		w.plant(mA.tenant, "pat_"+canary("")[1:13], "Cyd", mB.adapter)
		w.say(scA, client{}, "Find the patient Cyd")
		hits := scan(fresh(), map[string]string{"a record of the application": mB.adapter})
		if len(hits) == 0 {
			t.Error("the scan did not see B's record planted in A's application data")
		}
		// The client of A supplies a line, a UI tool description and a parameter carrying B's markers.
		before = w.mark()
		w.say(scA, client{
			lines: []string{"Open screen: " + mB.client},
			tools: []dialogue.UIToolDeclaration{{Name: "open_panel", Description: "Opens " + mB.tool,
				Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"}}}`, mB.schema))}},
		}, askFree)
		hits = scan(fresh(), map[string]string{"a line of client context": mB.client, "a UI tool description": mB.tool, "a UI tool parameter": mB.schema})
		if len(hits) < 3 {
			t.Errorf("the scan did not see B's client text planted in A's client (hits: %v)", hits)
		}
		// The history of A holds a message with B's marker.
		before = w.mark()
		w.say(scA, client{}, "Find the patient Ada. Note for later: my locker code is "+mB.history)
		w.say(scA, client{}, askFreeLater)
		if hits := scan(fresh(), map[string]string{"a message of the history": mB.history}); len(hits) < 2 {
			t.Errorf("the scan did not see B's marker in A's history, asked and replayed (hits: %v)", hits)
		}
		// A document of A's source holds B's sentence.
		before = w.mark()
		w.addFile(srcA, "planted.md", "# Planted\n\nThe reception desk code for visitors of this clinic is "+mB.rag+" planted.\n")
		w.say(scA, client{}, askKnowledge)
		if hits := scan(fresh(), map[string]string{"a sentence of a document": mB.rag}); len(hits) == 0 {
			t.Error("the scan did not see B's sentence planted in A's documents")
		}
	})
}
