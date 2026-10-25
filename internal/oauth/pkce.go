package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// randomURLSafe returns n random bytes from the operating system's CSPRNG as unpadded base64url. State,
// nonce and PKCE verifier all come from here (never from a time-ordered ID).
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fail(contracts.ErrInternal, ReasonBadRequest, "oauth: no randomness available")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newVerifier returns a PKCE code verifier (RFC 7636 4.1): 32 random bytes, 43 characters.
func newVerifier() (credentials.Secret, error) {
	v, err := randomURLSafe(32)
	return credentials.Secret(v), err
}

// ChallengeS256 returns the PKCE S256 code challenge of a verifier (RFC 7636 4.2):
// BASE64URL(SHA256(verifier)) without padding. S256 is the only method this engine uses; "plain" is
// never offered.
func ChallengeS256(verifier credentials.Secret) string {
	sum := sha256.Sum256([]byte(verifier.Reveal()))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// equalConst compares two strings in constant time (for state, nonce and consent digests).
func equalConst(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
