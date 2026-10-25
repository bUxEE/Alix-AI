package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func exampleFiles(t testing.TB, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(schemaRoot, filepath.FromSlash(dir), "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples in %s (%v)", dir, err)
	}
	return files
}

// The JSON Schema and the Go parser are two independent statements of the same
// closed format. They must accept and reject the same documents, for the whole
// example corpus, including every policy example M1-01 shipped.
func TestHostPolicySchemaAndParserAgree(t *testing.T) {
	schema := compileSchema(t, "urn:alixai:schema:policy:host-policy:v1")

	corpora := []struct {
		dir  string
		want bool
	}{
		{"policy/examples/host-policy/valid", true},
		{"policy/examples/host-policy/invalid", false},
		// M1-01's policy is valid policy/v1 with no rules; what it rejects,
		// policy/v1 rejects too.
		{"application-package/examples/policy/valid", true},
		{"application-package/examples/policy/invalid", false},
	}
	for _, c := range corpora {
		for _, f := range exampleFiles(t, c.dir) {
			name := c.dir + "/" + filepath.Base(f)
			t.Run(name, func(t *testing.T) {
				raw, inst := readJSONFile(t, f)
				schemaErr := schema.Validate(inst)
				_, parseErr := ParseHostPolicy(raw)
				if (schemaErr == nil) != c.want {
					t.Errorf("schema accepts = %v, want %v: %v", schemaErr == nil, c.want, schemaErr)
				}
				if (parseErr == nil) != c.want {
					t.Errorf("parser accepts = %v, want %v: %v", parseErr == nil, c.want, parseErr)
				}
				if parseErr != nil && contracts.CodeOf(parseErr) != contracts.ErrSchemaInvalid {
					t.Errorf("parser error code = %q, want schema_invalid", contracts.CodeOf(parseErr))
				}
			})
		}
	}
}

