package loop

import (
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// AllowedKinds returns the step kinds present in the grammar for the effective abilities.
// answer, clarify and act are always available once the loop is entered; read needs loop
// (already true); compute/search/fetch need their abilities.
func AllowedKinds(a AbilityFlags) []an.StepProposalKind {
	out := []an.StepProposalKind{
		an.StepProposalKindAnswer,
		an.StepProposalKindClarify,
		an.StepProposalKindAct,
		an.StepProposalKindRead,
	}
	if a.Compute {
		out = append(out, an.StepProposalKindCompute)
	}
	if a.WebSearch {
		out = append(out, an.StepProposalKindSearch)
	}
	if a.WebFetch {
		out = append(out, an.StepProposalKindFetch)
	}
	return out
}

// KindAllowed reports whether kind is in the grammar for a.
func KindAllowed(a AbilityFlags, kind an.StepProposalKind) bool {
	for _, k := range AllowedKinds(a) {
		if k == kind {
			return true
		}
	}
	return false
}

// AbilityForKind returns the ability a kind requires, if any.
func AbilityForKind(kind an.StepProposalKind) (an.AbilityID, bool) {
	switch kind {
	case an.StepProposalKindCompute:
		return an.AbilityIDCompute, true
	case an.StepProposalKindSearch:
		return an.AbilityIDWebSearch, true
	case an.StepProposalKindFetch:
		return an.AbilityIDWebFetch, true
	}
	return "", false
}

// RefuseDisabled returns ability_disabled when the kind's ability is off.
func RefuseDisabled(a AbilityFlags, kind an.StepProposalKind) error {
	if KindAllowed(a, kind) {
		if kind == an.StepProposalKindCompute && a.Compute {
			return nil
		}
		return nil
	}
	id, ok := AbilityForKind(kind)
	if !ok {
		id = an.AbilityIDLoop
	}
	return contracts.New(contracts.ErrAbilityDisabled, "analyst step kind is disabled for this turn").
		WithDetails("kind", string(kind)).
		WithDetails("ability", string(id))
}

// ForecastOp reports whether the compute op requires the forecasts ability.
func ForecastOp(op an.ComputeOpOp) bool {
	switch op {
	case an.ComputeOpOpForecastLinear, an.ComputeOpOpForecastHolt, an.ComputeOpOpForecastSeasonalNaive:
		return true
	}
	return false
}

// AbilityNamesForPrompt lists enabled ability ids for the step prompt (disabled ones absent).
func AbilityNamesForPrompt(a AbilityFlags) []string {
	var out []string
	add := func(on bool, id string) {
		if on {
			out = append(out, id)
		}
	}
	add(a.Loop, string(an.AbilityIDLoop))
	add(a.Compute, string(an.AbilityIDCompute))
	add(a.Forecasts, string(an.AbilityIDForecasts))
	add(a.Presentation, string(an.AbilityIDPresentation))
	add(a.Drafts, string(an.AbilityIDDrafts))
	add(a.Knowledge, string(an.AbilityIDKnowledge))
	add(a.WebSearch, string(an.AbilityIDWebSearch))
	add(a.WebFetch, string(an.AbilityIDWebFetch))
	return out
}
