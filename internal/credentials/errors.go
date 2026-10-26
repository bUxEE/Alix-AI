package credentials

import (
	"errors"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reason is a fixed, machine-readable cause carried in Details["reason"] of
// every error this package returns and in the audit record of a denial. It is
// never free text and never quotes a key, a token, a reference or anything a
// backend, an operating system or a provider said: the messages of those are
// not passed on, because they can echo a secret.
type Reason string

// The reasons.
const (
	ReasonNotBound           Reason = "not_bound"           // no such reference in this application and tenant (one answer for both, so existence does not leak)
	ReasonDeploymentMismatch Reason = "deployment_mismatch" // the reference belongs to another deployment
	ReasonProviderMismatch   Reason = "provider_mismatch"   // the reference is for another provider
	ReasonScopeNotPermitted  Reason = "scope_not_permitted" // a requested provider scope is not in the permitted scopes
	ReasonRevoked            Reason = "revoked"
	ReasonExpired            Reason = "expired"
	ReasonInvalid            Reason = "invalid_credential" // the provider rejected it: fails closed, no alternate provider
	ReasonQuarantined        Reason = "quarantined"
	ReasonPending            Reason = "pending"
	ReasonReauthRequired     Reason = "reauthorization_required"
	ReasonNoRefresh          Reason = "no_refresh_value"
	ReasonSecretMissing      Reason = "secret_missing" // the backend does not hold it (or it cannot be read)
	ReasonBackendFailed      Reason = "backend_failed"
	ReasonBackendReadOnly    Reason = "backend_read_only"
	ReasonBackendUnknown     Reason = "backend_unknown"
	ReasonAuditFailed        Reason = "audit_failed" // the use could not be recorded, so nothing is released
	ReasonValidatorMissing   Reason = "validator_missing"
	ReasonValidationFailed   Reason = "validation_failed" // the validation request could not be made
	ReasonRejected           Reason = "rejected_by_provider"
	ReasonBadRequest         Reason = "bad_request"
	ReasonUnsupported        Reason = "unsupported"
	ReasonCorrupt            Reason = "corrupt_record"
	ReasonInsecure           Reason = "insecure_permissions"
	// ReasonPrincipalMismatch: a principal-bound credential (SIWC) was resolved by another principal.
	ReasonPrincipalMismatch Reason = "credential_principal_mismatch"
)

func fail(code contracts.ErrCode, r Reason, msg string) *contracts.Error {
	return contracts.New(code, msg).WithDetails("reason", string(r))
}

// deny is a refusal to use or change a credential.
func deny(r Reason, msg string) *contracts.Error { return fail(contracts.ErrScopeViolation, r, msg) }

// ReasonOf returns the Reason of an error from this package, or "".
func ReasonOf(err error) Reason {
	var e *contracts.Error
	if errors.As(err, &e) && e != nil {
		if s, ok := e.Details["reason"].(string); ok {
			return Reason(s)
		}
	}
	return ""
}

// Errors of the backends. They carry no text of the operating system.
var (
	// ErrNotFound: the backend holds nothing for the slot.
	ErrNotFound = errors.New("credentials: no such secret")
	// ErrReadOnly: the backend cannot store (environment and file references).
	ErrReadOnly = errors.New("credentials: the backend is read only")
	// ErrInsecurePermissions: a key file, secret file or directory is readable or writable by others.
	ErrInsecurePermissions = errors.New("credentials: permissions are not owner only")
)
