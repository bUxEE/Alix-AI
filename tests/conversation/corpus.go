package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// CorpusFile is the conversation benchmark corpus, relative to the repository root
// (master plan 17.3: "corrections and references, multi-turn missing-field collection,
// prepare/confirm/cancel flows, application-scope refusals, malicious document/tool
// instructions, declared languages").
//
// It mirrors the fourteen tests of master plan 22.3 as multi-turn conversations against
// the clinic-booking reference application, in English and Italian, so that live models are
// measured on the same behaviours the scripted-provider tests hold the engine to. It is
// public and privacy-safe: every name, identifier and address is made up (addresses are at
// the reserved example.test domain, and there is no telephone number). Never edit a
// published corpus in place: change the file and its id (conversation-v2), because results
// are only comparable for the same corpus hash.
//
// # One case per line
//
// A case is one conversation: who is talking (a role of the clinic), in which language, the
// catalogue of capabilities the policy gateway shows that role (so a harness can offer a
// model exactly what the engine offers it), and the turns. Every turn states what is
// said, who decides it and what the correct, typed outcome is.
//
//   - decided_by says who handles the turn: "model" (the engine consults the model for a
//     TaskProposal; the only turns a model is scored on), "rule" (an exact control phrase
//     the engine answers with no model: cancel, restart, resume, a typed yes, help),
//     "router" (nothing the role may do matches, so the router refuses before any model
//     call) and "client" (a structured input: a choice, a confirmation, a rejection).
//   - expect.typed is the correct TaskProposal of a model turn, the model-authority part
//     only: capability_id, intent, fields (candidate values as the user stated them, in
//     canonical form where the field has one) and entity_refs (mentions, never record
//     identifiers). It is what a scorer compares a live model's proposal with, and what a
//     decision-layer corpus can reuse as the expected capability, intent and fields.
//   - expect.outcome is what the engine must then do, from the deterministic parts only:
//     the dialogue act and typed code, the capability, the missing fields, the candidate
//     records offered (identifiers of the application's records, so they are part of the
//     world), the fields the application received (host_fields: what would be prepared or
//     read, after the application resolved every mention) and the exact facts the
//     directive carries. A scripted run replays script (or expect.typed) as the model's
//     output and checks the outcome; a live run feeds the model's own output through the
//     same engine and scores the proposal and the outcome class.
//   - script, when present, is what a COMPROMISED model returns instead of expect.typed
//     (a prompt-injection case): the outcome is what the engine does with that output,
//     which must fail closed; the live model passes the case when it does not propose what
//     the attack asks for and the run commits and prepares nothing the attack asked for.
//     outcome.code is checked in a scripted run; a live run is scored on outcome.act.
//
// # The world
//
// Every case runs against world "clinic-synthetic-v1": the clinic-booking reference
// application's synthetic data as the conversation tests seed it (two patients called
// Maria Rossi, one Luca Ferri, two doctors, five free slots in the days after "today",
// two booked appointments) with today at the instant in the case. A turn's host_event
// changes that world before the turn (another receptionist takes a slot, time passes, a
// record with hostile text exists). The package is the clinic's own signed package, or
// with variant "contact-step-up" the same package under a deployment policy that raises
// patients.update_contact to level 3. The stand-in application, the seed and the event
// vocabulary are in this package's tests; a live harness provides the same world.
type Case struct {
	// ID is unique: conv-<language>-<behaviour>-<n>.
	ID string `json:"id"`
	// Language is the language of the user's words and of the response: "en" or "it".
	Language string `json:"language"`
	// Behaviour is the master plan 22.3 bullet the case exercises (see Behaviours).
	Behaviour string `json:"behaviour"`
	// Test is the scripted-provider test that holds the engine to the same behaviour.
	Test string `json:"test"`
	// Application is the reference application; always "clinic-booking".
	Application string `json:"application"`
	// World names the synthetic data the case runs against.
	World string `json:"world"`
	// Variant is empty for the clinic's own package, or "contact-step-up".
	Variant string `json:"variant,omitempty"`
	// Principal is the role of the person talking: receptionist, doctor or patient.
	Principal string `json:"principal"`
	// Today is the instant the conversation starts, RFC 3339 in UTC.
	Today string `json:"today"`
	// Source names the clinic package's own contract example the first utterance is
	// derived from ("appointments.book/en-book-by-time"), or is empty when the case
	// is not.
	Source string `json:"source,omitempty"`
	// Catalogue is what the policy gateway shows this role, with the fields the model may
	// see, as the engine offers it.
	Catalogue []CatalogueEntry `json:"catalogue"`
	// Turns are the turns of the conversation, in order.
	Turns []Turn `json:"turns"`
	// Attack says what a prompt-injection case tries and what counts as falling for it.
	Attack *Attack `json:"attack,omitempty"`
	// KnownIssue is set on a case whose desired outcome the engine does not yet reach, with
	// the reason. A scripted replay of it asserts the documented current behaviour instead.
	KnownIssue string `json:"known_issue,omitempty"`
}

