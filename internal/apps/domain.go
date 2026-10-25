// Package apps is where one process decides which applications may share it (ADR 0016).
//
// # The rule
//
// Several applications may be active in one process only within one security domain. The domain is
// the optional `security_domain` slug of the application manifest, recorded when the package is
// installed. A process refuses to install, and refuses to activate, an application whose domain
// differs from the domain of the applications already active in it. Two readings are fixed here,
// because the manifest schema leaves them open:
//
//   - Only OTHER applications count. A new version of an application, which replaces its own
//     previous version when it is activated, never conflicts with itself.
//   - Equality is of the stored text. An application that declares no domain has the domain "", which
//     equals only "": undeclared applications share a process with each other (as they always did)
//     and with no named domain. A named domain is never implied for one that declares none, and the
//     refusal for either direction names both sides.
//
// # Where it is enforced
//
// Two layers, so that neither can be skipped:
//
//   - The store (migrations/0001_security_domain.sql): two triggers on apppkg_packages that run
//     inside the transaction which inserts or activates a package. They are the authority: an
//     installer, an audit runner or a caller written tomorrow cannot get past them, and a concurrent
//     activation of another application cannot slip between a check and a change.
//   - Guard: reads what the triggers read and gives the refusal a type (code conflict, details
//     reason security_domain_mismatch) and an audit event (package.domain_refused) on the refused
//     application's chain. AsRefusal gives the same type to the error a trigger raised, for a caller
//     that only has the store's error.
//
// Applications of one domain still share nothing: this package reads one column of the package table
// and writes only the refusal's event on the refused application's own chain.
package apps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// ReasonSecurityDomain is the Details["reason"] of every refusal of the rule.
const ReasonSecurityDomain = "security_domain_mismatch"

// ErrSecurityDomain is the cause of every refusal of the rule (use errors.Is).
var ErrSecurityDomain = errors.New("the security domain differs from the active applications of this process")

// Phase is the step of an application's life the rule refused.
type Phase string

// The two steps the rule applies to.
const (
	PhaseInstall    Phase = "install"
	PhaseActivation Phase = "activation"
)

// Valid reports whether p is one of the two phases.
func (p Phase) Valid() bool { return p == PhaseInstall || p == PhaseActivation }

// Candidate is the application that asks to be installed or activated.
type Candidate struct {
	ApplicationID string
	// SecurityDomain is the manifest's security domain, or "" when it declares none.
	SecurityDomain string
	// ManifestHash is the package, when known: it is only recorded in the audit event.
	ManifestHash string
	Phase        Phase
}

// Active is an application that is active in the process, with its security domain.
type Active struct {
	ApplicationID  string
	SecurityDomain string
}

// Conflict returns the first application (in the order given) that is active under another
// application id and has another security domain than c. It is the whole rule as a pure function:
// the migration's triggers say the same in SQL.
func Conflict(c Candidate, active []Active) (Active, bool) {
	for _, a := range active {
		if a.ApplicationID != c.ApplicationID && a.SecurityDomain != c.SecurityDomain {
			return a, true
		}
	}
	return Active{}, false
}

// describe words a domain for a message: "" is "no security domain".
func describe(domain string) string {
	if domain == "" {
		return "no security domain"
	}
	return fmt.Sprintf("security domain %q", domain)
}

// refusal builds the typed refusal for c against the active application a.
func refusal(c Candidate, a Active) *contracts.Error {
	return contracts.Wrap(contracts.ErrConflict, ErrSecurityDomain,
		fmt.Sprintf("apps: %s refused: application %q declares %s but application %q, active in this process, declares %s; one process serves one security domain, so deploy it separately or take the other applications out of service first",
			c.Phase, c.ApplicationID, describe(c.SecurityDomain), a.ApplicationID, describe(a.SecurityDomain))).
		WithDetails("reason", ReasonSecurityDomain).
		WithDetails("phase", string(c.Phase)).
		WithDetails("application_id", c.ApplicationID).
		WithDetails("security_domain", c.SecurityDomain).
		WithDetails("active_application_id", a.ApplicationID).
		WithDetails("active_security_domain", a.SecurityDomain)
}

// The text the migration's triggers raise, matched by AsRefusal.
const (
	triggerInstall    = ReasonSecurityDomain + "(install)"
	triggerActivation = ReasonSecurityDomain + "(activation)"
)

// AsRefusal returns the typed refusal err stands for, or false when err is not a refusal of the
// rule. It recognises the error Guard returns and, for a caller that only has the store's error, the
// abort one of the migration's triggers raised: that one carries no application names (SQLite's RAISE
// takes a literal), so it is typed without those details.
func AsRefusal(err error) (*contracts.Error, bool) {
	if err == nil {
		return nil, false
	}
	if ce := findRefusal(err); ce != nil {
		return ce, true
	}
	msg := err.Error()
	var phase Phase
	switch {
	case strings.Contains(msg, triggerInstall):
		phase = PhaseInstall
	case strings.Contains(msg, triggerActivation):
		phase = PhaseActivation
	default:
		return nil, false
	}
	return contracts.Wrap(contracts.ErrConflict, ErrSecurityDomain,
		fmt.Sprintf("apps: %s refused: the application declares a security domain that differs from the active applications of this process", phase)).
		WithDetails("reason", ReasonSecurityDomain).
		WithDetails("phase", string(phase)), true
}

// findRefusal walks err's tree for a typed refusal of the rule.
func findRefusal(err error) *contracts.Error {
	if ce, ok := err.(*contracts.Error); ok && ce != nil && ce.Details["reason"] == ReasonSecurityDomain {
		return ce
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if inner := u.Unwrap(); inner != nil {
			return findRefusal(inner)
		}
	case interface{ Unwrap() []error }:
		for _, inner := range u.Unwrap() {
			if ce := findRefusal(inner); ce != nil {
				return ce
			}
		}
	}
	return nil
}