func TestParseHostPolicyStrict(t *testing.T) {
	const ok = `{"policy_id":"p","role_grants":[{"role":"member","capabilities":["catalog.find_title"]}],"risk_overrides":[],"field_projection":[]}`
	if _, err := ParseHostPolicy([]byte(ok)); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	rule := func(when, then string) string {
		return `{"policy_id":"p","role_grants":[{"role":"member","capabilities":["catalog.find_title"]}],"risk_overrides":[],"field_projection":[],"rules":[{"id":"r","capabilities":["catalog.find_title"],"when":` + when + `,"then":` + then + `,"reason":"x"}]}`
	}
	deep := strings.Repeat(`{"a":`, 40) + `1` + strings.Repeat(`}`, 40)

	tests := []struct {
		name string
		doc  string
		path string // fragment the error must name
	}{
		{"empty input", ``, ""},
		{"not an object", `[]`, ""},
		{"trailing data", ok + ` {}`, ""},
		{"trailing comma garbage", ok + `,`, ""},
		{"unknown top-level key", strings.Replace(ok, `"policy_id"`, `"default":"allow","policy_id"`, 1), "unknown key"},
		{"key in the wrong case", strings.Replace(ok, `"policy_id"`, `"Policy_ID"`, 1), "policy_id"},
		{"duplicate key", strings.Replace(ok, `"policy_id":"p"`, `"policy_id":"p","policy_id":"q"`, 1), "duplicate key"},
		{"duplicate nested key", strings.Replace(ok, `"role":"member"`, `"role":"member","role":"visitor"`, 1), "role_grants[0].role"},
		{"null value", strings.Replace(ok, `"risk_overrides":[]`, `"risk_overrides":null`, 1), "null"},
		{"null inside a rule", rule(`{"roles":{"any":["member"]}}`, `{"risk_min":null}`), "null"},
		{"missing role_grants", `{"policy_id":"p","risk_overrides":[],"field_projection":[]}`, "role_grants"},
		{"empty role_grants", strings.Replace(ok, `[{"role":"member","capabilities":["catalog.find_title"]}]`, `[]`, 1), "role_grants"},
		{"wildcard capability", strings.Replace(ok, `catalog.find_title`, `catalog.*`, 1), "wildcards"},
		{"wildcard role", strings.Replace(ok, `"member"`, `"*"`, 1), "identifier"},
		{"role with whitespace", strings.Replace(ok, `"member"`, `"mem ber"`, 1), "identifier"},
		{"policy id not a slug", strings.Replace(ok, `"p"`, `"P"`, 1), "policy_id"},
		{"format of another kind", strings.Replace(ok, `{"policy_id"`, `{"format":"cedar/v1","policy_id"`, 1), "format"},
		{"format as a number", strings.Replace(ok, `{"policy_id"`, `{"format":1,"policy_id"`, 1), "format"},
		{"duplicate capability in a grant", strings.Replace(ok, `["catalog.find_title"]`, `["catalog.find_title","catalog.find_title"]`, 1), "duplicate"},
		{"expression instead of a condition", rule(`"role == admin"`, `{"deny":true}`), "object"},
		{"regex operator", rule(`{"roles":{"matches":"a.*"}}`, `{"deny":true}`), "unknown key"},
		{"empty condition", rule(`{}`, `{"deny":true}`), "at least one condition"},
		{"empty set condition", rule(`{"roles":{}}`, `{"deny":true}`), "any, all or none"},
		{"empty list", rule(`{"roles":{"any":[]}}`, `{"deny":true}`), "any, all or none"},
		{"empty list beside a full one", rule(`{"roles":{"any":[],"all":["member"]}}`, `{"deny":true}`), "identifiers"},
		{"allow effect", rule(`{"roles":{"any":["member"]}}`, `{"allow":true}`), "unknown key"},
		{"empty effect", rule(`{"roles":{"any":["member"]}}`, `{}`), "deny, risk_min or hide_fields"},
		{"deny false", rule(`{"roles":{"any":["member"]}}`, `{"deny":false}`), "must be true"},
		{"deny with a raise", rule(`{"roles":{"any":["member"]}}`, `{"deny":true,"risk_min":2}`), "cannot be combined"},
		{"field value null", rule(`{"fields":{"weeks":{"eq":null}}}`, `{"deny":true}`), "null"},
		{"field value array", rule(`{"fields":{"weeks":{"eq":[1]}}}`, `{"deny":true}`), "string, number or boolean"},
		{"field value object", rule(`{"fields":{"weeks":{"eq":{"a":1}}}}`, `{"deny":true}`), "string, number or boolean"},
		{"string bound", rule(`{"fields":{"weeks":{"gt":"1"}}}`, `{"deny":true}`), "number"},
		{"empty predicate", rule(`{"fields":{"weeks":{}}}`, `{"deny":true}`), "at least one operator"},
		{"unknown operator", rule(`{"fields":{"weeks":{"between":[1,2]}}}`, `{"deny":true}`), "unknown key"},
		{"present as a string", rule(`{"fields":{"weeks":{"present":"yes"}}}`, `{"deny":true}`), "boolean"},
		{"field name in upper case", rule(`{"fields":{"Weeks":{"gt":1}}}`, `{"deny":true}`), "snake_case"},
		{"number out of range", rule(`{"fields":{"weeks":{"gt":1e999}}}`, `{"deny":true}`), "finite"},
		{"too many field conditions", rule(`{"fields":{`+manyFields(17)+`}}`, `{"deny":true}`), "1 to 16"},
		{"too many rules", strings.Replace(ok, `"field_projection":[]`, `"field_projection":[],"rules":[`+manyRules(257)+`]`, 1), "at most 256"},
		{"nesting too deep", strings.Replace(ok, `"risk_overrides":[]`, `"risk_overrides":`+deep, 1), "deeper"},
		{"duplicate rule id", strings.Replace(rule(`{"roles":{"any":["member"]}}`, `{"deny":true}`), `"rules":[`, `"rules":[{"id":"r","capabilities":["catalog.find_title"],"when":{"roles":{"any":["member"]}},"then":{"deny":true},"reason":"x"},`, 1), "duplicate rule ID"},
		{"string scalar too long", rule(`{"fields":{"weeks":{"eq":"`+strings.Repeat("a", 257)+`"}}}`, `{"deny":true}`), "256"},
		{"control character in a reason", strings.Replace(rule(`{"roles":{"any":["member"]}}`, `{"deny":true}`), `"reason":"x"`, `"reason":"x\u0007"`, 1), "reason"},
		{"hide_fields empty", rule(`{"roles":{"any":["member"]}}`, `{"hide_fields":[]}`), "field names"},
		{"projection without fields", strings.Replace(ok, `"field_projection":[]`, `"field_projection":[{"capability_id":"catalog.find_title","model_visible_fields":[]}]`, 1), "field names"},
		{"projection with empty roles", strings.Replace(ok, `"field_projection":[]`, `"field_projection":[{"capability_id":"catalog.find_title","roles":[],"model_visible_fields":["query"]}]`, 1), "identifiers"},
		{"risk override as a float", strings.Replace(ok, `"risk_overrides":[]`, `"risk_overrides":[{"capability_id":"catalog.find_title","minimum_risk_level":2.0,"reason":"x"}]`, 1), "integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := ParseHostPolicy([]byte(tc.doc))
			if err == nil {
				t.Fatalf("accepted: %+v", h)
			}
			if got := contracts.CodeOf(err); got != contracts.ErrSchemaInvalid {
				t.Errorf("code = %q, want schema_invalid", got)
			}
			if tc.path != "" && !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error %q does not mention %q", err, tc.path)
			}
		})
	}

	t.Run("a document over the size limit is refused unread", func(t *testing.T) {
		big := make([]byte, MaxPolicyBytes+1)
		for i := range big {
			big[i] = ' '
		}
		if _, err := ParseHostPolicy(big); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an M1-01 policy parses with no rules and no format", func(t *testing.T) {
		h, err := ParseHostPolicy([]byte(ok))
		if err != nil || h.Format != "" || len(h.Rules) != 0 {
			t.Fatalf("h = %+v, err = %v", h, err)
		}
	})
}

