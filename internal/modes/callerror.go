package modes

import (
	"errors"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// CallErrorOf maps the error a provider adapter returned to the CallError the router classifies. The provider's
// own text is never copied. It reads the adapters' own contract first: every adapter error carries
// Details["provider_error_kind"] and Details["sent"] (false only when nothing left: a refused credential, a
// refused dial or route, local validation). Without them it falls back to the error code.
//
// A refusal of the credential service, or of the credential's scope, is authentication_failed. It is sent=false
// when the error says so, so it can never become uncertain_outcome and its reservation never settles at the
// reserved bound for a call that did not leave. A refusal of the PROVIDER (401, 403) carries sent=true and keeps it.
func CallErrorOf(err error) *CallError {
	ce := &CallError{Kind: pv.ProviderErrorKindConnectionLost, Sent: true}
	var typed *contracts.Error
	hasKind := false
	if errors.As(err, &typed) && typed != nil {
		if s, ok := typed.Details["provider_error_kind"].(string); ok {
			if k := pv.ProviderErrorKind(s); k.Valid() {
				ce.Kind, hasKind = k, true
			}
		}
		if sent, ok := typed.Details["sent"].(bool); ok {
			ce.Sent = sent
		}
	}
	switch contracts.CodeOf(err) {
	case contracts.ErrRateLimited:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindRateLimited
		}
		if info, ok := providers.RateLimitFrom(err); ok {
			ce.RetryAfter = info.RetryAfter
		}
	case contracts.ErrTimeout:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindTimeout
		}
	case contracts.ErrUnavailable:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindUnavailable
		}
	case contracts.ErrSchemaInvalid:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindMalformedResponse
		}
	case contracts.ErrPermissionDenied:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindPermissionDenied
		}
	case contracts.ErrUnauthenticated, contracts.ErrScopeViolation, contracts.ErrScopeMissing:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindAuthenticationFailed
		}
	case contracts.ErrValidationFailed:
		if !hasKind {
			ce.Kind = pv.ProviderErrorKindBadRequest
		}
		ce.Sent = false
	}
	return ce
}

// notSentCallError returns the CallError of err when err proves that nothing was sent (Details["sent"] is false),
// and nil otherwise. The router uses it for a sender that returned a bare adapter error instead of a CallError.
func notSentCallError(err error) *CallError {
	var typed *contracts.Error
	if !errors.As(err, &typed) || typed == nil {
		return nil
	}
	if sent, ok := typed.Details["sent"].(bool); !ok || sent {
		return nil
	}
	return CallErrorOf(err)
}
