package apppkg

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/policy"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

type opKey struct{ endpoint, name string }

// opBinding is one operation of a capability, named for messages.
type opBinding struct {
	name string // "read", "validate", "prepare", "commit", "status" or "entity_types.<t>.resolve"
	b    ap.CapabilityOperationBinding
}

func operationBindings(c ap.Capability) []opBinding {
	var out []opBinding
	for _, o := range []struct {
		name string
		b    *ap.CapabilityOperationBinding
	}{
		{"read", c.Operations.Read},
		{"validate", c.Operations.Validate},
		{"prepare", c.Operations.Prepare},
		{"commit", c.Operations.Commit},
		{"status", c.Operations.Status},
	} {
		if o.b != nil {
			out = append(out, opBinding{o.name, *o.b})
		}
	}
	types := make([]string, 0, len(c.EntityTypes))
	for t := range c.EntityTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		if r := c.EntityTypes[t].Resolve; r != nil {
			out = append(out, opBinding{"entity_types." + t + ".resolve", *r})
		}
	}
	return out
}

// normalizeOperation folds an operation name so that look-alikes collide.
func normalizeOperation(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(s)
}

func (md *model) checkCapabilities() {
	byEndpoint := map[string]map[string]map[string]bool{} // endpoint -> normalized -> exact names
	users := map[opKey]map[string]bool{}                  // operation -> capabilities mapping it

	for _, id := range md.capIDs() {
		e := md.caps[id]
		md.checkCapability(id, e)
		for _, ob := range operationBindings(e.c) {
			ep, name := string(ob.b.Endpoint), operationName(bindingCall(ob.b))
			if md.callProblem(bindingCall(ob.b)) != "" || name == "" {
				continue
			}
			if byEndpoint[ep] == nil {
				byEndpoint[ep] = map[string]map[string]bool{}
			}
			n := normalizeOperation(name)
			if byEndpoint[ep][n] == nil {
				byEndpoint[ep][n] = map[string]bool{}
			}
			byEndpoint[ep][n][name] = true
			k := opKey{ep, name}
			if users[k] == nil {
				users[k] = map[string]bool{}
			}
			users[k][id] = true
		}
	}

	for _, ep := range sortedKeys(byEndpoint) {
		for _, n := range sortedKeys(byEndpoint[ep]) {
			if names := sortedKeys(byEndpoint[ep][n]); len(names) > 1 {
				md.c.add(chkLookalike, "", "", "endpoint %s maps look-alike operation names %s; they differ only by case or by '_', '-' and '.'", q(ep), qs(names))
			}
		}
	}
	keys := make([]opKey, 0, len(users))
	for k := range users {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].endpoint != keys[j].endpoint {
			return keys[i].endpoint < keys[j].endpoint
		}
		return keys[i].name < keys[j].name
	})
	for _, k := range keys {
		if caps := sortedKeys(users[k]); len(caps) > 1 {
			md.c.add(chkSharedOperation, "", "", "operation %s of endpoint %s is mapped by capabilities %s", q(k.name), q(k.endpoint), qs(caps))
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (md *model) checkCapability(id string, e *capEntry) {
	c, path := e.c, e.path
	add := func(ch *check, format string, args ...any) { md.c.add(ch, id, path, format, args...) }

	// uniqueness within the capability (item 4)
	dups := func(what string, items []string) {
		seen := map[string]bool{}
		for _, s := range items {
			if seen[s] {
				add(chkDuplicate, "%s %s is declared more than once in the capability", what, q(s))
			}
			seen[s] = true
		}
	}
	fieldNames := make([]string, len(c.Fields))
	fieldSet := map[string]ap.CapabilityField{}
	for i, f := range c.Fields {
		fieldNames[i] = string(f.Name)
		fieldSet[string(f.Name)] = f
	}
	dups("field", fieldNames)
	ids := func(n int, f func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	dups("ambiguity rule", ids(len(c.Ambiguity), func(i int) string { return string(c.Ambiguity[i].ID) }))
	dups("validation rule", ids(len(c.Validation), func(i int) string { return string(c.Validation[i].ID) }))
	dups("next action", ids(len(c.NextActions), func(i int) string { return string(c.NextActions[i].ID) }))
	dups("example", ids(len(c.Examples), func(i int) string { return string(c.Examples[i].ID) }))
	dups("failure state", ids(len(c.FailureStates), func(i int) string {
		return failureKey(string(c.FailureStates[i].Code), c.FailureStates[i].ApplicationCode)
	}))

	// fields (item 7)
	for _, f := range c.Fields {
		md.checkField(id, path, f, c)
	}
	for _, r := range c.Ambiguity {
		f, ok := fieldSet[string(r.Field)]
		switch {
		case !ok:
			add(chkAmbiguity, "ambiguity rule %s names field %s, which the capability does not declare", q(string(r.ID)), q(string(r.Field)))
		case string(r.Action) == "use_default" && f.Default == nil:
			add(chkAmbiguity, "ambiguity rule %s uses use_default but field %s has no default", q(string(r.ID)), q(string(r.Field)))
		}
	}
	for _, ex := range c.Examples {
		for _, name := range sortedKeys(ex.ExpectedFields) {
			if _, ok := fieldSet[name]; !ok {
				add(chkExample, "example %s expects field %s, which the capability does not declare", q(string(ex.ID)), q(name))
			}
		}
		if !md.langs[string(ex.Language)] {
			add(chkExampleLang, "example %s is in language %s, which the manifest does not support", q(string(ex.ID)), q(string(ex.Language)))
		}
	}
	for _, n := range c.NextActions {
		_, listed := md.m.Capabilities[string(n.CapabilityID)]
		switch {
		case !listed:
			add(chkNextAction, "next action %s points at capability %s, which the manifest does not declare", q(string(n.ID)), q(string(n.CapabilityID)))
		default:
			if t, ok := md.caps[string(n.CapabilityID)]; ok && string(t.c.Audience) != "user" {
				add(chkNextAction, "next action %s points at capability %s, whose audience is %s, not user", q(string(n.ID)), q(string(n.CapabilityID)), q(string(t.c.Audience)))
			}
		}
	}

	// validation (item 8)
	for _, r := range c.Validation {
		for _, fn := range r.Fields {
			if _, ok := fieldSet[string(fn)]; !ok {
				add(chkValidation, "validation rule %s checks field %s, which the capability does not declare", q(string(r.ID)), q(string(fn)))
			}
		}
		found := false
		for _, fs := range c.FailureStates {
			if fs.Code == r.ErrorCode && (r.ApplicationCode == nil || (fs.ApplicationCode != nil && *fs.ApplicationCode == *r.ApplicationCode)) {
				found = true
			}
		}
		if !found {
			add(chkValidation, "validation rule %s returns error code %s, which is not one of the capability's failure_states", q(string(r.ID)), q(string(r.ErrorCode)))
		}
	}

	// idempotency (item 9)
	if c.Idempotency != nil && c.PreparedAction != nil && c.Idempotency.WindowSeconds < c.PreparedAction.TTLSeconds {
		add(chkIdempotency, "idempotency.window_seconds is %d, shorter than prepared_action.ttl_seconds %d", c.Idempotency.WindowSeconds, c.PreparedAction.TTLSeconds)
	}

	// risk floor (section 6, master plan 11.4)
	if floor, known := policy.EffectClass(c.EffectClass).Floor(); !known || int64(c.RiskLevel) < int64(floor) {
		add(chkRiskFloor, "risk_level %d is below the platform floor %d of effect_class %s", int64(c.RiskLevel), int(floor), q(string(c.EffectClass)))
	}

	// operations: exact adapter mapping (SEC-052, 14.6)
	for _, ob := range operationBindings(c) {
		call := bindingCall(ob.b)
		if p := md.callProblem(call); p != "" {
			add(chkAdapterMapping, "operation %s has no exact adapter mapping: %s", ob.name, p)
		}
		if !md.schemaFileExists(string(ob.b.ResultSchema)) {
			add(chkResultSchema, "operation %s: result_schema %s does not exist in the package", ob.name, q(string(ob.b.ResultSchema)))
		}
	}
}

func failureKey(code string, app *ap.Slug) string {
	if app != nil {
		return code + "/" + string(*app)
	}
	return code
}

var (
	dateRE     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	durationRE = regexp.MustCompile(`^P(?:[0-9]+Y)?(?:[0-9]+M)?(?:[0-9]+W)?(?:[0-9]+D)?(?:T(?:[0-9]+H)?(?:[0-9]+M)?(?:[0-9]+(?:\.[0-9]+)?S)?)?$`)
)

// checkField checks one field's type, constraints and default.
func (md *model) checkField(id, path string, f ap.CapabilityField, c ap.Capability) {
	t := string(f.Type)
	name := q(string(f.Name))
	bad := func(format string, args ...any) {
		md.c.add(chkConstraint, id, path, "field %s: %s", name, fmt.Sprintf(format, args...))
	}

	if t == "enum" && len(f.Values) == 0 {
		bad("an enum field needs values")
	}
	if t != "enum" && len(f.Values) > 0 {
		bad("values apply only to type enum, not %s", t)
	}
	if t != "list" && f.ItemType != nil {
		bad("item_type applies only to type list, not %s", t)
	}
	if t == "list" && f.ItemType == nil {
		bad("a list field needs item_type")
	}
	if t != "string" && (f.MinLength != nil || f.MaxLength != nil) {
		bad("min_length and max_length apply only to type string, not %s", t)
	}
	if t != "integer" && t != "number" && (f.Minimum != nil || f.Maximum != nil) {
		bad("minimum and maximum apply only to types integer and number, not %s", t)
	}
	if t != "list" && f.MaxItems != nil {
		bad("max_items applies only to type list, not %s", t)
	}
	if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
		bad("min_length is greater than max_length")
	}
	if f.Minimum != nil && f.Maximum != nil && *f.Minimum > *f.Maximum {
		bad("minimum is greater than maximum")
	}

	switch {
	case t == "entity_ref" && f.EntityType == nil:
		md.c.add(chkEntityType, id, path, "field %s is an entity_ref but names no entity_type", name)
	case t == "entity_ref":
		if _, ok := c.EntityTypes[string(*f.EntityType)]; !ok {
			md.c.add(chkEntityType, id, path, "field %s names entity type %s, which the capability does not declare", name, q(string(*f.EntityType)))
		}
	case f.EntityType != nil:
		bad("entity_type applies only to type entity_ref, not %s", t)
	}

	if f.Default != nil {
		if why := defaultMisfit(f); why != "" {
			md.c.add(chkDefaultType, id, path, "field %s: the default %s", name, why)
		}
	}
}

// defaultMisfit returns why a field's default does not fit its type, or "".
func defaultMisfit(f ap.CapabilityField) string {
	t := string(f.Type)
	switch d := f.Default.Value.(type) {
	case ap.CapabilityFieldDefaultContext:
		switch string(d.Key) {
		case "today":
			if t != "date" && t != "string" {
				return "uses the context value today, which is a date, for a field of type " + t
			}
		default:
			if t != "string" && t != "entity_ref" {
				return "uses the context value " + string(d.Key) + ", which is an identifier, for a field of type " + t
			}
		}
	case ap.CapabilityFieldDefaultLiteral:
		switch v := d.Value.(type) {
		case string:
			switch t {
			case "string", "entity_ref":
			case "date":
				if !dateRE.MatchString(v) {
					return "is not a YYYY-MM-DD date"
				}
				if _, err := time.Parse("2006-01-02", v); err != nil {
					return "is not a valid date"
				}
			case "datetime":
				if _, err := time.Parse(time.RFC3339, v); err != nil || !strings.HasSuffix(v, "Z") {
					return "is not an RFC 3339 UTC timestamp"
				}
			case "duration":
				if v == "P" || !durationRE.MatchString(v) {
					return "is not an ISO 8601 duration"
				}
			case "enum":
				ok := false
				for _, allowed := range f.Values {
					if allowed == v {
						ok = true
					}
				}
				if !ok {
					return "is not one of the field's values"
				}
			default:
				return "is a string but the field is " + t
			}
			if t == "string" {
				n := len([]rune(v))
				if f.MinLength != nil && int64(n) < *f.MinLength {
					return "is shorter than min_length"
				}
				if f.MaxLength != nil && int64(n) > *f.MaxLength {
					return "is longer than max_length"
				}
			}
		case float64:
			switch t {
			case "integer":
				if v != math.Trunc(v) {
					return "is not a whole number"
				}
			case "number":
			default:
				return "is a number but the field is " + t
			}
			if f.Minimum != nil && v < *f.Minimum {
				return "is below minimum"
			}
			if f.Maximum != nil && v > *f.Maximum {
				return "is above maximum"
			}
		case bool:
			if t != "boolean" {
				return "is a boolean but the field is " + t
			}
		default:
			return "is not a string, number or boolean"
		}
	default:
		return "has an unknown source"
	}
	return ""
}