func manyFields(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, `"f`+string(rune('a'+i))+`":{"present":true}`)
	}
	return strings.Join(parts, ",")
}

func manyRules(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, `{"id":"r`+itoa(i)+`","capabilities":["catalog.find_title"],"when":{"roles":{"any":["member"]}},"then":{"deny":true},"reason":"x"}`)
	}
	return strings.Join(parts, ",")
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// "Allow comes from one place only": no shape of policy document can grant a
// capability that role_grants does not.
func TestNoRuleShapeCanGrant(t *testing.T) {
	// Structural guard: the effect of a rule has exactly the three narrowing
	// or raising fields. Adding a fourth (an allow, a risk lowering) breaks
	// this test on purpose and needs a new format version and a security review.
	var got []string
	et := reflect.TypeOf(Effect{})
	for i := 0; i < et.NumField(); i++ {
		got = append(got, et.Field(i).Name)
	}
	if !equalStrings(got, []string{"Deny", "RiskMin", "HideFields"}) {
		t.Fatalf("Effect fields = %v", got)
	}

	// Behavioural guard: for every rule document in the corpus, a principal
	// that role_grants does not grant stays denied.
	g, _ := newGateway()
	for _, dir := range []string{"policy/examples/host-policy/valid"} {
		for _, f := range exampleFiles(t, dir) {
			raw, _ := readJSONFile(t, f)
			h, err := ParseHostPolicy(raw)
			if err != nil {
				t.Fatal(err)
			}
			var capIDs []string
			grantedTo := map[string]bool{}
			for _, gr := range h.RoleGrants {
				grantedTo[gr.Role] = true
			}
			seen := map[string]bool{}
			for _, gr := range h.RoleGrants {
				for _, c := range gr.Capabilities {
					if !seen[c] {
						seen[c] = true
						capIDs = append(capIDs, c)
					}
				}
			}
			var decls []CapabilityDecl
			roles := map[string]bool{"outsider": true}
			for r := range grantedTo {
				roles[r] = true
			}
			for _, c := range capIDs {
				decls = append(decls, CapabilityDecl{ID: c, Audience: AudienceUser, EffectClass: EffectNone, RiskLevel: 0, Confirmation: ConfirmNone})
			}
			var roleList []string
			for r := range roles {
				roleList = append(roleList, r)
			}
			pkg, err := NewPackage(PackageSpec{
				ApplicationID: appID, Version: "1", Status: StatusActive, Roles: roleList,
				Capabilities: stripFieldRefs(decls, h), Policies: []*HostPolicy{stripFieldConditions(h)},
			})
			if err != nil {
				t.Fatalf("%s: %v", filepath.Base(f), err)
			}
			s := scopeWith(capIDs, withRoles("outsider"))
			for _, c := range capIDs {
				for _, ph := range []Phase{PhaseExpose, PhasePrepare, PhaseCommit} {
					d := decide(g, pkg, s, c, ph, func(r *Request) { r.Evidence = Evidence{Confirmed: true, StepUp: true} })
					if d.Allowed {
						t.Fatalf("%s: a role without a grant was allowed %s at %s", filepath.Base(f), c, ph)
					}
				}
			}
		}
	}
}

