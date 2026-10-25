package trust

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// signatureB64Len is the length of a 64-byte signature in padded standard
// base64.
const signatureB64Len = 88

func malformed(format string, args ...any) error {
	return fail(contracts.ErrSchemaInvalid, ErrMalformedManifest, format, args...)
}

// ParseSignatureManifest parses and strictly validates the bytes of a
// signature manifest against the signature-manifest schema. It checks shape
// and grammar only; it does not look at any file or signature value's
// authenticity.
//
// Strictness beyond encoding/json: duplicate keys, floats and invalid UTF-8
// are refused (see Canonicalize); a property the schema does not define, or one
// spelled with different letter case, is refused (the parsed value must
// re-encode to exactly the same canonical JSON); the file list must be sorted
// strictly ascending by byte order with no case-folded duplicates; no listed
// path is under signatures/; digests are lower-case hex; signature values are
// canonical padded base64 of exactly 64 bytes; timestamps are UTC with a literal
// Z; expires_at is later than signed_at. A failure is contracts.ErrSchemaInvalid
// wrapping ErrMalformedManifest.
func ParseSignatureManifest(raw []byte) (*SignatureManifest, error) {
	if len(raw) > MaxSignatureManifestBytes {
		return nil, malformed("signature manifest is larger than %d bytes", MaxSignatureManifestBytes)
	}
	canon, err := Canonicalize(raw)
	if err != nil {
		return nil, fail(contracts.ErrSchemaInvalid, ErrMalformedManifest, "signature manifest is not acceptable JSON: %v", err)
	}
	var sm SignatureManifest
	if err := json.Unmarshal(canon, &sm); err != nil {
		return nil, malformed("signature manifest does not match the schema: %v", err)
	}
	again, err := CanonicalJSON(sm)
	if err != nil {
		return nil, malformed("signature manifest cannot be re-encoded: %v", err)
	}
	if !bytes.Equal(canon, again) {
		return nil, malformed("signature manifest has a missing, unknown or differently cased property, or a null where a value is required")
	}
	if err := sm.Validate(); err != nil {
		return nil, err
	}
	return &sm, nil
}

// Validate checks sm against the schema's rules (see ParseSignatureManifest).
func (sm *SignatureManifest) Validate() error {
	if sm.Format != FormatV1 {
		return malformed("format is %q, want %q", clip(sm.Format), FormatV1)
	}
	if err := sm.Signed.Validate(); err != nil {
		return err
	}
	if n := len(sm.Signatures); n < 1 || n > MaxSignatures {
		return malformed("signatures has %d entries, want 1 to %d", n, MaxSignatures)
	}
	seen := map[string]bool{}
	for i, s := range sm.Signatures {
		if _, err := s.validate(); err != nil {
			return malformed("signatures[%d]: %v", i, err)
		}
		if seen[s.KeyID] {
			return malformed("signatures[%d]: key %s signs twice", i, quoteKeyID(s.KeyID))
		}
		seen[s.KeyID] = true
	}
	return nil
}

// Validate checks the signed payload against the schema's rules.
func (s Signed) Validate() error {
	if !appIDRE.MatchString(s.ApplicationID) {
		return malformed("signed.application_id %q is not a valid application id", clip(s.ApplicationID))
	}
	if len(s.PackageVersion) > 64 || !semverRE.MatchString(s.PackageVersion) {
		return malformed("signed.package_version %q is not a semantic version", clip(s.PackageVersion))
	}
	if _, err := parseTimestamp(s.CreatedAt); err != nil {
		return malformed("signed.created_at: %v", err)
	}
	if s.HashAlgorithm != HashSHA256 {
		return malformed("signed.hash_algorithm is %q, want %q", clip(s.HashAlgorithm), HashSHA256)
	}
	if n := len(s.Files); n < 1 || n > MaxFiles {
		return malformed("signed.files has %d entries, want 1 to %d", n, MaxFiles)
	}
	seen := make(map[string]struct{}, len(s.Files))
	prev := ""
	for i, f := range s.Files {
		if err := ValidateListedPath(f.Path); err != nil {
			return malformed("signed.files[%d]: %v", i, err)
		}
		if !sha256RE.MatchString(f.SHA256) {
			return malformed("signed.files[%d] (%s): sha256 is not 64 lower-case hex characters", i, clip(f.Path))
		}
		if f.Size < 0 || f.Size > MaxFileSize {
			return malformed("signed.files[%d] (%s): size %d is outside 0..%d", i, clip(f.Path), f.Size, MaxFileSize)
		}
		if i > 0 && f.Path <= prev {
			return malformed("signed.files is not sorted strictly ascending by path at %q (duplicates and disorder are refused)", clip(f.Path))
		}
		prev = f.Path
		folded := strings.ToLower(f.Path)
		if _, dup := seen[folded]; dup {
			return malformed("signed.files lists %q twice under case folding", clip(f.Path))
		}
		seen[folded] = struct{}{}
	}
	return nil
}

