package contracts

import (
	"bytes"
	"fmt"
	"strconv"
)

// RiskLevel is the action safety level of master plan §11.4.
//
//	0  read-only, low sensitivity      execute after authorization
//	1  reversible low-impact change    concise summary; policy may allow immediate commit
//	2  consequential / external effect prepare, show exact effects, explicit confirmation
//	3  high-risk, privileged, legal    authenticated application UI or step-up; AI cannot complete alone
//
// The host application may raise a level; it cannot silently lower a
// platform-enforced minimum (see Escalate).
//
// NOTE: level 0 is a valid, low-risk value, so the zero RiskLevel is NOT a
// safe default for "unknown". Any object that carries a risk level must make
// the field required in its schema and must never decode a missing field into
// RiskRead. UnmarshalJSON rejects null and out-of-range values for that reason.
type RiskLevel uint8

const (
	RiskRead          RiskLevel = 0
	RiskReversible    RiskLevel = 1
	RiskConsequential RiskLevel = 2
	RiskPrivileged    RiskLevel = 3
)

// Valid reports whether r is within 0..3.
func (r RiskLevel) Valid() bool { return r <= RiskPrivileged }

// Validate returns a validation_failed *Error for an out-of-range level.
func (r RiskLevel) Validate() error {
	if r.Valid() {
		return nil
	}
	return New(ErrValidationFailed, fmt.Sprintf("risk level %d out of range 0..3", uint8(r))).
		WithDetails("risk_level", int(r))
}

// ParseRiskLevel converts an integer to a RiskLevel, rejecting anything
// outside 0..3.
func ParseRiskLevel(n int) (RiskLevel, error) {
	if n < 0 || n > int(RiskPrivileged) {
		return 0, New(ErrValidationFailed, fmt.Sprintf("risk level %d out of range 0..3", n)).
			WithDetails("risk_level", n)
	}
	return RiskLevel(n), nil
}

// String returns "risk_<n>" ("risk_invalid" when out of range).
func (r RiskLevel) String() string {
	if !r.Valid() {
		return "risk_invalid"
	}
	return "risk_" + strconv.Itoa(int(r))
}

// RequiresConfirmation reports whether the platform default demands an
// explicit user confirmation (level 2 and above). An invalid level requires
// everything (fail closed).
func (r RiskLevel) RequiresConfirmation() bool { return !r.Valid() || r >= RiskConsequential }

// RequiresStepUp reports whether the AI cannot complete the action alone
// (level 3). An invalid level requires step-up (fail closed).
func (r RiskLevel) RequiresStepUp() bool { return !r.Valid() || r >= RiskPrivileged }

// Escalate applies a host-supplied level on top of a platform minimum. The
// result is never lower than the platform minimum: the host may raise a level
// but cannot silently lower it. If either input is invalid the result is the
// highest level (fail closed).
func (r RiskLevel) Escalate(host RiskLevel) RiskLevel {
	if !r.Valid() || !host.Valid() {
		return RiskPrivileged
	}
	if host > r {
		return host
	}
	return r
}

// MarshalJSON encodes the level as a bare integer 0..3 and refuses to encode
// an out-of-range value.
func (r RiskLevel) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return []byte(strconv.Itoa(int(r))), nil
}

// UnmarshalJSON accepts only the JSON integers 0, 1, 2 and 3. null, strings,
// fractions, exponents, signs and out-of-range numbers are rejected.
func (r *RiskLevel) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "0":
		*r = RiskRead
	case "1":
		*r = RiskReversible
	case "2":
		*r = RiskConsequential
	case "3":
		*r = RiskPrivileged
	default:
		return New(ErrSchemaInvalid, "risk level must be one of the integers 0, 1, 2, 3")
	}
	return nil
}
