package triggers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/policy"
)

// The kinds of change this package adds to the integrationaudit.TriggerKind
// values the report already records. The report hashes the permission model and
// the source definitions whole; the watcher also needs the two things that call
// for a stop and that a whole-value hash cannot tell from a rewording (the
// scope a source or capability is bound to, the key that signed the package), and
// two that come from outside the package (the pinned schema of an MCP tool, the
// absence of a report to compare with).
const (
	// KindSignature: the package no longer verifies, or another publisher key
	// signed it.
	KindSignature integrationaudit.TriggerKind = "signature"
	// KindScope: a tenancy, capability or source scope declaration changed.
	KindScope integrationaudit.TriggerKind = "scope"
	// KindMCPSchema: an MCP tool differs from the schema pinned at audit time.
	KindMCPSchema integrationaudit.TriggerKind = "mcp_schema"
	// KindBaseline: there is no verifiable audit report of the active package to
	// compare with, so nothing can be said to be unchanged.
	KindBaseline integrationaudit.TriggerKind = "baseline"
	// KindAuditBlocked: a re-audit of the active package came back blocked, so
	// the report that cleared it no longer holds.
	KindAuditBlocked integrationaudit.TriggerKind = "audit_blocked"
	// KindQuarantined: a package the watcher quarantined is audited again so that
	// a report postdates the quarantine.
	KindQuarantined integrationaudit.TriggerKind = "quarantined"
)

// Change is one detected difference. Critical is decided in one place,
// criticalKind, from the kind alone: nothing a package, an adapter or a model
// says about a change can make it less critical.
type Change struct {
	Kind    integrationaudit.TriggerKind `json:"kind"`
	Subject string                       `json:"subject"`
	// Package is the manifest hash of the package the change is about.
	Package  string `json:"package,omitempty"`
	Critical bool   `json:"critical"`
	// Detail says in a few words what happened ("changed", "added", "removed" or
	// a short reason). It never carries text the application or a package
	// chose.
	Detail string `json:"detail"`
}

// criticalKind reports whether a change of this kind stops live traffic before
// anything is audited (master plan 4.5, 9.4; SEC-049, SEC-062): the signature,
// the scope, the permission model, a pinned MCP schema and a re-audit that came
// back blocked. Every other kind
// calls for a re-audit while the previous activation stays in force.
func criticalKind(k integrationaudit.TriggerKind) bool {
	switch k {
	case KindSignature, KindScope, KindMCPSchema, KindAuditBlocked, integrationaudit.TriggerPermissionModel:
		return true
	}
	return false
}

func newChange(k integrationaudit.TriggerKind, subject, detail string) Change {
	return Change{Kind: k, Subject: subject, Critical: criticalKind(k), Detail: detail}
}

// anyCritical reports whether any change is critical.
func anyCritical(cs []Change) bool {
	for _, c := range cs {
		if c.Critical {
			return true
		}
	}
	return false
}

func sortChanges(cs []Change) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Kind != cs[j].Kind {
			return cs[i].Kind < cs[j].Kind
		}
		return cs[i].Subject < cs[j].Subject
	})
}

// facet is one watched value of a package.
type facet struct {
	kind    integrationaudit.TriggerKind
	subject string
	hash    string
}

func (f facet) key() string { return string(f.kind) + "\x00" + f.subject }

