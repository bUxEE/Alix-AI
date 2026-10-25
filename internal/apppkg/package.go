package apppkg

import (
	"sort"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/trust"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// ContractKey identifies a conversation contract: one per capability and
// language.
type ContractKey struct {
	Capability string
	Language   string
}

// Package is a loaded, validated Application Integration Package. It is built
// only from a package with no critical finding, from a Snapshot, and never
// changes: the maps and slices belong to the Package and callers must treat
// them as read-only.
//
// Its activation state is whatever the policy gateway sees through
// policy.ActivePackage: a Package that Load returns is quarantined, and
// nothing in this package can make it active. Only the integration audit
// (M1-17) moves a stored package to active, through its own transition of the
// installed record; Installer.Open then reports the stored state.
type Package struct {
	// Manifest is alixai.yaml.
	Manifest ap.Manifest
	// Capabilities holds every capability by ID.
	Capabilities map[string]ap.Capability
	// Contracts holds the conversation contracts by capability and language.
	Contracts map[ContractKey]ap.ConversationContract
	// Policies are the parsed policies/*.yaml documents, in file order.
	Policies []*policy.HostPolicy
	// Terms holds the terminology by term ID.
	Terms map[string]ap.TerminologyTerm
	// Documentation is documentation/index.yaml.
	Documentation ap.DocumentationIndex
	// Sources holds the RAG source declarations by ID.
	Sources map[string]ap.RAGSource
	// Tests are the test suites, in file order.
	Tests []ap.PackageTests
	// Files lists every file of the package, sorted.
	Files []string

	// ManifestHash is the content address of the package: the SHA-256 of the
	// canonical signed integrity manifest. Empty only for a package that was
	// not checked against one.
	ManifestHash string
	// Authenticity says how far the signature was checked when it was loaded.
	Authenticity Authenticity
	// Verification is the signature that authenticated the package, when it
	// was verified against a trust anchor.
	Verification *trust.Accepted

	gate *policy.Package
}

var _ policy.ActivePackage = (*Package)(nil)

// ApplicationID implements policy.ActivePackage.
func (p *Package) ApplicationID() string { return p.gate.ApplicationID() }

// PackageVersion implements policy.ActivePackage.
func (p *Package) PackageVersion() string { return p.gate.PackageVersion() }

// Status implements policy.ActivePackage: policy.StatusQuarantined for a
// package fresh from Load, the stored state for one opened from an Installer.
func (p *Package) Status() policy.PackageStatus { return p.gate.Status() }

// Roles implements policy.ActivePackage.
func (p *Package) Roles() []string { return p.gate.Roles() }

// Capability implements policy.ActivePackage: the part of a capability the
// policy gateway decides on.
func (p *Package) Capability(id string) (policy.CapabilityDecl, bool) { return p.gate.Capability(id) }

// CapabilityIDs implements policy.ActivePackage.
func (p *Package) CapabilityIDs() []string { return p.gate.CapabilityIDs() }

// Policy implements policy.ActivePackage: the compiled host policy.
func (p *Package) Policy() *policy.Policy { return p.gate.Policy() }

// Spec returns the full typed contract of a capability.
func (p *Package) Spec(id string) (ap.Capability, bool) {
	c, ok := p.Capabilities[id]
	return c, ok
}

// ContractFor returns the conversation contract of a capability in a language.
func (p *Package) ContractFor(capability, language string) (ap.ConversationContract, bool) {
	c, ok := p.Contracts[ContractKey{Capability: capability, Language: language}]
	return c, ok
}

// SecurityDomain is the optional security domain label of the manifest
// (ADR-0016), or empty.
func (p *Package) SecurityDomain() string {
	if p.Manifest.SecurityDomain == nil {
		return ""
	}
	return string(*p.Manifest.SecurityDomain)
}

// SortedCapabilityIDs lists capability IDs in order.
func (p *Package) SortedCapabilityIDs() []string {
	ids := make([]string, 0, len(p.Capabilities))
	for id := range p.Capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// withStatus returns a copy whose policy view is in another state. It is
// unexported: only the installer re-reading a stored record uses it, to report
// the state the record holds, never to choose one.
func (p *Package) withStatus(s policy.PackageStatus) *Package {
	cp := *p
	cp.gate = p.gate.WithStatus(s)
	return &cp
}
