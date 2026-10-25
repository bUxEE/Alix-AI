package trust

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// SignPayload signs the canonical JSON of payload (CanonicalJSON) with key and
// returns the detached signature record. It is the one signer behind package
// signatures, integration-audit reports and release manifests: the signature
// covers exactly the canonical bytes, with no prefix or envelope, so any
// RFC 8785 implementation can verify it. A payload that must be bound to an
// application or a purpose carries that binding inside itself (as the package
// manifest carries application_id).
//
// signedAt and expiresAt are written as UTC timestamps at second precision;
// expiresAt is mandatory and must be later than signedAt. The signature is
// verified against the key's public half before it is returned.
func SignPayload(payload any, keyID string, key ed25519.PrivateKey, signedAt, expiresAt time.Time) (Signature, error) {
	if err := checkKeyID(keyID); err != nil {
		return Signature{}, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return Signature{}, fail(contracts.ErrValidationFailed, nil, "private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(key))
	}
	sa, ea := signedAt.UTC().Truncate(time.Second), expiresAt.UTC().Truncate(time.Second)
	if sa.IsZero() || !ea.After(sa) {
		return Signature{}, fail(contracts.ErrValidationFailed, nil, "expires_at must be set and later than signed_at")
	}
	canon, err := CanonicalJSON(payload)
	if err != nil {
		return Signature{}, err
	}
	raw := ed25519.Sign(key, canon)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), canon, raw) {
		return Signature{}, fail(contracts.ErrInternal, nil, "freshly made signature does not verify")
	}
	return Signature{
		KeyID:     keyID,
		Algorithm: AlgEd25519,
		Signature: base64.StdEncoding.EncodeToString(raw),
		SignedAt:  formatTime(sa),
		ExpiresAt: formatTime(ea),
	}, nil
}

// SigningKey is a private key and the ID it is registered under.
type SigningKey struct {
	KeyID string
	Key   ed25519.PrivateKey
}

// SignOptions configures SignPackageDir.
type SignOptions struct {
	PackageIdentity
	// Keys sign the package: one to MaxSignatures keys. Several keys are how a
	// key rotation overlaps: a package signed by the old and the new key
	// verifies under either.
	Keys []SigningKey
	// Now is the signing time, and created_at of a new manifest.
	Now time.Time
	// ExpiresAt is when every signature stops being valid. Mandatory.
	ExpiresAt time.Time
	// Append adds the signatures to the manifest already in the package
	// instead of replacing it. The package must still match that manifest
	// exactly (created_at is kept), and the new keys must not have signed it
	// yet; the total stays within MaxSignatures.
	Append bool
}

// SignPackageDir builds the integrity manifest of the package in root, signs
// it with every key in opts and writes signatures/signature-manifest.json. An
// existing signature manifest is replaced unless opts.Append is set. It
// returns the manifest it wrote.
//
// The package is listed under the same fixed rules as BuildSigned: a symbolic
// link, a path outside the safe grammar, a stray file in signatures/ and any
// other entry that cannot be listed fails the signing; nothing is skipped.
func SignPackageDir(root string, opts SignOptions) (*SignatureManifest, error) {
	if err := opts.PackageIdentity.validate(); err != nil {
		return nil, err
	}
	if n := len(opts.Keys); n < 1 || n > MaxSignatures {
		return nil, fail(contracts.ErrValidationFailed, nil, "signing needs 1 to %d keys, got %d", MaxSignatures, n)
	}
	seen := map[string]bool{}
	for _, k := range opts.Keys {
		if err := checkKeyID(k.KeyID); err != nil {
			return nil, err
		}
		if seen[k.KeyID] {
			return nil, fail(contracts.ErrValidationFailed, nil, "key %s listed twice", quoteKeyID(k.KeyID))
		}
		seen[k.KeyID] = true
	}
	now := opts.Now.UTC().Truncate(time.Second)
	if now.IsZero() {
		return nil, fail(contracts.ErrValidationFailed, nil, "signing time not set")
	}

	fsys, err := DirFS(root)
	if err != nil {
		return nil, err
	}

	var (
		signed   Signed
		existing []Signature
	)
	if opts.Append {
		prev, err := ReadSignatureManifest(fsys)
		if err != nil {
			return nil, err
		}
		if prev.Signed.ApplicationID != opts.ApplicationID || prev.Signed.PackageVersion != opts.PackageVersion {
			return nil, fail(contracts.ErrConflict, ErrBindingMismatch,
				"existing signature manifest is for %q %q, not %q %q",
				clip(prev.Signed.ApplicationID), clip(prev.Signed.PackageVersion), opts.ApplicationID, opts.PackageVersion)
		}
		createdAt, err := parseTimestamp(prev.Signed.CreatedAt)
		if err != nil {
			return nil, malformed("signed.created_at: %v", err)
		}
		signed, err = BuildSigned(fsys, opts.PackageIdentity, createdAt)
		if err != nil {
			return nil, err
		}
		want, err := prev.Signed.Canonical()
		if err != nil {
			return nil, err
		}
		got, err := signed.Canonical()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(want, got) {
			return nil, fail(contracts.ErrConflict, ErrTampered,
				"the package changed since it was first signed; sign it again without append")
		}
		existing = prev.Signatures
		for _, s := range existing {
			if seen[s.KeyID] {
				return nil, fail(contracts.ErrConflict, nil, "key %s has already signed this package", quoteKeyID(s.KeyID))
			}
		}
		if len(existing)+len(opts.Keys) > MaxSignatures {
			return nil, fail(contracts.ErrValidationFailed, nil,
				"a manifest holds at most %d signatures (%d present, %d to add)", MaxSignatures, len(existing), len(opts.Keys))
		}
	} else {
		signed, err = BuildSigned(fsys, opts.PackageIdentity, now)
		if err != nil {
			return nil, err
		}
	}

	sigs := append([]Signature(nil), existing...)
	for _, k := range opts.Keys {
		s, err := SignPayload(signed, k.KeyID, k.Key, now, opts.ExpiresAt)
		if err != nil {
			return nil, err
		}
		sigs = append(sigs, s)
	}
	sm := &SignatureManifest{Format: FormatV1, Signed: signed, Signatures: sigs}
	if err := WriteSignatureManifest(root, sm); err != nil {
		return nil, err
	}
	return sm, nil
}
