package trust

import (
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Sentinel causes. Every failure of this package is a *contracts.Error (use
// contracts.CodeOf / HasCode for the classification) that also wraps one or
// more of these, so callers and tests can ask errors.Is for the exact reason
// instead of matching text.
var (
	// ErrUnsigned: the package has no signature manifest.
	ErrUnsigned = errors.New("package has no signature manifest")
	// ErrMalformedManifest: the signature manifest does not match the
	// signature-manifest schema (shape, grammar, ordering, limits).
	ErrMalformedManifest = errors.New("signature manifest is malformed")
	// ErrTampered: the package content differs from what was signed.
	ErrTampered = errors.New("package content differs from the signed manifest")
	// ErrUnsafePackage: the package holds an entry that cannot be listed
	// safely (symbolic link, special file, path outside the safe grammar,
	// path that escapes the root, stray file in signatures/).
	ErrUnsafePackage = errors.New("package holds an unsafe entry")
	// ErrBindingMismatch: the signed application or package version is not the
	// one the caller expects, so the signature belongs to another package.
	ErrBindingMismatch = errors.New("signature is bound to another application or package version")

	// ErrUnknownKey: the signing key is not registered for the application.
	ErrUnknownKey = errors.New("signing key is not registered for the application")
	// ErrRevokedKey: the signing key was revoked.
	ErrRevokedKey = errors.New("signing key is revoked")
	// ErrRetiredKey: the signing key was retired before the signature was made.
	ErrRetiredKey = errors.New("signing key was retired before this signature")
	// ErrKeyNotListed: the key is not among the publisher keys the package
	// names (integrity.publisher_key_ids).
	ErrKeyNotListed = errors.New("signing key is not listed as a publisher key of the package")
	// ErrWrongPurpose: the key is registered, but not for the kind of artifact
	// being verified (a checkpoint key over a release manifest, say).
	ErrWrongPurpose = errors.New("signing key is not registered for this purpose")
	// ErrSignatureExpired: the signature is past its expires_at.
	ErrSignatureExpired = errors.New("signature has expired")
	// ErrSignatureNotYetValid: signed_at lies in the future.
	ErrSignatureNotYetValid = errors.New("signature is dated in the future")
	// ErrBadSignature: the Ed25519 signature does not verify.
	ErrBadSignature = errors.New("signature does not verify")
	// ErrMalformedSignature: a signature record is not well formed.
	ErrMalformedSignature = errors.New("signature record is malformed")
	// ErrUnsupportedAlgorithm: only ed25519 exists.
	ErrUnsupportedAlgorithm = errors.New("unsupported signature algorithm")

	// ErrTestOnlyKey: a published TEST-ONLY key cannot be registered in a
	// trust store that has not opted in to test keys.
	ErrTestOnlyKey = errors.New("key is a published TEST-ONLY key")
	// ErrKeyConflict: the key ID is already registered, or the key material is
	// already registered (or revoked) elsewhere.
	ErrKeyConflict = errors.New("key conflicts with an existing registration")
)

// Reason classifies why one signature was not accepted.
type Reason string

// The reasons a signature can fail verification.
const (
	ReasonUnknownKey           Reason = "unknown_key"
	ReasonRevokedKey           Reason = "revoked_key"
	ReasonRetiredKey           Reason = "retired_key"
	ReasonKeyNotListed         Reason = "key_not_listed"
	ReasonWrongPurpose         Reason = "key_wrong_purpose"
	ReasonExpired              Reason = "signature_expired"
	ReasonNotYetValid          Reason = "signed_in_the_future"
	ReasonBadSignature         Reason = "bad_signature"
	ReasonMalformed            Reason = "malformed_signature"
	ReasonUnsupportedAlgorithm Reason = "unsupported_algorithm"
)

func (r Reason) sentinel() error {
	switch r {
	case ReasonUnknownKey:
		return ErrUnknownKey
	case ReasonRevokedKey:
		return ErrRevokedKey
	case ReasonRetiredKey:
		return ErrRetiredKey
	case ReasonKeyNotListed:
		return ErrKeyNotListed
	case ReasonWrongPurpose:
		return ErrWrongPurpose
	case ReasonExpired:
		return ErrSignatureExpired
	case ReasonNotYetValid:
		return ErrSignatureNotYetValid
	case ReasonMalformed:
		return ErrMalformedSignature
	case ReasonUnsupportedAlgorithm:
		return ErrUnsupportedAlgorithm
	default:
		return ErrBadSignature
	}
}

// SignatureFailure is why one signature of a set was not accepted.
type SignatureFailure struct {
	// Index is the position in the signatures array.
	Index int
	// KeyID is the key the signature claims. It is attacker-controlled text.
	KeyID  string
	Reason Reason
}

// VerifyError is returned when no signature of a set is acceptable. Failures
// lists the reason for every signature. Its contracts code is
// ErrUnauthenticated, or ErrExpired when expiry is the only reason found.
type VerifyError struct {
	Failures []SignatureFailure
	code     *contracts.Error
}

func newVerifyError(failures []SignatureFailure) *VerifyError {
	code := contracts.ErrUnauthenticated
	if len(failures) > 0 {
		allExpired := true
		for _, f := range failures {
			if f.Reason != ReasonExpired {
				allExpired = false
			}
		}
		if allExpired {
			code = contracts.ErrExpired
		}
	}
	e := &VerifyError{Failures: failures}
	e.code = contracts.New(code, "trust: no acceptable signature")
	return e
}

func (e *VerifyError) Error() string {
	if len(e.Failures) == 0 {
		return "trust: no signature to verify"
	}
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("signature %d (key %s): %s", f.Index, quoteKeyID(f.KeyID), f.Reason))
	}
	return "trust: no acceptable signature: " + strings.Join(parts, "; ")
}

