package sdktest

import (
	"net/http"
	"slices"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The decision-layer area of the fake: the read-only status. The status is advisory information about the
// optional decision layer and never an authorisation (master plan 8.5). A test sets what the layer reports
// with SetDecisionStatus, or switches it to "no decision service" with SetDecisionUnavailable.

// DecisionStatusCapability is the capability a token needs to read the status, as the core requires it. The
// default claims do not carry it: a test that reads the status says so in Claims.CapabilityScopes.
const DecisionStatusCapability = "decision.status.read"

type decisionState struct {
	set         bool
	status      decision.DecisionStatus
	unavailable bool
}

// DecisionStatusFixture is a status fixture for host tests: the layer in the given mode with the given
// fallback counts (a reason maps to how many results fell back for it). Accepted is the number of accepted
// results. The rejected count is derived, so the fixture is always consistent with itself. Only the modes
// "off", "shadow" and "advisory" are valid.
func DecisionStatusFixture(mode decision.DecisionStatusMode, accepted int64, fallbacks map[decision.FallbackReason]int64) decision.DecisionStatus {
	status := decision.DecisionStatus{
		Mode:           mode,
		ProviderID:     "fake-decision-provider",
		AcceptedCount:  accepted,
		FallbackCounts: []decision.DecisionStatusFallbackCount{},
		Since:          "2026-10-01T00:00:00.000Z",
		GeneratedAt:    "2026-10-01T00:00:00.000Z",
	}
	if mode != decision.DecisionStatusModeOff {
		model, revision := common.Identifier("fake-scorer"), actions.OpaqueVersion("r1")
		status.ModelID, status.ModelRevision = &model, &revision
	}
	reasons := make([]string, 0, len(fallbacks))
	for reason := range fallbacks {
		reasons = append(reasons, string(reason))
	}
	slices.Sort(reasons)
	for _, reason := range reasons {
		count := fallbacks[decision.FallbackReason(reason)]
		status.FallbackCounts = append(status.FallbackCounts, decision.DecisionStatusFallbackCount{Reason: decision.FallbackReason(reason), Count: count})
		status.RejectedCount += count
	}
	return status
}

// SetDecisionStatus sets the status the fake reports. The fake reports it as given: a test that wants an
// inconsistent one (to see the SDK refuse it) can build one by hand.
func (f *Fake) SetDecisionStatus(status decision.DecisionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decision.set = true
	f.decision.status = status
}

// SetDecisionUnavailable makes the status answer `unavailable` (503), as a server without a decision service
// does, or restores it.
func (f *Fake) SetDecisionUnavailable(unavailable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decision.unavailable = unavailable
}

func (c *call) decisionStatus() {
	if c.r.URL.RawQuery != "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "this route takes no query string")
		return
	}
	if !slices.Contains(c.claims.CapabilityScopes, DecisionStatusCapability) {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the token does not carry "+DecisionStatusCapability)
		return
	}
	if c.f.decision.unavailable {
		c.fail(http.StatusServiceUnavailable, contracts.ErrUnavailable, "the server has no decision service")
		return
	}
	status := c.f.decision.status
	if !c.f.decision.set {
		status = DecisionStatusFixture(decision.DecisionStatusModeOff, 0, nil)
	}
	status.GeneratedAt = common.Timestamp(c.f.stamp(c.f.now()))
	c.json(http.StatusOK, status)
}