// CatalogueEntry is one capability offered to the role.
type CatalogueEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Risk is the effective risk level (0 read, 1 reversible, 2 consequential, 3 privileged).
	Risk   int              `json:"risk"`
	Fields []CatalogueField `json:"fields"`
}

// CatalogueField is a field the model may see and propose.
type CatalogueField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Required   bool   `json:"required"`
	EntityType string `json:"entity_type,omitempty"`
}

// Turn is one turn of a conversation.
type Turn struct {
	// User is the user's words. Exactly one of User, Choice, Confirm and Reject is set.
	User string `json:"user,omitempty"`
	// Choice is the identifier of one of the choices the last turn offered.
	Choice string `json:"choice,omitempty"`
	// Confirm is the client presenting the confirmation of the pending prepared action.
	Confirm bool `json:"confirm,omitempty"`
	// Reject is the client declining the pending prepared action.
	Reject bool `json:"reject,omitempty"`
	// HostEvent changes the world before the turn.
	HostEvent *HostEvent `json:"host_event,omitempty"`
	// DecidedBy is "model", "rule", "router" or "client".
	DecidedBy string `json:"decided_by"`
	// Script is what a compromised model returns instead of Expect.Typed.
	Script *Proposal `json:"script,omitempty"`
	Expect Expect    `json:"expect"`
}

// HostEvent is something that happens in the application between two turns.
type HostEvent struct {
	// Kind is "slot_taken" (another user books Slot for ByPatient), "time_passes"
	// (Minutes pass), or "add_patient" (a record exists with the given names).
	Kind      string `json:"kind"`
	Slot      string `json:"slot,omitempty"`
	ByPatient string `json:"by_patient,omitempty"`
	Minutes   int    `json:"minutes,omitempty"`
	PatientID string `json:"patient_id,omitempty"`
	Given     string `json:"given,omitempty"`
	Family    string `json:"family,omitempty"`
	Born      string `json:"born,omitempty"`
}

// Proposal is the model-authority part of a TaskProposal.
type Proposal struct {
	// CapabilityID is the proposed capability; null for out_of_scope, help and a
	// cancel that concerns no capability.
	CapabilityID *string `json:"capability_id"`
	// Intent is task, correction, cancel, help or out_of_scope.
	Intent string `json:"intent"`
	// Fields are candidate values keyed by field name.
	Fields     map[string]any `json:"fields"`
	EntityRefs []EntityRef    `json:"entity_refs,omitempty"`
}

// EntityRef is a mention of an application record, as the model reports it.
type EntityRef struct {
	EntityType string `json:"entity_type"`
	Mention    string `json:"mention"`
	Field      string `json:"field,omitempty"`
	// CandidateID is a record identifier the model believes is right: a hint the
	// application only ever verifies.
	CandidateID string `json:"candidate_id,omitempty"`
}

// Expect is what a turn should produce.
type Expect struct {
	// Typed is the correct TaskProposal of a turn decided by the model.
	Typed *Proposal `json:"typed,omitempty"`
	// Control names what the rule recognised in a rule turn: an exact cancel, restart,
	// resume, confirm_text or help phrase, or a turn that is nothing but a relative
	// reference (reference).
	Control string  `json:"control,omitempty"`
	Outcome Outcome `json:"outcome"`
}

