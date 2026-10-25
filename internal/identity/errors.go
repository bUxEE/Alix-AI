package identity

import (
	"errors"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reason says why a credential was refused, from a closed vocabulary. It rides
// in contracts.Error.Details["reason"] so integrators can debug a rejected
// token and tests can prove the check they name is the one that fired. A
// reason never carries token contents, key material or any other secret.
type Reason string

const (
	// ReasonNoCredential: no Authorization header was presented.
	ReasonNoCredential Reason = "no_credential"
	// ReasonMalformed: not a well-formed compact token (structure, size,
	// alphabet, signature length).
	ReasonMalformed Reason = "malformed_token"
	// ReasonBadHeader: the JOSE header breaks the profile (algorithm, type, key
	// id, unknown or duplicate members).
	ReasonBadHeader Reason = "bad_header"
	// ReasonUnknownKey: the key id is not a registered, usable issuer key.
	ReasonUnknownKey Reason = "unknown_key"
	// ReasonKeyStoreUnavailable: the issuer key store could not be consulted,
	// so the token is refused (fail closed). It says nothing about the token.
	ReasonKeyStoreUnavailable Reason = "key_store_unavailable"
	// ReasonKeyRetired: the issuer key passed its retirement time.
	ReasonKeyRetired Reason = "key_retired"
	// ReasonBadSignature: the signature does not verify under the registered key.
	ReasonBadSignature Reason = "bad_signature"
	// ReasonBadClaims: a claim is missing, mistyped, unknown, duplicated or
	// internally inconsistent.
	ReasonBadClaims Reason = "bad_claims"
	// ReasonWrongIssuer: iss is not the issuer registered for the key.
	ReasonWrongIssuer Reason = "wrong_issuer"
	// ReasonWrongAudience: aud is not this deployment's audience.
	ReasonWrongAudience Reason = "wrong_audience"
	// ReasonWrongApplication: the application claim differs from the
	// application the signing key is registered for.
	ReasonWrongApplication Reason = "wrong_application"
	// ReasonNotYetValid: nbf or iat lies in the future beyond the skew allowance.
	ReasonNotYetValid Reason = "not_yet_valid"
	// ReasonExpired: exp (plus the skew allowance) has passed.
	ReasonExpired Reason = "expired"
	// ReasonLifetime: the token's validity window exceeds the maximum lifetime.
	ReasonLifetime Reason = "lifetime_too_long"
	// ReasonReplayed: the jti was already presented.
	ReasonReplayed Reason = "replayed"
	// ReasonReplayUnavailable: the replay cache could not be consulted, so the
	// token is refused (fail closed).
	ReasonReplayUnavailable Reason = "replay_cache_unavailable"
)

// DetailReason is the contracts.Error.Details key that carries the Reason.
const DetailReason = "reason"

// reject builds the typed error for a refused credential. The message is
// generic on purpose; the Reason carries the specifics.
func reject(code contracts.ErrCode, reason Reason, msg string) *contracts.Error {
	return contracts.New(code, msg).WithDetails(DetailReason, string(reason))
}

func rejectWrap(code contracts.ErrCode, reason Reason, cause error, msg string) *contracts.Error {
	return contracts.Wrap(code, cause, msg).WithDetails(DetailReason, string(reason))
}

// unauthenticated is the common case: the token cannot be trusted.
func unauthenticated(reason Reason) *contracts.Error {
	return reject(contracts.ErrUnauthenticated, reason, "context token rejected")
}

// ReasonOf returns the Reason recorded on err, or "" when err carries none.
func ReasonOf(err error) Reason {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil {
		return ""
	}
	if r, ok := ce.Details[DetailReason].(string); ok {
		return Reason(r)
	}
	return ""
}
