package policy

import (
	"sort"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// PolicyRef names one policy document in force by ID and canonical digest.
type PolicyRef struct {
	PolicyID string `json:"policy_id"`
	Digest   string `json:"digest"`
}

// Policy is the compiled, immutable union of a package's host policy documents
// (policies/*.yaml may hold several). Compiling is where ambiguity is refused:
// the gateway never has to pick between two statements about the same thing.
type Policy struct {
	refs        []PolicyRef
	grants      map[string]map[string]string // capability -> role -> policy that grants it
	overrides   map[string]override          // capability -> raise
	projections map[string][]projection      // capability -> projections
	rules       map[string][]*compiledRule   // capability -> rules, deterministic order
}

type override struct {
	level    contracts.RiskLevel
	policyID string
}

type projection struct {
	policyID string
	roles    map[string]bool // nil: applies to every role without a specific projection
	fields   map[string]bool
}

type compiledRule struct {
	rule     Rule
	policyID string
	hide     map[string]bool
}

// CompilePolicies validates and merges host policy documents.
//
// The merge is exactly what the package schema documents and nothing more
// permissive: a role granted in two role_grants entries, a capability with two
// risk overrides, two projections for one capability that overlap, a repeated
// policy ID or a repeated rule ID is a schema_invalid error, whichever document
// the repeats are in. Grants of different roles in different documents are
// simply both in force. Documents are ordered by policy ID so the result does
// not depend on file order.
func CompilePolicies(docs ...*HostPolicy) (*Policy, error) {
	if len(docs) == 0 {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "no policy documents to compile")
	}
	sorted := make([]*HostPolicy, 0, len(docs))
	for _, d := range docs {
		if d == nil {
			return nil, schemaErr("", "policy is nil")
		}
		if err := d.Validate(); err != nil {
			return nil, err
		}
		sorted = append(sorted, d)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].PolicyID < sorted[j].PolicyID })

	pol := &Policy{
		grants:      map[string]map[string]string{},
		overrides:   map[string]override{},
		projections: map[string][]projection{},
		rules:       map[string][]*compiledRule{},
	}
	policyIDs := map[string]bool{}
	grantedRoles := map[string]bool{}
	ruleIDs := map[string]bool{}

	for _, d := range sorted {
		if policyIDs[d.PolicyID] {
			return nil, conflictErr("policy_id", d.PolicyID, "two policy documents share this ID")
		}
		policyIDs[d.PolicyID] = true
		digest, err := d.Digest()
		if err != nil {
			return nil, err
		}
		pol.refs = append(pol.refs, PolicyRef{PolicyID: d.PolicyID, Digest: digest})

		for _, g := range d.RoleGrants {
			if grantedRoles[g.Role] {
				return nil, conflictErr("role_grants", g.Role, "role is granted in more than one role_grants entry")
			}
			grantedRoles[g.Role] = true
			for _, capID := range g.Capabilities {
				if pol.grants[capID] == nil {
					pol.grants[capID] = map[string]string{}
				}
				pol.grants[capID][g.Role] = d.PolicyID
			}
		}
		for _, o := range d.RiskOverrides {
			if _, dup := pol.overrides[o.CapabilityID]; dup {
				return nil, conflictErr("risk_overrides", o.CapabilityID, "capability has more than one risk override")
			}
			pol.overrides[o.CapabilityID] = override{level: o.MinimumRiskLevel, policyID: d.PolicyID}
		}
		for _, fp := range d.FieldProjection {
			np := projection{policyID: d.PolicyID, fields: toSet(fp.ModelVisibleFields)}
			if fp.Roles != nil {
				np.roles = toSet(fp.Roles)
			}
			for _, ex := range pol.projections[fp.CapabilityID] {
				if projectionsOverlap(ex, np) {
					return nil, conflictErr("field_projection", fp.CapabilityID, "two projections for the capability apply to the same role")
				}
			}
			pol.projections[fp.CapabilityID] = append(pol.projections[fp.CapabilityID], np)
		}
		for _, r := range d.Rules {
			if ruleIDs[r.ID] {
				return nil, conflictErr("rules", r.ID, "rule ID is used more than once")
			}
			ruleIDs[r.ID] = true
			cr := &compiledRule{rule: r, policyID: d.PolicyID, hide: toSet(r.Then.HideFields)}
			for _, capID := range r.Capabilities {
				pol.rules[capID] = append(pol.rules[capID], cr)
			}
		}
	}
	return pol, nil
}

func projectionsOverlap(a, b projection) bool {
	if a.roles == nil && b.roles == nil {
		return true
	}
	if a.roles == nil || b.roles == nil {
		return false
	}
	for r := range a.roles {
		if b.roles[r] {
			return true
		}
	}
	return false
}

func toSet(v []string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

func conflictErr(where, what, why string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "policy conflict in "+where+": "+what+": "+why).
		WithDetails("section", where)
}

// Refs returns the policies in force, ordered by policy ID.
func (p *Policy) Refs() []PolicyRef {
	if p == nil {
		return nil
	}
	return append([]PolicyRef(nil), p.refs...)
}

// grantedRoles returns role -> granting policy for a capability.
func (p *Policy) grantedRoles(capID string) map[string]string {
	if p == nil {
		return nil
	}
	return p.grants[capID]
}

func (p *Policy) overrideFor(capID string) (override, bool) {
	if p == nil {
		return override{}, false
	}
	o, ok := p.overrides[capID]
	return o, ok
}

// projectionFor returns the projection that applies to a role: the one that
// names the role, else the general one, else nil.
func (p *Policy) projectionFor(capID, role string) *projection {
	if p == nil {
		return nil
	}
	var general *projection
	list := p.projections[capID]
	for i := range list {
		pr := &list[i]
		if pr.roles == nil {
			general = pr
			continue
		}
		if pr.roles[role] {
			return pr
		}
	}
	return general
}

func (p *Policy) rulesFor(capID string) []*compiledRule {
	if p == nil {
		return nil
	}
	return p.rules[capID]
}
