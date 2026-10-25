package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// What the audit of the clinic reference application showed the probes could not do
// (M1-27): a capability whose validation rule asks for one of several optional fields,
// a seed that has to find a fixture principal, and a change of a record on request.

// ---- one-of fields ---------------------------------------------------------------

// jsonOf decodes a value of a generated package type from its JSON form, so that this file
// needs no package beyond the package types.
func jsonOf[T any](t testing.TB, doc string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// contactCapability is the shape of patients.update_contact: a required entity and two
// optional fields of which a rule requires one.
func contactCapability(t testing.TB, rule string, examples ...ap.CapabilityExample) ap.Capability {
	t.Helper()
	str := func(name string, min int) ap.CapabilityField {
		return jsonOf[ap.CapabilityField](t, fmt.Sprintf(`{"name":%q,"type":"string","required":false,"description":%q,"min_length":%d}`, name, name, min))
	}
	return ap.Capability{
		ID: "contacts.update", Audience: ap.CapabilityAudienceUser, ScopeLevel: ap.CapabilityScopeLevelWorkspace,
		Fields: []ap.CapabilityField{
			{Name: "patient", Type: ap.CapabilityFieldTypeEntityRef, Required: true, EntityType: ptr(ap.Slug("patient")), Description: "the patient"},
			str("phone", 5),
			str("email", 0),
		},
		Validation: []ap.CapabilityValidationRule{
			jsonOf[ap.CapabilityValidationRule](t, fmt.Sprintf(`{"id":"contact_change_present","fields":["phone","email"],"description":%q,"error_code":"validation_failed"}`, rule)),
			jsonOf[ap.CapabilityValidationRule](t, `{"id":"phone_format","fields":["phone"],"description":"A phone number holds at least 5 digits.","error_code":"validation_failed"}`),
		},
		Examples: examples,
	}
}

func example(id string, fields map[string]any) ap.CapabilityExample {
	return ap.CapabilityExample{ID: ap.Slug(id), ExpectedFields: fields}
}

// patientWorld is a world with one patient in the principal's own workspace.
func patientWorld() (*world, *wPrincipal) {
	w := &world{
		tenants: map[string]string{"t": "tnt_t"}, workspaces: map[string]string{"w": "ws_w"},
		records: map[string]*wRecord{"p1": {ref: "p1", entityType: "patient", tenantRef: "t", workspaceRef: "w", id: "pat_1"}},
	}
	return w, &wPrincipal{ref: "r", id: "usr_r", tenantRef: "t", workspaceRef: "w"}
}

func TestFieldsFillOneOfRequiredByARule(t *testing.T) {
	const oneOf = "At least one of the phone number and the email address must be given."
	phone := example("en-phone", map[string]any{"patient": "Maria", "phone": "+1 555 0123"})
	email := example("en-email", map[string]any{"patient": "me", "email": "maria@example.test"})

	cases := []struct {
		name    string
		c       ap.Capability
		want    map[string]any
		wantErr string
	}{
		{"the first field of the rule that an example gives a value", contactCapability(t, oneOf, phone, email),
			map[string]any{"patient": "pat_1", "phone": "+1 555 0123"}, ""},
		{"the next field when the first has no stated value", contactCapability(t, oneOf, email),
			map[string]any{"patient": "pat_1", "email": "maria@example.test"}, ""},
		{"the Italian wording of the rule", contactCapability(t, "Va indicato almeno uno tra telefono ed email.", phone),
			map[string]any{"patient": "pat_1", "phone": "+1 555 0123"}, ""},
		{"a rule that says nothing of the kind asks for no value", contactCapability(t, "The phone number and the email address must not be the same.", phone, email),
			map[string]any{"patient": "pat_1"}, ""},
		{"a rule whose fields have no stated value names the rule", contactCapability(t, oneOf),
			nil, `rule "contact_change_present" (one of "phone", "email")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, p := patientWorld()
			got, err := w.fieldsFor(tc.c, p, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "declare an example or a default") {
					t.Fatalf("error = %v, want one naming %s and the remediation", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("fields = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("field %s = %v, want %v (all: %v)", k, got[k], v, got)
				}
			}
		})
	}

	t.Run("a rule one of whose fields the request already carries adds nothing", func(t *testing.T) {
		w, p := patientWorld()
		c := contactCapability(t, oneOf, phone, email)
		c.Fields[2].Required = true // email is required: the rule is no longer over optional fields
		got, err := w.fieldsFor(c, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, has := got["phone"]; has || got["email"] != "maria@example.test" {
			t.Errorf("fields = %v: the required email satisfies the rule, no phone is added", got)
		}
	})

	t.Run("a rule over one field is a constraint on a value, not a request for one", func(t *testing.T) {
		w, p := patientWorld()
		c := contactCapability(t, oneOf, phone, email)
		c.Validation = []ap.CapabilityValidationRule{
			jsonOf[ap.CapabilityValidationRule](t, `{"id":"phone_present","fields":["phone"],"description":"At least one phone number must be given.","error_code":"validation_failed"}`),
		}
		got, err := w.fieldsFor(c, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, has := got["phone"]; has {
			t.Errorf("fields = %v", got)
		}
	})

	t.Run("the clinic's update_contact is probed with a phone number it accepts", func(t *testing.T) {
		pkg := loadRef(t, clinicDir)
		suite := pkg.Tests[0]
		base := contracts.Scope{DeploymentID: "alixai-audit", ApplicationID: string(pkg.Manifest.Application.ID)}
		w, _, err := provision(context.Background(), idProvisioner{}, suite, base, pkg.SortedCapabilityIDs(), clock.NewFake(simT0))
		if err != nil {
			t.Fatal(err)
		}
		c := pkg.Capabilities["patients.update_contact"]
		for _, probe := range []struct{ principal, target string }{{"patient-a1", "patient-rec-a1"}, {"reception-a1", "patient-rec-a1b"}} {
			got, err := w.fieldsFor(c, w.principals[probe.principal], w.records[probe.target])
			if err != nil {
				t.Fatalf("%s: %v", probe.principal, err)
			}
			if got["patient"] != w.records[probe.target].id || got["phone"] != "+1 555 0123" || len(got) != 2 {
				t.Errorf("%s: fields = %v, want the patient and the phone of the capability's example", probe.principal, got)
			}
		}
	})
}

// ---- principals by reference -----------------------------------------------------

// refRecorder is the sim behind a provisioner that remembers the tenants it was asked
// to create, so a test can see exactly what a transport would be given. Embedding the
// sim keeps its RecordChanger and FaultInjector.
type refRecorder struct {
	*sim
	mu      sync.Mutex
	tenants []Tenant
}

func (r *refRecorder) CreateTenant(ctx context.Context, t Tenant) error {
	r.mu.Lock()
	r.tenants = append(r.tenants, t)
	r.mu.Unlock()
	return r.sim.CreateTenant(ctx, t)
}

// A transport that has to turn a reference of the seed data into the identifier the
// audit generated reads Principal.Ref. It must not need to know how the identifier is
// built, so the scheme can change without breaking every seed.
func TestPrincipalRefResolvesWithoutTheNamingScheme(t *testing.T) {
	was := principalIDOf
	principalIDOf = func(ref, suffix string) string {
		return "p" + strings.ToUpper(suffix) + "-" + strings.ToUpper(slug(ref))
	}
	t.Cleanup(func() { principalIDOf = was })

	e := newEnv(t)
	rec := &refRecorder{sim: e.sim}
	opts := e.opts()
	opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) { return rec, nil }
	var check integrationaudit.Check
	for _, c := range Checks(opts) {
		if c.Section() == integrationaudit.SectionScope {
			check = c
		}
	}
	out, err := check.Run(context.Background(), e.in)
	if err != nil {
		t.Fatal(err)
	}
	if hasCritical(out) {
		t.Fatalf("a different identifier scheme must not matter to a correct host:\n%s", dump(out))
	}

	// resolve is what a provisioner does with a reference of its seed data
	resolve := func(t Tenant, ref string) (string, bool) {
		for _, p := range t.Principals {
			if p.Ref == ref {
				return p.ID, true
			}
		}
		return "", false
	}
	suite := simSuite()
	if len(rec.tenants) == 0 {
		t.Fatal("no tenant was created")
	}
	seen := 0
	for _, tn := range rec.tenants {
		refs := map[string]bool{}
		for _, p := range tn.Principals {
			if p.Ref == "" {
				t.Errorf("principal %s of tenant %s carries no reference", p.ID, tn.ID)
			}
			if refs[p.Ref] {
				t.Errorf("reference %s is carried by two principals of tenant %s", p.Ref, tn.ID)
			}
			refs[p.Ref] = true
			if strings.HasPrefix(p.ID, "usr_") {
				t.Errorf("principal %s was named by the old scheme although the scheme changed: the test proves nothing", p.ID)
			}
		}
		for _, fp := range suite.Fixtures.Principals {
			if !strings.HasPrefix(tn.ID, "tnt_"+slug(string(fp.Tenant))+"_") {
				continue
			}
			id, ok := resolve(tn, string(fp.Ref))
			if !ok {
				t.Errorf("fixture principal %s of tenant %s cannot be found by its reference", fp.Ref, tn.ID)
				continue
			}
			if want := principalIDOf(string(fp.Ref), tn.ID[strings.LastIndexByte(tn.ID, '_')+1:]); id != want {
				t.Errorf("principal %s resolved to %s, the audit generated %s", fp.Ref, id, want)
			}
			seen++
		}
	}
	if seen == 0 {
		t.Error("no principal was resolved by its reference")
	}
}

// ---- change on request -----------------------------------------------------------

// With a provisioner that can change a record, Validate noticing a change and a
// preparation made before it being refused are both shown, and nothing is left
// unverified; without one the same two are recorded as unverified, never passed.
func TestRecordChangerMakesValidateNoticesChangeVerified(t *testing.T) {
	notices := func(o *integrationaudit.Outcome) (unverified int) {
		for _, u := range o.Unverified {
			if u.Subject == "safety:validate_notices_change" {
				unverified++
			}
		}
		return unverified
	}

	e := newEnv(t)
	with := e.run(integrationaudit.SectionSafeExecution)
	if hasCritical(with) {
		t.Fatalf("a correct host that can change a record:\n%s", dump(with))
	}
	if n := notices(with); n != 0 {
		t.Errorf("%d validate_notices_change items unverified although the provisioner can change a record:\n%s", n, dump(with))
	}
	for _, subject := range []string{"validate notices a changed record", "a preparation made before a record changed is refused"} {
		p := probeNamed(with, subject)
		if p == nil || !p.Passed || p.Capability != capUpdate {
			t.Errorf("probe %q = %+v, want it run and passed for %s", subject, p, capUpdate)
		}
	}

	e = newEnv(t)
	opts := e.opts()
	opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) {
		return plainProvisioner{e.sim}, nil
	}
	var check integrationaudit.Check
	for _, c := range Checks(opts) {
		if c.Section() == integrationaudit.SectionSafeExecution {
			check = c
		}
	}
	without, err := check.Run(context.Background(), e.in)
	if err != nil {
		t.Fatal(err)
	}
	if n := notices(without); n != 1 {
		t.Errorf("%d validate_notices_change items unverified without a RecordChanger, want 1:\n%s", n, dump(without))
	}
	if p := probeNamed(without, "validate notices a changed record"); p != nil {
		t.Errorf("a probe that cannot run was recorded: %+v", p)
	}
}