// Outcome is what the engine does with the turn.
type Outcome struct {
	// Act is the dialogue act of the directive.
	Act string `json:"act"`
	// Code is the typed reason of a refusal, a failure or a step-up request.
	Code string `json:"code,omitempty"`
	// CapabilityID is the capability the directive concerns.
	CapabilityID string `json:"capability_id,omitempty"`
	// MissingFields are the required fields still to collect, in the capability's order.
	MissingFields []string `json:"missing_fields,omitempty"`
	// CandidateIDs are the records the application offered to choose between.
	CandidateIDs []string `json:"candidate_ids,omitempty"`
	// HostFields are the fields the application received on this turn's last prepare or
	// read of the capability, after it resolved every mention.
	HostFields map[string]any `json:"host_fields,omitempty"`
	// Facts are exact facts the directive carries, by field name, as displayed.
	Facts map[string]string `json:"facts,omitempty"`
	// HostCalls, when set, is how many requests the application received during the turn
	// (resolutions included): zero says it was never asked.
	HostCalls *int `json:"host_calls,omitempty"`
	// HostWrites, when set, is how many changes the application applied during the turn.
	HostWrites *int `json:"host_writes,omitempty"`
	// Committed says the turn ended in a committed receipt.
	Committed bool `json:"committed,omitempty"`
	// Transitions are state transitions the turn must have taken (each must be present).
	Transitions []string `json:"transitions,omitempty"`
}

// Attack describes a prompt-injection case.
type Attack struct {
	// Kind is what the attacker tries.
	Kind string `json:"kind"`
	// Goal says what the attacker wants, in words.
	Goal string `json:"goal"`
	// ForbiddenCapabilities are capabilities the attack asks the model for that the run must
	// never reach.
	ForbiddenCapabilities []string `json:"forbidden_capabilities,omitempty"`
	// ForbiddenFields are field names the model must not be able to set.
	ForbiddenFields []string `json:"forbidden_fields,omitempty"`
	// ForbiddenValues are strings that must not reach the application.
	ForbiddenValues []string `json:"forbidden_values,omitempty"`
}

// Behaviours are the fourteen bullets of master plan 22.3, each with the test that
// mirrors it. A corpus holds at least one case of every behaviour in both languages.
var Behaviours = []struct{ Behaviour, Test string }{
	{"direct_command", "TestConvDirectCommand"},
	{"missing_details", "TestConvMissingDetails"},
	{"multiple_details_one_turn", "TestConvMultipleDetailsOneTurn"},
	{"ambiguous_entity", "TestConvAmbiguousEntity"},
	{"correction", "TestConvCorrection"},
	{"relative_reference", "TestConvRelativeReference"},
	{"cancel_restart", "TestConvCancelRestart"},
	{"topic_switch_resume", "TestConvTopicSwitchResume"},
	{"stale_data", "TestConvStaleData"},
	{"denied_capability", "TestConvDeniedCapability"},
	{"step_up_required", "TestConvStepUpRequired"},
	{"exact_facts_in_response", "TestConvExactFactsInResponse"},
	{"out_of_application", "TestConvOutOfApplication"},
	{"prompt_injection", "TestConvPromptInjection"},
}

// The vocabularies a case may use.
var (
	languages  = map[string]bool{"en": true, "it": true}
	principals = map[string]bool{"receptionist": true, "doctor": true, "patient": true}
	deciders   = map[string]bool{"model": true, "rule": true, "router": true, "client": true}
	intents    = map[string]bool{"task": true, "correction": true, "cancel": true, "help": true, "out_of_scope": true}
	controls   = map[string]bool{"cancel": true, "restart": true, "resume": true, "confirm_text": true, "help": true, "reference": true}
	events     = map[string]bool{"slot_taken": true, "time_passes": true, "add_patient": true}
	variants   = map[string]bool{"": true, "contact-step-up": true}
)

