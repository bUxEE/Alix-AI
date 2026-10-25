package trust

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io/fs"
	"slices"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ClockSkew is how far into the future a signature's signed_at may lie before
// it is refused. It absorbs ordinary clock drift between the signing machine
// and this one and is not configurable.
const ClockSkew = 5 * time.Minute

// Verifier verifies detached signatures against a trust store. It fails
// closed: every path that is not an explicit, complete success is an error,
// and an error is never a partial result.
type Verifier struct {
	keys KeyLookup
	clk  clock.Clock
}

// NewVerifier returns a Verifier that looks keys up in keys and reads the
// time from clk (nil means the wall clock).
func NewVerifier(keys KeyLookup, clk clock.Clock) *Verifier {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Verifier{keys: keys, clk: clk}
}

// Accepted describes the signature that made a verification succeed.
type Accepted struct {
	// KeyID is the registered key that signed.
	KeyID string
	// Index is the position of the signature in the signatures array.
	Index     int
	SignedAt  time.Time
	ExpiresAt time.Time
}

// PayloadOptions says whose signature a payload needs.
type PayloadOptions struct {
	// ApplicationID is the application whose registered keys are trusted for
	// this payload. Required. Use PlatformApplicationID for AlixAI's own
	// artifacts.
	ApplicationID string
	// AllowedKeyIDs, when not nil, narrows the accepted keys to these IDs on
	// top of the registration (a package's integrity.publisher_key_ids). An
	// empty non-nil slice accepts nothing.
	AllowedKeyIDs []string
	// Purpose, when set, accepts only keys registered for exactly this purpose
	// (a key with no purpose or another one fails with ReasonWrongPurpose). The
	// platform's own artifacts name theirs: release manifests PurposeRelease,
	// audit checkpoints and tombstones PurposeAuditCheckpoint. Empty checks no
	// purpose.
	Purpose KeyPurpose
}

// VerifyPayload verifies that payload carries at least one acceptable
// signature in sigs: the signature verifies over CanonicalJSON(payload), its
// key is registered for opts.ApplicationID (for opts.Purpose, when set) and is
// allowed by opts.AllowedKeyIDs, the key is not revoked (and, if retired, retired after
// the signature was made), the signature has not expired and is not dated in
// the future. If none qualifies the error is a *VerifyError listing why each
// signature failed; its code is contracts.ErrUnauthenticated, or ErrExpired
// when expiry is the only reason. A trust store that cannot answer returns its
// own error (contracts.ErrUnavailable): that is a failure too.
func (v *Verifier) VerifyPayload(ctx context.Context, payload any, sigs []Signature, opts PayloadOptions) (Accepted, error) {
	canon, err := CanonicalJSON(payload)
	if err != nil {
		return Accepted{}, err
	}
	return v.verifyCanonical(ctx, canon, sigs, opts)
}

func (v *Verifier) verifyCanonical(ctx context.Context, canon []byte, sigs []Signature, opts PayloadOptions) (Accepted, error) {
	if v == nil || v.keys == nil {
		return Accepted{}, fail(contracts.ErrInternal, nil, "verifier has no trust store")
	}
	if err := checkApplicationID(opts.ApplicationID); err != nil {
		return Accepted{}, err
	}
	if len(sigs) == 0 {
		return Accepted{}, fail(contracts.ErrUnauthenticated, ErrUnsigned, "no signature to verify")
	}
	if len(sigs) > MaxSignatures {
		return Accepted{}, fail(contracts.ErrSchemaInvalid, ErrMalformedSignature, "%d signatures, at most %d allowed", len(sigs), MaxSignatures)
	}
	now := v.clk.Now().UTC()

	failures := make([]SignatureFailure, 0, len(sigs))
	for i, s := range sigs {
		reason, accepted, err := v.checkSignature(ctx, canon, s, opts, now)
		if err != nil {
			return Accepted{}, err
		}
		if reason == "" {
			accepted.Index = i
			return accepted, nil
		}
		failures = append(failures, SignatureFailure{Index: i, KeyID: s.KeyID, Reason: reason})
	}
	return Accepted{}, newVerifyError(failures)
}

