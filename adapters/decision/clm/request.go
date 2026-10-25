package clm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// errShape marks a request the service cannot take without changing its meaning
// (truncation) or at all. It is a verdict about the request, never about the
// runtime; ScoreDecision turns it into ood_unsupported_shape before any connection.
var errShape = errors.New("clm: the request does not fit the service's limits")

// EstimateTokens is the adapter's conservative estimate of the encoder tokens of s:
// one token per three ASCII bytes and one per non-ASCII rune, rounded up. It is an
// upper-leaning heuristic, not a tokenizer: the adapter has none, and refusing a
// state that might be truncated is the safe error.
func EstimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+2)/3 + other
}

// rankRequest is the body of POST /v1/rank, exactly the documented members.
type rankRequest struct {
	Context  string   `json:"context"`
	Question string   `json:"question"`
	Answers  []string `json:"answers"`
}

type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions,omitempty"`
	Criteria     map[string]string `json:"criteria"`
}

// systemOneRequest is the body of POST /v1/systemone; the temperature member is left
// to the service's default.
type systemOneRequest struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]question `json:"questions"`
}

const choiceID = "choice"

// questions are fixed per decision type and never carry user or application text.
var questions = map[gen.DecisionType]string{
	gen.DecisionTypeCapabilityRouting:   "Which option is the capability the state asks for?",
	gen.DecisionTypeDialogueAct:         "Which option is the dialogue act of the state?",
	gen.DecisionTypeItemSelection:       "Which option is the item the state refers to?",
	gen.DecisionTypeActionSelection:     "Which option is the action the state asks for?",
	gen.DecisionTypeScopeClassification: "Which option is the scope class of the state?",
	gen.DecisionTypePathVerification:    "Which option is the path the state is consistent with?",
}

// call is one built scoring request.
type call struct {
	path string
	body []byte
	// byAnswer maps the answer text sent to /v1/rank back to its candidate identifier.
	byAnswer map[string]string
	// ids are the candidate identifiers offered as options of /v1/systemone.
	ids map[string]struct{}
}

// build checks req against the limits and encodes it for the configured mode.
func (p *Provider) build(req gen.DecisionRequest) (call, error) {
	cands := req.CandidateSet.Candidates
	if len(cands) < 2 || len(cands) > p.cfg.MaxCandidates {
		return call{}, errShape
	}
	state, err := wire.StateString(req)
	if err != nil {
		return call{}, err
	}
	if EstimateTokens(state) > p.cfg.MaxStateTokens {
		return call{}, errShape
	}
	ids := make(map[string]struct{}, len(cands))
	for _, c := range cands {
		id := string(c.CandidateID)
		if _, dup := ids[id]; dup || id == "" {
			return call{}, errShape
		}
		ids[id] = struct{}{}
	}
	var out call
	var body []byte
	if p.cfg.Mode == ModeSystemOne {
		crit := make(map[string]string, len(cands))
		for _, c := range cands {
			crit[string(c.CandidateID)] = wire.CandidateText(c)
		}
		body, err = json.Marshal(systemOneRequest{Model: p.cfg.Model, State: state,
			Questions: map[string]question{choiceID: {Type: "choice", Instructions: questions[req.DecisionType], Criteria: crit}}})
		out = call{path: pathSystemOne, ids: ids}
	} else {
		texts, by, ok := answerTexts(cands)
		if !ok {
			return call{}, errShape
		}
		body, err = json.Marshal(rankRequest{Context: state, Question: questions[req.DecisionType], Answers: texts})
		out = call{path: pathRank, byAnswer: by}
	}
	if err != nil || len(body) > p.cfg.MaxBodyBytes {
		return call{}, errShape
	}
	out.body = body
	return out, nil
}

// answerTexts builds the answer strings of a rank request. Two candidates with the
// same text could not be told apart in the answer, so only those get their
// identifier appended; if that does not separate them the shape is unsupported.
func answerTexts(cands []gen.CandidateSetCandidate) ([]string, map[string]string, bool) {
	count := map[string]int{}
	for _, c := range cands {
		count[wire.CandidateText(c)]++
	}
	texts := make([]string, 0, len(cands))
	by := make(map[string]string, len(cands))
	for _, c := range cands {
		t := wire.CandidateText(c)
		if count[t] > 1 {
			t += " [" + string(c.CandidateID) + "]"
		}
		if _, dup := by[t]; dup {
			return nil, nil, false
		}
		by[t] = string(c.CandidateID)
		texts = append(texts, t)
	}
	return texts, by, true
}

