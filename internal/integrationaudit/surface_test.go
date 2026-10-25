package integrationaudit

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func opsOf(ops ...adapters.Operation) []adapters.Operation { return ops }

func messages(r Report, checkID string) string {
	var out []string
	for _, f := range findings(r, checkID) {
		out = append(out, f.Message)
	}
	return strings.Join(out, "\n")
}

func section(r Report, n int) SectionResult { return r.Sections[n-1] }

func capabilityStatus(r Report, id string) CapabilityStatus {
	for _, c := range r.Capabilities {
		if c.ID == id {
			return c.Status
		}
	}
	return ""
}

// SEC-048
func TestUndocumentedOperationReported(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	run := h.audit(newStub(t, func(s *stub) {
		s.desc.Unmapped = []string{"admin.purge_all", "export_everything"}
	}))
	rep := run.Signed.Report

	got := findings(rep, "surface.undocumented_operation")
	if len(got) != 2 {
		t.Fatalf("want 2 undocumented-operation findings, got %d:\n%s", len(got), reportJSON(rep))
	}
	for _, f := range got {
		if f.Severity != contracts.SeverityWarning {
			t.Errorf("an operation nobody can call is a warning, got %s", f.Severity)
		}
		if f.Remediation == "" || f.Capability != "" {
			t.Errorf("a host operation concerns no capability and needs remediation: %+v", f)
		}
	}
	msg := messages(rep, "surface.undocumented_operation")
	for _, name := range []string{`"admin.purge_all"`, `"export_everything"`} {
		if !strings.Contains(msg, name) {
			t.Errorf("the finding must name %s: %s", name, msg)
		}
	}
	if want := []string{"admin.purge_all", "export_everything"}; strings.Join(rep.Surface.Unmapped, ",") != strings.Join(want, ",") {
		t.Errorf("surface.unmapped = %v, want %v", rep.Surface.Unmapped, want)
	}
	if rep.Summary.Outcome != DispositionNeedsAcceptance {
		t.Errorf("an undocumented operation needs an operator to accept it: outcome %s", rep.Summary.Outcome)
	}
	// A faithful application reports none.
	h2 := newHarness(t)
	h2.install(validMinimalDir)
	clean := h2.audit(newStub(t)).Signed.Report
	if len(findings(clean, "surface.undocumented_operation")) != 0 {
		t.Errorf("a faithful application must report no undocumented operation")
	}
}

// SEC-048
func TestUnreachableOperationReported(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	run := h.audit(newStub(t, func(s *stub) {
		// commit is not offered for reservations.create, and the whole of
		// catalog.find_title is missing.
		s.desc.Capabilities = []adapters.CapabilityOps{
			{CapabilityID: "reservations.create", Operations: opsOf(adapters.OpValidate, adapters.OpPrepare, adapters.OpStatus)},
		}
	}))
	rep := run.Signed.Report

	got := findings(rep, "surface.unreachable_operation")
	if len(got) != 2 {
		t.Fatalf("want 2 unreachable findings (commit, read), got %d:\n%s", len(got), reportJSON(rep))
	}
	byCap := map[string]string{}
	for _, f := range got {
		if f.Severity != contracts.SeverityCritical {
			t.Errorf("a declared operation the application does not offer is critical, got %s", f.Severity)
		}
		byCap[f.Capability] = f.Message
	}
	if !strings.Contains(byCap["reservations.create"], "commit") || !strings.Contains(byCap["reservations.create"], "commit_reservation") {
		t.Errorf("the finding must name the operation and the mapped call: %q", byCap["reservations.create"])
	}
	if !strings.Contains(byCap["catalog.find_title"], "read") {
		t.Errorf("a capability missing from Describe is unreachable: %q", byCap["catalog.find_title"])
	}
	for _, id := range []string{"reservations.create", "catalog.find_title"} {
		if capabilityStatus(rep, id) != CapabilityFailed {
			t.Errorf("capability %s must be failed, got %s", id, capabilityStatus(rep, id))
		}
	}
	if section(rep, SectionControlSurface).Status != StatusFail || rep.Summary.Outcome != DispositionBlocked {
		t.Errorf("section 7 must fail and the report be blocked: %s / %s", section(rep, SectionControlSurface).Status, rep.Summary.Outcome)
	}
	// What is reachable is counted as such.
	for _, c := range rep.Coverage {
		if c.Name == "control_surface_operations" && (c.Covered != 3 || c.Total != 5) {
			t.Errorf("control_surface_operations = %d/%d, want 3/5", c.Covered, c.Total)
		}
	}
}