// stripFieldRefs and stripFieldConditions let a corpus document be loaded
// against synthetic capabilities that declare no fields: field-referring
// content is dropped, everything that could grant is kept.
func stripFieldRefs(decls []CapabilityDecl, h *HostPolicy) []CapabilityDecl {
	need := map[string]map[string]bool{}
	add := func(capID, field string) {
		if need[capID] == nil {
			need[capID] = map[string]bool{}
		}
		need[capID][field] = true
	}
	for _, fp := range h.FieldProjection {
		for _, f := range fp.ModelVisibleFields {
			add(fp.CapabilityID, f)
		}
	}
	for _, r := range h.Rules {
		for _, c := range r.Capabilities {
			for _, f := range r.Then.HideFields {
				add(c, f)
			}
			for f := range r.When.Fields {
				add(c, f)
			}
		}
	}
	out := make([]CapabilityDecl, len(decls))
	for i, d := range decls {
		out[i] = d
		for _, f := range sortedKeys(need[d.ID]) {
			out[i].Fields = append(out[i].Fields, FieldDecl{Name: f})
		}
	}
	return out
}

func stripFieldConditions(h *HostPolicy) *HostPolicy {
	cp := *h
	// Rules in the corpus may name capabilities the grants do not; keep only
	// those a grant mentions so the package accepts the document.
	granted := map[string]bool{}
	for _, g := range h.RoleGrants {
		for _, c := range g.Capabilities {
			granted[c] = true
		}
	}
	cp.RiskOverrides = nil
	for _, o := range h.RiskOverrides {
		if granted[o.CapabilityID] {
			cp.RiskOverrides = append(cp.RiskOverrides, o)
		}
	}
	cp.FieldProjection = nil
	for _, fp := range h.FieldProjection {
		if granted[fp.CapabilityID] {
			cp.FieldProjection = append(cp.FieldProjection, fp)
		}
	}
	cp.Rules = nil
	for _, r := range h.Rules {
		var caps []string
		for _, c := range r.Capabilities {
			if granted[c] {
				caps = append(caps, c)
			}
		}
		if len(caps) > 0 {
			r.Capabilities = caps
			cp.Rules = append(cp.Rules, r)
		}
	}
	return &cp
}

