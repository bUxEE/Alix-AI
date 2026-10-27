package capabilities

import (
	"sort"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/policy"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Package is the view of one application package that the registry compiles and
// the router decides against. policy.ActivePackage is what the gateway needs;
// the other methods are the typed contract the router reads keywords from.
//
// An *apppkg.Package satisfies it through FromApppkg. The interface exists so
// that the registry never depends on how a package was loaded, and so that tests
// can build an active package without a signed archive.
//
// Every method must be safe for concurrent use and must not change what it
// returns: the registry compiles the package once, when it is activated.
type Package interface {
	policy.ActivePackage

	// ContentHash is the content address of the package (the manifest hash of
	// apppkg). Two packages with the same hash are the same bytes.
	ContentHash() string
	// SupportedLanguages are the manifest's supported language tags.
	SupportedLanguages() []string
	// Spec returns the full typed contract of a capability.
	Spec(id string) (ap.Capability, bool)
	// ContractFor returns the conversation contract of a capability in a
	// language.
	ContractFor(capability, language string) (ap.ConversationContract, bool)
	// Terminology returns every term of the package, in term ID order.
	Terminology() []ap.TerminologyTerm
}

// appPackage adapts *apppkg.Package, whose terms and languages are fields and
// whose hash is a field, to Package.
type appPackage struct{ *apppkg.Package }

// FromApppkg wraps a loaded package as a Package. It adds nothing to what the
// package says about itself, including its activation state: a package fresh
// from the loader reports quarantined and the registry will refuse it.
func FromApppkg(p *apppkg.Package) Package {
	if p == nil {
		return nil
	}
	return appPackage{p}
}

// ContentHash implements Package.
func (a appPackage) ContentHash() string { return a.Package.ManifestHash }

// SupportedLanguages implements Package.
func (a appPackage) SupportedLanguages() []string {
	out := make([]string, 0, len(a.Package.Manifest.Languages.Supported))
	for _, l := range a.Package.Manifest.Languages.Supported {
		out = append(out, string(l))
	}
	return out
}

// Terminology implements Package.
func (a appPackage) Terminology() []ap.TerminologyTerm {
	ids := make([]string, 0, len(a.Package.Terms))
	for id := range a.Package.Terms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ap.TerminologyTerm, 0, len(ids))
	for _, id := range ids {
		out = append(out, a.Package.Terms[id])
	}
	return out
}

// Assistant implements Package.
func (a appPackage) Assistant() *ap.ManifestAssistant {
	if a.Package == nil {
		return nil
	}
	return a.Package.Manifest.Assistant
}

var _ Package = appPackage{}