type rankResponse struct {
	Model  string `json:"model"`
	Ranked []struct {
		Rank      *int     `json:"rank"`
		Candidate *string  `json:"candidate"`
		Prob      *float64 `json:"prob"`
	} `json:"ranked"`
}

type systemOneResponse struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Choice        *string            `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    *float64           `json:"confidence"`
	} `json:"answers"`
}

// ask sends the call and returns the scored candidates, best first.
func (p *Provider) ask(ctx context.Context, c call) ([]wire.Scored, error) {
	resp, err := p.client.Do(ctx, http.MethodPost, c.path, c.body)
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, wire.StatusError("clm", resp)
	}
	if c.path == pathRank {
		return p.readRank(resp.Body, c)
	}
	return p.readSystemOne(resp.Body, c)
}

func malformed(why string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "clm: "+why)
}

func (p *Provider) mismatch(model string) error {
	if model != "" && model != p.cfg.Model {
		return contracts.Wrap(contracts.ErrUnavailable, ErrModelMismatch, "clm: the service answered as another model than the configured one")
	}
	return nil
}

func (p *Provider) readRank(body []byte, c call) ([]wire.Scored, error) {
	var out rankResponse
	if err := wire.Decode("clm", body, &out); err != nil {
		return nil, err
	}
	if err := p.mismatch(out.Model); err != nil {
		return nil, err
	}
	if len(out.Ranked) == 0 {
		return nil, malformed("the ranking is empty")
	}
	type row struct {
		rank int
		s    wire.Scored
	}
	rows := make([]row, 0, len(out.Ranked))
	seen := map[string]struct{}{}
	for i, r := range out.Ranked {
		if r.Candidate == nil || r.Prob == nil || !wire.Unit(*r.Prob) {
			return nil, malformed("a ranked entry is not the documented shape")
		}
		id, ok := c.byAnswer[*r.Candidate]
		if !ok {
			return nil, malformed("the ranking names a candidate that was not offered")
		}
		if _, dup := seen[id]; dup {
			return nil, malformed("the ranking repeats a candidate")
		}
		seen[id] = struct{}{}
		rank := i + 1
		if r.Rank != nil {
			rank = *r.Rank
		}
		rows = append(rows, row{rank, wire.Scored{ID: id, Prob: *r.Prob, Confidence: *r.Prob}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].rank < rows[j].rank })
	ranked := make([]wire.Scored, len(rows))
	for i, r := range rows {
		ranked[i] = r.s
	}
	return ranked, nil
}

func (p *Provider) readSystemOne(body []byte, c call) ([]wire.Scored, error) {
	var out systemOneResponse
	if err := wire.Decode("clm", body, &out); err != nil {
		return nil, err
	}
	if err := p.mismatch(out.Model); err != nil {
		return nil, err
	}
	a, ok := out.Answers[choiceID]
	if !ok || a.Choice == nil {
		return nil, malformed("the answer is not the documented choice answer")
	}
	if _, offered := c.ids[*a.Choice]; !offered {
		return nil, malformed("the answer names a candidate that was not offered")
	}
	for id, pr := range a.Probabilities {
		if _, offered := c.ids[id]; !offered || !wire.Unit(pr) {
			return nil, malformed("the probabilities are not the documented shape")
		}
	}
	if a.Confidence != nil && !wire.Unit(*a.Confidence) {
		return nil, malformed("the confidence is not in [0, 1]")
	}
	pick := wire.Scored{ID: *a.Choice}
	prob, hasProb := a.Probabilities[*a.Choice]
	switch {
	case hasProb && a.Confidence != nil:
		pick.Prob, pick.Confidence = prob, *a.Confidence
	case hasProb:
		pick.Prob, pick.Confidence = prob, prob
	case a.Confidence != nil:
		pick.Prob, pick.Confidence = *a.Confidence, *a.Confidence
	default:
		return nil, malformed("the answer carries neither probabilities nor a confidence")
	}
	ranked := []wire.Scored{pick}
	for id, pr := range a.Probabilities {
		if id != pick.ID {
			ranked = append(ranked, wire.Scored{ID: id, Prob: pr, Confidence: pr})
		}
	}
	return ranked, nil
}
