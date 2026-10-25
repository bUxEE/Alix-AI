package policy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestDecisionRecordExamplesBehaveAsDeclared(t *testing.T) {
	schema := compileSchema(t, "urn:alixai:schema:policy:decision-record:v1")
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{"policy/examples/decision-record/valid", true},
		{"policy/examples/decision-record/invalid", false},
	} {
		for _, f := range exampleFiles(t, c.dir) {
			_, inst := readJSONFile(t, f)
			if err := schema.Validate(inst); (err == nil) != c.want {
				t.Errorf("%s/%s: accepted=%v, want %v: %v", c.dir, filepath.Base(f), err == nil, c.want, err)
			}
		}
	}
}

// The log redactor matches key names by substring (internal/log). A record that
// reaches the log as a structured value must not trip it, or the audit trail
// would be silently censored.
func TestRecordKeysAvoidRedactedSubstrings(t *testing.T) {
	g, _ := newGateway()
	pkg := demoPackage(t)
	s := scopeWith(allUserCaps())
	forbidden := []string{"token", "secret", "password", "authorization", "apikey", "cookie", "privatekey", "credential"}

	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				lk := strings.ToLower(k)
				for _, f := range forbidden {
					if strings.Contains(lk, f) {
						t.Errorf("record key %s.%s contains %q", path, k, f)
					}
				}
				walk(path+"."+k, val)
			}
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
		}
	}
	for _, d := range []Decision{
		decide(g, pkg, s, capReserve, PhaseExpose),
		decide(g, pkg, scopeWith([]string{capFind}), capReserve, PhaseExpose),
	} {
		raw, err := json.Marshal(d.Record)
		if err != nil {
			t.Fatal(err)
		}
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			t.Fatal(err)
		}
		walk("record", tree)
	}
}

// The errors a denial maps to are all in the shared taxonomy, so the HTTP and
// SDK layers can pass them on unchanged.
func TestDenialCodesAreInTheErrorTaxonomy(t *testing.T) {
	g, _ := newGateway()
	pkg := demoPackage(t)
	s := scopeWith([]string{capFind})
	seen := map[contracts.ErrCode]bool{}
	for _, d := range []Decision{
		decide(g, pkg, s, capReserve, PhaseExpose),
		decide(g, pkg.WithStatus(StatusQuarantined), s, capFind, PhaseExpose),
		decide(g, pkg, s, "no.such", PhaseExpose),
		decide(g, pkg, scopeWith(allUserCaps(), withRoles("visitor")), capReserve, PhaseExpose),
		decide(g, pkg, s, capFind, PhaseExpose, func(r *Request) { r.Binding.TenantID = tenantB }),
		decide(g, pkg, scopeWith(allUserCaps(), withRoles("staff")), capReserve, PhaseCommit),
		decide(g, pkg, scopeWith(allUserCaps(), withRoles("staff")), capDelete, PhaseCommit, func(r *Request) { r.Evidence.Confirmed = true }),
	} {
		if d.Allowed || !d.Code.Valid() {
			t.Fatalf("decision %+v", d)
		}
		seen[d.Code] = true
	}
	for _, want := range []contracts.ErrCode{
		contracts.ErrScopeViolation, contracts.ErrQuarantined, contracts.ErrUnknownCapability,
		contracts.ErrPermissionDenied, contracts.ErrConfirmationRequired, contracts.ErrStepUpRequired,
	} {
		if !seen[want] {
			t.Errorf("no decision produced %s", want)
		}
	}
}
