package decision

import (
	"context"
	"strings"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// stage is the dialogue.DecisionStage of a Layer. It runs after the deterministic rules and
// the policy-exposed capability shortlist and before context assembly, and it can only
// narrow the capabilities the controller is offered to one of them: the engine validates the
// answer against the shortlist again, the proposal is still the controller's, and every
// later step (validation, entity resolution, policy, prepare, confirm, commit, response
// verification) is the normal pipeline's.
type stage struct{ l *Layer }

var _ dialogue.DecisionStage = stage{}

// Decide implements dialogue.DecisionStage.
//
// The order is fixed, every step fails closed to the unchanged controller path with a typed
// reason, and the layer being off is checked first so that "off" is exactly "no stage":
//
//  1. layer disabled                 -> disabled (no request, no provider call, no audit)
//  2. no candidates                  -> candidate_set_rejected
//  3. an active goal                 -> ood_unsupported_shape: a slot answer or a topic switch
//     is for the controller, and narrowing could remove the goal's own capability from its prompt
//  4. exact id or title              -> selected, no scorer (still re-resolved by the resolver)
//  5. fewer than two candidates, or an utterance past the bound -> ood_unsupported_shape
//  6. the scorer: guard, provider, gates, re-resolution (internal/decision)
func (s stage) Decide(ctx context.Context, req dialogue.DecisionRequest) dialogue.DecisionResult {
	l := s.l
	l.st.with(func(t *Snapshot) { t.Decisions++ })
	fallback := func(r dialogue.FallbackReason) dialogue.DecisionResult {
		l.st.fallback(string(r), "")
		return dialogue.DecisionResult{Fallback: r}
	}
	if !l.enabled() {
		return fallback(dialogue.FallbackDisabled)
	}
	if len(req.Candidates) == 0 {
		return fallback(dialogue.FallbackCandidateSetReject)
	}
	if req.Goal != nil {
		return fallback(dialogue.FallbackOODUnsupportedShape)
	}
	pkg, err := l.d.Packages.Package(ctx, req.Scope.ApplicationID)
	if err != nil || pkg == nil {
		return fallback(dialogue.FallbackUnavailable)
	}
	ctx = withExposureMemo(ctx)

	named := make([]Named, len(req.Candidates))
	cands := make([]gen.CandidateSetCandidate, len(req.Candidates))
	hash := actions.OpaqueVersion(pkg.ContentHash())
	for i, c := range req.Candidates {
		label := c.CapabilityID
		named[i] = Named{ID: c.CapabilityID, Names: []string{c.CapabilityID}}
		if spec, ok := pkg.Spec(c.CapabilityID); ok && spec.Title != "" {
			label = string(spec.Title)
			named[i].Names = append(named[i].Names, label)
		}
		v := hash
		cands[i] = gen.CandidateSetCandidate{CandidateID: common.Identifier(c.CapabilityID), Label: clip(label, 128), DataVersion: &v}
	}

	if id, ok := ExactCapability(req.Utterance, named); ok {
		if _, err := l.res.Resolve(ctx, req.Scope, id); err != nil {
			return fallback(dialogue.FallbackPolicyConflict)
		}
		l.st.with(func(t *Snapshot) { t.ExactSelected++ })
		return dialogue.DecisionResult{Selected: &dialogue.DecisionSelection{CapabilityID: id, Confidence: 1}}
	}

	text := strings.TrimSpace(req.Utterance)
	if len(cands) < 2 || text == "" || len(text) > l.cfg.MaxUtteranceBytes {
		return fallback(dialogue.FallbackOODUnsupportedShape)
	}
	budget := l.budget(ctx)
	if budget <= 0 {
		return fallback(dialogue.FallbackTimeout)
	}
	greq := l.request(req.Scope, gen.DecisionTypeCapabilityRouting, ContractRouting, req.Language,
		map[string]gen.BoundedScalar{"user_text": text, "language": req.Language}, nil, cands, budget)
	sel, reason := l.rank(ctx, req.Scope, greq)
	if reason != dialogue.FallbackNone {
		return dialogue.DecisionResult{Fallback: reason}
	}
	return dialogue.DecisionResult{Selected: &dialogue.DecisionSelection{CapabilityID: sel.id, Confidence: sel.confidence}}
}
