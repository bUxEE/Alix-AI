package wire

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Declined builds the result of a provider that declines to score: a fallback
// disposition with a typed reason, no selections, and the distribution status that
// goes with it. The engine's gate returns the reason as recorded. Nothing about the
// request is carried.
func Declined(req gen.DecisionRequest, m gen.ScorerManifest, reason gen.FallbackReason) gen.DecisionScoreResult {
	ood := gen.DecisionScoreResultOodStatusNotEvaluated
	if reason == gen.FallbackReasonOodUnsupportedShape {
		ood = gen.DecisionScoreResultOodStatusUnsupportedShape
	}
	model, rev := m.ModelID, m.ModelRevision
	return gen.DecisionScoreResult{
		Disposition:     gen.DecisionScoreResultDispositionFallback,
		FallbackReason:  &reason,
		ProviderID:      m.ProviderID,
		ModelID:         &model,
		ModelRevision:   &rev,
		ContractVersion: req.ContractVersion,
		DecisionType:    req.DecisionType,
		SchemaVersion:   req.SchemaVersion,
		TenantID:        req.TenantID,
		ApplicationID:   req.ApplicationID,
		Selections:      []gen.DecisionScoreResultSelection{},
		OodStatus:       ood,
		CacheStatus:     gen.DecisionScoreResultCacheStatusBypassed,
	}
}

// ReasonOf is the fallback reason the decision engine records for what a provider
// returned: the reason of a declined result, or the reason of a failure. It mirrors
// the engine's mapping so the adapters can state, and their tests check, that every
// failure ends in a typed reason and never in a success. An unclassified failure is
// unavailable.
func ReasonOf(res gen.DecisionScoreResult, err error) gen.FallbackReason {
	if err == nil {
		if res.Disposition == gen.DecisionScoreResultDispositionFallback && res.FallbackReason != nil {
			return *res.FallbackReason
		}
		return ""
	}
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

// StatusKind is how an adapter reads a non-200 HTTP status.
type StatusKind int

// The status kinds.
const (
	// StatusUnsupported: the route, backend or model kind is absent on this runtime.
	StatusUnsupported StatusKind = iota
	StatusTimeout
	StatusBusy
	StatusRejected
	StatusUnavailable
)

// ClassifyStatus maps a non-success status (and the lower-cased error message of
// its body, used for choosing only and never copied anywhere) to a kind.
//
//   - 404 and 405 on the scoring route: the runtime has no such route
//     (unsupported), unless the message says the model is what is missing
//     (unavailable: the operator has not installed it);
//   - 501: the backend does not implement the API (unsupported);
//   - 400 naming an unsupported model or backend: unsupported; any other 400 or
//     422 is our request being rejected (rejected: not a runtime verdict);
//   - 408, 504: timeout; 429 and 503: busy (resource gate); 5xx otherwise:
//     unavailable.
func ClassifyStatus(status int, msg string) StatusKind {
	modelMissing := strings.Contains(msg, "model") && (strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || strings.Contains(msg, "not installed") || strings.Contains(msg, "unknown"))
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		if modelMissing {
			return StatusUnavailable
		}
		return StatusUnsupported
	case http.StatusNotImplemented:
		return StatusUnsupported
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if strings.Contains(msg, "unsupported") || strings.Contains(msg, "not support") || strings.Contains(msg, "decisions") && strings.Contains(msg, "usecase") {
			return StatusUnsupported
		}
		return StatusRejected
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return StatusTimeout
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return StatusBusy
	}
	return StatusUnavailable
}

// StatusError converts a non-success response into a *contracts.Error with a fixed
// phrase and the status, never the body. who prefixes it. A StatusUnsupported
// answer wraps ErrUnsupportedRuntime.
func StatusError(who string, resp *Response) error {
	msg := ErrorMessage(resp.Body)
	switch ClassifyStatus(resp.Status, msg) {
	case StatusUnsupported:
		return contracts.Wrap(contracts.ErrUnavailable, ErrUnsupportedRuntime, who+": the runtime does not provide this scoring route or model kind").
			WithDetails("upstream_status", resp.Status)
	case StatusTimeout:
		return contracts.New(contracts.ErrTimeout, who+": the runtime timed out").WithDetails("upstream_status", resp.Status)
	case StatusBusy:
		return contracts.New(contracts.ErrResourceExhausted, who+": the runtime is at its concurrency or resource limit").WithDetails("upstream_status", resp.Status)
	case StatusRejected:
		return contracts.New(contracts.ErrInternal, who+": the runtime rejected the request").WithDetails("upstream_status", resp.Status)
	}
	return contracts.New(contracts.ErrUnavailable, who+": the runtime is unavailable").WithDetails("upstream_status", resp.Status)
}