// digest is the SHA-256 of the JSON encoding of v. encoding/json is
// deterministic for the typed values hashed here (struct fields in order, map
// keys sorted). A value that cannot be encoded is an error, never a constant:
// two unencodable values must not compare equal.
func digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// facetsOf lists what is watched in an installed package. Two packages are
// compared facet by facet, both opened through apppkg.Installer.Open, so both
// are verified against the trust store and their recorded hash.
func facetsOf(pkg *apppkg.Package, rec apppkg.Record) ([]facet, error) {
	var out []facet
	var errs []error
	add := func(k integrationaudit.TriggerKind, subject string, v any) {
		h, err := digest(v)
		if err != nil {
			errs = append(errs, err)
			return
		}
		out = append(out, facet{k, subject, h})
	}
	add(integrationaudit.TriggerPackage, rec.ApplicationID, rec.ManifestHash)
	add(KindSignature, "publisher_key", rec.SignerKeyID)
	add(integrationaudit.TriggerDocumentation, "documentation/index.yaml", pkg.Documentation)
	add(integrationaudit.TriggerApplicationVersion, "application", pkg.Manifest.Application.Version)
	for _, id := range pkg.SortedCapabilityIDs() {
		add(integrationaudit.TriggerCapability, id, pkg.Capabilities[id])
	}
	for k, c := range pkg.Contracts {
		add(integrationaudit.TriggerContract, k.Capability+"/"+k.Language, c)
	}
	for id, s := range pkg.Sources {
		add(integrationaudit.TriggerSource, id, s)
	}
	add(integrationaudit.TriggerPermissionModel, "permissions", permissionModel(pkg))
	add(KindScope, "scope", scopeModel(pkg))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// permissionModel is what decides who may do what: the role IDs the application
// asserts, the host policies (grants, risk overrides, field projection, rules)
// and the permissions and audience of every capability. Wording is left out
// (descriptions and reasons), so rewording a policy is a documentation change
// and not a reason to stop traffic.
func permissionModel(pkg *apppkg.Package) any {
	roles := make([]string, 0, len(pkg.Manifest.Tenancy.Roles))
	for r := range pkg.Manifest.Tenancy.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)

	pols := make([]policy.HostPolicy, 0, len(pkg.Policies))
	for _, p := range pkg.Policies {
		if p == nil {
			continue
		}
		c := *p
		c.Description = ""
		c.RiskOverrides = append([]policy.RiskOverride(nil), p.RiskOverrides...)
		for i := range c.RiskOverrides {
			c.RiskOverrides[i].Reason = ""
		}
		c.Rules = append([]policy.Rule(nil), p.Rules...)
		for i := range c.Rules {
			c.Rules[i].Reason = ""
		}
		pols = append(pols, c)
	}
	sort.SliceStable(pols, func(i, j int) bool { return pols[i].PolicyID < pols[j].PolicyID })

	type capPerm struct {
		Audience    string
		Permissions []string
	}
	caps := map[string]capPerm{}
	for id, c := range pkg.Capabilities {
		ps := make([]string, 0, len(c.Permissions))
		for _, p := range c.Permissions {
			ps = append(ps, string(p))
		}
		sort.Strings(ps)
		caps[id] = capPerm{string(c.Audience), ps}
	}
	return struct {
		Roles        []string
		Policies     []policy.HostPolicy
		Capabilities map[string]capPerm
	}{roles, pols, caps}
}

// scopeModel is every declaration that says whose data a capability or a source
// may reach: the principal kinds, the scope level of each capability and the
// scope and query scope of each RAG source.
func scopeModel(pkg *apppkg.Package) any {
	kinds := make([]string, 0, len(pkg.Manifest.Tenancy.PrincipalKinds))
	for _, k := range pkg.Manifest.Tenancy.PrincipalKinds {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	levels := map[string]string{}
	for id, c := range pkg.Capabilities {
		levels[id] = string(c.ScopeLevel)
	}
	type srcScope struct {
		Scope any
		Query any
	}
	srcs := map[string]srcScope{}
	for id, s := range pkg.Sources {
		srcs[id] = srcScope{s.Scope, s.QueryScope}
	}
	return struct {
		PrincipalKinds []string
		ScopeLevels    map[string]string
		Sources        map[string]srcScope
	}{kinds, levels, srcs}
}

// diffPackages lists what differs between the package that is live and another
// installed package of the same application. A different manifest hash is
// always a change (the package itself); the rest says what moved.
func diffPackages(live, other []facet) []Change {
	idx := func(fs []facet) map[string]facet {
		m := make(map[string]facet, len(fs))
		for _, f := range fs {
			m[f.key()] = f
		}
		return m
	}
	a, b := idx(live), idx(other)
	var out []Change
	for k, f := range b {
		switch o, ok := a[k]; {
		case !ok:
			out = append(out, newChange(f.kind, f.subject, "added"))
		case o.hash != f.hash:
			out = append(out, newChange(f.kind, f.subject, "changed"))
		}
	}
	for k, f := range a {
		if _, ok := b[k]; !ok {
			out = append(out, newChange(f.kind, f.subject, "removed"))
		}
	}
	sortChanges(out)
	return out
}

// SurfaceHash is the digest the integration audit records as the adapter_surface
// re-audit trigger of a report for what an adapter described: the transport, the
// contract versions, the operations of each capability and the unmapped host
// operations, in a fixed order, so the hash does not move when the adapter lists
// the same operations differently. It must stay byte-for-byte equal to the
// report's (TestSurfaceHashMatchesTheReport holds it to a real report).
func SurfaceHash(d adapters.Description) string {
	type capOps struct {
		ID  string
		Ops []string
	}
	var caps []capOps
	for _, c := range d.Capabilities {
		var ops []string
		for _, o := range c.Operations {
			ops = append(ops, string(o))
		}
		sort.Strings(ops)
		caps = append(caps, capOps{c.CapabilityID, ops})
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].ID < caps[j].ID })
	unmapped := []string{}
	seen := map[string]bool{}
	for _, s := range d.Unmapped {
		if !seen[s] {
			seen[s] = true
			unmapped = append(unmapped, s)
		}
	}
	sort.Strings(unmapped)
	h, err := digest(struct {
		Transport string
		Versions  []int
		Caps      []capOps
		Unmapped  []string
	}{d.Transport, append([]int(nil), d.ContractVersions...), caps, unmapped})
	if err != nil {
		// Unreachable for these types; the audit uses a constant here, and a
		// constant that no description hashes to cannot match a baseline.
		return "unencodable"
	}
	return h
}