// checkSignature returns the reason s is not acceptable, or an empty reason and
// the Accepted record. A non-nil error means the check could not be made.
func (v *Verifier) checkSignature(ctx context.Context, canon []byte, s Signature, opts PayloadOptions, now time.Time) (Reason, Accepted, error) {
	if s.Algorithm != AlgEd25519 {
		return ReasonUnsupportedAlgorithm, Accepted{}, nil
	}
	parsed, err := s.validate()
	if err != nil {
		return ReasonMalformed, Accepted{}, nil
	}
	if opts.AllowedKeyIDs != nil && !slices.Contains(opts.AllowedKeyIDs, s.KeyID) {
		return ReasonKeyNotListed, Accepted{}, nil
	}

	key, err := v.keys.LookupKey(ctx, opts.ApplicationID, s.KeyID)
	switch {
	case errors.Is(err, ErrUnknownKey):
		return ReasonUnknownKey, Accepted{}, nil
	case err != nil:
		return "", Accepted{}, err
	}
	switch key.Status {
	case KeyActive:
	case KeyRetired:
		if key.RetiredAt.IsZero() || !parsed.signedAt.Before(key.RetiredAt) {
			return ReasonRetiredKey, Accepted{}, nil
		}
	default: // revoked, and anything this version does not know
		return ReasonRevokedKey, Accepted{}, nil
	}
	if len(key.PublicKey) != ed25519.PublicKeySize {
		return "", Accepted{}, fail(contracts.ErrInternal, nil, "registered key %s has a malformed public key", quoteKeyID(s.KeyID))
	}
	if opts.Purpose != "" && key.Purpose != opts.Purpose {
		return ReasonWrongPurpose, Accepted{}, nil
	}

	if !ed25519.Verify(key.PublicKey, canon, parsed.sig) {
		return ReasonBadSignature, Accepted{}, nil
	}
	if !now.Before(parsed.expiresAt) {
		return ReasonExpired, Accepted{}, nil
	}
	if parsed.signedAt.After(now.Add(ClockSkew)) {
		return ReasonNotYetValid, Accepted{}, nil
	}
	return "", Accepted{KeyID: s.KeyID, SignedAt: parsed.signedAt, ExpiresAt: parsed.expiresAt}, nil
}

// PackageOptions says which package a signature manifest must authenticate.
// All three come from the package's own alixai.yaml, which is only believed
// after the signature verified.
type PackageOptions struct {
	// ApplicationID is the manifest's application.id. Required.
	ApplicationID string
	// PackageVersion is the manifest's package.version. Required.
	PackageVersion string
	// PublisherKeyIDs is the manifest's integrity.publisher_key_ids. Required
	// and not empty: a package that names no publisher key cannot verify.
	PublisherKeyIDs []string
}

func (o PackageOptions) validate() error {
	if err := checkApplicationID(o.ApplicationID); err != nil {
		return err
	}
	if len(o.PackageVersion) > 64 || !semverRE.MatchString(o.PackageVersion) {
		return fail(contracts.ErrValidationFailed, nil, "package version %q is not a semantic version", clip(o.PackageVersion))
	}
	if len(o.PublisherKeyIDs) == 0 {
		return fail(contracts.ErrValidationFailed, ErrKeyNotListed, "the package names no publisher key")
	}
	for _, id := range o.PublisherKeyIDs {
		if err := checkKeyID(id); err != nil {
			return err
		}
	}
	return nil
}

// PackageVerification is the result of a successful VerifyPackage.
type PackageVerification struct {
	// Manifest is the parsed signature manifest. Its Signed part is
	// authentic; Signed.Files is exactly the content of the package.
	Manifest *SignatureManifest
	// Accepted is the signature that authenticated it.
	Accepted Accepted
}

