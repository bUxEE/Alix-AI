package decision

import (
	"context"
	"errors"
	"math"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// gate verifies a provider's result against the request and the operator's policy and
// returns the fallback reason, or "" when the result is acceptable. The provider's
// words are never trusted: identity echoes, selections and scores are all checked.
//
// The order is fixed and documented because a result can fail several gates at once and
// the first one names the reason: structure (malformed_output), tenant echo
// (tenant_mismatch), model identity and contract version (model_mismatch), selection
// structure (malformed_output), out-of-distribution (ood_unsupported_shape),
// calibration (calibration_failed), confidence (low_confidence).
func (e *Engine) gate(m gen.ScorerManifest, req gen.DecisionRequest, ids map[string]struct{}, maxSel int, res gen.DecisionScoreResult) gen.FallbackReason {
	if !res.Disposition.Valid() {
		return gen.FallbackReasonMalformedOutput
	}
	if res.Disposition == gen.DecisionScoreResultDispositionFallback {
		// The provider itself declined. Its reason must be a valid one and it must not
		// have selected anything.
		if res.FallbackReason == nil || !res.FallbackReason.Valid() || len(res.Selections) != 0 {
			return gen.FallbackReasonMalformedOutput
		}
		return *res.FallbackReason
	}
	if res.FallbackReason != nil {
		return gen.FallbackReasonMalformedOutput
	}
	if res.SchemaVersion != 1 || res.SchemaVersion != req.SchemaVersion || res.DecisionType != req.DecisionType {
		return gen.FallbackReasonMalformedOutput
	}
	if res.TenantID != req.TenantID || res.ApplicationID != req.ApplicationID {
		return gen.FallbackReasonTenantMismatch
	}
	if res.ProviderID != m.ProviderID || res.ModelID == nil || *res.ModelID != m.ModelID ||
		res.ModelRevision == nil || *res.ModelRevision != m.ModelRevision ||
		res.ContractVersion != req.ContractVersion ||
		(e.cfg.PinnedModelID != "" && e.cfg.PinnedModelID != string(m.ModelID)) ||
		(e.cfg.PinnedRevision != "" && e.cfg.PinnedRevision != string(m.ModelRevision)) {
		return gen.FallbackReasonModelMismatch
	}

	if len(res.Selections) == 0 || len(res.Selections) > maxSel {
		return gen.FallbackReasonMalformedOutput
	}
	seen := make(map[string]struct{}, len(res.Selections))
	for _, s := range res.Selections {
		id := string(s.CandidateID)
		if _, ok := ids[id]; !ok {
			return gen.FallbackReasonMalformedOutput // invented or foreign candidate id
		}
		if _, dup := seen[id]; dup {
			return gen.FallbackReasonMalformedOutput
		}
		seen[id] = struct{}{}
		if !unit(float64(s.Score)) || !unit(float64(s.CalibratedConfidence)) {
			return gen.FallbackReasonMalformedOutput
		}
	}

	if res.OodStatus != gen.DecisionScoreResultOodStatusInDistribution {
		// out_of_distribution, unsupported_shape and not_evaluated all fail closed: a
		// result whose distribution status is unknown is not an acceptable result.
		return gen.FallbackReasonOodUnsupportedShape
	}

	cal, ok := e.cfg.calibration(string(m.ModelID), string(m.ModelRevision), string(req.ContractVersion))
	if !ok || cal.ECE > cal.MaxECE {
		return gen.FallbackReasonCalibrationFailed
	}
	for _, s := range res.Selections {
		if float64(s.CalibratedConfidence) < e.cfg.MinConfidence {
			return gen.FallbackReasonLowConfidence
		}
	}
	return ""
}

func unit(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 1 }

// providerFailure maps an error from the provider to its fallback reason. Anything
// unrecognised is unavailable, never a success.
func providerFailure(err error) gen.FallbackReason {
	switch {
	case errors.Is(err, ErrUnsupportedRuntime):
		return gen.FallbackReasonUnsupportedRuntime
	case errors.Is(err, context.DeadlineExceeded), contracts.HasCode(err, contracts.ErrTimeout):
		return gen.FallbackReasonTimeout
	case contracts.HasCode(err, contracts.ErrSchemaInvalid):
		return gen.FallbackReasonMalformedOutput
	case contracts.HasCode(err, contracts.ErrResourceExhausted):
		return gen.FallbackReasonResourceGate
	case contracts.HasCode(err, contracts.ErrScopeViolation), contracts.HasCode(err, contracts.ErrScopeMissing):
		return gen.FallbackReasonTenantMismatch
	}
	return gen.FallbackReasonUnavailable
}
