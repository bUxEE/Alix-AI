package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// CorpusID names the corpus format the tool reads.
const CorpusID = "decision-v1"

// The decision types a fixture may carry (a subset of the decision schema's types).
const (
	TypeRouting = "capability_routing"
	TypeAct     = "dialogue_act"
	TypeItem    = "item_selection"
)

// The fixture categories. Every category is covered in both languages.
const (
	CatExact        = "exact_command"
	CatParaphrase   = "paraphrase"
	CatConfirmation = "confirmation"
	CatCancellation = "cancellation"
	CatCorrection   = "correction"
	CatAmbiguous    = "ambiguous_entity"
	CatRAG          = "rag_operation"
	CatUnauthorised = "unauthorised"
	CatInjection    = "prompt_injection"
	CatDocument     = "untrusted_document"
	CatOOD          = "out_of_distribution"
	CatFailure      = "scorer_failure"
)

// Categories lists every category in the order results report them.
var Categories = []string{
	CatExact, CatParaphrase, CatConfirmation, CatCancellation, CatCorrection, CatAmbiguous,
	CatRAG, CatUnauthorised, CatInjection, CatDocument, CatOOD, CatFailure,
}

// Languages are the declared languages.
var Languages = []string{"en", "it"}

// The outcome kinds. A decision ends in exactly one: a selected candidate, a request for
// clarification (nothing is executed) or a refusal.
const (
	OutSelect  = "select"
	OutClarify = "clarify"
	OutRefuse  = "refuse"
)

// The failures a fixture may inject into the scorer (fake scorer only).
const (
	FailTimeout     = "timeout"
	FailMalformed   = "malformed"
	FailUnavailable = "unavailable"
)

// Candidate is one candidate of the set a decision chooses among.
type Candidate struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Unauthorised marks a candidate the principal may not use. It is test data: at run
	// time the deterministic resolver refuses it, the way the real adapter would.
	Unauthorised bool `json:"unauthorised,omitempty"`
}

// ActState is the dialogue state of an act decision (the bounded state a scorer sees).
type ActState struct {
	HasGoal  bool `json:"has_goal"`
	Prepared bool `json:"prepared"`
	Pending  bool `json:"pending"`
	Choices  bool `json:"choices"`
}

// Script is what the FAKE scorer is told to answer for a case. It is read by the fake
// scorer only; a real scorer never sees it.
type Script struct {
	Pick       string  `json:"pick"`
	Confidence float64 `json:"confidence"`
}

// Expect is the correct outcome.
type Expect struct {
	Outcome     string `json:"outcome"`
	CandidateID string `json:"candidate_id,omitempty"`
}

// Case is one fixture line of decision-v1.jsonl.
type Case struct {
	ID          string `json:"id"`
	Pair        string `json:"pair"`
	Language    string `json:"language"`
	Category    string `json:"category"`
	Application string `json:"application"`
	// Contract names the signed conversation contract the fixture was built from
	// (<application>/<capability id>). It is provenance, never runtime policy.
	Contract     string      `json:"contract"`
	DecisionType string      `json:"decision_type"`
	Utterance    string      `json:"utterance"`
	State        *ActState   `json:"state,omitempty"`
	Candidates   []Candidate `json:"candidates"`
	// Document is untrusted retrieved text that travelled with the turn. It must never
	// reach the scorer: the harness never puts it in a request.
	Document string `json:"document,omitempty"`
	// AttackTarget is the candidate an injected instruction tries to trigger.
	AttackTarget  string  `json:"attack_target,omitempty"`
	NeedsCitation bool    `json:"needs_citation,omitempty"`
	Failure       string  `json:"failure,omitempty"`
	Script        *Script `json:"script,omitempty"`
	Expect        Expect  `json:"expect"`
}

var (
	validTypes = map[string]bool{TypeRouting: true, TypeAct: true, TypeItem: true}
	validCats  = func() map[string]bool {
		m := map[string]bool{}
		for _, c := range Categories {
			m[c] = true
		}
		return m
	}()
	validFailures = map[string]bool{"": true, FailTimeout: true, FailMalformed: true, FailUnavailable: true}
)

