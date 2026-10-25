package adapters

import (
	"fmt"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// ReasonResponseTooLarge is the Details["reason"] of the schema_invalid error
// that ReadLimited returns when a reply exceeds the size limit.
const ReasonResponseTooLarge = "response_too_large"

// schemaInvalid is the error for anything a host sent that is not what the
// contract requires. Messages built here name locations and rules only; they
// never copy a value out of the reply, because a reply is untrusted text.
func schemaInvalid(msg string) *contracts.Error {
	return contracts.New(contracts.ErrSchemaInvalid, msg)
}

func schemaInvalidf(format string, args ...any) *contracts.Error {
	return schemaInvalid(fmt.Sprintf(format, args...))
}

// UncertainOutcome wraps cause in the uncertain_outcome error that a transport
// returns when a Commit may have reached the host but its answer is lost or
// unusable. The cause stays reachable with errors.Is and errors.As. The caller
// must reconcile with Status and must not retry blindly.
func UncertainOutcome(cause error) *contracts.Error {
	return contracts.Wrap(contracts.ErrUncertainOutcome, cause,
		"the commit may have been applied; its outcome is unknown and must be checked by status before any retry")
}

// definiteCodes are the error codes that tell the caller that a Commit either
// never reached the host (unavailable, scope and validation failures raised
// here) or was answered by the host with a refusal. After one of these the
// outcome is known: nothing was applied. Every other failure of a Commit leaves
// the outcome unknown (timeout, a reply that cannot be used, an internal fault,
// a cancelled context) and is reported as uncertain_outcome.
var definiteCodes = map[contracts.ErrCode]bool{
	contracts.ErrUnavailable:          true,
	contracts.ErrScopeMissing:         true,
	contracts.ErrUnauthenticated:      true,
	contracts.ErrPermissionDenied:     true,
	contracts.ErrScopeViolation:       true,
	contracts.ErrUnknownCapability:    true,
	contracts.ErrAmbiguous:            true,
	contracts.ErrMissingFields:        true,
	contracts.ErrValidationFailed:     true,
	contracts.ErrStale:                true,
	contracts.ErrExpired:              true,
	contracts.ErrReplayed:             true,
	contracts.ErrConfirmationRequired: true,
	contracts.ErrStepUpRequired:       true,
	contracts.ErrConflict:             true,
	contracts.ErrDegraded:             true,
	contracts.ErrRateLimited:          true,
	contracts.ErrResourceExhausted:    true,
	contracts.ErrNoSupportedEvidence:  true,
	contracts.ErrPartialFailure:       true,
	contracts.ErrQuarantined:          true,
	contracts.ErrUncertainOutcome:     true,
}