func TestCompilePoliciesRefusesAmbiguity(t *testing.T) {
	p := func(id, body string) *HostPolicy {
		return mustParsePolicy(t, `{"policy_id":"`+id+`",`+body+`}`)
	}
	grant := func(role, cap string) string {
		return `"role_grants":[{"role":"` + role + `","capabilities":["` + cap + `"]}],"risk_overrides":[],"field_projection":[]`
	}
	if _, err := CompilePolicies(); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("no documents: %v", err)
	}
	if _, err := CompilePolicies(nil); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("nil document: %v", err)
	}
	if _, err := CompilePolicies(&HostPolicy{PolicyID: "Bad"}); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("unvalidated document: %v", err)
	}

	conflicts := map[string][]*HostPolicy{
		"the same policy ID twice": {p("a", grant("member", "x.one")), p("a", grant("visitor", "x.two"))},
		"a role granted in two entries of one policy": {p("a",
			`"role_grants":[{"role":"member","capabilities":["x.one"]},{"role":"member","capabilities":["x.two"]}],"risk_overrides":[],"field_projection":[]`)},
		"a role granted in two policies": {p("a", grant("member", "x.one")), p("b", grant("member", "x.two"))},
	}
	conflicts["two overrides for a capability in one policy"] = []*HostPolicy{p("a",
		`"role_grants":[{"role":"member","capabilities":["x.one"]}],"risk_overrides":[{"capability_id":"x.one","minimum_risk_level":1,"reason":"a"},{"capability_id":"x.one","minimum_risk_level":2,"reason":"b"}],"field_projection":[]`)}
	conflicts["two overrides for a capability across policies"] = []*HostPolicy{
		p("a", `"role_grants":[{"role":"member","capabilities":["x.one"]}],"risk_overrides":[{"capability_id":"x.one","minimum_risk_level":1,"reason":"a"}],"field_projection":[]`),
		p("b", `"role_grants":[{"role":"visitor","capabilities":["x.one"]}],"risk_overrides":[{"capability_id":"x.one","minimum_risk_level":2,"reason":"b"}],"field_projection":[]`),
	}
	conflicts["two general projections"] = []*HostPolicy{
		p("a", `"role_grants":[{"role":"member","capabilities":["x.one"]}],"risk_overrides":[],"field_projection":[{"capability_id":"x.one","model_visible_fields":["a"]},{"capability_id":"x.one","model_visible_fields":["b"]}]`)}
	conflicts["two projections for one role"] = []*HostPolicy{
		p("a", `"role_grants":[{"role":"member","capabilities":["x.one"]}],"risk_overrides":[],"field_projection":[{"capability_id":"x.one","roles":["member","staff"],"model_visible_fields":["a"]},{"capability_id":"x.one","roles":["member"],"model_visible_fields":["b"]}]`)}
	conflicts["a repeated rule ID across policies"] = []*HostPolicy{
		p("a", grant("member", "x.one")+`,"rules":[{"id":"r","capabilities":["x.one"],"when":{"roles":{"any":["member"]}},"then":{"deny":true},"reason":"a"}]`),
		p("b", grant("visitor", "x.one")+`,"rules":[{"id":"r","capabilities":["x.one"],"when":{"roles":{"any":["visitor"]}},"then":{"deny":true},"reason":"b"}]`),
	}
	for name, docs := range conflicts {
		if _, err := CompilePolicies(docs...); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%s: compiled (err %v)", name, err)
		}
	}

	// Not conflicts: a specific projection beside a general one, and grants of
	// different roles in different documents.
	ok := []*HostPolicy{
		p("a", `"role_grants":[{"role":"member","capabilities":["x.one"]}],"risk_overrides":[],"field_projection":[{"capability_id":"x.one","model_visible_fields":["a"]},{"capability_id":"x.one","roles":["member"],"model_visible_fields":["b"]}]`),
		p("b", grant("visitor", "x.one")),
	}
	pol, err := CompilePolicies(ok...)
	if err != nil {
		t.Fatalf("compatible policies refused: %v", err)
	}
	if got := pol.Refs(); len(got) != 2 || got[0].PolicyID != "a" || got[1].PolicyID != "b" {
		t.Errorf("refs = %+v, want ordered by policy ID", got)
	}
	// Order of the arguments does not change the result.
	rev, err := CompilePolicies(ok[1], ok[0])
	if err != nil || !equalRefs(rev.Refs(), pol.Refs()) {
		t.Errorf("reversed compile: %v / %+v", err, rev.Refs())
	}
}

