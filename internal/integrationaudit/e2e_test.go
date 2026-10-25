package integrationaudit

import (
	"github.com/buxee/alix-ai/internal/apppkg"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The reference application's real signed package, from install to a served
// capability registry: the shape of the M1 activation path.
func TestClinicBookingReferencePackage(t *testing.T) {
	const app = "clinic-booking"
	h := newHarness(t)
	h.registerPublisher(app)
	res, err := h.inst.Install(ctx, apppkg.FromDir(clinicDir))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	rec := res.Record
	pkg, _, err := h.inst.Open(ctx, app, rec.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}

	faithful := func() *stub {
		return newStub(t, func(s *stub) {
			s.desc = descriptionFor(pkg)
			s.insp = inspectionOf(t, strings.ReplaceAll(okInspection, "2.4.0", pkg.Manifest.Application.Version))
			s.health.ApplicationVersion = pkg.Manifest.Application.Version
		})
	}
	run, err := h.runner.Audit(ctx, RunRequest{ApplicationID: app, Adapter: faithful()})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	rep := run.Signed.Report

	// the loader's accepted warning (resolve_patient is mapped by two
	// capabilities) is passed through and needs an operator
	shared := findings(rep, "adapter.operation_shared")
	if len(shared) != 1 || shared[0].Severity != contracts.SeverityWarning {
		t.Fatalf("want the loader's adapter.operation_shared warning, got %+v\n%s", shared, reportJSON(rep))
	}
	if rep.Summary.Critical != 0 || rep.Summary.Outcome != DispositionNeedsAcceptance {
		t.Errorf("summary = %+v", rep.Summary)
	}
	if section(rep, SectionControlSurface).Status != StatusPass {
		t.Errorf("section 7 = %s: a warning does not fail a section", section(rep, SectionControlSurface).Status)
	}
	if len(rep.Capabilities) != 5 {
		t.Errorf("capabilities = %d, want 5", len(rep.Capabilities))
	}
	for _, c := range rep.Capabilities {
		if c.Status != CapabilityPassed {
			t.Errorf("capability %s = %s", c.ID, c.Status)
		}
	}
	for _, c := range rep.Coverage {
		if c.Covered != c.Total {
			t.Errorf("coverage %s = %d/%d", c.Name, c.Covered, c.Total)
		}
	}
	if len(rep.RAGSources) == 0 {
		t.Errorf("the clinic package declares RAG sources")
	}

	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: app}); reason(err) != ReasonWarningsPending {
		t.Fatalf("Activate before acceptance = %v", err)
	}
	h.clk.Advance(time.Second)
	if _, err := h.runner.AcceptWarning(ctx, AcceptRequest{
		ApplicationID: app, FindingKey: shared[0].Key, OperatorID: "alice",
		Reason: "resolve_patient is deliberately shared by the booking and cancellation capabilities",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: app, OperatorID: "alice"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	info, err := h.reg.Info(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, id := range pkg.SortedCapabilityIDs() {
		if pkg.Capabilities[id].Audience == "user" {
			want = append(want, id)
		}
	}
	if diff := cmp.Diff(want, info.Capabilities); diff != "" {
		t.Errorf("the registry serves the package's user capabilities (-want +got):\n%s", diff)
	}

	// the loader's other direction: an application that drops an operation of the
	// consequential capability is blocked, on the real package too
	h2 := newHarness(t)
	h2.registerPublisher(app)
	h2.install(clinicDir)
	broken := faithful()
	for i := range broken.desc.Capabilities {
		if broken.desc.Capabilities[i].CapabilityID == "appointments.book" {
			broken.desc.Capabilities[i].Operations = []adapters.Operation{adapters.OpValidate, adapters.OpPrepare}
		}
	}
	bad, err := h2.runner.Audit(ctx, RunRequest{ApplicationID: app, Adapter: broken})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Disposition() != DispositionBlocked || !hasFinding(bad.Signed.Report, "surface.unreachable_operation", contracts.SeverityCritical) {
		t.Errorf("a missing commit blocks:\n%s", reportJSON(bad.Signed.Report))
	}
}

// The report records the hash of everything whose change calls for a new audit.
func TestReauditTriggersRecordHashes(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	first := h.audit(newStub(t)).Signed.Report

	kinds := map[TriggerKind]int{}
	for _, tr := range first.ReauditTriggers {
		kinds[tr.Kind]++
		if len(tr.Hash) != 64 || tr.Subject == "" {
			t.Errorf("trigger %+v", tr)
		}
	}
	want := map[TriggerKind]int{
		TriggerPackage: 1, TriggerDocumentation: 1, TriggerCapability: 2, TriggerContract: 4, TriggerSource: 1,
		TriggerPermissionModel: 1, TriggerAdapterSurface: 1, TriggerApplicationVersion: 1, TriggerCoreVersion: 1,
	}
	if diff := cmp.Diff(want, kinds); diff != "" {
		t.Errorf("trigger kinds (-want +got):\n%s", diff)
	}

	hash := func(r Report, kind TriggerKind, subject string) string {
		for _, tr := range r.ReauditTriggers {
			if tr.Kind == kind && tr.Subject == subject {
				return tr.Hash
			}
		}
		return ""
	}

	// a changed adapter surface moves only the adapter-surface hash
	h.clk.Advance(time.Second)
	second := h.audit(newStub(t, func(s *stub) { s.desc.Unmapped = []string{"new.op"} })).Signed.Report
	if hash(second, TriggerAdapterSurface, "fake") == hash(first, TriggerAdapterSurface, "fake") {
		t.Errorf("a new host operation must move the adapter-surface hash")
	}
	for _, k := range []struct {
		kind    TriggerKind
		subject string
	}{{TriggerPackage, appID}, {TriggerDocumentation, "documentation/index.yaml"}, {TriggerCapability, "reservations.create"}, {TriggerPermissionModel, "permissions"}} {
		if hash(second, k.kind, k.subject) != hash(first, k.kind, k.subject) {
			t.Errorf("%s/%s moved though the package did not change", k.kind, k.subject)
		}
	}

	// a changed package moves the package hash and the hash of what changed
	dir := copyDir(t, validMinimalDir)
	editFile(t, dir, "capabilities/reservations-create.yaml", "ttl_seconds: 600", "ttl_seconds: 300")
	editFile(t, dir, "capabilities/reservations-create.yaml", "window_seconds: 86400", "window_seconds: 86400")
	editFile(t, dir, "alixai.yaml", "version: 1.0.0\n  description: 'Initial", "version: 1.0.1\n  description: 'Initial")
	resign(t, dir, appID, "1.0.1")
	h.install(dir)
	h.clk.Advance(time.Second)
	third := h.audit(newStub(t)).Signed.Report
	if hash(third, TriggerPackage, appID) == hash(first, TriggerPackage, appID) {
		t.Errorf("a new package moves the package hash")
	}
	if hash(third, TriggerCapability, "reservations.create") == hash(first, TriggerCapability, "reservations.create") {
		t.Errorf("a changed capability contract moves its hash")
	}
	if hash(third, TriggerCapability, "catalog.find_title") != hash(first, TriggerCapability, "catalog.find_title") {
		t.Errorf("an unchanged capability keeps its hash")
	}
}
