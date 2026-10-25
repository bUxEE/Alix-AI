package dialogue

import (
	"context"

	"github.com/buxee/alix-ai/internal/capabilities"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/policy"
)

// visibleFromPolicy builds the dctx.Visible function of the turn: what of the session state
// (the active goal, the goals set aside, the completed-goal capsules) the model may be shown.
// It is derived from the policy gateway's decisions of this turn and from nothing else:
//
//   - Visible(capability, "") is the decision's Allowed: a capability the caller may not use
//     is not named, in the goal, a set-aside goal, a capsule or the digest of older capsules;
//   - Visible(capability, field) is the decision's FieldVisible: a field the policy
//     projection withholds from the model (a sensitive field) is not shown, whatever value a
//     slot or a capsule fact still holds;
//   - a capability the router did not shortlist this turn but that the state mentions gets a
//     fresh exposure decision of its own (PhaseExpose, the same decision the router would
//     have made). The state keeps outliving the shortlist: a capsule from ten turns ago is
//     about a capability this message has nothing to do with, and it is shown only if the
//     gateway still allows it;
//   - everything else, a capability the package does not declare included, is hidden. The
//     answer is fail-closed and fixed for the turn: it is computed once, here, so the builder
//     and the replay selection cannot disagree about what is visible.
//
// The task fields of the goal's capability are the ones it waits for (the field the user is
// asked about may be shown even if the projection would otherwise withhold it); every other
// capability gets none.
func (t *turn) visibleFromPolicy(ctx context.Context) dctx.Visible {
	decisions := map[string]policy.Decision{}
	for _, c := range t.route.Candidates {
		decisions[c.CapabilityID] = c.Decision
	}
	st := &t.sess.State
	var ids []string
	if g := st.Goal; g != nil {
		ids = append(ids, g.CapabilityID)
	}
	for _, g := range st.Suspended {
		if g != nil {
			ids = append(ids, g.CapabilityID)
		}
	}
	for _, c := range st.Capsules {
		ids = append(ids, c.CapabilityID)
	}
	for _, id := range ids {
		if _, known := decisions[id]; known || id == "" {
			continue
		}
		if _, declared := t.pkg.Capability(id); !declared {
			continue // not a capability of this application: never visible, nothing to decide
		}
		var task []string
		if g := st.Goal; g != nil && g.CapabilityID == id {
			task = pendingFields(g)
		}
		decisions[id] = t.e.policy.Decide(ctx, policy.Request{
			Scope: t.scope, Package: t.pkg, CapabilityID: id, Binding: policy.BindingOf(t.sess.Scope),
			Phase: policy.PhaseExpose, TaskFields: task,
		})
	}
	return func(capabilityID, field string) bool {
		d, ok := decisions[capabilityID]
		if !ok {
			return false
		}
		if field == "" {
			return d.Allowed
		}
		return d.FieldVisible(field)
	}
}

// capShortlist keeps the best candidates the profile lets a prompt describe. The router the
// engine builds already returns no more; this is for a router handed in through Deps, and it
// keeps what the prompt lists and what a proposal may name the same set.
func capShortlist(cands []capabilities.Candidate, max int) []capabilities.Candidate {
	if max > 0 && len(cands) > max {
		return cands[:max]
	}
	return cands
}
