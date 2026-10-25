package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/trust"
)

// The release signing key built into the binary (keys/alixai-release-2026.pub;
// the private half is held offline by the maintainer and never reaches CI). A
// release manifest it signs verifies with no trust store setup at all; it is
// revoked like any other key, by registering it and revoking it (`alixai trust
// add-key -app alixai.platform -key-id alixai-release-2026 -purpose release`,
// then `alixai trust revoke-key`).
const (
	// PinnedKeyID is the key id of the built-in release key.
	PinnedKeyID = "alixai-release-2026"
	// pinnedKeyB64 is its raw 32-byte Ed25519 public key, standard base64.
	pinnedKeyB64 = "ok5oQ98ihECPbbWYuXWyfAlVbK6kZV97vQliBsYfzOU="
)

// pinnedKeyID and pinnedKey are what releaseKeys pins: variables so a test can
// pin a key it holds the private half of.
var (
	pinnedKeyID = PinnedKeyID
	pinnedKey   = mustDecode(pinnedKeyB64)
)

func mustDecode(s string) ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic("release: the pinned release key is malformed")
	}
	return ed25519.PublicKey(raw)
}

// PinnedPublicKey returns the built-in release key.
func PinnedPublicKey() ed25519.PublicKey { return append(ed25519.PublicKey(nil), pinnedKey...) }

// releaseKeys is the key lookup Verify uses: the keys of the lookup it is given,
// narrowed to release keys, plus the pinned key (security finding H1). Every
// signing key of AlixAI is registered under the platform application, so the
// application alone does not say which of them may sign a release; the purpose
// does (trust.PurposeRelease, checked by the verifier). On top of the lookup:
//
//   - the pinned id is the pinned key: when the lookup holds a key under that id
//     it must be the same material (its status, a revocation, applies), and when
//     it holds none the pinned key is served as an active release key;
//   - the pinned material counts as a release key under any id it was registered
//     with before purposes existed;
//   - material the lookup knows to be revoked (trust.RevocationLookup) is
//     revoked, under every id;
//   - a published TEST-ONLY key is unknown unless the lookup says it was
//     explicitly allowed (trust.TestOnlyPolicy): a key registered in a
//     development deployment does not stay a release key once the store is
//     opened normally.
type releaseKeys struct{ inner trust.KeyLookup }

func (r releaseKeys) LookupKey(ctx context.Context, applicationID, keyID string) (trust.Key, error) {
	k, err := r.inner.LookupKey(ctx, applicationID, keyID)
	switch {
	case err == nil:
	case errors.Is(err, trust.ErrUnknownKey) && keyID == pinnedKeyID && applicationID == trust.PlatformApplicationID:
		k = trust.Key{ApplicationID: applicationID, KeyID: keyID, PublicKey: PinnedPublicKey(), Status: trust.KeyActive,
			Purpose: trust.PurposeRelease, Label: "built-in release key"}
	default:
		return trust.Key{}, err
	}
	pinned := bytes.Equal(k.PublicKey, pinnedKey)
	switch {
	case keyID == pinnedKeyID && !pinned:
		return trust.Key{}, fmt.Errorf("%w: %s is registered with other key material than the built-in release key", trust.ErrUnknownKey, keyID)
	case pinned && k.Purpose == trust.PurposeNone:
		k.Purpose = trust.PurposeRelease
	}
	if trust.IsTestOnlyKey(k.PublicKey) && !allowsTestOnly(r.inner) {
		return trust.Key{}, fmt.Errorf("%w: %s is a published TEST-ONLY key", trust.ErrUnknownKey, keyID)
	}
	if rl, ok := r.inner.(trust.RevocationLookup); ok && k.Status != trust.KeyRevoked {
		revoked, err := rl.MaterialRevoked(ctx, k.PublicKey)
		if err != nil {
			return trust.Key{}, err
		}
		if revoked {
			k.Status = trust.KeyRevoked
		}
	}
	return k, nil
}

func allowsTestOnly(l trust.KeyLookup) bool {
	p, ok := l.(trust.TestOnlyPolicy)
	return ok && p.AllowsTestOnlyKeys()
}

// StaticKeys is a trust.KeyLookup over public keys given by the operator (files, flags), for when no trust store
// is open. Only the platform application is served, and every key is a release key: the operator named it for
// verifying release manifests. The published TEST-ONLY keys are refused unless AllowTestOnly is set, the same rule
// trust.Store applies.
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
	return trust.Key{ApplicationID: applicationID, KeyID: keyID, PublicKey: pub, Status: trust.KeyActive, Purpose: trust.PurposeRelease}, nil
}

// AllowsTestOnlyKeys implements trust.TestOnlyPolicy.
func (s StaticKeys) AllowsTestOnlyKeys() bool { return s.AllowTestOnly }

var (
	_ trust.KeyLookup      = StaticKeys{}
	_ trust.TestOnlyPolicy = StaticKeys{}
)
