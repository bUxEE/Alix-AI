package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The token profile of ADR 0013. Changing any of these changes the wire
// contract with every host application and SDK.
const (
	// Algorithm is the one and only accepted JWS "alg". It is the RFC 8037
	// name for Ed25519; RFC 9864 deprecates it in favour of "Ed25519", which
	// golang-jwt, PyJWT and go-jose cannot emit yet (ADR 0013 records the
	// evidence and the trigger for switching). The verifier accepts exactly
	// this string, never both, and verifies with crypto/ed25519 only, so the
	// polymorphism RFC 9864 objects to does not exist here.
	Algorithm = "EdDSA"
	// TokenType is the explicit "typ" (RFC 8725 section 3.11), so a token of
	// another kind signed by the same key is never taken for a context token.
	TokenType = "alixai-context+jwt"
	// MaxTokenSize caps the compact token, checked before anything is decoded.
	MaxTokenSize = 8 << 10
	// MaxLifetime is the hard ceiling on a token's validity window. A verifier
	// may be configured lower, never higher.
	MaxLifetime = 15 * time.Minute
	// ClockSkew is the fixed allowance for clock difference between the host
	// and AlixAI, applied to nbf, iat and exp.
	ClockSkew = 30 * time.Second
)

var b64 = base64.RawURLEncoding.Strict()

// Options configures a Verifier.
type Options struct {
	// DeploymentID is the deployment level of every Scope this verifier
	// produces. Required; it comes from deployment configuration and is never
	// read from a token.
	DeploymentID string
	// Audience is the "aud" every token must carry. Empty means DeploymentID.
	Audience string
	// MaxLifetime is the longest accepted validity window (exp minus the
	// earlier of iat and nbf). Zero means the 15 minute hard ceiling; a larger
	// value is refused.
	MaxLifetime time.Duration
	// Keys resolves issuer keys. Required.
	Keys KeyResolver
	// Replay makes tokens single use. Required: there is no verifier without a
	// replay cache.
	Replay ReplayCache
	// Clock is the time source. Nil means the real clock.
	Clock clock.Clock
	// Logger receives operational faults (never token contents). Nil discards.
	Logger *slog.Logger
}

// Verifier verifies application context tokens and produces the Scope they
// assert. It is safe for concurrent use.
type Verifier struct {
	deploymentID string
	audience     string
	maxLifetime  time.Duration
	keys         KeyResolver
	replay       ReplayCache
	clk          clock.Clock
	log          *slog.Logger
}

var _ Authenticator = (*Verifier)(nil)

// NewVerifier validates o and returns a Verifier. It fails closed: a missing
// deployment ID, key resolver or replay cache, or a lifetime above the
// ceiling, is an error rather than a default.
func NewVerifier(o Options) (*Verifier, error) {
	if !validIdentifier(o.DeploymentID) {
		return nil, contracts.New(contracts.ErrValidationFailed, "identity: deployment id is missing or not a valid scope identifier")
	}
	aud := o.Audience
	if aud == "" {
		aud = o.DeploymentID
	}
	if !validAudience(aud) {
		return nil, contracts.New(contracts.ErrValidationFailed, "identity: audience is malformed")
	}
	maxLife := o.MaxLifetime
	switch {
	case maxLife == 0:
		maxLife = MaxLifetime
	case maxLife < 0 || maxLife > MaxLifetime:
		return nil, contracts.Newf(contracts.ErrValidationFailed, "identity: maximum token lifetime must be above zero and at most %s", MaxLifetime)
	}
	if o.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "identity: a key resolver is required")
	}
	if o.Replay == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "identity: a replay cache is required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Verifier{
		deploymentID: o.DeploymentID,
		audience:     aud,
		maxLifetime:  maxLife,
		keys:         o.Keys,
		replay:       o.Replay,
		clk:          clk,
		log:          log,
	}, nil
}

