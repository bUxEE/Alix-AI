package loop

import (
	"bytes"
	"encoding/json"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

const maxStepBytes = 64 << 10

// ParseStep validates and decodes a model step proposal, then attaches traceID.
func ParseStep(raw []byte, a AbilityFlags, traceID string) (an.StepProposal, error) {
	var none an.StepProposal
	if len(raw) == 0 || len(raw) > maxStepBytes {
		return none, contracts.New(contracts.ErrSchemaInvalid, "analyst step output is empty or too large").
			WithDetails("output_len", len(raw))
	}
	// Ability check before schema: a strong model that emits a disabled kind
	// (grammar should have omitted it) gets ability_disabled, not only schema_invalid.
	var peek struct {
		Kind an.StepProposalKind `json:"kind"`
	}
	_ = json.Unmarshal(raw, &peek)
	if peek.Kind != "" {
		if err := RefuseDisabled(a, peek.Kind); err != nil {
			return none, err
		}
	}
	if err := validateStepJSON(raw, a); err != nil {
		return none, contracts.New(contracts.ErrSchemaInvalid, "analyst step output is not a valid step proposal")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var sp an.StepProposal
	if err := dec.Decode(&sp); err != nil {
		return none, contracts.New(contracts.ErrSchemaInvalid, "analyst step output does not decode")
	}
	if err := RefuseDisabled(a, sp.Kind); err != nil {
		return none, err
	}
	if err := checkPayload(sp); err != nil {
		return none, err
	}
	if sp.Kind == an.StepProposalKindCompute && sp.Compute != nil && ForecastOp(sp.Compute.Op) && !a.Forecasts {
		return none, contracts.New(contracts.ErrAbilityDisabled, "forecast ops require the forecasts ability").
			WithDetails("ability", string(an.AbilityIDForecasts))
	}
	sp.TraceID = common.TraceID(traceID)
	return sp, nil
}

func checkPayload(sp an.StepProposal) error {
	need := func(ok bool, name string) error {
		if ok {
			return nil
		}
		return contracts.New(contracts.ErrSchemaInvalid, "analyst step missing payload for kind").
			WithDetails("kind", string(sp.Kind)).WithDetails("payload", name)
	}
	switch sp.Kind {
	case an.StepProposalKindRead:
		return need(sp.Read != nil, "read")
	case an.StepProposalKindCompute:
		return need(sp.Compute != nil, "compute")
	case an.StepProposalKindSearch:
		return need(sp.Search != nil, "search")
	case an.StepProposalKindFetch:
		return need(sp.Fetch != nil, "fetch")
	case an.StepProposalKindAnswer:
		return need(sp.Answer != nil, "answer")
	case an.StepProposalKindClarify:
		return need(sp.Clarify != nil, "clarify")
	case an.StepProposalKindAct:
		return need(sp.Act != nil, "act")
	}
	return contracts.New(contracts.ErrSchemaInvalid, "unknown analyst step kind")
}
