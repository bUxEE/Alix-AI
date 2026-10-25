package sdkgo

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// DecisionClient is the decision-layer area of the public API (master plan 8.5 and 14.2), read-only.
//
// The decision layer is optional, bounded, non-generative and non-authoritative: it scores a short list of
// candidates that were already authorised and never authorises, validates, prepares, confirms or commits
// anything. Nothing this client returns is therefore an authorisation: the status is advisory, operational
// information about the layer (which provider and model are configured, in what mode, how often it fell back
// and why) and no counter in it may be read as permission to do anything. Authority stays with the
// deterministic controller and the application's own checks.
//
// Get one with Client.Decision; it shares the client's options, transport and token provider. The operator-only
// configuration routes (GET and PUT /v1/decision/config) are not part of the SDK surface.
type DecisionClient struct{ c *Client }

// Decision returns the client of the decision-layer area.
func (c *Client) Decision() *DecisionClient { return &DecisionClient{c: c} }

// DecisionStatusCapability is the exact capability the context token must carry to read the status. Without
// it the call is `permission_denied`.
const DecisionStatusCapability = "decision.status.read"

// Status returns the decision-layer status of the caller's own application and tenant: the configured provider
// and model, the mode (off, shadow or advisory), how many results were accepted and rejected since the counters
// started, and how often the layer fell back, by reason.
//
// The result is advisory aggregate state. It is never an authorisation, a score or a commit result, and it
// carries no threshold, gate, candidate, key or tenant datum. A deployment without a decision service answers
// `unavailable`: that is the absence of the layer, never an empty status, and every flow runs through the
// controller either way. The caller needs the capability DecisionStatusCapability.
//
// The body is refused unless it is a complete status that is consistent with itself (every counter
// non-negative, one entry per fallback reason, RejectedCount the sum of the entries).
func (d *DecisionClient) Status(ctx context.Context, options ...CallOption) (*decision.DecisionStatus, error) {
	req := request{op: opGetDecisionStatus, accept: "application/json"}
	return doJSON(ctx, d.c.t, req, applyOptions(options).deadline(d.c.timeout), checkDecisionStatus)
}

func checkDecisionStatus(s *decision.DecisionStatus) error {
	if !s.Mode.Valid() || s.ProviderID == "" || s.Since == "" || s.GeneratedAt == "" {
		return fmt.Errorf("decision status: mode, provider_id, since and generated_at are required")
	}
	if s.AcceptedCount < 0 || s.RejectedCount < 0 {
		return fmt.Errorf("decision status: a count is negative")
	}
	var total int64
	seen := map[decision.FallbackReason]bool{}
	for _, f := range s.FallbackCounts {
		if !f.Reason.Valid() || f.Count < 0 {
			return fmt.Errorf("decision status: a fallback entry has an unknown reason or a negative count")
		}
		if seen[f.Reason] {
			return fmt.Errorf("decision status: fallback reason %q is listed twice", f.Reason)
		}
		seen[f.Reason] = true
		total += f.Count
	}
	if total != s.RejectedCount {
		return fmt.Errorf("decision status: rejected_count is not the sum of fallback_counts")
	}
	return nil
}