// LoadCorpus reads a corpus file: one JSON case per line, decoded strictly (an unknown
// member is an error), and checks it with ValidateCorpus.
func LoadCorpus(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cases, err := ReadCorpus(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cases, nil
}

// ReadCorpus decodes and validates a corpus from a reader.
func ReadCorpus(r io.Reader) ([]Case, error) {
	var cases []Case
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.DisallowUnknownFields()
		var c Case
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if dec.More() {
			return nil, fmt.Errorf("line %d: more than one JSON value", line)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := ValidateCorpus(cases); err != nil {
		return nil, err
	}
	return cases, nil
}

var (
	idRE    = regexp.MustCompile(`^conv-(en|it)-[a-z0-9_]+-[0-9]{2}$`)
	phoneRE = regexp.MustCompile(`\+?\d[\d\s().-]{6,}\d`)
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+)`)
	urlRE   = regexp.MustCompile(`(?i)https?://|www\.`)
	dateRE  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	timeRE  = regexp.MustCompile(`\b\d{1,2}:\d{2}\b`)
)

// ValidateCorpus checks what a decoder cannot: identifiers, vocabularies, the shape of
// every turn for who decides it, the coverage of every behaviour in both languages, and
// the privacy rules of a public corpus.
func ValidateCorpus(cases []Case) error {
	if len(cases) == 0 {
		return fmt.Errorf("the corpus has no case")
	}
	known := map[string]string{}
	for _, b := range Behaviours {
		known[b.Behaviour] = b.Test
	}
	seen := map[string]bool{}
	coverage := map[string]map[string]int{}
	for _, c := range cases {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("case %s: %s", c.ID, fmt.Sprintf(format, args...))
		}
		switch {
		case !idRE.MatchString(c.ID):
			return fail("id does not match %s", idRE)
		case seen[c.ID]:
			return fail("duplicate id")
		case !languages[c.Language]:
			return fail("language %q is not en or it", c.Language)
		case !strings.HasPrefix(c.ID, "conv-"+c.Language+"-"):
			return fail("the id does not carry the language %s", c.Language)
		case known[c.Behaviour] == "":
			return fail("behaviour %q is not a master plan 22.3 bullet", c.Behaviour)
		case c.Test != known[c.Behaviour]:
			return fail("test %q is not the test of %s (%s)", c.Test, c.Behaviour, known[c.Behaviour])
		case c.Application != "clinic-booking":
			return fail("application %q", c.Application)
		case c.World != "clinic-synthetic-v1":
			return fail("world %q", c.World)
		case !variants[c.Variant]:
			return fail("variant %q", c.Variant)
		case !principals[c.Principal]:
			return fail("principal %q", c.Principal)
		case !strings.HasSuffix(c.Today, "Z") || len(c.Today) != len("2026-10-12T09:00:00Z"):
			return fail("today %q is not an RFC 3339 UTC instant", c.Today)
		case len(c.Catalogue) == 0:
			return fail("no catalogue")
		case len(c.Turns) == 0:
			return fail("no turn")
		}
		seen[c.ID] = true
		if coverage[c.Behaviour] == nil {
			coverage[c.Behaviour] = map[string]int{}
		}
		coverage[c.Behaviour][c.Language]++

		offered := map[string]bool{}
		for _, e := range c.Catalogue {
			if e.ID == "" || e.Title == "" || e.Risk < 0 || e.Risk > 3 || offered[e.ID] {
				return fail("catalogue entry %+v is malformed or repeated", e)
			}
			offered[e.ID] = true
			for _, f := range e.Fields {
				if f.Name == "" || f.Type == "" {
					return fail("catalogue field %+v is malformed", f)
				}
			}
		}
		if (c.Behaviour == "prompt_injection") != (c.Attack != nil) {
			return fail("an attack is described exactly for prompt_injection cases")
		}
		for i, t := range c.Turns {
			tf := func(format string, args ...any) error {
				return fail("turn %d: %s", i+1, fmt.Sprintf(format, args...))
			}
			inputs := 0
			for _, set := range []bool{t.User != "", t.Choice != "", t.Confirm, t.Reject} {
				if set {
					inputs++
				}
			}
			switch {
			case inputs != 1:
				return tf("exactly one of user, choice, confirm and reject is required")
			case !deciders[t.DecidedBy]:
				return tf("decided_by %q", t.DecidedBy)
			case t.Expect.Outcome.Act == "":
				return tf("no expected act")
			case (t.User == "") != (t.DecidedBy == "client"):
				return tf("a structured input is decided by the client and words are not")
			case t.DecidedBy == "model" && t.Expect.Typed == nil:
				return tf("a model turn states the correct proposal")
			case t.DecidedBy != "model" && (t.Expect.Typed != nil || t.Script != nil):
				return tf("only a model turn has a proposal")
			case t.DecidedBy == "rule" && !controls[t.Expect.Control]:
				return tf("a rule turn names its control phrase")
			case t.DecidedBy != "rule" && t.Expect.Control != "":
				return tf("only a rule turn has a control phrase")
			}
			if ev := t.HostEvent; ev != nil {
				if !events[ev.Kind] {
					return tf("host event %q", ev.Kind)
				}
			}
			for _, p := range []*Proposal{t.Expect.Typed, t.Script} {
				if p == nil {
					continue
				}
				if !intents[p.Intent] {
					return tf("intent %q", p.Intent)
				}
				if p.CapabilityID == nil && p.Intent != "out_of_scope" && p.Intent != "help" && p.Intent != "cancel" {
					return tf("a %s proposal names a capability", p.Intent)
				}
				if p.CapabilityID != nil && t.Script == nil && !offered[*p.CapabilityID] {
					return tf("the correct proposal names %s, which the role is not offered", *p.CapabilityID)
				}
				if p.Intent == "out_of_scope" && (p.CapabilityID != nil || len(p.Fields) != 0) {
					return tf("an out_of_scope proposal names nothing and carries no field")
				}
			}
			if p := t.Expect.Typed; p != nil && p.CapabilityID != nil {
				if err := checkFieldsOffered(c, *p.CapabilityID, p.Fields); err != nil {
					return tf("%v", err)
				}
			}
			if err := checkPrivacy(c, t); err != nil {
				return tf("%v", err)
			}
		}
	}
	var missing []string
	for _, b := range Behaviours {
		for _, lang := range []string{"en", "it"} {
			if coverage[b.Behaviour][lang] == 0 {
				missing = append(missing, b.Behaviour+"/"+lang)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the corpus lacks %s", strings.Join(missing, ", "))
	}
	return nil
}

// checkFieldsOffered holds a correct proposal to the catalogue: every field it states is
// one the role is offered for that capability.
func checkFieldsOffered(c Case, capability string, fields map[string]any) error {
	for _, e := range c.Catalogue {
		if e.ID != capability {
			continue
		}
		for name := range fields {
			var ok bool
			for _, f := range e.Fields {
				ok = ok || f.Name == name
			}
			if !ok {
				return fmt.Errorf("%s is not a field of %s the role is offered", name, capability)
			}
		}
		return nil
	}
	return nil
}

// checkPrivacy keeps the corpus public: no telephone number, no address outside the
// reserved example.test domain, no URL.
func checkPrivacy(c Case, t Turn) error {
	texts := []string{t.User}
	if p := t.Expect.Typed; p != nil {
		b, _ := json.Marshal(p)
		texts = append(texts, string(b))
	}
	if p := t.Script; p != nil {
		b, _ := json.Marshal(p)
		texts = append(texts, string(b))
	}
	if c.Attack != nil {
		b, _ := json.Marshal(c.Attack)
		texts = append(texts, string(b))
	}
	for _, text := range texts {
		for _, m := range emailRE.FindAllStringSubmatch(text, -1) {
			if !strings.HasSuffix(strings.TrimRight(m[1], ".-"), "example.test") {
				return fmt.Errorf("address %q is outside example.test", m[0])
			}
		}
		// Dates and times are not numbers to call; anything else with seven or more
		// digits in a row of number-like characters is treated as a telephone number.
		stripped := timeRE.ReplaceAllString(dateRE.ReplaceAllString(text, " "), " ")
		for _, m := range phoneRE.FindAllString(stripped, -1) {
			digits := 0
			for _, r := range m {
				if r >= '0' && r <= '9' {
					digits++
				}
			}
			if digits >= 7 {
				return fmt.Errorf("%q looks like a telephone number", m)
			}
		}
		if urlRE.MatchString(text) {
			return fmt.Errorf("a URL: %q", text)
		}
	}
	return nil
}