func equalRefs(a, b []PolicyRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewPackageChecksPolicyReferences(t *testing.T) {
	spec := func(doc string) PackageSpec {
		return PackageSpec{
			ApplicationID: appID, Version: "1", Status: StatusActive, Roles: demoRoles(), Capabilities: demoCaps(),
			Policies: []*HostPolicy{mustParsePolicy(t, doc)},
		}
	}
	const base = `"policy_id":"p","role_grants":[{"role":"member","capabilities":["catalog.find_title"]}],"risk_overrides":[],"field_projection":[]`
	bad := map[string]string{
		"grant of an undeclared capability":    `"policy_id":"p","role_grants":[{"role":"member","capabilities":["invoices.void"]}],"risk_overrides":[],"field_projection":[]`,
		"grant to an undeclared role":          `"policy_id":"p","role_grants":[{"role":"ghost","capabilities":["catalog.find_title"]}],"risk_overrides":[],"field_projection":[]`,
		"override of an undeclared capability": `"policy_id":"p","role_grants":[{"role":"member","capabilities":["catalog.find_title"]}],"risk_overrides":[{"capability_id":"invoices.void","minimum_risk_level":2,"reason":"x"}],"field_projection":[]`,
		"projection of an undeclared field":    `"policy_id":"p","role_grants":[{"role":"member","capabilities":["catalog.find_title"]}],"risk_overrides":[],"field_projection":[{"capability_id":"catalog.find_title","model_visible_fields":["nope"]}]`,
		"projection for an undeclared role":    `"policy_id":"p","role_grants":[{"role":"member","capabilities":["catalog.find_title"]}],"risk_overrides":[],"field_projection":[{"capability_id":"catalog.find_title","roles":["ghost"],"model_visible_fields":["query"]}]`,
		"rule on an undeclared capability":     base + `,"rules":[{"id":"r","capabilities":["invoices.void"],"when":{"roles":{"any":["member"]}},"then":{"deny":true},"reason":"x"}]`,
		"rule naming an undeclared role":       base + `,"rules":[{"id":"r","capabilities":["catalog.find_title"],"when":{"roles":{"any":["ghost"]}},"then":{"deny":true},"reason":"x"}]`,
		"rule hiding an undeclared field":      base + `,"rules":[{"id":"r","capabilities":["catalog.find_title"],"when":{"roles":{"any":["member"]}},"then":{"hide_fields":["nope"]},"reason":"x"}]`,
		"rule testing an undeclared field":     base + `,"rules":[{"id":"r","capabilities":["catalog.find_title"],"when":{"fields":{"nope":{"present":true}}},"then":{"deny":true},"reason":"x"}]`,
	}
	for name, doc := range bad {
		if _, err := NewPackage(spec("{" + doc + "}")); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%s: NewPackage err = %v", name, err)
		}
	}
	if _, err := NewPackage(spec("{" + base + "}")); err != nil {
		t.Errorf("valid policy refused: %v", err)
	}

	// The package itself.
	dup := PackageSpec{ApplicationID: appID, Capabilities: append(demoCaps(), demoCaps()[0])}
	if _, err := NewPackage(dup); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("duplicate capability: %v", err)
	}
	if _, err := NewPackage(PackageSpec{ApplicationID: "", Capabilities: demoCaps()}); err == nil {
		t.Error("empty application ID accepted")
	}
	if _, err := NewPackage(PackageSpec{ApplicationID: appID, Roles: []string{"a", "a"}}); err == nil {
		t.Error("duplicate role accepted")
	}
	if _, err := NewPackage(PackageSpec{ApplicationID: appID, Capabilities: []CapabilityDecl{{ID: "x.do", Audience: AudienceUser, EffectClass: EffectNone, RiskLevel: 5, Confirmation: ConfirmNone}}}); err == nil {
		t.Error("invalid risk level accepted")
	}
	// A new package is not active unless it says so.
	p, err := NewPackage(PackageSpec{ApplicationID: appID})
	if err != nil || p.Status() == StatusActive {
		t.Errorf("status = %q, err = %v: a package must not default to active", p.Status(), err)
	}
}

func TestPolicyDigestIsCanonical(t *testing.T) {
	a := mustParsePolicy(t, `{"policy_id":"p","role_grants":[{"role":"member","capabilities":["a.b"]}],"risk_overrides":[],"field_projection":[]}`)
	b := mustParsePolicy(t, "{\n  \"field_projection\": [],\n  \"risk_overrides\": [ ],\n  \"role_grants\": [ { \"capabilities\": [\"a.b\"], \"role\": \"member\" } ],\n  \"policy_id\": \"p\"\n}")
	c := mustParsePolicy(t, `{"policy_id":"p","role_grants":[{"role":"member","capabilities":["a.c"]}],"risk_overrides":[],"field_projection":[]}`)
	da, _ := a.Digest()
	db, _ := b.Digest()
	dc, _ := c.Digest()
	if len(da) != 64 || da != db {
		t.Errorf("same policy, different spelling: %q vs %q", da, db)
	}
	if da == dc {
		t.Error("different policies share a digest")
	}
	// A programmatic document with nil slices digests the same as the parsed one.
	d := &HostPolicy{PolicyID: "p", RoleGrants: []RoleGrant{{Role: "member", Capabilities: []string{"a.b"}}}}
	dd, err := d.Digest()
	if err != nil || dd != da {
		t.Errorf("programmatic digest %q (%v), want %q", dd, err, da)
	}
}

