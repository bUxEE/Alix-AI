package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ErrManifestUnsigned is wrapped by the error ManifestVerifier.Verify returns when the manifest
// carries no usable signature, no validity window, or no verifier was configured.
var ErrManifestUnsigned = errors.New("lifecycle: the scorer manifest is not signed")

// ManifestSignatureWindow is the validity window of a manifest signature. The manifest
// schema's signature object has no window of its own (algorithm, key id and value only), while
// trust.Verifier requires one, so a Revision carries it beside the manifest and the signer
// binds it into the signed bytes (ManifestSigningPayload): rewriting either end invalidates the
// signature, so an expired manifest cannot be revived by editing its window.
type ManifestSignatureWindow struct {
	SignedAt  time.Time
	ExpiresAt time.Time
}

// ManifestSigningPayload returns exactly what a manifest signer signs and ManifestVerifier
// verifies: the canonical manifest without its signature member, together with the signature
// window. Sign it with trust.SignPayload.
func ManifestSigningPayload(m gen.ScorerManifest, w ManifestSignatureWindow) (map[string]any, error) {
	if w.SignedAt.IsZero() || !w.ExpiresAt.After(w.SignedAt) {
		return nil, contracts.New(contracts.ErrValidationFailed, "lifecycle: the signature window needs signed_at and a later expires_at")
	}
	// The generated type refuses to encode an unsigned manifest; the member is dropped below.
	m.Signature = gen.ScorerManifestSignature{Algorithm: gen.ScorerManifestSignatureAlgorithmEd25519, KeyID: "placeholder", Value: "AAAA"}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "lifecycle: the manifest cannot be encoded for signing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "lifecycle: the manifest cannot be encoded for signing")
	}
	delete(doc, "signature")
	return map[string]any{
		"manifest":   doc,
		"signed_at":  windowTime(w.SignedAt),
		"expires_at": windowTime(w.ExpiresAt),
	}, nil
}

func windowTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339Nano) }

// ManifestVerifier checks the signature of a scorer manifest against the trust store
// before anything the manifest says (file hashes, encoder hash, revision) is believed.
type ManifestVerifier struct {
	tv   *trust.Verifier
	opts trust.PayloadOptions
}

// NewManifestVerifier returns a verifier that resolves release keys in keys under the platform
// application (scorer manifests are AlixAI artifacts) and reads the time from clk. A non-nil
// allowedKeyIDs narrows the accepted keys further; an empty non-nil slice accepts nothing.
func NewManifestVerifier(keys trust.KeyLookup, clk clock.Clock, allowedKeyIDs []string) (*ManifestVerifier, error) {
	if keys == nil {
		return nil, errors.New("lifecycle: a manifest verifier needs a trust store")
	}
	return &ManifestVerifier{
		tv:   trust.NewVerifier(keys, clk),
		opts: trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID, AllowedKeyIDs: allowedKeyIDs},
	}, nil
}

// Verify fails closed. It returns nil only when the manifest names ed25519, a key id and a value,
// the window is well formed, and trust.Verifier accepts the signature (registered, not revoked,
// verifies over ManifestSigningPayload, not expired, not dated in the future). A bad signature
// is contracts.ErrUnauthenticated, an expired one contracts.ErrExpired, a trust store that
// cannot answer contracts.ErrUnavailable.
func (v *ManifestVerifier) Verify(ctx context.Context, m gen.ScorerManifest, w ManifestSignatureWindow) error {
	if v == nil || v.tv == nil {
		return contracts.Wrap(contracts.ErrUnauthenticated, ErrManifestUnsigned, "lifecycle: no manifest verifier is configured, so no revision can be trusted")
	}
	sig := m.Signature
	if sig.Algorithm != gen.ScorerManifestSignatureAlgorithmEd25519 || sig.KeyID == "" || sig.Value == "" {
		return contracts.Wrap(contracts.ErrUnauthenticated, ErrManifestUnsigned, "lifecycle: the scorer manifest is unsigned")
	}
	if w.SignedAt.IsZero() || !w.ExpiresAt.After(w.SignedAt) {
		return contracts.Wrap(contracts.ErrUnauthenticated, ErrManifestUnsigned, "lifecycle: the manifest signature has no validity window")
	}
	payload, err := ManifestSigningPayload(m, w)
	if err != nil {
		return err
	}
	_, err = v.tv.VerifyPayload(ctx, payload, []trust.Signature{{
		KeyID: string(sig.KeyID), Algorithm: trust.AlgEd25519, Signature: sig.Value,
		SignedAt: windowTime(w.SignedAt), ExpiresAt: windowTime(w.ExpiresAt),
	}}, v.opts)
	switch {
	case err == nil:
		return nil
	case contracts.HasCode(err, contracts.ErrExpired):
		return contracts.Wrap(contracts.ErrExpired, err, "lifecycle: the manifest signature has expired")
	case contracts.HasCode(err, contracts.ErrUnauthenticated):
		return contracts.Wrap(contracts.ErrUnauthenticated, err, "lifecycle: the manifest signature is not acceptable")
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "lifecycle: the manifest signature could not be checked")
}
