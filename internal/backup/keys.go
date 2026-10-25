package backup

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/buxee/alix-ai/internal/trust"
)

// StaticKeys is a trust.KeyLookup over public keys supplied by the operator
// (files, flags), for the cases where the database that would hold a trust store
// is the thing being restored or has not been opened yet. Only the platform
// application is served. The published TEST-ONLY keys are refused unless
// AllowTestOnly is set, the rule trust.Store applies.
type StaticKeys struct {
	Keys          map[string]ed25519.PublicKey
	AllowTestOnly bool
}

// LookupKey implements trust.KeyLookup.
func (s StaticKeys) LookupKey(_ context.Context, applicationID, keyID string) (trust.Key, error) {
	if applicationID != trust.PlatformApplicationID {
		return trust.Key{}, fmt.Errorf("%w: only the platform application is served", trust.ErrUnknownKey)
	}
	pub, ok := s.Keys[keyID]
	if !ok {
		return trust.Key{}, fmt.Errorf("%w: %s", trust.ErrUnknownKey, keyID)
	}
	if trust.IsTestOnlyKey(pub) && !s.AllowTestOnly {
		return trust.Key{}, fmt.Errorf("%w: %s is a published TEST-ONLY key", trust.ErrUnknownKey, keyID)
	}
	return trust.Key{ApplicationID: applicationID, KeyID: keyID, PublicKey: pub, Status: trust.KeyActive}, nil
}

var _ trust.KeyLookup = StaticKeys{}