func TestParseCapabilityDecl(t *testing.T) {
	want := map[string]struct {
		effect EffectClass
		risk   contracts.RiskLevel
		conf   ConfirmationMode
		fields int
	}{
		"catalog-find-title.json":             {EffectNone, 0, ConfirmNone, 3},
		"field-local-only.json":               {EffectNone, 0, ConfirmNone, 3},
		"reservations-create.json":            {EffectExternal, 2, ConfirmExplicit, 3},
		"members-delete-account-step-up.json": {EffectPrivileged, 3, ConfirmStepUp, 0},
		"loans-extend-immediate.json":         {EffectReversible, 1, ConfirmNone, 0},
	}
	for _, f := range exampleFiles(t, "application-package/examples/capability/valid") {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ParseCapabilityDecl(raw)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
		w, ok := want[filepath.Base(f)]
		if !ok {
			t.Fatalf("unexpected example %s", filepath.Base(f))
		}
		if c.EffectClass != w.effect || c.RiskLevel != w.risk || c.Confirmation != w.conf || c.Audience != AudienceUser {
			t.Errorf("%s: %+v", filepath.Base(f), c)
		}
		if w.fields != 0 && len(c.Fields) != w.fields {
			t.Errorf("%s: %d fields", filepath.Base(f), len(c.Fields))
		}
		if floor, _ := c.EffectClass.Floor(); c.RiskLevel < floor {
			t.Errorf("%s: declared level %d below its floor %d (the package schema forbids this)", filepath.Base(f), c.RiskLevel, floor)
		}
	}
	// The sensitive flag survives.
	raw, _ := os.ReadFile(filepath.Join(schemaRoot, "application-package", "examples", "capability", "valid", "reservations-create.json"))
	c, _ := ParseCapabilityDecl(raw)
	var sensitive []string
	for _, f := range c.Fields {
		if f.Sensitive {
			sensitive = append(sensitive, f.Name)
		}
	}
	if !equalStrings(sensitive, []string{"notes"}) {
		t.Errorf("sensitive fields = %v", sensitive)
	}

	// A document the gateway cannot decide on is refused, never defaulted.
	for _, name := range []string{"missing-risk-level.json", "missing-effect-class.json", "missing-audience.json"} {
		raw, err := os.ReadFile(filepath.Join(schemaRoot, "application-package", "examples", "capability", "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseCapabilityDecl(raw); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%s: err = %v, want schema_invalid", name, err)
		}
	}
	for name, doc := range map[string]string{
		"not json":              `{`,
		"unknown effect class":  `{"id":"a.b","audience":"user","effect_class":"harmless","risk_level":0,"confirmation":{"mode":"none"}}`,
		"unknown audience":      `{"id":"a.b","audience":"robot","effect_class":"none","risk_level":0,"confirmation":{"mode":"none"}}`,
		"null risk":             `{"id":"a.b","audience":"user","effect_class":"none","risk_level":null,"confirmation":{"mode":"none"}}`,
		"risk out of range":     `{"id":"a.b","audience":"user","effect_class":"none","risk_level":4,"confirmation":{"mode":"none"}}`,
		"risk as a string":      `{"id":"a.b","audience":"user","effect_class":"none","risk_level":"0","confirmation":{"mode":"none"}}`,
		"no confirmation":       `{"id":"a.b","audience":"user","effect_class":"none","risk_level":0}`,
		"unknown confirmation":  `{"id":"a.b","audience":"user","effect_class":"none","risk_level":0,"confirmation":{"mode":"maybe"}}`,
		"wildcard id":           `{"id":"a.*","audience":"user","effect_class":"none","risk_level":0,"confirmation":{"mode":"none"}}`,
		"field without name":    `{"id":"a.b","audience":"user","effect_class":"none","risk_level":0,"confirmation":{"mode":"none"},"fields":[{"required":true}]}`,
		"duplicate field names": `{"id":"a.b","audience":"user","effect_class":"none","risk_level":0,"confirmation":{"mode":"none"},"fields":[{"name":"x","required":true},{"name":"x","required":false}]}`,
	} {
		if _, err := ParseCapabilityDecl([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