// SEC-048
func TestOverBroadOperationReported(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	run := h.audit(newStub(t, func(s *stub) {
		s.desc.Capabilities = []adapters.CapabilityOps{
			// declared read only: commit makes it over-broad and critical;
			// cancel is part of the adapter contract and is never over-broad;
			// validate is a read-only extra and only a warning.
			{CapabilityID: "catalog.find_title", Operations: opsOf(adapters.OpRead, adapters.OpCommit, adapters.OpValidate, adapters.OpCancel)},
			{CapabilityID: "reservations.create", Operations: opsOf(adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus, adapters.OpRead)},
			// a capability the package does not declare at all.
			{CapabilityID: "admin.export", Operations: opsOf(adapters.OpRead, adapters.OpCommit)},
		}
	}))
	rep := run.Signed.Report

	crit := findings(rep, "surface.over_broad_operation")
	if len(crit) != 1 || crit[0].Capability != "catalog.find_title" || crit[0].Severity != contracts.SeverityCritical {
		t.Fatalf("want one critical over-broad finding for catalog.find_title (commit), got %+v\n%s", crit, reportJSON(rep))
	}
	if !strings.Contains(crit[0].Message, "commit") {
		t.Errorf("the finding names the operation: %q", crit[0].Message)
	}
	minor := findings(rep, "surface.over_broad_operation_minor")
	if len(minor) != 2 {
		t.Fatalf("want 2 read-only over-broad warnings (validate on find_title, read on create), got %d", len(minor))
	}
	for _, f := range minor {
		if f.Severity != contracts.SeverityWarning {
			t.Errorf("a read-only extra is a warning: %+v", f)
		}
		if strings.Contains(f.Message, "cancel") {
			t.Errorf("cancel must never be reported as over-broad: %+v", f)
		}
	}
	undeclared := findings(rep, "surface.undeclared_capability")
	if len(undeclared) != 1 || !strings.Contains(undeclared[0].Message, "admin.export") || undeclared[0].Severity != contracts.SeverityWarning {
		t.Fatalf("want one undeclared-capability warning for admin.export, got %+v", undeclared)
	}
	if rep.Summary.Outcome != DispositionBlocked {
		t.Errorf("an operation that can commit under a capability that does not declare it blocks: %s", rep.Summary.Outcome)
	}
}

// An unmapped host operation that looks like a mapped one is ambiguous and
// critical; one that is exactly a mapped call is not unmapped at all.
func TestLookalikeOperationIsAmbiguous(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	rep := h.audit(newStub(t, func(s *stub) {
		s.desc.Unmapped = []string{"Commit-Reservation", "commit.reservation", "find_title", "something_else"}
	})).Signed.Report

	amb := findings(rep, "surface.ambiguous_operation")
	if len(amb) != 2 {
		t.Fatalf("want 2 ambiguous findings, got %d:\n%s", len(amb), reportJSON(rep))
	}
	for _, f := range amb {
		if f.Severity != contracts.SeverityCritical || !strings.Contains(f.Message, "commit_reservation") {
			t.Errorf("an ambiguous operation is critical and names the mapped one: %+v", f)
		}
	}
	und := findings(rep, "surface.undocumented_operation")
	if len(und) != 1 || !strings.Contains(und[0].Message, "something_else") {
		t.Errorf("only something_else is merely undocumented (find_title is mapped): %+v", und)
	}
	if rep.Summary.Outcome != DispositionBlocked {
		t.Errorf("ambiguity blocks: %s", rep.Summary.Outcome)
	}
}

func TestSurfaceVersionAndApplicationVersion(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	rep := h.audit(newStub(t, func(s *stub) {
		s.desc.ContractVersions = []int{99}
		s.desc.ApplicationVersion = "9.9.9"
	})).Signed.Report
	if !hasFinding(rep, "surface.version_incompatible", contracts.SeverityCritical) {
		t.Errorf("a host that speaks only another contract version is critical:\n%s", reportJSON(rep))
	}
	if !hasFinding(rep, "surface.application_version_mismatch", contracts.SeverityWarning) {
		t.Errorf("a running version other than the one the package was written for is a warning")
	}
}

func TestDescribeFailureIsCritical(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	rep := h.audit(newStub(t, func(s *stub) {
		s.descErr = contracts.New(contracts.ErrUnavailable, "the application said something private")
	})).Signed.Report
	if !hasFinding(rep, "surface.describe_failed", contracts.SeverityCritical) {
		t.Fatalf("an adapter that cannot describe the application blocks:\n%s", reportJSON(rep))
	}
	if section(rep, SectionControlSurface).Status == StatusPass {
		t.Errorf("section 7 must not pass when Describe failed")
	}
	if strings.Contains(reportJSON(rep), "something private") {
		t.Errorf("the error text of the application never reaches the report, only its typed code")
	}
	if !strings.Contains(messages(rep, "surface.describe_failed"), "unavailable") {
		t.Errorf("the finding carries the typed code")
	}
}
