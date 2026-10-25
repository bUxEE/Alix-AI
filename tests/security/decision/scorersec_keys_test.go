package decisionsec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

const scorerSecKeyID = "scorer-release-1"

// scorerSecKeys is a release key pair generated for one test (never written anywhere) and the
// trust store that registers its public half under the platform application.
type scorerSecKeys struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	// other is a second key pair the store does not know.
	other  ed25519.PrivateKey
	status trust.KeyStatus
}

func newScorerSecKeys(t testing.TB) *scorerSecKeys {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &scorerSecKeys{pub: pub, priv: priv, other: other, status: trust.KeyActive}
}

// LookupKey implements trust.KeyLookup.
func (k *scorerSecKeys) LookupKey(_ context.Context, app, id string) (trust.Key, error) {
	if app != trust.PlatformApplicationID || id != scorerSecKeyID {
		return trust.Key{}, trust.ErrUnknownKey
	}
	return trust.Key{ApplicationID: app, KeyID: id, PublicKey: k.pub, Status: k.status}, nil
}

func (k *scorerSecKeys) verifier(t testing.TB, clk clock.Clock) *lifecycle.ManifestVerifier {
	t.Helper()
	v, err := lifecycle.NewManifestVerifier(k, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// window is a validity window around now.
func scorerSecWindow(now time.Time) lifecycle.ManifestSignatureWindow {
	return lifecycle.ManifestSignatureWindow{SignedAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour)}
}

// signWith signs rev with key under keyID and window w, the way a release would.
func (k *scorerSecKeys) signWith(t testing.TB, rev lifecycle.Revision, key ed25519.PrivateKey, keyID string, w lifecycle.ManifestSignatureWindow) lifecycle.Revision {
	t.Helper()
	payload, err := lifecycle.ManifestSigningPayload(rev.Manifest, w)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := trust.SignPayload(payload, keyID, key, w.SignedAt, w.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	rev.Manifest.Signature = gen.ScorerManifestSignature{
		Algorithm: gen.ScorerManifestSignatureAlgorithmEd25519, KeyID: common.Identifier(keyID), Value: sig.Signature,
	}
	rev.SignatureWindow = w
	return rev
}

func (k *scorerSecKeys) sign(t testing.TB, rev lifecycle.Revision, now time.Time) lifecycle.Revision {
	t.Helper()
	return k.signWith(t, rev, k.priv, scorerSecKeyID, scorerSecWindow(now))
}