// Unwrap exposes the contracts error first (so contracts.CodeOf finds it) and
// then one sentinel per distinct reason.
func (e *VerifyError) Unwrap() []error {
	out := []error{e.code}
	seen := map[error]bool{}
	for _, f := range e.Failures {
		s := f.Reason.sentinel()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// quoteKeyID prints an untrusted key ID safely and bounded.
func quoteKeyID(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return fmt.Sprintf("%q", s)
}

// DiffKind says how the package differs from what was signed.
type DiffKind string

// The kinds of difference TamperError reports.
const (
	DiffModified DiffKind = "modified" // listed, but another digest or size
	DiffAdded    DiffKind = "added"    // in the package, not in the manifest
	DiffMissing  DiffKind = "missing"  // in the manifest, not in the package
	DiffUnsafe   DiffKind = "unsafe"   // an entry that cannot be listed safely
)

// Difference is one way the package differs from its signed manifest.
type Difference struct {
	Path   string   `json:"path"`
	Kind   DiffKind `json:"kind"`
	Detail string   `json:"detail,omitempty"`
}

// maxReportedDifferences bounds TamperError.Differences.
const maxReportedDifferences = 100

// TamperError reports that the package is not what was signed: the signature
// itself may be valid, the content does not match it. A file in the package
// and not in the manifest counts exactly like a modified one. Its contracts
// code is ErrUnauthenticated.
type TamperError struct {
	// Differences holds up to 100 differences, sorted by path.
	Differences []Difference
	// Total counts all of them.
	Total int
	code  *contracts.Error
}

func newTamperError(diffs []Difference) *TamperError {
	e := &TamperError{Total: len(diffs)}
	if len(diffs) > maxReportedDifferences {
		diffs = diffs[:maxReportedDifferences]
	}
	e.Differences = diffs
	e.code = contracts.New(contracts.ErrUnauthenticated, "trust: package content does not match its signed manifest")
	return e
}

func (e *TamperError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "trust: package content does not match its signed manifest (%d difference(s)", e.Total)
	for i, d := range e.Differences {
		if i == 5 {
			b.WriteString("; ...")
			break
		}
		fmt.Fprintf(&b, "; %s %q", d.Kind, d.Path)
	}
	b.WriteString(")")
	return b.String()
}

// Unwrap exposes the contracts error and ErrTampered.
func (e *TamperError) Unwrap() []error { return []error{e.code, ErrTampered} }

// fail returns a *contracts.Error with the given code whose cause is the
// sentinel.
func fail(code contracts.ErrCode, cause error, format string, args ...any) *contracts.Error {
	return contracts.Wrap(code, cause, "trust: "+fmt.Sprintf(format, args...))
}