// VerifyPackage is package authenticity (master plan 4.5 check 1). It
// returns nil error only when all of these hold:
//
//  1. signatures/signature-manifest.json exists and matches the schema
//     (ParseSignatureManifest); a missing file is ErrUnsigned;
//  2. signed.application_id and signed.package_version equal opts: the
//     signature belongs to this package and no other;
//  3. at least one signature is acceptable (VerifyPayload) for a key
//     registered for opts.ApplicationID and named in opts.PublisherKeyIDs;
//  4. only then is the content looked at: every file outside signatures/ is
//     listed, with the listed SHA-256 and size, and nothing else is present.
//     A file in the package that is not in the manifest, a missing file, a
//     changed byte, a symbolic link and a stray file in signatures/ are all a
//     *TamperError (contracts.ErrUnauthenticated).
//
// Content is checked after the signature on purpose: nothing about the
// package is believed before its signature verified. The verification reads
// each file once; a package that someone can write to while it is being
// verified, or after, is outside what this function can guarantee.
func (v *Verifier) VerifyPackage(ctx context.Context, fsys fs.FS, opts PackageOptions) (*PackageVerification, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	sm, err := ReadSignatureManifest(fsys)
	if err != nil {
		return nil, err
	}
	if sm.Signed.ApplicationID != opts.ApplicationID {
		return nil, fail(contracts.ErrUnauthenticated, ErrBindingMismatch,
			"signature is for application %q, the package declares %q", clip(sm.Signed.ApplicationID), opts.ApplicationID)
	}
	if sm.Signed.PackageVersion != opts.PackageVersion {
		return nil, fail(contracts.ErrUnauthenticated, ErrBindingMismatch,
			"signature is for package version %q, the package declares %q", clip(sm.Signed.PackageVersion), opts.PackageVersion)
	}
	canon, err := sm.Signed.Canonical()
	if err != nil {
		return nil, err
	}
	accepted, err := v.verifyCanonical(ctx, canon, sm.Signatures, PayloadOptions{
		ApplicationID: opts.ApplicationID,
		AllowedKeyIDs: append([]string{}, opts.PublisherKeyIDs...),
	})
	if err != nil {
		return nil, err
	}
	if err := checkContent(fsys, sm.Signed); err != nil {
		return nil, err
	}
	return &PackageVerification{Manifest: sm, Accepted: accepted}, nil
}

// VerifyPackageDir is VerifyPackage for a directory on disk; the root itself
// must not be a symbolic link.
func (v *Verifier) VerifyPackageDir(ctx context.Context, root string, opts PackageOptions) (*PackageVerification, error) {
	fsys, err := DirFS(root)
	if err != nil {
		return nil, err
	}
	return v.VerifyPackage(ctx, fsys, opts)
}

// checkContent requires the package to be exactly the signed file list.
func checkContent(fsys fs.FS, signed Signed) error {
	l, err := scanFS(fsys)
	if err != nil {
		return err
	}
	var diffs []Difference
	for _, p := range l.problems {
		diffs = append(diffs, Difference{Path: p.path, Kind: DiffUnsafe, Detail: p.detail})
	}

	listed := make(map[string]FileEntry, len(signed.Files))
	for _, f := range signed.Files {
		listed[f.Path] = f
	}
	present := make(map[string]bool, len(l.files))
	for _, p := range l.files {
		present[p] = true
		if _, ok := listed[p]; !ok {
			diffs = append(diffs, Difference{Path: p, Kind: DiffAdded, Detail: "file is not in the signed manifest"})
		}
	}
	for _, f := range signed.Files {
		if !present[f.Path] {
			diffs = append(diffs, Difference{Path: f.Path, Kind: DiffMissing, Detail: "file is in the signed manifest but not in the package"})
		}
	}
	if len(diffs) > 0 {
		sortDiffs(diffs)
		return newTamperError(diffs)
	}

	for _, p := range l.files {
		got, err := hashFile(fsys, p)
		switch {
		case errors.Is(err, ErrUnsafePackage):
			diffs = append(diffs, Difference{Path: p, Kind: DiffUnsafe, Detail: "entry changed type after it was listed"})
			continue
		case err != nil:
			return err
		}
		want := listed[p]
		if got.SHA256 != want.SHA256 || got.Size != want.Size {
			diffs = append(diffs, Difference{Path: p, Kind: DiffModified, Detail: "content differs from the signed digest"})
		}
	}
	if len(diffs) > 0 {
		sortDiffs(diffs)
		return newTamperError(diffs)
	}
	return nil
}

func sortDiffs(d []Difference) {
	sort.SliceStable(d, func(i, j int) bool { return d[i].Path < d[j].Path })
}