// Validate checks one fixture.
func (c Case) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("case %q: %s", c.ID, fmt.Sprintf(format, a...))
	}
	switch {
	case c.ID == "" || c.Pair == "" || strings.ContainsAny(c.ID, " \t"):
		return bad("id and pair are required and the id has no whitespace")
	case c.Language != "en" && c.Language != "it":
		return bad("language must be en or it")
	case !validCats[c.Category]:
		return bad("unknown category %q", c.Category)
	case c.Application == "" || !strings.Contains(c.Contract, "/"):
		return bad("application and contract (<application>/<id>) are required")
	case !validTypes[c.DecisionType]:
		return bad("unknown decision_type %q", c.DecisionType)
	case strings.TrimSpace(c.Utterance) == "" || len(c.Utterance) > 256:
		return bad("utterance must be 1 to 256 bytes")
	case len(c.Candidates) == 0 || len(c.Candidates) > 16:
		return bad("1 to 16 candidates are required")
	case !validFailures[c.Failure]:
		return bad("unknown failure %q", c.Failure)
	case (c.DecisionType == TypeAct) != (c.State != nil):
		return bad("state is required for dialogue_act and only there")
	}
	ids := map[string]Candidate{}
	for _, cd := range c.Candidates {
		if cd.ID == "" || cd.Label == "" || len(cd.Label) > 256 {
			return bad("every candidate needs an id and a label of at most 256 bytes")
		}
		if _, dup := ids[cd.ID]; dup {
			return bad("duplicate candidate %q", cd.ID)
		}
		ids[cd.ID] = cd
	}
	switch c.Expect.Outcome {
	case OutSelect:
		cd, ok := ids[c.Expect.CandidateID]
		if !ok || cd.Unauthorised {
			return bad("expected selection %q must be an authorised candidate", c.Expect.CandidateID)
		}
	case OutClarify, OutRefuse:
		if c.Expect.CandidateID != "" {
			return bad("only a select outcome names a candidate")
		}
	default:
		return bad("unknown expected outcome %q", c.Expect.Outcome)
	}
	if c.Script != nil {
		if _, ok := ids[c.Script.Pick]; !ok || c.Script.Confidence < 0 || c.Script.Confidence > 1 {
			return bad("script must pick a candidate with a confidence in [0, 1]")
		}
	}
	if c.AttackTarget != "" {
		if _, ok := ids[c.AttackTarget]; !ok {
			return bad("attack_target must be a candidate")
		}
	}
	switch c.Category {
	case CatInjection, CatDocument:
		if c.AttackTarget == "" {
			return bad("%s needs an attack_target", c.Category)
		}
	case CatUnauthorised:
		if !c.hasUnauthorised() || c.Expect.Outcome != OutRefuse {
			return bad("unauthorised cases carry an unauthorised candidate and expect refuse")
		}
	}
	if (c.Category == CatDocument) != (strings.TrimSpace(c.Document) != "") {
		return bad("only untrusted_document cases carry a document, and they must")
	}
	if c.Category == CatFailure && c.Failure == "" {
		return bad("scorer_failure needs a failure")
	}
	if c.Category != CatFailure && c.Failure != "" {
		return bad("only scorer_failure cases inject a failure")
	}
	return nil
}

func (c Case) hasUnauthorised() bool {
	for _, cd := range c.Candidates {
		if cd.Unauthorised {
			return true
		}
	}
	return false
}

func (c Case) unauthorisedIDs() map[string]bool {
	m := map[string]bool{}
	for _, cd := range c.Candidates {
		if cd.Unauthorised {
			m[cd.ID] = true
		}
	}
	return m
}

// Corpus is a loaded fixture file.
type Corpus struct {
	Cases  []Case
	SHA256 string
}

// maxCorpusBytes bounds a corpus file.
const maxCorpusBytes = 8 << 20

// LoadCorpus reads and strictly validates a JSONL fixture file: unknown keys are errors,
// ids are unique and a pair holds exactly one case per language.
func LoadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxCorpusBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxCorpusBytes)
	}
	return ParseCorpus(data)
}

// ParseCorpus is LoadCorpus over bytes.
func ParseCorpus(data []byte) (*Corpus, error) {
	sum := sha256.Sum256(data)
	c := &Corpus{SHA256: hex.EncodeToString(sum[:])}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	seen := map[string]bool{}
	pairs := map[string]map[string]bool{}
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var cs Case
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cs); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if err := cs.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if seen[cs.ID] {
			return nil, fmt.Errorf("line %d: case id %q appears twice", line, cs.ID)
		}
		seen[cs.ID] = true
		if pairs[cs.Pair] == nil {
			pairs[cs.Pair] = map[string]bool{}
		}
		if pairs[cs.Pair][cs.Language] {
			return nil, fmt.Errorf("line %d: pair %q has two %s cases", line, cs.Pair, cs.Language)
		}
		pairs[cs.Pair][cs.Language] = true
		c.Cases = append(c.Cases, cs)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(c.Cases) == 0 {
		return nil, fmt.Errorf("the corpus holds no case")
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(pairs[k]) != 2 {
			return nil, fmt.Errorf("pair %q must hold one en and one it case", k)
		}
	}
	return c, nil
}
