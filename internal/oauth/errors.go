package oauth

import (
	"errors"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reason is a fixed, machine-readable cause carried in Details["reason"] of every error this package
// returns. It is never free text: nothing a provider said (an error description, a response body) and no
// token, code, state or URL is ever copied into an error.
type Reason string

// The reasons.
const (
	ReasonManualFallback   Reason = "manual_fallback" // the provider has no OAuth flow here: use the manual API-key path
	ReasonBadRequest       Reason = "bad_request"     // a malformed or unknown input
	ReasonBadConfig        Reason = "bad_provider_config"
	ReasonUnknownProvider  Reason = "unknown_provider"
	ReasonUnknownCapab     Reason = "unknown_capability" // a capability or scope the provider config does not list
	ReasonConsentMismatch  Reason = "consent_mismatch"   // the consent digest is not the one prepared
	ReasonRedirectRejected Reason = "redirect_rejected"  // not the registered redirect, nor a valid loopback redirect
	ReasonStateUnknown     Reason = "state_unknown"
	ReasonStateExpired     Reason = "state_expired"
	ReasonReplayed         Reason = "replayed" // the state was already used
	ReasonScopeMismatch    Reason = "scope_mismatch"
	ReasonTooManyPending   Reason = "too_many_pending"
	ReasonAccessDenied     Reason = "access_denied" // the user declined at the provider
	ReasonProviderError    Reason = "provider_error"
	ReasonIssuerMismatch   Reason = "issuer_mismatch"
	ReasonExchangeFailed   Reason = "exchange_failed"
	ReasonBadTokenResponse Reason = "bad_token_response"
	ReasonIDToken          Reason = "id_token_invalid"
	ReasonNonce            Reason = "nonce_mismatch"
	ReasonAccountMismatch  Reason = "account_mismatch" // another account answered a re-authorization that was not a switch
	ReasonProviderDown     Reason = "provider_unavailable"
	ReasonEgressDenied     Reason = "egress_denied"
	ReasonReauthRequired   Reason = "reauthorization_required" // the provider no longer honours the refresh value
	ReasonNotOAuth         Reason = "not_oauth"                // the reference is not an OAuth credential
	ReasonStoreFailed      Reason = "store_failed"
)

func fail(code contracts.ErrCode, r Reason, msg string) *contracts.Error {
	return contracts.New(code, msg).WithDetails("reason", string(r))
}

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

// IsManualFallback reports whether err says that the provider has no usable OAuth flow and the manual
// API-key entry of internal/credentials applies.
func IsManualFallback(err error) bool { return ReasonOf(err) == ReasonManualFallback }
