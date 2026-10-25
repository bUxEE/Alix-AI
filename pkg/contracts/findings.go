package contracts

import (
	"fmt"
	"sort"
)

// Severity ranks a Finding. Critical findings block activation of an
// application (master plan 4.5: "Critical failures block activation"); warnings
// may be accepted only by an explicitly authorized operator and never widen the
// model's tools or scope; info findings are facts worth recording.
type Severity string

// The three severities. An unknown value is never treated as harmless: Valid
// reports false and Rank sorts it with critical.
const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Valid reports whether s is one of the three defined severities.
func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// Rank orders severities, most severe first: critical 0, warning 1, info 2. An
// unrecognised severity ranks as critical so it is never hidden below a valid
// one.
func (s Severity) Rank() int {
	switch s {
	case SeverityWarning:
		return 1
	case SeverityInfo:
		return 2
	}
	return 0
}

// Finding is one result of a deterministic check of an application package or
// of an integrated application: the package loader (M1-04) produces them from
// the package alone and the integration audit (M1-17) adds the ones it finds
// against the running application. The shape is the same on purpose, so a
// remediation list can mix both.
//
// A Finding is a statement of fact written from the check's own inputs. It is
// never produced by a model, and its Message and Remediation are fixed wording
// plus identifiers, never text copied from the package under inspection beyond
// bounded, quoted names.
type Finding struct {
	// CheckID names the check, a stable dotted lower-case identifier such as
	// "docs.facet_missing". It is the key the audit report, the remediation
	// list and tests refer to; renaming one is an API change.
	CheckID string `json:"check_id"`
	// Severity is critical, warning or info.
	Severity Severity `json:"severity"`
	// Capability is the capability the finding concerns, or empty for a finding
	// about the package as a whole.
	Capability string `json:"capability,omitempty"`
	// Path is the package-relative file the finding was found in, when there
	// is one.
	Path string `json:"path,omitempty"`
	// Message says what is wrong, specifically enough to find it.
	Message string `json:"message"`
	// Remediation says what the package author or operator must change.
	Remediation string `json:"remediation"`
}

// Validate checks that a finding is complete: a check ID, a known severity, a
// message and a remediation. The loader and the audit refuse to emit one that
// is not.
func (f Finding) Validate() error {
	switch {
	case f.CheckID == "":
		return New(ErrValidationFailed, "finding has no check_id")
	case !f.Severity.Valid():
		return Newf(ErrValidationFailed, "finding %s has unknown severity %q", f.CheckID, string(f.Severity))
	case f.Message == "":
		return Newf(ErrValidationFailed, "finding %s has no message", f.CheckID)
	case f.Remediation == "":
		return Newf(ErrValidationFailed, "finding %s has no remediation", f.CheckID)
	}
	return nil
}

// String renders the finding on one line for logs and test failures.
func (f Finding) String() string {
	where := ""
	if f.Capability != "" {
		where += " capability=" + f.Capability
	}
	if f.Path != "" {
		where += " path=" + f.Path
	}
	return fmt.Sprintf("%s %s%s: %s", f.Severity, f.CheckID, where, f.Message)
}

// HasCritical reports whether any finding is critical, or has a severity that
// is not one of the defined three (an unknown severity blocks, never passes).
func HasCritical(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity.Rank() == 0 {
			return true
		}
	}
	return false
}

// CountSeverity returns how many findings are critical, warning and info. An
// unknown severity counts as critical.
func CountSeverity(fs []Finding) (critical, warning, info int) {
	for _, f := range fs {
		switch f.Severity.Rank() {
		case 0:
			critical++
		case 1:
			warning++
		default:
			info++
		}
	}
	return critical, warning, info
}

// SortFindings orders findings deterministically in place: most severe first,
// then by check ID, capability, path and message. The same inputs always
// produce the same report bytes.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := a.Severity.Rank(), b.Severity.Rank(); ra != rb {
			return ra < rb
		}
		if a.CheckID != b.CheckID {
			return a.CheckID < b.CheckID
		}
		if a.Capability != b.Capability {
			return a.Capability < b.Capability
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Message < b.Message
	})
}
