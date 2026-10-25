package apppkg

import (
	"fmt"
	"sort"

	"github.com/buxee/alix-ai/internal/policy"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// projectionSeen is a projection already met, kept to find overlaps.
type projectionSeen struct {
	roles map[string]bool // nil: applies to every role without a specific projection
	path  string
}

// checkPolicies implements section 14 item 13. Each policy file has already
// been parsed by policy.ParseHostPolicy, the grammar the runtime uses; this
// ties the documents to the package (roles, capabilities, fields) and reports
// conflicts one by one, so an author learns about every problem and not only
// the first one policy.NewPackage would refuse. NewPackage still runs after
// these checks, on the same parsed documents, so the loader cannot accept a
// package the runtime would refuse.
func (md *model) checkPolicies() {
	roles := md.m.Tenancy.Roles
	declared := func(capID string) bool { _, ok := md.m.Capabilities[capID]; return ok }
	known := func(capID string) (ap.Capability, bool) {
		e, ok := md.caps[capID]
		if !ok {
			return ap.Capability{}, false
		}
		return e.c, true
	}
	checkRole := func(path, where, role string) {
		if _, ok := roles[role]; !ok {
			md.c.add(chkPolicyRef, "", path, "%s names role %s, which tenancy.roles does not declare", where, q(role))
		}
	}
	checkCap := func(path, where, capID string) bool {
		if !declared(capID) {
			md.c.add(chkPolicyRef, capID, path, "%s names capability %s, which the manifest does not declare", where, q(capID))
			return false
		}
		return true
	}
	checkField := func(path, where, capID, field string) {
		c, ok := known(capID)
		if !ok {
			return
		}
		for _, f := range c.Fields {
			if string(f.Name) == field {
				return
			}
		}
		md.c.add(chkPolicyRef, capID, path, "%s names field %s, which capability %s does not declare", where, q(field), q(capID))
	}
	requiredFields := func(capID string) []string {
		c, ok := known(capID)
		if !ok {
			return nil
		}
		var out []string
		for _, f := range c.Fields {
			if f.Required {
				out = append(out, string(f.Name))
			}
		}
		return out
	}

	policyIDs := map[string]string{}
	ruleIDs := map[string]string{}
	grantedRoles := map[string]string{}
	overrides := map[string]string{}
	projections := map[string][]projectionSeen{}

	for _, pe := range md.policies {
		hp, path := pe.p, pe.path
		if prev, dup := policyIDs[hp.PolicyID]; dup {
			md.c.add(chkPolicyConflict, "", path, "policy_id %s is already used by %s", q(hp.PolicyID), q(prev))
		}
		policyIDs[hp.PolicyID] = path

		for i, g := range hp.RoleGrants {
			where := fmt.Sprintf("role_grants[%d]", i)
			checkRole(path, where, g.Role)
			if prev, dup := grantedRoles[g.Role]; dup {
				md.c.add(chkPolicyConflict, "", path, "role %s is granted more than once (first in %s)", q(g.Role), q(prev))
			}
			grantedRoles[g.Role] = path
			for _, capID := range g.Capabilities {
				checkCap(path, where, capID)
			}
		}

		for i, o := range hp.RiskOverrides {
			where := fmt.Sprintf("risk_overrides[%d]", i)
			if !checkCap(path, where, o.CapabilityID) {
				continue
			}
			if prev, dup := overrides[o.CapabilityID]; dup {
				md.c.add(chkPolicyConflict, o.CapabilityID, path, "capability %s has more than one risk override (first in %s)", q(o.CapabilityID), q(prev))
			}
			overrides[o.CapabilityID] = path
			if c, ok := known(o.CapabilityID); ok && int64(o.MinimumRiskLevel) <= int64(c.RiskLevel) {
				md.c.add(chkPolicyOverride, o.CapabilityID, path,
					"%s raises capability %s to at least %d, which is not higher than its declared risk level %d, so it changes nothing",
					where, q(o.CapabilityID), int(o.MinimumRiskLevel), int64(c.RiskLevel))
			}
		}

		for i, fp := range hp.FieldProjection {
			where := fmt.Sprintf("field_projection[%d]", i)
			if !checkCap(path, where, fp.CapabilityID) {
				continue
			}
			var roleSet map[string]bool
			if fp.Roles != nil {
				roleSet = map[string]bool{}
				for _, r := range fp.Roles {
					checkRole(path, where, r)
					roleSet[r] = true
				}
			}
			for _, prev := range projections[fp.CapabilityID] {
				if projectionsOverlap(prev.roles, roleSet) {
					md.c.add(chkPolicyConflict, fp.CapabilityID, path, "capability %s has two projections that apply to the same role (the other is in %s)", q(fp.CapabilityID), q(prev.path))
					break
				}
			}
			projections[fp.CapabilityID] = append(projections[fp.CapabilityID], projectionSeen{roles: roleSet, path: path})

			visible := map[string]bool{}
			for _, f := range fp.ModelVisibleFields {
				checkField(path, where, fp.CapabilityID, string(f))
				visible[string(f)] = true
			}
			for _, req := range requiredFields(fp.CapabilityID) {
				if !visible[req] {
					md.c.add(chkPolicyHidden, fp.CapabilityID, path, "%s does not list required field %s of capability %s, so the model could never complete it", where, q(req), q(fp.CapabilityID))
				}
			}
		}

		for i, r := range hp.Rules {
			where := fmt.Sprintf("rules[%d]", i)
			if prev, dup := ruleIDs[r.ID]; dup {
				md.c.add(chkPolicyConflict, "", path, "rule id %s is already used in %s", q(r.ID), q(prev))
			}
			ruleIDs[r.ID] = path
			md.checkRule(path, where, r, checkRole, checkCap, checkField, requiredFields)
		}
	}
}

// checkRule checks one conditional rule against the package.
func (md *model) checkRule(
	path, where string,
	r policy.Rule,
	checkRole func(path, where, role string),
	checkCap func(path, where, capID string) bool,
	checkField func(path, where, capID, field string),
	requiredFields func(capID string) []string,
) {
	if sc := r.When.Roles; sc != nil {
		for _, set := range [][]string{sc.Any, sc.All, sc.None} {
			for _, role := range set {
				checkRole(path, where+".when.roles", role)
			}
		}
	}
	for _, capID := range r.Capabilities {
		if !checkCap(path, where, capID) {
			continue
		}
		for _, name := range sortedKeys(r.When.Fields) {
			checkField(path, where+".when.fields", capID, name)
		}
		for _, name := range r.Then.HideFields {
			checkField(path, where+".then.hide_fields", capID, name)
		}
		hidden := map[string]bool{}
		for _, name := range r.Then.HideFields {
			hidden[name] = true
		}
		for _, req := range requiredFields(capID) {
			if hidden[req] {
				md.c.add(chkPolicyRuleHides, capID, path, "%s (rule %s) hides required field %s of capability %s while it applies", where, q(r.ID), q(req), q(capID))
			}
		}
	}
}

// projectionsOverlap mirrors the runtime: two role-less projections overlap,
// a role-less and a role-specific one do not (the specific one wins), and two
// role-specific ones overlap when they share a role.
func projectionsOverlap(a, b map[string]bool) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	for r := range a {
		if b[r] {
			return true
		}
	}
	return false
}

// checkTerminology implements section 14 item 14.
func (md *model) checkTerminology() {
	entityTypes := map[string]bool{}
	for _, e := range md.caps {
		for t := range e.c.EntityTypes {
			entityTypes[t] = true
		}
	}
	ids := make([]string, 0, len(md.terms))
	for id := range md.terms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		te := md.terms[id]
		if te.t.EntityType != nil && !entityTypes[string(*te.t.EntityType)] {
			md.c.add(chkTermEntity, "", te.path, "term %s names entity type %s, which no capability declares", q(id), q(string(*te.t.EntityType)))
		}
		var missing []string
		for _, lang := range md.m.Languages.Supported {
			if _, ok := te.t.Labels[string(lang)]; !ok {
				missing = append(missing, string(lang))
			}
		}
		if len(missing) > 0 {
			md.c.add(chkTermLanguage, "", te.path, "term %s has no label in language %s", q(id), qs(missing))
		}
	}
}
