package policy

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func f64(v float64) *float64 { return &v }
func sc(s Scalar) *Scalar    { return &s }
func tp(b bool) *bool        { return &b }

func TestFieldPredicateSemantics(t *testing.T) {
	absent := struct{}{} // marker: the field has no value
	tests := []struct {
		name string
		p    FieldPredicate
		v    any
		want tri
	}{
		// equality
		{"eq string match", FieldPredicate{Eq: sc(String("EUR"))}, "EUR", triTrue},
		{"eq string miss", FieldPredicate{Eq: sc(String("EUR"))}, "USD", triFalse},
		{"eq is case-sensitive", FieldPredicate{Eq: sc(String("EUR"))}, "eur", triFalse},
		{"eq number match", FieldPredicate{Eq: sc(Number(5))}, 5, triTrue},
		{"eq number against float", FieldPredicate{Eq: sc(Number(5))}, 5.0, triTrue},
		{"eq number against json.Number", FieldPredicate{Eq: sc(Number(5))}, json.Number("5"), triTrue},
		{"eq bool", FieldPredicate{Eq: sc(Bool(true))}, true, triTrue},
		{"eq bool miss", FieldPredicate{Eq: sc(Bool(true))}, false, triFalse},
		{"eq type mismatch is undecided", FieldPredicate{Eq: sc(String("5"))}, 5, triIndeterminate},
		{"eq against a list is undecided", FieldPredicate{Eq: sc(String("a"))}, []string{"a"}, triIndeterminate},
		{"ne match", FieldPredicate{Ne: sc(String("EUR"))}, "USD", triTrue},
		{"ne miss", FieldPredicate{Ne: sc(String("EUR"))}, "EUR", triFalse},
		{"ne type mismatch is undecided, not true", FieldPredicate{Ne: sc(String("EUR"))}, 3, triIndeterminate},
		// membership
		{"in hit", FieldPredicate{In: []Scalar{String("a"), String("b")}}, "b", triTrue},
		{"in miss", FieldPredicate{In: []Scalar{String("a"), String("b")}}, "c", triFalse},
		{"in with no same-type entry is undecided", FieldPredicate{In: []Scalar{String("a")}}, 1, triIndeterminate},
		{"in with a mixed list", FieldPredicate{In: []Scalar{String("a"), Number(1)}}, 1, triTrue},
		{"not_in hit", FieldPredicate{NotIn: []Scalar{String("EUR"), String("USD")}}, "CHF", triTrue},
		{"not_in miss", FieldPredicate{NotIn: []Scalar{String("EUR"), String("USD")}}, "EUR", triFalse},
		{"not_in with no same-type entry is undecided", FieldPredicate{NotIn: []Scalar{String("EUR")}}, 7, triIndeterminate},
		// bounds
		{"gt true", FieldPredicate{Gt: f64(10)}, 11, triTrue},
		{"gt equal", FieldPredicate{Gt: f64(10)}, 10, triFalse},
		{"gte equal", FieldPredicate{Gte: f64(10)}, 10, triTrue},
		{"lt true", FieldPredicate{Lt: f64(10)}, 9.5, triTrue},
		{"lte equal", FieldPredicate{Lte: f64(10)}, 10, triTrue},
		{"lte above", FieldPredicate{Lte: f64(10)}, 10.01, triFalse},
		{"gt on a string is undecided", FieldPredicate{Gt: f64(10)}, "99", triIndeterminate},
		{"gt on a bool is undecided", FieldPredicate{Gt: f64(10)}, true, triIndeterminate},
		{"gt on NaN is undecided", FieldPredicate{Gt: f64(10)}, math.NaN(), triIndeterminate},
		{"gt on +Inf is undecided", FieldPredicate{Gt: f64(10)}, math.Inf(1), triIndeterminate},
		{"gt on an unparsable json.Number is undecided", FieldPredicate{Gt: f64(10)}, json.Number("1e999"), triIndeterminate},
		{"gt on uint64", FieldPredicate{Gt: f64(10)}, uint64(11), triTrue},
		{"gt on int8", FieldPredicate{Gt: f64(10)}, int8(11), triTrue},
		// ranges are ANDs
		{"range inside", FieldPredicate{Gte: f64(1), Lt: f64(10)}, 5, triTrue},
		{"range above", FieldPredicate{Gte: f64(1), Lt: f64(10)}, 10, triFalse},
		{"range with a wrong type", FieldPredicate{Gte: f64(1), Lt: f64(10)}, "5", triIndeterminate},
		// presence and absence
		{"present on a value", FieldPredicate{Present: tp(true)}, "x", triTrue},
		{"present on a missing value", FieldPredicate{Present: tp(true)}, absent, triFalse},
		{"present false on a missing value", FieldPredicate{Present: tp(false)}, absent, triTrue},
		{"present false on a value", FieldPredicate{Present: tp(false)}, "x", triFalse},
		{"a comparison on a missing value is undecided", FieldPredicate{Gt: f64(10)}, absent, triIndeterminate},
		{"eq on a missing value is undecided", FieldPredicate{Eq: sc(String("a"))}, absent, triIndeterminate},
		{"not_in on a missing value is undecided", FieldPredicate{NotIn: []Scalar{String("a")}}, absent, triIndeterminate},
		{"present:true plus a comparison on a missing value is false", FieldPredicate{Present: tp(true), Gt: f64(10)}, absent, triFalse},
		{"present:true plus a comparison that holds", FieldPredicate{Present: tp(true), Gt: f64(10)}, 11, triTrue},
		{"present:true plus a comparison of the wrong type stays undecided", FieldPredicate{Present: tp(true), Gt: f64(10)}, "x", triIndeterminate},
		{"a definite false beats an undecided operator", FieldPredicate{Eq: sc(String("a")), Gt: f64(10)}, 5, triFalse},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			present := true
			v := tc.v
			if tc.v == absent {
				present, v = false, nil
			}
			if got := evalPredicate(tc.p, v, present); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestIdentityConditions(t *testing.T) {
	have := toSet([]string{"member", "staff"})
	tests := []struct {
		name string
		c    SetCondition
		want bool
	}{
		{"any hit", SetCondition{Any: []string{"admin", "staff"}}, true},
		{"any miss", SetCondition{Any: []string{"admin"}}, false},
		{"all hit", SetCondition{All: []string{"member", "staff"}}, true},
		{"all miss", SetCondition{All: []string{"member", "admin"}}, false},
		{"none hit", SetCondition{None: []string{"admin"}}, true},
		{"none miss", SetCondition{None: []string{"staff"}}, false},
		{"any and none", SetCondition{Any: []string{"member"}, None: []string{"admin"}}, true},
		{"any and none contradict", SetCondition{Any: []string{"member"}, None: []string{"staff"}}, false},
		{"exact match only", SetCondition{Any: []string{"Member"}}, false},
	}
	for _, tc := range tests {
		if got := setMatches(tc.c, have); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	vtests := []struct {
		name string
		c    ValueCondition
		v    string
		want bool
	}{
		{"in hit", ValueCondition{In: []string{"a", "b"}}, "a", true},
		{"in miss", ValueCondition{In: []string{"a", "b"}}, "c", false},
		{"not_in hit", ValueCondition{NotIn: []string{"a"}}, "b", true},
		{"not_in miss", ValueCondition{NotIn: []string{"a"}}, "a", false},
		{"in and not_in", ValueCondition{In: []string{"a", "b"}, NotIn: []string{"b"}}, "b", false},
	}
	for _, tc := range vtests {
		if got := valueMatches(tc.c, tc.v); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Field conditions are decided with the real values at prepare and commit, and
// fail closed when a value is missing or of an unexpected type.
func TestFieldRulesAtPrepareAndExposure(t *testing.T) {
	g, _ := newGateway()
	doc := `{
	  "policy_id": "p",
	  "role_grants": [{ "role": "member", "capabilities": ["loans.extend"] }],
	  "risk_overrides": [],
	  "field_projection": [],
	  "rules": [
	    { "id": "deny-long", "capabilities": ["loans.extend"], "when": { "fields": { "weeks": { "gt": 8 } } },
	      "then": { "deny": true }, "reason": "Too long." },
	    { "id": "deny-long-when-present", "capabilities": ["loans.extend"],
	      "when": { "fields": { "loan": { "present": true, "eq": "L-BLOCKED" } } },
	      "then": { "deny": true }, "reason": "Blocked loan." },
	    { "id": "hide-weeks-for-big-loans", "capabilities": ["loans.extend"], "when": { "fields": { "loan": { "eq": "L-BIG" } } },
	      "then": { "hide_fields": ["weeks"] }, "reason": "Fixed term." }
	  ]
	}`
	pkg := demoPackage(t, doc)
	s := scopeWith(allUserCaps())
	prep := func(fields map[string]any) Decision {
		return decide(g, pkg, s, capExtend, PhasePrepare, func(r *Request) { r.Fields = fields })
	}

	t.Run("deny rules with field conditions are deferred at exposure, not applied", func(t *testing.T) {
		d := decide(g, pkg, s, capExtend, PhaseExpose)
		if !d.Allowed {
			t.Fatalf("exposure denied by a rule that needs values: %+v", d.Reasons)
		}
		if !equalStrings(d.Record.RulesDeferred, []string{"deny-long", "deny-long-when-present", "hide-weeks-for-big-loans"}) {
			t.Errorf("rules_deferred = %v", d.Record.RulesDeferred)
		}
		for _, a := range d.Record.RulesApplied {
			if a.RuleID == "deny-long" || a.RuleID == "deny-long-when-present" {
				t.Errorf("deny rule %s applied at exposure", a.RuleID)
			}
		}
	})
	t.Run("a hide rule with field conditions applies at exposure, conservatively", func(t *testing.T) {
		d := decide(g, pkg, s, capExtend, PhaseExpose)
		if !equalStrings(d.VisibleFields, []string{"loan"}) {
			t.Fatalf("visible = %v, want weeks hidden until prepare shows the rule does not apply", d.VisibleFields)
		}
	})
	t.Run("the deferred deny rule is decided with the values at prepare", func(t *testing.T) {
		if d := prep(map[string]any{"loan": "L-1", "weeks": 9}); d.Allowed || d.Reasons[len(d.Reasons)-1].RuleID != "deny-long" {
			t.Fatalf("weeks=9: allowed=%v reasons=%+v", d.Allowed, d.Reasons)
		}
		if d := prep(map[string]any{"loan": "L-1", "weeks": 8}); !d.Allowed {
			t.Fatalf("weeks=8 denied: %+v", d.Reasons)
		}
		if d := prep(map[string]any{"loan": "L-1", "weeks": json.Number("12")}); d.Allowed {
			t.Fatal("json.Number weeks=12 allowed")
		}
	})
	t.Run("a missing value does not let a rule silently stop applying", func(t *testing.T) {
		d := prep(map[string]any{"loan": "L-1"})
		if d.Allowed || d.Reasons[len(d.Reasons)-1].RuleID != "deny-long" {
			t.Fatalf("no weeks: allowed=%v reasons=%+v", d.Allowed, d.Reasons)
		}
		if d := prep(nil); d.Allowed {
			t.Fatal("no field values at all: allowed")
		}
		if d := prep(map[string]any{"loan": "L-1", "weeks": nil}); d.Allowed {
			t.Fatal("null weeks: allowed")
		}
	})
	t.Run("a value of the wrong type does not let a rule silently stop applying", func(t *testing.T) {
		if d := prep(map[string]any{"loan": "L-1", "weeks": "lots"}); d.Allowed {
			t.Fatal("string weeks: allowed")
		}
		if d := prep(map[string]any{"loan": "L-1", "weeks": []any{9}}); d.Allowed {
			t.Fatal("list weeks: allowed")
		}
	})
	t.Run("present:true scopes a rule to calls that carry the field", func(t *testing.T) {
		// loan absent: deny-long-when-present does not apply (it would be
		// undecided otherwise); weeks=2 keeps deny-long away.
		if d := prep(map[string]any{"weeks": 2}); !d.Allowed {
			t.Fatalf("denied: %+v", d.Reasons)
		}
		if d := prep(map[string]any{"loan": "L-BLOCKED", "weeks": 2}); d.Allowed || d.Reasons[len(d.Reasons)-1].RuleID != "deny-long-when-present" {
			t.Fatalf("L-BLOCKED: allowed=%v reasons=%+v", d.Allowed, d.Reasons)
		}
	})
	t.Run("a hide rule is decided with the values at prepare", func(t *testing.T) {
		d := prep(map[string]any{"loan": "L-BIG", "weeks": 2})
		if !d.Allowed || !equalStrings(d.VisibleFields, []string{"loan"}) {
			t.Fatalf("L-BIG: allowed=%v visible=%v", d.Allowed, d.VisibleFields)
		}
		d = prep(map[string]any{"loan": "L-SMALL", "weeks": 2})
		if !d.Allowed || !equalStrings(d.VisibleFields, []string{"loan", "weeks"}) {
			t.Fatalf("L-SMALL: allowed=%v visible=%v", d.Allowed, d.VisibleFields)
		}
	})
	t.Run("the commit decision uses the same rules as prepare", func(t *testing.T) {
		d := decide(g, pkg, s, capExtend, PhaseCommit, func(r *Request) {
			r.Fields = map[string]any{"loan": "L-1", "weeks": 9}
		})
		if d.Allowed || d.Code != contracts.ErrPermissionDenied {
			t.Fatalf("commit: allowed=%v code=%q", d.Allowed, d.Code)
		}
	})
	t.Run("field values never reach the record", func(t *testing.T) {
		d := prep(map[string]any{"loan": sentinelVal, "weeks": 9})
		raw, _ := json.Marshal(d.Record)
		if containsStr(string(raw), sentinelVal) {
			t.Fatalf("record leaked a value: %s", raw)
		}
	})
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Conditions on roles, workspaces and token scopes are decided from the
// verified context, in every phase.
func TestIdentityRulesNarrow(t *testing.T) {
	g, _ := newGateway()
	doc := `{
	  "policy_id": "p",
	  "role_grants": [{ "role": "member", "capabilities": ["catalog.find_title", "reservations.create"] }],
	  "risk_overrides": [],
	  "field_projection": [],
	  "rules": [
	    { "id": "closed-site", "capabilities": ["reservations.create"], "when": { "workspaces": { "in": ["ws-closed"] } },
	      "then": { "deny": true }, "reason": "Closed." },
	    { "id": "outside-pilot", "capabilities": ["reservations.create"], "when": { "tenants": { "not_in": ["tenant-a"] } },
	      "then": { "deny": true }, "reason": "Pilot only." },
	    { "id": "narrow-token-needs-confirmation", "capabilities": ["catalog.find_title"],
	      "when": { "scopes": { "none": ["reservations.create"] } }, "then": { "risk_min": 1 }, "reason": "Narrow tokens are logged." }
	  ]
	}`
	pkg := demoPackage(t, doc)

	s := scopeWith(allUserCaps())
	if d := decide(g, pkg, s, capReserve, PhaseExpose); !d.Allowed {
		t.Fatalf("pilot tenant, open site denied: %+v", d.Reasons)
	}
	closed := scopeWith(allUserCaps(), func(sc *contracts.Scope) { sc.WorkspaceID = "ws-closed" })
	if d := decide(g, pkg, closed, capReserve, PhaseExpose); d.Allowed || d.Reasons[len(d.Reasons)-1].RuleID != "closed-site" {
		t.Fatalf("closed site: allowed=%v reasons=%+v", d.Allowed, d.Reasons)
	}
	other := scopeWith(allUserCaps(), func(sc *contracts.Scope) { sc.TenantID = tenantB })
	if d := decide(g, pkg, other, capReserve, PhaseExpose); d.Allowed || d.Reasons[len(d.Reasons)-1].RuleID != "outside-pilot" {
		t.Fatalf("other tenant: allowed=%v reasons=%+v", d.Allowed, d.Reasons)
	}
	// The other tenant can still use what the rule does not touch.
	if d := decide(g, pkg, other, capFind, PhaseExpose); !d.Allowed {
		t.Fatalf("other tenant denied find: %+v", d.Reasons)
	}
	// Scopes condition: a token without reservations.create is logged at 1.
	narrow := scopeWith([]string{capFind})
	if d := decide(g, pkg, narrow, capFind, PhaseExpose); !d.Allowed || d.EffectiveRisk != contracts.RiskReversible {
		t.Fatalf("narrow token: allowed=%v risk=%v", d.Allowed, d.EffectiveRisk)
	}
	if d := decide(g, pkg, s, capFind, PhaseExpose); d.EffectiveRisk != contracts.RiskRead {
		t.Fatalf("wide token: risk=%v", d.EffectiveRisk)
	}
}
