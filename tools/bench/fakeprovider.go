package main

import (
	"encoding/json"
	"fmt"

	"github.com/buxee/alix-ai/internal/providers/fake"
)

// newOracle returns a scripted fake provider that answers every case of the
// corpus with exactly the proposal the case expects. It exists to exercise the
// harness end to end without a model (`--provider fake`): the result is flagged
// synthetic and says nothing about any model. The case is found through the
// scope's session id, which the harness sets to the case id; a request for any
// other case fails, like every call the fake has no rule for.
func newOracle(corpus *Corpus, opts ...fake.Option) *fake.Provider {
	byID := make(map[string]Case, len(corpus.Cases))
	for _, c := range corpus.Cases {
		byID[c.ID] = c
	}
	p := fake.New(opts...)
	p.On(fake.Any()).ReplyFunc(func(call fake.Call) (string, error) {
		c, ok := byID[call.Scope.SessionID]
		if !ok {
			return "", fmt.Errorf("oracle: no case %q in the corpus", call.Scope.SessionID)
		}
		return oracleReply(c), nil
	})
	return p
}

// oracleProposal is the model-authority part of a TaskProposal with its keys
// in the contract's order, which is also the order the output grammar demands.
type oracleProposal struct {
	CapabilityID *string        `json:"capability_id"`
	Fields       map[string]any `json:"fields"`
	EntityRefs   []any          `json:"entity_refs"`
	Intent       string         `json:"intent"`
	Uncertainty  struct {
		Overall float64 `json:"overall"`
	} `json:"uncertainty"`
}

// oracleReply is the proposal a perfect model would write for the case.
func oracleReply(c Case) string {
	p := oracleProposal{CapabilityID: c.Expected.CapabilityID, Fields: c.Expected.Fields, EntityRefs: []any{}, Intent: c.Expected.Intent}
	if p.Fields == nil {
		p.Fields = map[string]any{}
	}
	p.Uncertainty.Overall = 1
	b, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("bench: cannot marshal the oracle reply: %v", err)) // only decoded corpus values go in
	}
	return string(b)
}