func validAudience(s string) bool {
	if s == "" || len(s) > maxIssuerLen {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Authenticate implements Authenticator.
func (v *Verifier) Authenticate(ctx context.Context, credential string) (*Verified, error) {
	return v.Verify(ctx, credential)
}

// Verify checks token and, if every rule holds, returns the Verified context.
// Anything else is an error carrying a contracts code and a Reason: expired
// and replayed tokens say so, an incomplete context is scope_missing, a
// malformed context value is scope_violation, and everything that makes the
// token untrustworthy is unauthenticated. Verify never returns a partial
// result with a nil error, and it never logs or returns any part of token.
//
// The order is fixed and cheap-first: size and structure, header profile, key
// lookup, signature, and only then the claims, so unauthenticated input never
// reaches the claim parser. The nonce is consumed last, after the Scope has
// validated, so a token that is refused for any other reason burns nothing.
func (v *Verifier) Verify(ctx context.Context, token string) (*Verified, error) {
	if len(token) == 0 || len(token) > MaxTokenSize {
		return nil, unauthenticated(ReasonMalformed)
	}
	headerB64, payloadB64, sigB64, ok := splitCompact(token)
	if !ok {
		return nil, unauthenticated(ReasonMalformed)
	}
	headerJSON, err := b64.DecodeString(headerB64)
	if err != nil {
		return nil, unauthenticated(ReasonMalformed)
	}
	payloadJSON, err := b64.DecodeString(payloadB64)
	if err != nil {
		return nil, unauthenticated(ReasonMalformed)
	}
	sig, err := b64.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, unauthenticated(ReasonMalformed)
	}

	hdr, err := parseHeader(headerJSON)
	if err != nil {
		return nil, err
	}

	key, err := v.keys.Lookup(ctx, hdr.keyID)
	switch {
	case errors.Is(err, ErrKeyNotFound):
		return nil, unauthenticated(ReasonUnknownKey)
	case err != nil:
		v.log.Error("issuer key lookup failed", "error", err)
		return nil, rejectWrap(contracts.ErrUnavailable, ReasonKeyStoreUnavailable, err, "issuer keys cannot be consulted")
	case len(key.Public) != ed25519.PublicKeySize || key.KeyID != hdr.keyID:
		// A resolver bug, not an attacker; ed25519.Verify would panic on the
		// length, so refuse here and say so loudly.
		v.log.Error("issuer key resolver returned an unusable key", "key_id_requested", hdr.keyID)
		return nil, unauthenticated(ReasonUnknownKey)
	}
	now := v.clk.Now()
	if !key.NotAfter.IsZero() && !now.Before(key.NotAfter) {
		return nil, unauthenticated(ReasonKeyRetired)
	}

	// The signing input is the first two segments exactly as received.
	if !ed25519.Verify(key.Public, []byte(headerB64+"."+payloadB64), sig) {
		return nil, unauthenticated(ReasonBadSignature)
	}

	c, err := parseClaims(payloadJSON)
	if err != nil {
		return nil, err
	}
	if c.issuer != key.Issuer {
		return nil, unauthenticated(ReasonWrongIssuer)
	}
	if c.audience != v.audience {
		return nil, unauthenticated(ReasonWrongAudience)
	}
	// A blank application is a missing level (scope_missing, from NewScope below),
	// not a mismatch with the key: only a non-blank claim is compared (SEC-035).
	if strings.TrimSpace(c.context.ApplicationID) != "" && c.context.ApplicationID != key.ApplicationID {
		return nil, unauthenticated(ReasonWrongApplication)
	}
	if err := v.checkTimes(c, now); err != nil {
		return nil, err
	}

	scope, err := NewScope(v.deploymentID, c.context)
	if err != nil {
		return nil, err
	}
	if scope.ApplicationID != key.ApplicationID {
		// Unreachable while the check above holds; kept so the guarantee that
		// a key speaks only for its own application does not depend on it.
		return nil, unauthenticated(ReasonWrongApplication)
	}

	expiry := time.Unix(c.exp, 0).UTC()
	if err := v.replay.Consume(ctx, scope, c.tokenID, expiry.Add(ClockSkew)); err != nil {
		var ce *contracts.Error
		if errors.As(err, &ce) && ce.Code == contracts.ErrReplayed {
			return nil, err
		}
		v.log.Error("replay cache failed", "error", err)
		return nil, rejectWrap(contracts.ErrUnavailable, ReasonReplayUnavailable, err, "context token cannot be checked for reuse")
	}

	return &Verified{
		Scope:     scope,
		Issuer:    key.Issuer,
		KeyID:     key.KeyID,
		TokenID:   c.tokenID,
		IssuedAt:  time.Unix(c.issuedAt, 0).UTC(),
		ExpiresAt: expiry,
	}, nil
}

// checkTimes applies the lifetime rules. Every comparison is on whole seconds
// of values bounded by maxNumericDateDigits, so none can overflow.
func (v *Verifier) checkTimes(c claims, now time.Time) error {
	n := now.Unix()
	skew := int64(ClockSkew / time.Second)
	maxLife := int64(v.maxLifetime / time.Second)

	switch {
	case c.exp <= c.issuedAt || c.exp <= c.notBefore:
		return unauthenticated(ReasonBadClaims)
	case c.exp-c.issuedAt > maxLife || c.exp-c.notBefore > maxLife:
		return unauthenticated(ReasonLifetime)
	case c.issuedAt > n+skew || c.notBefore > n+skew:
		return unauthenticated(ReasonNotYetValid)
	case c.exp-n > maxLife+skew:
		// Cannot follow from the rules above; it is the backstop that keeps a
		// token from ever being usable for longer than the ceiling.
		return unauthenticated(ReasonLifetime)
	case n >= c.exp+skew:
		return reject(contracts.ErrExpired, ReasonExpired, "context token expired")
	}
	return nil
}

// splitCompact splits a compact token into its three segments, each non-empty
// and made only of the base64url alphabet (no padding, no whitespace, no
// stray dots). The standard decoder skips newlines, so the alphabet is checked
// here rather than trusted to it.
func splitCompact(token string) (header, payload, sig string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", "", "", false
		}
		for i := 0; i < len(p); i++ {
			c := p[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			default:
				return "", "", "", false
			}
		}
	}
	return parts[0], parts[1], parts[2], true
}
