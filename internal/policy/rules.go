package policy

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
)

// tri is the three-valued result of testing a field predicate.
type tri uint8

const (
	// triFalse: the predicate definitely does not hold.
	triFalse tri = iota
	// triIndeterminate: the value is absent or of a type the operator cannot
	// compare. A narrowing rule treats this as a match, so that a value the
	// host did not anticipate can never make a rule silently stop applying.
	triIndeterminate
	// triTrue: the predicate definitely holds.
	triTrue
)

// identityMatches tests the parts of a condition that depend only on the
// verified context: roles, token capability scopes, tenant and workspace.
func identityMatches(c Condition, roles, scopes map[string]bool, tenant, workspace string) bool {
	if c.Roles != nil && !setMatches(*c.Roles, roles) {
		return false
	}
	if c.Scopes != nil && !setMatches(*c.Scopes, scopes) {
		return false
	}
	if c.Tenants != nil && !valueMatches(*c.Tenants, tenant) {
		return false
	}
	if c.Workspaces != nil && !valueMatches(*c.Workspaces, workspace) {
		return false
	}
	return true
}

func setMatches(c SetCondition, have map[string]bool) bool {
	if len(c.Any) > 0 {
		hit := false
		for _, v := range c.Any {
			if have[v] {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, v := range c.All {
		if !have[v] {
			return false
		}
	}
	for _, v := range c.None {
		if have[v] {
			return false
		}
	}
	return true
}

func valueMatches(c ValueCondition, v string) bool {
	if len(c.In) > 0 && !contains(c.In, v) {
		return false
	}
	if len(c.NotIn) > 0 && contains(c.NotIn, v) {
		return false
	}
	return true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// fieldsMatch reports whether every field predicate of a condition can hold for
// the given field values: a predicate that definitely does not hold makes the
// rule not apply, one that holds or cannot be decided lets it apply. Fields is
// never trusted to be well typed.
func fieldsMatch(preds map[string]FieldPredicate, values map[string]any) bool {
	for _, name := range sortedKeys(preds) {
		v, present := values[name]
		if v == nil {
			present = false
		}
		if evalPredicate(preds[name], v, present) == triFalse {
			return false
		}
	}
	return true
}

// evalPredicate ANDs the operators of a predicate. A definite false wins over
// an indeterminate operator, so present: true next to a comparison says "only
// when the field is there".
func evalPredicate(p FieldPredicate, v any, present bool) tri {
	result := triTrue
	merge := func(t tri) {
		switch {
		case t == triFalse:
			result = triFalse
		case t == triIndeterminate && result == triTrue:
			result = triIndeterminate
		}
	}
	if p.Present != nil {
		if present == *p.Present {
			merge(triTrue)
		} else {
			merge(triFalse)
		}
	}
	if !present {
		// Every other operator needs a value.
		if p.Eq != nil || p.Ne != nil || p.In != nil || p.NotIn != nil ||
			p.Gt != nil || p.Gte != nil || p.Lt != nil || p.Lte != nil {
			merge(triIndeterminate)
		}
		return result
	}
	val := normalize(v)
	if p.Eq != nil {
		merge(equalOrUndecided(val, *p.Eq, false))
	}
	if p.Ne != nil {
		merge(equalOrUndecided(val, *p.Ne, true))
	}
	if p.In != nil {
		merge(memberOrUndecided(val, p.In, false))
	}
	if p.NotIn != nil {
		merge(memberOrUndecided(val, p.NotIn, true))
	}
	for _, c := range []struct {
		bound *float64
		ok    func(v, b float64) bool
	}{
		{p.Gt, func(v, b float64) bool { return v > b }},
		{p.Gte, func(v, b float64) bool { return v >= b }},
		{p.Lt, func(v, b float64) bool { return v < b }},
		{p.Lte, func(v, b float64) bool { return v <= b }},
	} {
		if c.bound == nil {
			continue
		}
		if val.Kind != ScalarNumber {
			merge(triIndeterminate)
		} else if c.ok(val.Num, *c.bound) {
			merge(triTrue)
		} else {
			merge(triFalse)
		}
	}
	return result
}

func equalOrUndecided(val, want Scalar, negate bool) tri {
	if val.Kind == ScalarInvalid || val.Kind != want.Kind {
		return triIndeterminate
	}
	eq := scalarEqual(val, want)
	if eq != negate {
		return triTrue
	}
	return triFalse
}

func memberOrUndecided(val Scalar, list []Scalar, negate bool) tri {
	if val.Kind == ScalarInvalid {
		return triIndeterminate
	}
	comparable := false
	member := false
	for _, s := range list {
		if s.Kind != val.Kind {
			continue
		}
		comparable = true
		if scalarEqual(val, s) {
			member = true
			break
		}
	}
	if !comparable {
		return triIndeterminate
	}
	if member != negate {
		return triTrue
	}
	return triFalse
}

func scalarEqual(a, b Scalar) bool {
	switch a.Kind {
	case ScalarString:
		return a.Str == b.Str
	case ScalarNumber:
		return a.Num == b.Num
	case ScalarBool:
		return a.Bool == b.Bool
	}
	return false
}

// normalize maps a Go value from a field map (as decoded from JSON by the
// application or built by the dialogue engine) onto a Scalar. Anything that is
// not a string, boolean or finite number is ScalarInvalid.
func normalize(v any) Scalar {
	switch x := v.(type) {
	case string:
		return Scalar{Kind: ScalarString, Str: x}
	case bool:
		return Scalar{Kind: ScalarBool, Bool: x}
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return Scalar{}
		}
		return Scalar{Kind: ScalarNumber, Num: f}
	case nil:
		return Scalar{}
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return Scalar{Kind: ScalarNumber, Num: float64(rv.Int())}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Scalar{Kind: ScalarNumber, Num: float64(rv.Uint())}
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return Scalar{}
		}
		return Scalar{Kind: ScalarNumber, Num: f}
	case reflect.String:
		return Scalar{Kind: ScalarString, Str: rv.String()}
	case reflect.Bool:
		return Scalar{Kind: ScalarBool, Bool: rv.Bool()}
	}
	return Scalar{}
}
