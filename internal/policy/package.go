package policy

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// PackageStatus is the activation state of an installed application package.
// Only StatusActive exposes anything. Every other value, including the empty
// one and values this build does not know, is treated as not active.
type PackageStatus string

// Statuses the loader and the integration audit use. Any other string behaves
// like StatusQuarantined.
const (
	StatusActive      PackageStatus = "active"
	StatusQuarantined PackageStatus = "quarantined"
)

// EffectClass is what a capability does to the world, declared by the
// application; the platform risk floor is derived from it (master plan 11.4).
type EffectClass string

// Effect classes of schemas/application-package/capability.schema.json.
const (
	EffectNone       EffectClass = "none"
	EffectReversible EffectClass = "reversible"
	EffectExternal   EffectClass = "external"
	EffectPrivileged EffectClass = "privileged"
)

// Floor returns the platform risk floor of the effect class: none 0,
// reversible 1, external (a consequential change or an effect outside the
// application) 2, privileged (privileged, legally sensitive or irreversible) 3.
// An unrecognised class has floor 3 and known is false: a package that does not
// say what a capability does is treated as the most dangerous kind.
func (e EffectClass) Floor() (floor contracts.RiskLevel, known bool) {
	switch e {
	case EffectNone:
		return contracts.RiskRead, true
	case EffectReversible:
		return contracts.RiskReversible, true
	case EffectExternal:
		return contracts.RiskConsequential, true
	case EffectPrivileged:
		return contracts.RiskPrivileged, true
	}
	return contracts.RiskPrivileged, false
}

// Audience says who a capability is for.
type Audience string

// Audiences of schemas/application-package/capability.schema.json.
const (
	AudienceUser  Audience = "user"
	AudienceAdmin Audience = "admin"
)

// ConfirmationMode is the confirmation rule a capability declares.
type ConfirmationMode string

// Confirmation modes of schemas/application-package/capability.schema.json.
const (
	ConfirmNone     ConfirmationMode = "none"
	ConfirmExplicit ConfirmationMode = "explicit"
	ConfirmStepUp   ConfirmationMode = "step_up"
)

// FieldDecl is the part of a capability field the gateway needs.
type FieldDecl struct {
	Name      string
	Required  bool
	Sensitive bool
}

// CapabilityDecl is the part of a declared capability the gateway needs. It is
// a deliberately small local model of schemas/application-package/capability.
// ParseCapabilityDecl reads it from a capability document; a typed package
// from the loader can fill it directly.
type CapabilityDecl struct {
	ID           string
	Audience     Audience
	EffectClass  EffectClass
	RiskLevel    contracts.RiskLevel
	Confirmation ConfirmationMode
	Fields       []FieldDecl
}

// validate checks the declaration the way the package schema does. A package
// that was validated by the loader always passes; the check exists so that a
// hand-built or corrupted declaration is denied instead of trusted.
func (c CapabilityDecl) validate() error {
	if !validCapabilityID(c.ID) {
		return contracts.New(contracts.ErrSchemaInvalid, "capability ID is not valid")
	}
	if _, known := c.EffectClass.Floor(); !known {
		return contracts.New(contracts.ErrSchemaInvalid, "capability effect class is not none, reversible, external or privileged")
	}
	if c.Audience != AudienceUser && c.Audience != AudienceAdmin {
		return contracts.New(contracts.ErrSchemaInvalid, "capability audience is not user or admin")
	}
	if !c.RiskLevel.Valid() {
		return contracts.New(contracts.ErrSchemaInvalid, "capability risk level is not 0 to 3")
	}
	switch c.Confirmation {
	case ConfirmNone, ConfirmExplicit, ConfirmStepUp:
	default:
		return contracts.New(contracts.ErrSchemaInvalid, "capability confirmation mode is not none, explicit or step_up")
	}
	seen := make(map[string]bool, len(c.Fields))
	for _, f := range c.Fields {
		if !fieldNameRE.MatchString(f.Name) || seen[f.Name] {
			return contracts.New(contracts.ErrSchemaInvalid, "capability field names must be unique lower snake_case names")
		}
		seen[f.Name] = true
	}
	return nil
}

