package dialogue

import (
	"context"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// AnalystDeps is the optional analyst step-loop wiring (ADR 0047). Nil Runner
// keeps Local / analyst-off behaviour byte-identical to 1.4.
type AnalystDeps struct {
	Runner *loop.Runner
	// AnalystCapable reports whether the resolved controller is analyst_capable.
	// Nil treats every controller as not capable.
	AnalystCapable func(ctx context.Context, scope contracts.Scope) bool
}

// ClientCapabilitiesForTurn is the documented accessor for the emit layer: the
// announcement stored for turnID (from TurnInput.ClientCapabilities).
func ClientCapabilitiesForTurn(turnID string) (an.ClientCapabilities, bool) {
	return loop.ClientCapabilitiesFor(turnID)
}

// deploymentAbilityResolver implements loop.AbilityResolver from dialogue.analyst
// config alone (application/tenant narrowing belongs to the settings sibling).
type deploymentAbilityResolver struct {
	cfg AnalystConfig
}

func (d deploymentAbilityResolver) Resolve(_ context.Context, _ contracts.Scope, capable bool) (loop.Effective, error) {
	on := func(flag bool) bool { return d.cfg.Enabled && flag && capable }
	flags := loop.AbilityFlags{
		Loop: on(d.cfg.Abilities.Loop), Compute: on(d.cfg.Abilities.Compute),
		Forecasts: on(d.cfg.Abilities.Forecasts), Presentation: on(d.cfg.Abilities.Presentation),
		Drafts: on(d.cfg.Abilities.Drafts), Knowledge: on(d.cfg.Abilities.Knowledge),
		WebSearch: on(d.cfg.Abilities.WebSearch), WebFetch: on(d.cfg.Abilities.WebFetch),
	}
	wanted := d.cfg.Enabled && d.cfg.Abilities.Loop
	eff := loop.Effective{
		Enabled: flags.Loop, AnalystCapable: capable, Abilities: flags,
		Wanted: wanted && !capable,
	}
	if eff.Wanted {
		eff.WantedReason = contracts.ErrModelNotAnalystCapable
	}
	return eff, nil
}

// coordinatorReader adapts actions.Coordinator.Read to loop.CapabilityReader.
type coordinatorReader struct {
	actions *actions.Coordinator
	session func() contracts.Scope
}

func (r coordinatorReader) Read(ctx context.Context, scope contracts.Scope, req loop.ReadRequest) (loop.ReadResult, error) {
	ro, err := r.actions.Read(ctx, scope, actions.ReadInput{
		Session: r.session(), CapabilityID: req.CapabilityID, Fields: req.Arguments,
	})
	if err != nil {
		return loop.ReadResult{}, err
	}
	fields := make([]an.DatasetDatasetField, 0, len(ro.Result.Values))
	row := an.DatasetRow{}
	seen := map[string]bool{}
	for _, f := range ro.Result.Values {
		name := string(f.Field)
		if !seen[name] {
			seen[name] = true
			fields = append(fields, an.DatasetDatasetField{
				Name: common.FieldName(name), Type: an.FieldTypeString, DataClass: an.DataClassInternal,
			})
		}
		row[name] = factText(f)
	}
	capID := common.CapabilityID(req.CapabilityID)
	return loop.ReadResult{Dataset: an.Dataset{
		Fields: fields, Rows: []an.DatasetRow{row}, RecordCount: 1,
		Provenance: an.DatasetDatasetProvenance{
			Kind: an.DatasetDatasetProvenanceKindRead, CapabilityID: &capID, Arguments: req.Arguments,
		},
	}}, nil
}

// tryAnalyst runs the analyst loop when a Runner is configured. handled=false
// means keep the single-capability path (analyst off or model not capable).
func (t *turn) tryAnalyst(ctx context.Context) (outcome, bool, error) {
	r := t.e.analyst
	if r == nil || t.userText == "" {
		return outcome{}, false, nil
	}
	capable := false
	if t.e.analystCapable != nil {
		capable = t.e.analystCapable(ctx, t.scope)
	}
	in := loop.Input{
		TurnID: t.id, Utterance: t.userText, AnalystCapable: capable,
		StreamVerified: t.e.cfg.StreamVerified,
	}
	if t.in.HasClientCapabilities {
		in.ClientCaps = t.in.ClientCapabilities
		in.HasClientCaps = true
		loop.RememberClientCapabilities(t.id, t.in.ClientCapabilities)
	}
	res, err := r.Run(ctx, t.scope, in)
	if err != nil {
		return outcome{}, false, err
	}
	if res.Kind == loop.OutcomeFallback {
		if res.Code != "" {
			t.trace.AnalystReason = string(res.Code)
		}
		return outcome{}, false, nil
	}
	t.stage(StageAnalyst)
	t.trace.AnalystReason = string(res.Kind)
	switch res.Kind {
	case loop.OutcomeAnswer:
		d := t.newDirective(conversation.DialogueDirectiveActPresentResult, "")
		return outcome{dir: d, analystReply: res.Text}, true, nil
	case loop.OutcomeClarify:
		d := t.newDirective(conversation.DialogueDirectiveActAskMissing, "")
		return outcome{dir: d}, true, nil
	case loop.OutcomeAct:
		return t.analystActHandoff(ctx, res.Act)
	case loop.OutcomeRefuse:
		code := res.Code
		if code == "" {
			code = contracts.ErrAnalystCapExceeded
		}
		return t.failure(code, ""), true, nil
	}
	return outcome{}, false, nil
}

func (t *turn) analystActHandoff(ctx context.Context, act *an.StepProposalActStep) (outcome, bool, error) {
	if act == nil {
		return t.failure(contracts.ErrSchemaInvalid, ""), true, nil
	}
	// Hand off to prepare → confirm → commit via the normal task path. Nothing
	// was written inside the loop.
	t.stage(StageShortlist)
	route, err := t.e.router.Route(ctx, capabilities.Request{
		Scope: t.scope, Binding: policy.BindingOf(t.sess.Scope),
		Utterance: t.userText, Language: t.in.Language, Goal: t.sess.State.Goal.Active(),
	})
	if err != nil {
		out, rerr := t.routeError(err)
		return out, true, rerr
	}
	t.route = route
	t.trace.Route = route.Outcome
	t.trace.Shortlist = route.IDs()
	cands := capShortlist(t.decide(ctx, route.Candidates), t.e.cfg.Profile.MaxShortlist)
	tp := contracts.TaskProposal{
		CapabilityID: &act.CapabilityID,
		Fields:       map[string]conversation.TaskProposalCandidateValue{},
		EntityRefs:   []conversation.TaskProposalEntityRef{},
		Intent:       conversation.TaskProposalIntentTask,
		Uncertainty:  conversation.TaskProposalUncertainty{Overall: 1},
		TraceID:      common.TraceID(t.e.ids.New("trc")),
	}
	for k, v := range act.Fields {
		tp.Fields[k] = v
	}
	out, aerr := t.apply(ctx, tp, cands)
	return out, true, aerr
}
