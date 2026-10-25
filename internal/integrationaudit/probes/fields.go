package probes

import (
	"fmt"
	"sort"
	"strings"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// fieldsFor builds the request fields of a probe for a capability from what the
// package declares and the records the suite seeded. Nothing is invented that
// the package does not state:
//
//   - an entity_ref field takes the identifier of a seeded record: the probe's
//     target when its entity type is the field's, otherwise a record of that type
//     in the acting principal's own workspace, so the target is the only thing
//     that can be wrong. A probe that names no target of its own (a probe of a
//     result, a safety or a smoke path) acts on the target_record of the first
//     allow probe the suite declares for that principal and capability, when it is
//     a record of a type a required field takes (ownTarget): the record the suite
//     itself says the principal may act on, not the first record of the workspace,
//     which a principal with per-principal visibility may not see;
//   - a literal default is used, a default taken from the verified context is
//     filled from the principal, and a computed one ("today") is left out for the
//     application to apply;
//   - an enum field takes its first value;
//   - any other required field takes the value of a capability example
//     (examples[].expected_fields) or, for a boolean, an integer, a number or a
//     string, the smallest value the declared bounds allow;
//   - when the target record's type is no field's entity type, its identifier is
//     offered as the value of the first required string field: a search-like
//     capability finds a record by what it is asked for, and that is how a
//     cross-scope search is attempted;
//   - optional fields are left out, except one that a validation rule requires
//     one-of (oneOfRules): the rule names two or more optional fields and says at
//     least one of them must be given, so a request with none of them is refused
//     by the application for the very rule it documents. One field of the rule is
//     filled, the first whose value the package itself states (a default, an enum
//     value or an example); a value is not made up for it, because an invented
//     one (a string of the right length) would fail the field's own format rule.
//
// A required field that cannot be built this way, or a one-of rule none of whose
// fields has a stated value, makes the probe unverified: the error names the
// fields, so the remediation is exact (declare an example or a default).
func (w *world) fieldsFor(c ap.Capability, p *wPrincipal, target *wRecord) (map[string]any, error) {
	out := map[string]any{}
	var missing []string
	if target == nil {
		target = w.ownTarget(c, p)
	}
	targetUsed := target == nil
	for _, f := range c.Fields {
		name := string(f.Name)
		if f.Type == ap.CapabilityFieldTypeEntityRef {
			et := ""
			if f.EntityType != nil {
				et = string(*f.EntityType)
			}
			switch {
			case target != nil && target.entityType == et:
				out[name] = target.id
				targetUsed = true
			case f.Required:
				if own := w.recordsOf(et, p); len(own) > 0 {
					out[name] = own[0].id
				} else {
					missing = append(missing, name)
				}
			}
			continue
		}
		if !f.Required {
			continue
		}
		v, include, ok := w.literalFor(c, f, p)
		switch {
		case !ok:
			missing = append(missing, name)
		case include:
			out[name] = v
		}
	}
	if !targetUsed {
		// The target is of no entity type the capability takes: offer its
		// identifier to the first required string field.
		for _, f := range c.Fields {
			if f.Type == ap.CapabilityFieldTypeString && f.Required {
				out[string(f.Name)] = target.id
				missing = removeString(missing, string(f.Name))
				targetUsed = true
				break
			}
		}
	}
	if !targetUsed {
		return nil, fmt.Errorf("the capability takes no field a record of type %s can be offered to", quote(target.entityType))
	}
	var unstated []string
	for _, rule := range oneOfRules(c) {
		if rule.satisfiedBy(out) {
			continue
		}
		if name, v, ok := w.oneOfValue(c, rule, p); ok {
			out[name] = v
			continue
		}
		unstated = append(unstated, "rule "+quote(rule.id)+" (one of "+quoteAll(rule.fields)+")")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("no value can be built for the required field(s) %s (declare an example or a default, or seed a record of the field's entity type in the principal's workspace)", quoteAll(missing))
	}
	if len(unstated) > 0 {
		return nil, fmt.Errorf("a validation rule requires one of several optional fields and the package states no value for any of them: %s (declare an example or a default for one of the fields)", strings.Join(unstated, "; "))
	}
	return out, nil
}

// ownTarget returns the target_record of the first allow probe the suite declares for the
// capability and the principal (in suite order), or nil. It is a record the suite itself
// says the principal may act on. Only a record of the entity type of a required entity_ref
// field of the capability qualifies, so the request it is offered to is one the capability
// takes: anything else leaves the request to be built as before. Nothing is trusted about
// the record beyond what the suite declares: an allow probe that names a record the
// principal cannot see is still reported by the scope check, and a probe of a result that
// is built on it is refused and recorded as not probed, never moved to another record.
func (w *world) ownTarget(c ap.Capability, p *wPrincipal) *wRecord {
	for _, sp := range w.suite.ScopeProbes {
		if sp.Expect != ap.PackageTestsScopeProbeExpectAllow || sp.Operation == ap.PackageTestsScopeProbeOperationRetrieve ||
			sp.CapabilityID == nil || *sp.CapabilityID != c.ID || string(sp.Principal) != p.ref || sp.TargetRecord == nil {
			continue
		}
		rec, ok := w.records[string(*sp.TargetRecord)]
		if !ok || rec.id == "" {
			continue
		}
		for _, f := range c.Fields {
			if f.Required && f.Type == ap.CapabilityFieldTypeEntityRef && f.EntityType != nil && string(*f.EntityType) == rec.entityType {
				return rec
			}
		}
	}
	return nil
}

// oneOfRule is a validation rule that requires at least one of several optional
// fields to be given.
type oneOfRule struct {
	id     string
	fields []string
}

// satisfiedBy reports whether the request already carries one of the rule's fields.
func (r oneOfRule) satisfiedBy(fields map[string]any) bool {
	for _, f := range r.fields {
		if _, ok := fields[f]; ok {
			return true
		}
	}
	return false
}

// oneOfPhrases are what a rule's description says when it requires one of several
// fields (the package format has no typed form for it). They are matched in lower
// case, in English and in Italian, the two languages the reference packages ship.
var oneOfPhrases = []string{
	"at least one", "one or more", "one of the", "any of the", "any one of",
	"almeno uno", "almeno una", "uno tra", "una tra", "uno dei", "una delle",
}

// oneOfRules lists the validation rules of a capability that require one of
// several of its optional fields. A rule qualifies when it checks two or more
// fields, every one of them a declared, optional field, and its description says
// that at least one of them must be given. A rule over one field, over a required
// field or over a field the capability does not declare, and a rule whose
// description says nothing of the kind, are not one-of rules: they constrain a
// value the request has, they do not ask for one.
func oneOfRules(c ap.Capability) []oneOfRule {
	optional := map[string]bool{}
	for _, f := range c.Fields {
		optional[string(f.Name)] = !f.Required
	}
	var out []oneOfRule
	for _, r := range c.Validation {
		if len(r.Fields) < 2 {
			continue
		}
		desc := strings.ToLower(string(r.Description))
		says := false
		for _, ph := range oneOfPhrases {
			says = says || strings.Contains(desc, ph)
		}
		if !says {
			continue
		}
		rule := oneOfRule{id: string(r.ID)}
		all := true
		for _, name := range r.Fields {
			all = all && optional[string(name)]
			rule.fields = append(rule.fields, string(name))
		}
		if all {
			out = append(out, rule)
		}
	}
	return out
}

// oneOfValue picks the field of a one-of rule to fill and its value: the first
// field, in the rule's order, whose value the package states. An entity field takes
// a record of its type in the principal's own workspace.
func (w *world) oneOfValue(c ap.Capability, r oneOfRule, p *wPrincipal) (string, any, bool) {
	byName := map[string]ap.CapabilityField{}
	for _, f := range c.Fields {
		byName[string(f.Name)] = f
	}
	for _, name := range r.fields {
		f := byName[name]
		if f.Type == ap.CapabilityFieldTypeEntityRef {
			et := ""
			if f.EntityType != nil {
				et = string(*f.EntityType)
			}
			if own := w.recordsOf(et, p); len(own) > 0 {
				return name, own[0].id, true
			}
			continue
		}
		if v, ok := w.statedValue(c, f, p); ok {
			return name, v, true
		}
	}
	return "", nil, false
}

// statedValue returns the value the package itself gives a non-entity field: its
// literal default, a value taken from the verified context, its first enum value
// or the value of a capability example. A computed default ("today") is the
// application's to apply, so it states no value to send.
func (w *world) statedValue(c ap.Capability, f ap.CapabilityField, p *wPrincipal) (any, bool) {
	if f.Default != nil {
		switch d := f.Default.Value.(type) {
		case ap.CapabilityFieldDefaultLiteral:
			return d.Value, true
		case ap.CapabilityFieldDefaultContext:
			switch d.Key {
			case ap.CapabilityFieldDefaultContextKeyPrincipalID:
				return p.id, true
			case ap.CapabilityFieldDefaultContextKeyTenantID:
				return w.tenants[p.tenantRef], true
			case ap.CapabilityFieldDefaultContextKeyWorkspaceID:
				return w.workspaces[p.workspaceRef], true
			}
		}
	}
	if f.Type == ap.CapabilityFieldTypeEnum && len(f.Values) > 0 {
		return f.Values[0], true
	}
	return exampleValue(c, string(f.Name))
}

func removeString(s []string, x string) []string {
	out := s[:0:0]
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// literalFor returns the value of a required, non-entity field: the value,
// whether to send it, and whether one could be built.
func (w *world) literalFor(c ap.Capability, f ap.CapabilityField, p *wPrincipal) (any, bool, bool) {
	if f.Default != nil {
		switch d := f.Default.Value.(type) {
		case ap.CapabilityFieldDefaultLiteral:
			return d.Value, true, true
		case ap.CapabilityFieldDefaultContext:
			switch d.Key {
			case ap.CapabilityFieldDefaultContextKeyPrincipalID:
				return p.id, true, true
			case ap.CapabilityFieldDefaultContextKeyTenantID:
				return w.tenants[p.tenantRef], true, true
			case ap.CapabilityFieldDefaultContextKeyWorkspaceID:
				return w.workspaces[p.workspaceRef], true, true
			}
			// today: the application decides; leave the field out.
			return nil, false, true
		}
	}
	if f.Type == ap.CapabilityFieldTypeEnum {
		if len(f.Values) > 0 {
			return f.Values[0], true, true
		}
		return nil, false, false
	}
	if v, ok := exampleValue(c, string(f.Name)); ok {
		return v, true, true
	}
	switch f.Type {
	case ap.CapabilityFieldTypeBoolean:
		return true, true, true
	case ap.CapabilityFieldTypeInteger:
		return boundedNumber(f, true), true, true
	case ap.CapabilityFieldTypeNumber:
		return boundedNumber(f, false), true, true
	case ap.CapabilityFieldTypeString:
		return boundedString(f), true, true
	}
	// date, datetime, duration and list values cannot be invented: they
	// have to be valid in the application's own calendar and rules.
	return nil, false, false
}

// exampleValue returns the value a capability example gives a field, taking the
// examples in order of their IDs. A scalar or a flat list of scalars qualifies.
func exampleValue(c ap.Capability, name string) (any, bool) {
	ex := append([]ap.CapabilityExample(nil), c.Examples...)
	sort.Slice(ex, func(i, j int) bool { return ex[i].ID < ex[j].ID })
	for _, e := range ex {
		v, ok := e.ExpectedFields[name]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case string, bool, float64, int, int64:
			return x, true
		case []any:
			flat := true
			for _, item := range x {
				switch item.(type) {
				case string, bool, float64, int, int64:
				default:
					flat = false
				}
			}
			if flat {
				return x, true
			}
		}
	}
	return nil, false
}

func boundedNumber(f ap.CapabilityField, integer bool) any {
	v := 1.0
	if f.Minimum != nil {
		v = *f.Minimum
	}
	if f.Maximum != nil && v > *f.Maximum {
		v = *f.Maximum
	}
	if integer {
		return int64(v)
	}
	return v
}

func boundedString(f ap.CapabilityField) string {
	s := "synthetic"
	if f.MinLength != nil && int64(len(s)) < *f.MinLength {
		s += strings.Repeat("x", int(*f.MinLength)-len(s))
	}
	if f.MaxLength != nil && int64(len(s)) > *f.MaxLength {
		s = s[:*f.MaxLength]
	}
	return s
}

// operationsOf lists the operations a capability declares, in contract order.
func operationsOf(c ap.Capability) []string {
	var ops []string
	add := func(name string, b *ap.CapabilityOperationBinding) {
		if b != nil {
			ops = append(ops, name)
		}
	}
	add("read", c.Operations.Read)
	add("validate", c.Operations.Validate)
	add("prepare", c.Operations.Prepare)
	add("commit", c.Operations.Commit)
	add("status", c.Operations.Status)
	return ops
}

func hasOp(c ap.Capability, op string) bool {
	for _, o := range operationsOf(c) {
		if o == op {
			return true
		}
	}
	return false
}

// consequential reports whether a capability changes anything: risk level 1 and
// above needs prepare, commit and every safe-execution probe.
func consequential(c ap.Capability) bool { return c.RiskLevel >= 1 }

// principalCandidates lists, in suite order and without repeats, the fixture
// principals the suite itself declares for a capability: the principals of its
// allow probes first, then of its safety probes, smoke paths and conversations.
func principalCandidates(suite ap.PackageTests, capID string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	for _, p := range suite.ScopeProbes {
		if p.Expect == ap.PackageTestsScopeProbeExpectAllow && p.CapabilityID != nil && string(*p.CapabilityID) == capID &&
			p.Operation != ap.PackageTestsScopeProbeOperationRetrieve {
			add(string(p.Principal))
		}
	}
	for _, p := range suite.SafetyProbes {
		if string(p.CapabilityID) == capID {
			add(string(p.Principal))
		}
	}
	for _, p := range suite.Smoke.Paths {
		if p.CapabilityID != nil && string(*p.CapabilityID) == capID {
			add(string(p.Principal))
		}
	}
	for _, c := range suite.ConversationCases {
		if string(c.CapabilityID) == capID {
			add(string(c.Principal))
		}
	}
	return out
}
