package m1

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// renamedCommitTool makes the package say that the booking is committed through a tool the
// application does not have: the package and the application disagree about what the application
// can do. The package is valid and signed, so it installs; only the audit, which asks the running
// application, can tell.
func renamedCommitTool(t tb, dir string) {
	editFile(t, filepath.Join(dir, "capabilities", "appointments-book.yaml"), func(s string) string {
		return strings.Replace(s, "mcp_tool: appointments_book_commit\n", "mcp_tool: appointments_book_commit_v2\n", 1)
	})
	editFile(t, filepath.Join(dir, "schemas", "adapters", "clinic-mcp.yaml"), func(s string) string {
		return strings.Replace(s, "    - appointments_book_commit\n", "    - appointments_book_commit\n    - appointments_book_commit_v2\n", 1)
	})
}

// An application cannot activate until its integration audit is clear (master plan 4.5, 21): a
// package is installed into quarantine and serves nothing; an audit that finds a critical problem
// blocks it, and nobody can accept a critical finding; an audit that finds only warnings does not
// activate it until an operator who is authorised to says the warnings are acceptable; and the
// activation, like a quarantine, takes effect on the running service at once. The operator is the
// real command line, the audit probes the real application, and the service that is asked to serve
// is up throughout.
// SEC-060
func TestM1ActivationRequiresCleanAudit(t *testing.T) {
	clinic := startClinic(t)
	op := newOperator(t)
	dep := &deployment{clinic: clinic, op: op}
	w := seedWorld(t, clinic, "act")
	r := newRig(t, dep, withTemplateOnly())
	rc := r.convo(t, w, w.Reception)
	slots := w.slots(t, w.Reception)

	dirty := signedClinicPackage(t, clinic, renamedCommitTool)
	good := signedClinicPackage(t, clinic)
	op.addIssuerKey(good)
	start := clinic.stats(t)

	// ---- a package that disagrees with its application ----
	dirtyInst := op.install(dirty)
	servesNothing(t, r, dep, rc, "quarantined")

	run, res := op.audit()
	if res.Code != cli.ExitError || run.Activated || run.Outcome != "blocked" || run.StaysInQuarantine == "" {
		t.Fatalf("the audit of a package that disagrees with its application: exit %d\n%s\nstderr: %s", res.Code, run, res.Stderr)
	}
	if run.ManifestHash != dirtyInst.ManifestHash {
		t.Errorf("the audit ran on %s, the dirty package is %s", run.ManifestHash, dirtyInst.ManifestHash)
	}
	var critical auditFinding
	for _, f := range run.severities("critical") {
		if f.CheckID == "surface.unreachable_operation" && f.Capability == "appointments.book" {
			critical = f
		}
	}
	if critical.Key == "" {
		t.Fatalf("the audit did not find that the application has no tool to commit a booking with:\n%s", run)
	}
	if run.Sections[6].Status != "fail" { // section 7, the complete control surface
		t.Errorf("section 7 is %q, want fail", run.Sections[6].Status)
	}
	// nobody, however authorised, can accept a critical finding
	if got := op.run("audit", "accept-warning", "-finding", critical.Key, "-operator", gateOperator, "-reason", "the gate tries", clinicApplication); got.Code == cli.ExitOK {
		t.Errorf("a critical finding was accepted: %s", got.Stdout)
	}
	servesNothing(t, r, dep, rc, "quarantined")

	// ---- the right package: warnings are not consent ----
	goodInst := op.install(good)
	if goodInst.ManifestHash == dirtyInst.ManifestHash {
		t.Fatal("the two packages are one")
	}
	probes := clinic.stats(t)
	run, res = op.auditPackage(goodInst.ManifestHash)
	after := clinic.stats(t)
	if run.Summary.Critical != 0 || run.Outcome != "needs_acceptance" || run.Activated || res.Code != cli.ExitError {
		t.Fatalf("the audit of the clinic's own package: exit %d\n%s\nstderr: %s", res.Code, run, res.Stderr)
	}
	for _, n := range []int{1, 2, 7, 8, 9} { // authenticity, deterministic results, control surface, safe execution, degradation
		if run.Sections[n-1].Status != "pass" {
			t.Errorf("section %d (%s) is %q, want pass", n, run.Sections[n-1].Title, run.Sections[n-1].Status)
		}
	}
	// the audit really ran against the application: it provisioned disposable tenants, probed
	// through the capability tools, and left nothing behind
	if after.Calls["test_create_tenant"] <= probes.Calls["test_create_tenant"] || after.Calls["appointments_book_prepare"] <= probes.Calls["appointments_book_prepare"] {
		t.Errorf("the audit did not probe the application: %s", after.callsSince(probes))
	}
	if after.Calls["test_cleanup"] < after.Calls["test_create_tenant"]-probes.Calls["test_create_tenant"]+probes.Calls["test_cleanup"] {
		t.Errorf("the audit left a disposable tenant behind: %s", after.callsSince(probes))
	}
	if after.Totals["tenants"] != probes.Totals["tenants"] || after.Totals["appointments"] != probes.Totals["appointments"] {
		t.Errorf("the audit left rows in the application: before %v, after %v", probes.Totals, after.Totals)
	}
	warnings := run.severities("warning")
	if len(warnings) == 0 {
		t.Fatalf("the clinic's audit raised no warning for an operator to accept: %s", run)
	}
	servesNothing(t, r, dep, rc, "quarantined")

	// an operator the configuration does not authorise cannot accept them; one it does can; and
	// accepting them activates nothing by itself
	if got := op.run("audit", "accept-warning", "-finding", warnings[0].Key, "-operator", "mallory", "-reason", "looks fine to me", clinicApplication); got.Code == cli.ExitOK ||
		!strings.Contains(got.Stderr, "not authorised") {
		t.Errorf("an operator who is not authorised accepted a warning: exit %d\n%s", got.Code, got.Stderr)
	}
	for _, f := range warnings {
		op.acceptWarning(f.Key, "reviewed by the gate: "+f.CheckID)
	}
	servesNothing(t, r, dep, rc, "quarantined")

	// ---- the clear audit activates it, and the running service serves it at once ----
	run, res = op.auditPackage(goodInst.ManifestHash)
	if !run.Activated || res.Code != cli.ExitOK || run.Summary.Critical != 0 || run.StaysInQuarantine != "" {
		t.Fatalf("the audit with every warning accepted: exit %d\n%s\nstderr: %s", res.Code, run, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "the running server reloaded the capability registry") {
		t.Errorf("the activation did not tell the running service:\n%s", res.Stderr)
	}
	for _, f := range run.severities("warning") {
		if !f.Accepted || f.AcceptedBy != gateOperator {
			t.Errorf("the activating report does not show who accepted %s: %+v", f.CheckID, f)
		}
	}
	if status, pkgs, reply := rc.application(); status != "active" || len(pkgs) != 2 {
		t.Errorf("the application after the activation: %q with %d packages (%s)", status, len(pkgs), reply)
	} else {
		for _, p := range pkgs {
			if (p.ManifestHash == goodInst.ManifestHash) != (p.State == "active") {
				t.Errorf("package %s is %s: only the audited one is active", p.ManifestHash, p.State)
			}
		}
	}
	if opened := rc.open(); opened.Status != http.StatusOK {
		t.Fatalf("the service does not serve the activated application at once: %s", opened)
	}
	r.proposal(w.Tenant, "availability.list", "task", map[string]any{"date": slots[0].Date})
	read := rc.say("Which appointments are free on "+slots[0].Date+"?", "en")
	if !read.ran() || read.act() != "present_result" || read.code() != "" {
		t.Fatalf("a read after the activation: %s", read)
	}

	// ---- a booking waits for its confirmation when the package is quarantined ----
	booking := r.convo(t, w, w.Reception)
	booking.open()
	_, pid, secret := prepareBooking(t, r, w, booking, slots[0])
	before := clinic.stats(t)
	modelCalls := r.model.Calls()
	q := op.quarantine("the gate quarantines the application with a booking waiting")
	if !strings.Contains(q.Stderr, "evicted") {
		t.Errorf("the quarantine did not tell the running service:\n%s", q.Stderr)
	}
	servesNothing(t, r, dep, booking, "quarantined")
	if got := booking.confirmAction(pid, secret); got.code() != contracts.ErrQuarantined {
		t.Errorf("the booking that was waiting was confirmed after the quarantine: %s", got)
	}
	if got := booking.confirm(pid, secret); got.ran() || got.code() != contracts.ErrQuarantined {
		t.Errorf("a confirmation turn after the quarantine: %s", got)
	}
	end := clinic.stats(t)
	if end.commits() != before.commits() || end.totalsSince(start, "appointments") != 0 {
		t.Errorf("a commit reached the application: %s", end.callsSince(before))
	}
	if r.model.Calls() != modelCalls {
		t.Errorf("the model was called for a quarantined application")
	}

	// ---- the record of it ----
	events := eventTypes(r.applicationEvents())
	want := []string{
		"package.installed", "audit_run.started", "audit_run.completed", // the dirty package, blocked
		"package.installed", "audit_run.started", "audit_run.completed", // the clinic's, with warnings
		"audit_run.warning_accepted",
		"audit_run.started", "audit_run.completed", "package.activated",
		"package.quarantined",
	}
	if !subsequence(events, want) {
		t.Errorf("the application's audit chain lacks %v in order:\n%v", want, events)
	}
	activations := 0
	for _, e := range events {
		if e == "package.activated" {
			activations++
		}
	}
	if activations != 1 {
		t.Errorf("%d packages were activated, want the one the clear audit activated", activations)
	}
}

// An application that cannot be reached cannot be audited clean: the audit cannot probe what it
// cannot ask, says so as critical for every consequential capability, and the package stays in
// quarantine.
func TestM1ActivationNeedsTheApplicationToAnswer(t *testing.T) {
	clinic := startClinic(t)
	op := newOperator(t)
	pkg := signedClinicPackage(t, clinic)
	op.install(pkg)
	op.addIssuerKey(pkg)
	clinic.stop()
	run, res := op.audit()
	if res.Code != cli.ExitError || run.Activated || run.Outcome != "blocked" {
		t.Fatalf("the audit of an application that is down: exit %d\n%s", res.Code, run)
	}
	for _, id := range []string{"appointments.book", "appointments.cancel", "patients.update_contact"} {
		found := false
		for _, f := range run.severities("critical") {
			found = found || (f.CheckID == "probes.capability_not_probed" && f.Capability == id)
		}
		if !found {
			t.Errorf("the audit did not say it could not probe %s:\n%s", id, run)
		}
	}
}
