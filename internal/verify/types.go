package verify

import (
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Language is a response language the verifier and the templates support.
type Language string

const (
	LangEN Language = "en"
	LangIT Language = "it"
)

// ParseLanguage maps a BCP 47 or POSIX tag ("en", "it-IT", "en_GB") to a supported Language. The
// tag is matched on its primary subtag, case-insensitively.
func ParseLanguage(tag string) (Language, error) {
	base := strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(base, "-_."); i >= 0 {
		base = base[:i]
	}
	switch Language(base) {
	case LangEN, LangIT:
		return Language(base), nil
	}
	return "", failure(contracts.ErrValidationFailed, ErrUnsupportedLanguage, "language %q is not supported (supported: en, it)", tag)
}

// The sentinel errors of this package. Each is wrapped in a *contracts.Error, so errors.Is
// matches the sentinel and contracts.CodeOf gives the typed code.
var (
	// ErrUnsupportedLanguage: the language has no verifier rules or templates (validation_failed).
	ErrUnsupportedLanguage = errors.New("verify: unsupported language")
	// ErrInvalidDirective: the DialogueDirective or ActionReceipt is not usable: a must_state name
	// that no fact carries, a fact value of no known kind or with a malformed value, an act outside
	// the enum (schema_invalid). The directive is built by the core, so this is a bug upstream; it
	// is never answered with a verified "ok".
	ErrInvalidDirective = errors.New("verify: invalid directive or receipt")
	// ErrReceiptMismatch: the receipt is not the one the directive names (internal).
	ErrReceiptMismatch = errors.New("verify: receipt does not match the directive")
	// ErrTemplateUnverified: a deterministic template did not pass the verifier. It is a bug in the
	// templates or the verifier, found by the check that runs on every template before it is
	// returned; nothing is shown to the user from it (internal).
	ErrTemplateUnverified = errors.New("verify: template failed verification")
)

func failure(code contracts.ErrCode, sentinel error, format string, args ...any) error {
	return contracts.Wrap(code, sentinel, fmt.Sprintf(format, args...))
}

// Options configure verification and rendering.
type Options struct {
	// Language is the language of the response. Numbers, dates and the templates are read and
	// written in its conventions. Required.
	Language Language
	// FieldLabels maps a capability field name to the label the application's terminology gives
	// it in Language ("slot_start" -> "Appointment time"). Optional: a field without a label is
	// shown as its name with underscores turned into spaces. The labels are the application's own
	// text, so numbers in them are allowed in a response.
	FieldLabels map[string]string
}

// ViolationKind classifies one reason a response was rejected.
type ViolationKind string

const (
	// ViolationEmptyText: the response has no text.
	ViolationEmptyText ViolationKind = "empty_text"
	// ViolationTextTooLong: the response exceeds MaxTextBytes.
	ViolationTextTooLong ViolationKind = "text_too_long"
	// ViolationInvisibleCharacters: the text contains format or control characters (zero-width
	// characters, bidirectional overrides, a byte order mark). They display as nothing, or reorder
	// what is displayed, so what the verifier read is not what a reader sees. The response is not
	// shown even if what remains verifies.
	ViolationInvisibleCharacters ViolationKind = "invisible_characters"
	// ViolationMissingFact: a fact named in must_state is not stated.
	ViolationMissingFact ViolationKind = "missing_fact"
	// ViolationUnknownNumber: a number the directive does not carry.
	ViolationUnknownNumber ViolationKind = "unknown_number"
	// ViolationUnknownDate: a date, or an instant, the directive does not carry.
	ViolationUnknownDate ViolationKind = "unknown_date"
	// ViolationUnknownTime: a time of day the directive does not carry.
	ViolationUnknownTime ViolationKind = "unknown_time"
	// ViolationUnknownAmount: an amount of money the directive does not carry.
	ViolationUnknownAmount ViolationKind = "unknown_amount"
	// ViolationCurrencyMismatch: an amount the directive carries, in another currency.
	ViolationCurrencyMismatch ViolationKind = "currency_mismatch"
	// ViolationWeekdayMismatch: a weekday that does not belong to the date it is stated with, or
	// to any date the directive carries.
	ViolationWeekdayMismatch ViolationKind = "weekday_mismatch"
	// ViolationUnknownIdentifier: an identifier-like token (ID, reference, URL, e-mail) the
	// directive does not carry.
	ViolationUnknownIdentifier ViolationKind = "unknown_identifier"
	// ViolationCompletionWithoutReceipt: the response says something was done and no committed
	// ActionReceipt backs it.
	ViolationCompletionWithoutReceipt ViolationKind = "completion_without_receipt"
	// ViolationCitationNotInEvidence: a citation to a source the directive does not list (or lists
	// as removed).
	ViolationCitationNotInEvidence ViolationKind = "citation_not_in_evidence"
	// ViolationReceiptMissing: the act reports an outcome and no receipt was supplied, so nothing
	// can be verified against it.
	ViolationReceiptMissing ViolationKind = "receipt_missing"
)

// Violation is one reason a response failed verification. It never carries the offending text
// of an identifier, citation or completion claim, because model output can echo secrets and
// results are logged; Fingerprint lets two violations be correlated instead.
type Violation struct {
	Kind ViolationKind
	// Field is the fact field of a missing_fact violation.
	Field string
	// Got is the canonical form of the offending number, date, time or amount (never raw model
	// text), e.g. "24", "2026-10-15", "10:45", "50 EUR".
	Got string
	// Fingerprint is the first eight hex digits of the SHA-256 of the offending text.
	Fingerprint string
}

func (v Violation) String() string {
	var b strings.Builder
	b.WriteString(string(v.Kind))
	if v.Field != "" {
		b.WriteString(" field=" + v.Field)
	}
	if v.Got != "" {
		b.WriteString(" got=" + v.Got)
	}
	if v.Fingerprint != "" {
		b.WriteString(" fp=" + v.Fingerprint)
	}
	return b.String()
}

// Result is the outcome of verifying one response.
type Result struct {
	// OK is true only when the response states every must_state fact and nothing the directive
	// and receipt do not carry. A zero Result is not OK.
	OK         bool
	Violations []Violation
}

// Has reports whether the result contains a violation of the given kind.
func (r Result) Has(kind ViolationKind) bool {
	for _, v := range r.Violations {
		if v.Kind == kind {
			return true
		}
	}
	return false
}

// MaxTextBytes bounds the response text the verifier accepts. A longer text fails closed.
const MaxTextBytes = 16 << 10

// maxViolations bounds the violations reported for one response.
const maxViolations = 32
