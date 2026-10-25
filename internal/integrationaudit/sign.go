package integrationaudit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"time"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// timeLayout is the one timestamp format of the attestation: UTC, second
// precision.
const timeLayout = "2006-01-02T15:04:05Z"

// DefaultReportValidity is how long the signature on a report stays valid. It
// bounds how long the report can be verified as evidence, not how long it may
// be used to activate a package: that is Config.MaxReportAge.
const DefaultReportValidity = 365 * 24 * time.Hour

// Attestation is the signed statement that binds a report to the moment it was
// made and to the package it audited.
//
// The Report is deterministic and holds no time, so that two audits of the
// same inputs are byte-identical. The attestation holds what differs: when the
// audit was made, which is bound to the signature so it cannot be changed
// without breaking it. What is signed is attestationPayload, not the report: the
// payload names the report by its SHA-256, and carries its own format so that a
// signature made by the same platform key over another kind of document (an
// audit-log checkpoint, a release manifest) can never verify as an attestation.
type Attestation struct {
	Format        string `json:"format"`
	ReportSHA256  string `json:"report_sha256"`
	ApplicationID string `json:"application_id"`
	ManifestHash  string `json:"manifest_hash"`
	// IssuedAt is when the audit was made, from the injected clock.
	IssuedAt   string            `json:"issued_at"`
	Signatures []trust.Signature `json:"signatures"`
}

// attestationPayload is exactly what the signature covers.
type attestationPayload struct {
	Format        string `json:"format"`
	ReportSHA256  string `json:"report_sha256"`
	ApplicationID string `json:"application_id"`
	ManifestHash  string `json:"manifest_hash"`
	IssuedAt      string `json:"issued_at"`
}

func (a Attestation) payload() attestationPayload {
	return attestationPayload{
		Format: a.Format, ReportSHA256: a.ReportSHA256, ApplicationID: a.ApplicationID,
		ManifestHash: a.ManifestHash, IssuedAt: a.IssuedAt,
	}
}

// SignedReport is the audit result as stored and exported: the report and the
// platform signature over its attestation. It is the only evidence on which a
// package is activated.
type SignedReport struct {
	Report      Report      `json:"report"`
	Attestation Attestation `json:"attestation"`
}

// IssuedTime parses the attestation's issue time.
func (a Attestation) IssuedTime() (time.Time, error) {
	t, err := time.Parse(time.RFC3339, a.IssuedAt)
	if err != nil || a.IssuedAt != t.UTC().Format(timeLayout) {
		return time.Time{}, contracts.New(contracts.ErrSchemaInvalid, "integrationaudit: the attestation's issued_at is not a UTC second-precision RFC 3339 time")
	}
	return t.UTC(), nil
}

// SignReport signs a report with the platform key. now is when the audit was
// made; the signature is valid for validity (DefaultReportValidity when zero).
// The report is canonicalised first, so a report that cannot be signed (a
// float, an invalid string) is an error and never a half-signed document.
func SignReport(r Report, keyID string, key ed25519.PrivateKey, now time.Time, validity time.Duration) (SignedReport, error) {
	if validity == 0 {
		validity = DefaultReportValidity
	}
	sha, err := Digest(r)
	if err != nil {
		return SignedReport{}, err
	}
	issued := now.UTC().Truncate(time.Second)
	a := Attestation{
		Format:        AttestationFormat,
		ReportSHA256:  sha,
		ApplicationID: r.Subject.ApplicationID,
		ManifestHash:  r.Subject.ManifestHash,
		IssuedAt:      issued.Format(timeLayout),
	}
	sig, err := trust.SignPayload(a.payload(), keyID, key, issued, issued.Add(validity))
	if err != nil {
		return SignedReport{}, err
	}
	a.Signatures = []trust.Signature{sig}
	return SignedReport{Report: r, Attestation: a}, nil
}

// Verified is what VerifyReport establishes about a signed report.
type Verified struct {
	// IssuedAt is when the audit was made, as the signature binds it.
	IssuedAt time.Time
	// Signer is the platform key that signed.
	Signer trust.Accepted
	// ReportSHA256 identifies the report content.
	ReportSHA256 string
}

// VerifyReport checks a signed report. It succeeds only when
//
//   - the attestation and the report carry the formats this version defines;
//   - the SHA-256 the attestation names is the SHA-256 of the report as it
//     stands, so an altered report fails;
//   - the attestation is about the application and package the report says it
//     audited;
//   - a signature over the attestation's payload verifies under a key
//     registered for trust.PlatformApplicationID that is not revoked, retired
//     before it signed, or expired. A key registered for a host application
//     never verifies a report, however it signed;
//   - that signature is dated at the attestation's issued_at.
//
// Any other result is an error with the code unauthenticated (or schema_invalid
// for a malformed document); there is no partial success.
func VerifyReport(ctx context.Context, v *trust.Verifier, s SignedReport) (Verified, error) {
	if v == nil {
		return Verified{}, contracts.New(contracts.ErrInternal, "integrationaudit: no verifier to check a report")
	}
	if s.Attestation.Format != AttestationFormat || s.Report.Format != ReportFormat {
		return Verified{}, contracts.New(contracts.ErrUnauthenticated, "integrationaudit: the document is not an integration-audit report of a known format")
	}
	sha, err := Digest(s.Report)
	if err != nil {
		return Verified{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "integrationaudit: the report cannot be canonicalised")
	}
	if subtle.ConstantTimeCompare([]byte(sha), []byte(s.Attestation.ReportSHA256)) != 1 {
		return Verified{}, contracts.New(contracts.ErrUnauthenticated, "integrationaudit: the report does not match the digest that was signed")
	}
	if s.Attestation.ApplicationID != s.Report.Subject.ApplicationID || s.Attestation.ManifestHash != s.Report.Subject.ManifestHash {
		return Verified{}, contracts.New(contracts.ErrUnauthenticated, "integrationaudit: the attestation is about another application or package than the report")
	}
	issued, err := s.Attestation.IssuedTime()
	if err != nil {
		return Verified{}, err
	}
	acc, err := v.VerifyPayload(ctx, s.Attestation.payload(), s.Attestation.Signatures,
		trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID})
	if err != nil {
		return Verified{}, err
	}
	// The signature's own signed_at is not covered by it, and it is what the key's
	// retirement is checked against: it must be the issue time the signature does
	// cover (SignReport signs at that instant), or it could be dated at will.
	if !acc.SignedAt.Equal(issued) {
		return Verified{}, contracts.New(contracts.ErrUnauthenticated, "integrationaudit: the signature is not dated when the audit was made")
	}
	return Verified{IssuedAt: issued, Signer: acc, ReportSHA256: sha}, nil
}

// Marshal encodes a signed report as canonical JSON, the form that is stored
// and compared.
func (s SignedReport) Marshal() ([]byte, error) { return canonical(s) }

// ParseSignedReport decodes a signed report. Unknown members are an error, so a
// document that carries more than this version defines is not read as if it did
// not.
func ParseSignedReport(raw []byte) (SignedReport, error) {
	var s SignedReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return SignedReport{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "integrationaudit: the stored report is not a signed report")
	}
	return s, nil
}
