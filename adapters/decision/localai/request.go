package localai

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Question types of the typed API. The adapter decides with choice questions;
// score questions are encoded and limit-checked for callers of the typed API, and
// noul (yes, no, unknown) is not used.
const (
	TypeChoice = "choice"
	TypeScore  = "score"
)

// Question is one typed question: its options (choice) or levels (score) are the
// keys of Criteria, each with a description.
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions,omitempty"`
	Criteria     map[string]string `json:"criteria"`
}

// systemOneRequest is the body of POST /v1/systemone. There is no images member:
// the scorer never receives an image.
type systemOneRequest struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// errShape is a request the documented limits cannot carry. It is a verdict about
// the request, never about the runtime; the provider turns it into
// ood_unsupported_shape without opening a connection.
var errShape = errors.New("localai decisions: the request does not fit the documented limits")

// encodeSystemOne validates a typed request against the documented limits and
// encodes it: a non-blank state, 1 to 64 questions, a choice with 2 to maxOptions
// options, a score with at least 2 levels, a body of at most 64 KiB. maxOptions
// is the effective option cap (at most MaxChoiceOptions).
func encodeSystemOne(model, state string, qs map[string]Question, maxOptions int) ([]byte, error) {
	if strings.TrimSpace(state) == "" || len(qs) < 1 || len(qs) > MaxQuestions {
		return nil, errShape
	}
	if maxOptions < 1 || maxOptions > MaxChoiceOptions {
		maxOptions = MaxChoiceOptions
	}
	for id, q := range qs {
		if id == "" {
			return nil, errShape
		}
		switch q.Type {
		case TypeChoice:
			if len(q.Criteria) < MinChoiceOptions || len(q.Criteria) > maxOptions {
				return nil, errShape
			}
		case TypeScore:
			if len(q.Criteria) < MinScoreLevels {
				return nil, errShape
			}
		default:
			return nil, errShape
		}
	}
	body, err := json.Marshal(systemOneRequest{Model: model, State: state, Questions: qs})
	if err != nil || len(body) > MaxBodyBytes {
		return nil, errShape
	}
	return body, nil
}

// choiceID is the identifier of the one question a decision asks.
const choiceID = "choice"

// instructions are fixed per decision type. They never carry user or application
// text.
var instructions = map[gen.DecisionType]string{
	gen.DecisionTypeCapabilityRouting:   "Choose the option that is the capability the state asks for.",
	gen.DecisionTypeDialogueAct:         "Choose the option that is the dialogue act of the state.",
	gen.DecisionTypeItemSelection:       "Choose the option that is the item the state refers to.",
	gen.DecisionTypeActionSelection:     "Choose the option that is the action the state asks for.",
	gen.DecisionTypeScopeClassification: "Choose the option that is the scope class of the state.",
	gen.DecisionTypePathVerification:    "Choose the option that is the path the state is consistent with.",
}

// decisionQuestions builds the single choice question of a request. Option keys
// are the candidate identifiers; a duplicate identifier or an identifier the API
// cannot carry as a key makes the shape unsupported.
func decisionQuestions(req gen.DecisionRequest) (map[string]Question, error) {
	crit := make(map[string]string, len(req.CandidateSet.Candidates))
	for _, c := range req.CandidateSet.Candidates {
		id := string(c.CandidateID)
		if id == "" {
			return nil, errShape
		}
		if _, dup := crit[id]; dup {
			return nil, errShape
		}
		crit[id] = wire.CandidateText(c)
	}
	return map[string]Question{choiceID: {Type: TypeChoice, Instructions: instructions[req.DecisionType], Criteria: crit}}, nil
}

// systemOneResponse is the part of the answer the adapter reads. Unknown members
// are ignored; every member it uses is checked.
type systemOneResponse struct {
	Model    string `json:"model"`
	Revision string `json:"model_revision"`
	Answers  map[string]struct {
		Type          string             `json:"type"`
		Choice        *string            `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    *float64           `json:"confidence"`
	} `json:"answers"`
}