// parsedSignature is a Signature whose fields passed validation.
type parsedSignature struct {
	sig       []byte
	signedAt  time.Time
	expiresAt time.Time
}

// validate checks one signature record against the schema's rules and
// returns its parsed values.
func (s Signature) validate() (parsedSignature, error) {
	if !validIdentifier(s.KeyID) {
		return parsedSignature{}, errors.New("key_id is not a valid identifier")
	}
	if s.Algorithm != AlgEd25519 {
		return parsedSignature{}, fmt.Errorf("algorithm is %q, only %q exists", clip(s.Algorithm), AlgEd25519)
	}
	sig, err := decodeSignature(s.Signature)
	if err != nil {
		return parsedSignature{}, err
	}
	signedAt, err := parseTimestamp(s.SignedAt)
	if err != nil {
		return parsedSignature{}, fmt.Errorf("signed_at: %w", err)
	}
	expiresAt, err := parseTimestamp(s.ExpiresAt)
	if err != nil {
		return parsedSignature{}, fmt.Errorf("expires_at: %w", err)
	}
	if !expiresAt.After(signedAt) {
		return parsedSignature{}, errors.New("expires_at must be later than signed_at")
	}
	return parsedSignature{sig: sig, signedAt: signedAt, expiresAt: expiresAt}, nil
}

// decodeSignature decodes the canonical padded standard base64 of a 64-byte
// signature. Strict decoding refuses non-zero trailing bits, so one signature
// has exactly one text form.
func decodeSignature(s string) ([]byte, error) {
	if len(s) != signatureB64Len {
		return nil, fmt.Errorf("signature must be %d base64 characters, got %d", signatureB64Len, len(s))
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != 64 {
		return nil, errors.New("signature is not canonical standard base64 of 64 bytes")
	}
	return b, nil
}

// validIdentifier mirrors schemas/common/ids identifier and pkg/contracts
// scopeValueState: 1 to 128 characters, no whitespace, no control character
// and no '*'.
func validIdentifier(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > contracts.MaxScopeValueLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ReadSignatureManifest reads and parses signatures/signature-manifest.json
// from the package in fsys. A missing file is ErrUnsigned (contracts
// ErrUnauthenticated): an unsigned package never verifies. The file must be a
// regular file of at most MaxSignatureManifestBytes.
func ReadSignatureManifest(fsys fs.FS) (*SignatureManifest, error) {
	f, err := fsys.Open(SignatureManifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fail(contracts.ErrUnauthenticated, ErrUnsigned, "package has no %s", SignatureManifestPath)
		}
		return nil, fail(contracts.ErrUnavailable, err, "cannot open %s", SignatureManifestPath)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fail(contracts.ErrUnavailable, err, "cannot stat %s", SignatureManifestPath)
	}
	if !st.Mode().IsRegular() {
		return nil, malformed("%s is not a regular file", SignatureManifestPath)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxSignatureManifestBytes+1))
	if err != nil {
		return nil, fail(contracts.ErrUnavailable, err, "cannot read %s", SignatureManifestPath)
	}
	return ParseSignatureManifest(raw)
}

// Marshal returns the file form of the manifest: indented JSON with a final
// newline. The indentation is cosmetic; signatures cover the canonical JSON
// of the signed part, never these bytes.
func (sm *SignatureManifest) Marshal() ([]byte, error) {
	if err := sm.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sm); err != nil {
		return nil, fail(contracts.ErrInternal, err, "cannot encode the signature manifest")
	}
	return b.Bytes(), nil
}

// WriteSignatureManifest writes sm to root/signatures/signature-manifest.json
// (the directory is created, mode 0755: package content is not secret). The
// file is written to a temporary name and renamed over the target, so a
// reader never sees a half-written manifest.
func WriteSignatureManifest(root string, sm *SignatureManifest) error {
	data, err := sm.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, SignaturesDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(contracts.ErrUnavailable, err, "cannot create %s", dir)
	}
	tmp, err := os.CreateTemp(dir, "signature-manifest-*.tmp")
	if err != nil {
		return fail(contracts.ErrUnavailable, err, "cannot create a temporary file in %s", dir)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fail(contracts.ErrUnavailable, err, "cannot write %s", tmpName)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fail(contracts.ErrUnavailable, err, "cannot write %s", tmpName)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return fail(contracts.ErrUnavailable, err, "cannot set the mode of %s", tmpName)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, SignatureManifestFile)); err != nil {
		cleanup()
		return fail(contracts.ErrUnavailable, err, "cannot install %s", SignatureManifestPath)
	}
	return nil
}