// ParseCapabilityDecl reads the fields the gateway needs from the JSON form of
// a capability document (the parsed capabilities/*.yaml). It does not validate
// the document against the package schema, which is the loader's job; it does
// refuse a document that lacks one of the fields the gateway decides on, or
// holds a value it does not recognise, because a missing risk level or effect
// class must never be read as the lowest one.
func ParseCapabilityDecl(data []byte) (CapabilityDecl, error) {
	var raw struct {
		ID           *string              `json:"id"`
		Audience     *string              `json:"audience"`
		EffectClass  *string              `json:"effect_class"`
		RiskLevel    *contracts.RiskLevel `json:"risk_level"`
		Confirmation *struct {
			Mode *string `json:"mode"`
		} `json:"confirmation"`
		Fields []struct {
			Name      *string `json:"name"`
			Required  *bool   `json:"required"`
			Sensitive bool    `json:"sensitive"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return CapabilityDecl{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "capability document is not valid JSON")
	}
	if raw.ID == nil || raw.Audience == nil || raw.EffectClass == nil || raw.RiskLevel == nil ||
		raw.Confirmation == nil || raw.Confirmation.Mode == nil {
		return CapabilityDecl{}, contracts.New(contracts.ErrSchemaInvalid,
			"capability document lacks id, audience, effect_class, risk_level or confirmation.mode")
	}
	c := CapabilityDecl{
		ID:           *raw.ID,
		Audience:     Audience(*raw.Audience),
		EffectClass:  EffectClass(*raw.EffectClass),
		RiskLevel:    *raw.RiskLevel,
		Confirmation: ConfirmationMode(*raw.Confirmation.Mode),
	}
	if _, known := c.EffectClass.Floor(); !known {
		return CapabilityDecl{}, contracts.New(contracts.ErrSchemaInvalid, "capability effect_class is not none, reversible, external or privileged")
	}
	for _, f := range raw.Fields {
		if f.Name == nil || f.Required == nil {
			return CapabilityDecl{}, contracts.New(contracts.ErrSchemaInvalid, "capability field lacks name or required")
		}
		c.Fields = append(c.Fields, FieldDecl{Name: *f.Name, Required: *f.Required, Sensitive: f.Sensitive})
	}
	if err := c.validate(); err != nil {
		return CapabilityDecl{}, err
	}
	return c, nil
}

// ActivePackage is the narrow view of an installed application package that the
// gateway decides against. The package loader and the capability registry
// satisfy it with their typed package; Package is the in-memory implementation
// used by tests and by callers that have no typed package yet.
//
// Every method must be safe for concurrent use and must not change what it
// returns for the life of a decision.
type ActivePackage interface {
	// ApplicationID is the application the package belongs to.
	ApplicationID() string
	// PackageVersion is the version of the package, for the audit record.
	PackageVersion() string
	// Status is the activation state. Only StatusActive exposes anything.
	Status() PackageStatus
	// Roles are the roles the package declares in tenancy.roles.
	Roles() []string
	// Capability returns the declaration of one capability.
	Capability(id string) (CapabilityDecl, bool)
	// CapabilityIDs lists every declared capability ID, sorted.
	CapabilityIDs() []string
	// Policy returns the compiled host policy, or nil when the package has none
	// (nothing is then granted).
	Policy() *Policy
}

// PackageSpec describes a package for NewPackage.
type PackageSpec struct {
	ApplicationID string
	Version       string
	Status        PackageStatus
	Roles         []string
	Capabilities  []CapabilityDecl
	Policies      []*HostPolicy
}

// Package is an immutable in-memory ActivePackage.
type Package struct {
	applicationID string
	version       string
	status        PackageStatus
	roles         []string
	roleSet       map[string]bool
	caps          map[string]CapabilityDecl
	capIDs        []string
	policy        *Policy
}

// NewPackage builds a package view and compiles its host policies. It performs
// the policy checks of the package loader (master plan 9.4, package doc 14.13)
// again at the point where a package becomes something decisions are made
// against, so a package whose policy contradicts itself or names something it
// does not declare can never be active: every role is declared in the roles,
// every capability is declared, every projected or hidden field exists, no role
// is granted twice, no capability has two risk overrides, no two projections
// overlap and rule IDs are unique.
func NewPackage(spec PackageSpec) (*Package, error) {
	if !validIdentifier(spec.ApplicationID) {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "package application ID is not a valid identifier")
	}
	p := &Package{
		applicationID: spec.ApplicationID,
		version:       spec.Version,
		status:        spec.Status,
		roles:         append([]string(nil), spec.Roles...),
		roleSet:       make(map[string]bool, len(spec.Roles)),
		caps:          make(map[string]CapabilityDecl, len(spec.Capabilities)),
	}
	sort.Strings(p.roles)
	for _, r := range spec.Roles {
		if !validIdentifier(r) || p.roleSet[r] {
			return nil, contracts.New(contracts.ErrSchemaInvalid, "package roles must be unique identifiers")
		}
		p.roleSet[r] = true
	}
	for _, c := range spec.Capabilities {
		if err := c.validate(); err != nil {
			return nil, err
		}
		if _, dup := p.caps[c.ID]; dup {
			return nil, contracts.New(contracts.ErrSchemaInvalid, "package declares a capability twice").WithDetails("capability_id", c.ID)
		}
		c.Fields = append([]FieldDecl(nil), c.Fields...)
		p.caps[c.ID] = c
		p.capIDs = append(p.capIDs, c.ID)
	}
	sort.Strings(p.capIDs)
	if len(spec.Policies) > 0 {
		pol, err := CompilePolicies(spec.Policies...)
		if err != nil {
			return nil, err
		}
		if err := pol.checkAgainst(p); err != nil {
			return nil, err
		}
		p.policy = pol
	}
	return p, nil
}

// ApplicationID implements ActivePackage. A nil *Package yields "".
func (p *Package) ApplicationID() string {
	if p == nil {
		return ""
	}
	return p.applicationID
}

// PackageVersion implements ActivePackage.
func (p *Package) PackageVersion() string {
	if p == nil {
		return ""
	}
	return p.version
}

// Status implements ActivePackage. A nil *Package is not active.
func (p *Package) Status() PackageStatus {
	if p == nil {
		return ""
	}
	return p.status
}

// Roles implements ActivePackage.
func (p *Package) Roles() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.roles...)
}

// Capability implements ActivePackage.
func (p *Package) Capability(id string) (CapabilityDecl, bool) {
	if p == nil {
		return CapabilityDecl{}, false
	}
	c, ok := p.caps[id]
	if !ok {
		return CapabilityDecl{}, false
	}
	c.Fields = append([]FieldDecl(nil), c.Fields...)
	return c, true
}

// CapabilityIDs implements ActivePackage.
func (p *Package) CapabilityIDs() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.capIDs...)
}

// Policy implements ActivePackage.
func (p *Package) Policy() *Policy {
	if p == nil {
		return nil
	}
	return p.policy
}

// WithStatus returns a copy of the package in another activation state, which
// is how tests (and a loader that re-reads state) move a package between
// quarantined and active without rebuilding it.
func (p *Package) WithStatus(s PackageStatus) *Package {
	if p == nil {
		return nil
	}
	cp := *p
	cp.status = s
	return &cp
}

var _ ActivePackage = (*Package)(nil)

// checkAgainst verifies the compiled policy only refers to what the package
// declares.
func (pol *Policy) checkAgainst(p *Package) error {
	for capID, roles := range pol.grants {
		c, ok := p.caps[capID]
		if !ok {
			return refErr("role_grants", capID, "capability is not declared by the package")
		}
		_ = c
		for role := range roles {
			if !p.roleSet[role] {
				return refErr("role_grants", role, "role is not declared in tenancy.roles")
			}
		}
	}
	for capID := range pol.overrides {
		if _, ok := p.caps[capID]; !ok {
			return refErr("risk_overrides", capID, "capability is not declared by the package")
		}
	}
	for capID, projs := range pol.projections {
		c, ok := p.caps[capID]
		if !ok {
			return refErr("field_projection", capID, "capability is not declared by the package")
		}
		for _, pr := range projs {
			for role := range pr.roles {
				if !p.roleSet[role] {
					return refErr("field_projection", role, "role is not declared in tenancy.roles")
				}
			}
			if err := checkFieldsDeclared(c, pr.fields, "field_projection"); err != nil {
				return err
			}
		}
	}
	for capID, rules := range pol.rules {
		c, ok := p.caps[capID]
		if !ok {
			return refErr("rules", capID, "capability is not declared by the package")
		}
		for _, r := range rules {
			if err := checkFieldsDeclared(c, r.hide, "rules"); err != nil {
				return err
			}
			for name := range r.rule.When.Fields {
				if err := checkFieldsDeclared(c, map[string]bool{name: true}, "rules"); err != nil {
					return err
				}
			}
			if r.rule.When.Roles != nil {
				for _, set := range [][]string{r.rule.When.Roles.Any, r.rule.When.Roles.All, r.rule.When.Roles.None} {
					for _, role := range set {
						if !p.roleSet[role] {
							return refErr("rules", role, "role is not declared in tenancy.roles")
						}
					}
				}
			}
		}
	}
	return nil
}

func checkFieldsDeclared(c CapabilityDecl, names map[string]bool, where string) error {
	declared := make(map[string]bool, len(c.Fields))
	for _, f := range c.Fields {
		declared[f.Name] = true
	}
	for _, n := range sortedKeys(names) {
		if !declared[n] {
			return refErr(where, n, fmt.Sprintf("field is not declared by capability %s", c.ID))
		}
	}
	return nil
}

func refErr(where, what, why string) error {
	return contracts.New(contracts.ErrSchemaInvalid, fmt.Sprintf("policy %s: %s: %s", where, what, why)).
		WithDetails("section", where)
}
