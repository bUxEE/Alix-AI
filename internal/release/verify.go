package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Parse parses a manifest as strictly as internal/trust parses a package signature manifest: the bytes are
// acceptable canonical-JSON input (no duplicate key, no float, valid UTF-8), no property is unknown, missing or
// spelled with other letter case, and the rules of the format hold (Signed.Validate). It does not check a
// signature: use Verify for anything that is going to be trusted.
func Parse(raw []byte) (*Manifest, error) {
	canon, err := trust.Canonicalize(raw)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest is not acceptable JSON")
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(canon))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest does not match the format")
	}
	again, err := trust.CanonicalJSON(m)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest cannot be re-encoded")
	}
	if !bytes.Equal(canon, again) {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "release manifest has a missing or differently cased property, or a null where a value is required")
	}
	if m.Format != Format {
		return nil, contracts.Newf(contracts.ErrSchemaInvalid, "release manifest format is %q, want %q", m.Format, Format)
	}
	if err := m.Signed.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest is invalid")
	}
	if len(m.Signatures) > trust.MaxSignatures {
		return nil, contracts.Newf(contracts.ErrSchemaInvalid, "release manifest has %d signatures, at most %d are allowed", len(m.Signatures), trust.MaxSignatures)
	}
	return &m, nil
}

// Sign adds one signature to the manifest in raw. The payload that is signed is exactly the `signed` part of the
// parsed file. A key that has already signed it is refused, and so is a fifth signature. The result is the file
// form (MarshalIndent) and parses back.
func Sign(raw []byte, key trust.SigningKey, now time.Time, validFor time.Duration) ([]byte, error) {
	m, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	for _, s := range m.Signatures {
		if s.KeyID == key.KeyID {
			return nil, contracts.Newf(contracts.ErrConflict, "key %s has already signed this manifest", key.KeyID)
		}
	}
	if len(m.Signatures) >= trust.MaxSignatures {
		return nil, contracts.Newf(contracts.ErrConflict, "the manifest already carries %d signatures, the most it can hold", len(m.Signatures))
	}
	if validFor <= 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "the signature validity must be positive")
	}
	sig, err := trust.SignPayload(m.Signed, key.KeyID, key.Key, now, now.Add(validFor))
	if err != nil {
		return nil, err
	}
	m.Signatures = append(m.Signatures, sig)
	out, err := MarshalIndent(m)
	if err != nil {
		return nil, err
	}
	if _, err := Parse(out); err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "signed manifest does not parse back")
	}
	return out, nil
}

// envelope is the manifest as far as the signature check needs it. The signed part stays raw JSON: the signature
// is over its canonical form, so it is authenticated before anything in it is interpreted.
type envelope struct {
	Signed     json.RawMessage   `json:"signed"`
	Signatures []trust.Signature `json:"signatures"`
}

// Verify authenticates the manifest in raw and returns it. It fails closed, in this order:
//
//  1. keys is nil: nothing could be trusted (unauthenticated);
//  2. the bytes are not acceptable canonical JSON, or have no signed part;
//  3. no signature, or none from a release key that is neither revoked nor expired, over the exact bytes of the
//     signed part (clk is read for expiry, nil is the wall clock). A release key is the built-in one (PinnedKeyID),
//     a key of keys registered for the platform application with trust.PurposeRelease, or a key of a StaticKeys
//     (named by the operator for this): the audit-checkpoint key, or any other platform key, is not one (H1);
//  4. only then is the content parsed strictly (Parse) and every pin checked.
//
// The returned trust.Accepted names the key that authenticated the manifest.
func Verify(ctx context.Context, raw []byte, keys trust.KeyLookup, clk clock.Clock) (*Manifest, trust.Accepted, error) {
	if keys == nil {
		return nil, trust.Accepted{}, contracts.New(contracts.ErrUnauthenticated, "release manifest: no keys to verify it with")
	}
	canon, err := trust.Canonicalize(raw)
	if err != nil {
		return nil, trust.Accepted{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest is not acceptable JSON")
	}
	var env envelope
	if err := json.Unmarshal(canon, &env); err != nil {
		return nil, trust.Accepted{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest does not match the format")
	}
	if len(env.Signed) == 0 || env.Signatures == nil {
		return nil, trust.Accepted{}, contracts.New(contracts.ErrSchemaInvalid, "release manifest: signed and signatures are required")
	}
	acc, err := trust.NewVerifier(releaseKeys{inner: keys}, clk).VerifyPayload(ctx, env.Signed, env.Signatures,
		trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID, Purpose: trust.PurposeRelease})
	if err != nil {
		return nil, trust.Accepted{}, err
	}
	m, err := Parse(raw)
	if err != nil {
		return nil, trust.Accepted{}, err
	}
	return m, acc, nil
}

// VerifyFiles re-hashes the files in dir against the manifest. Every listed file must be there, a regular file,
// with the listed size and SHA-256, and nothing else, whether a file, a link or a directory, may be there (except
// skip, normally the manifest itself): an unlisted entry is unsigned content. It checks the files against the manifest it is given: call it
// with the result of Verify.
func (m *Manifest) VerifyFiles(dir, skip string) error {
	var errs []error
	listed := map[string]bool{}
	for _, a := range m.Signed.Artifacts {
		listed[a.Name] = true
		p := filepath.Join(dir, a.Name)
		// A listed file is the file itself: a symbolic link, whose target can change after it
		// was hashed, or a directory is not.
		if st, err := os.Lstat(p); err == nil && !st.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("%s is not a regular file", a.Name))
			continue
		}
		sum, size, err := HashFile(p)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
		case sum != a.SHA256 || size != a.Size:
			errs = append(errs, fmt.Errorf("%s: sha256 %s (%d bytes) does not match the manifest (%s, %d bytes)", a.Name, sum, size, a.SHA256, a.Size))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "release files cannot be read")
	}
	for _, e := range entries {
		if !listed[e.Name()] && e.Name() != skip {
			errs = append(errs, fmt.Errorf("%s is in the directory but not in the manifest", e.Name()))
		}
	}
	if len(errs) > 0 {
		return contracts.Wrap(contracts.ErrValidationFailed, errors.Join(errs...), "release files do not match the signed manifest")
	}
	return nil
}

// HashFile returns the lower-case hex SHA-256 and the size of the regular file at path.
func HashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// MarshalIndent is the file form of a JSON document: indented, no HTML escaping, one final newline.
func MarshalIndent(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
