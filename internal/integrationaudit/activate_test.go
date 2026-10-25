package integrationaudit

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// reason returns the Details["reason"] of an activation refusal.
func reason(err error) string {
	var ce *contracts.Error
	if !asContractsError(err, &ce) {
		return ""
	}
	s, _ := ce.Details["reason"].(string)
	return s
}

func asContractsError(err error, target **contracts.Error) bool {
	for err != nil {
		if ce, ok := err.(*contracts.Error); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func mustNotServe(t *testing.T, h *harness, why string) {
	t.Helper()
	if _, err := h.reg.Info(ctx, appID); err == nil {
		t.Errorf("%s: the capability registry serves the application", why)
	}
	if apps := h.reg.Applications(); len(apps) != 0 {
		t.Errorf("%s: the registry holds %v", why, apps)
	}
}

// SEC-060: an application is never activated, or served, without a clean
// integration audit.
func TestCriticalFailureBlocksActivation(t *testing.T) {
	t.Run("a critical finding of the audit", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		mustNotServe(t, h, "just installed")
		// commit is not offered: the audit finds a critical problem that the
		// loader, which looks at the package alone, cannot.
		run := h.audit(newStub(t, func(s *stub) {
			s.desc.Capabilities[1].Operations = opsOf(adapters.OpValidate, adapters.OpPrepare, adapters.OpStatus)
		}))
		if run.Disposition() != DispositionBlocked || run.Signed.Report.Summary.Critical == 0 {
			t.Fatalf("fixture: want a blocked report:\n%s", reportJSON(run.Signed.Report))
		}
		// even with every warning accepted
		acceptAll(t, h, run)

		_, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID, OperatorID: "alice"})
		if !contracts.HasCode(err, contracts.ErrQuarantined) || reason(err) != ReasonCriticalFindings {
			t.Fatalf("Activate = %v (reason %q), want quarantined / %s", err, reason(err), ReasonCriticalFindings)
		}
		if got := h.state(rec); got != apppkg.StateQuarantined {
			t.Errorf("the package is %s, want quarantined", got)
		}
		got, _ := h.inst.Get(ctx, appID, rec.ManifestHash)
		if got.AuditReportID != "" {
			t.Errorf("a refused activation leaves no report id behind: %q", got.AuditReportID)
		}
		mustNotServe(t, h, "after a refused activation")
		for _, e := range h.events(appID) {
			if strings.HasPrefix(e, "package.activated") {
				t.Errorf("no activation may be recorded: %v", h.events(appID))
			}
		}
	})

	t.Run("a package that was never audited", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		_, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
		if !contracts.HasCode(err, contracts.ErrQuarantined) || reason(err) != ReasonNoReport {
			t.Fatalf("Activate = %v, want quarantined / %s", err, ReasonNoReport)
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
		// the database says the same, whoever asks: no report id, no activation
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active' WHERE application_id = ?`, appID); err == nil {
			t.Errorf("the table must refuse an activation that carries no audit report")
		}
		mustNotServe(t, h, "never audited")
	})

	t.Run("a publisher key revoked after install", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		if err := h.trust.RevokeKey(ctx, appID, devkeys.KeyID, "compromised"); err != nil {
			t.Fatal(err)
		}
		stub := newStub(t)
		run := h.audit(stub)
		rep := run.Signed.Report
		if rep.Summary.Outcome != DispositionBlocked || !hasFinding(rep, "signature.invalid", contracts.SeverityCritical) {
			t.Fatalf("a revoked signing key is a critical finding:\n%s", reportJSON(rep))
		}
		if got := section(rep, SectionAuthenticity).Status; got != StatusFail {
			t.Errorf("section 1 = %s, want fail", got)
		}
		// nothing else is audited on the strength of a package that does not verify
		if len(stub.calls) != 0 {
			t.Errorf("an unverified package must not be used to call the application: %v", stub.calls)
		}
		for _, c := range rep.Checks {
			if c.ID == "application.control_surface" && c.Status != StatusUnverified {
				t.Errorf("the control surface of an unverified package is unverified, got %s", c.Status)
			}
		}
		if rep.Surface.Inspected {
			t.Errorf("no inspection happened")
		}
		if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); !contracts.HasCode(err, contracts.ErrQuarantined) {
			t.Fatalf("Activate = %v, want quarantined", err)
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
		mustNotServe(t, h, "revoked publisher key")
	})

	t.Run("a key revoked between the audit and the activation", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		run := h.audit(newStub(t))
		acceptAll(t, h, run)
		if run.Disposition() == DispositionBlocked {
			t.Fatalf("fixture: want a report that would activate")
		}
		if err := h.trust.RevokeKey(ctx, appID, devkeys.KeyID, "compromised after the audit"); err != nil {
			t.Fatal(err)
		}
		_, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
		if !contracts.HasCode(err, contracts.ErrQuarantined) || reason(err) != "package_unverified" {
			t.Fatalf("Activate = %v (%s): the stored bytes are verified again at activation", err, reason(err))
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
	})

	t.Run("a package signature that expired", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		h.audit(newStub(t)) // clean today
		// the fixture's signature is valid until 2036-10-01
		h.clk.Set(time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC))
		run := h.audit(newStub(t))
		rep := run.Signed.Report
		if rep.Summary.Outcome != DispositionBlocked || !hasFinding(rep, "signature.invalid", contracts.SeverityCritical) {
			t.Fatalf("an expired signature is critical:\n%s", reportJSON(rep))
		}
		if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err == nil {
			t.Fatalf("a package whose signature expired must not activate")
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
		mustNotServe(t, h, "expired signature")
	})

	t.Run("a package that was installed and changed on disk", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		run := h.audit(newStub(t))
		acceptAll(t, h, run)
		editFile(t, h.dataDir+"/"+rec.Path, "documentation/index.yaml", "title:", "title: tampered\n  x-title:")
		if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); reason(err) != "package_unverified" {
			t.Fatalf("a package whose files changed after the audit must not activate: %v", err)
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
	})
}

// SEC-061: a warning is accepted only by an authorised operator, with a reason,
// and the acceptance is recorded with who and when.
func TestWarningRequiresOperatorAcceptance(t *testing.T) {
	h := newHarness(t)
	rec := h.install(validMinimalDir)
	run := h.audit(newStub(t, func(s *stub) { s.desc.Unmapped = []string{"admin.purge_all"} }))
	rep := run.Signed.Report
	if run.Disposition() != DispositionNeedsAcceptance {
		t.Fatalf("want needs_acceptance, got %s:\n%s", run.Disposition(), reportJSON(rep))
	}
	warnings := findings(rep, "surface.undocumented_operation")
	if len(warnings) != 1 {
		t.Fatalf("want one warning, got %d", len(warnings))
	}
	key := warnings[0].Key

	// 1. not accepted: not activated
	_, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
	if !contracts.HasCode(err, contracts.ErrQuarantined) || reason(err) != ReasonWarningsPending {
		t.Fatalf("Activate = %v (%s), want quarantined / %s", err, reason(err), ReasonWarningsPending)
	}
	var ce *contracts.Error
	if asContractsError(err, &ce); ce == nil || !strings.Contains(strings.Join(toStrings(ce.Details["pending"]), ","), key) {
		t.Errorf("the refusal must name the warning that is pending: %+v", ce)
	}
	mustNotServe(t, h, "warning not accepted")

	accept := func(req AcceptRequest) (*Acceptance, error) {
		req.ApplicationID = appID
		if req.FindingKey == "" {
			req.FindingKey = key
		}
		return h.runner.AcceptWarning(ctx, req)
	}
	count := func() int {
		m, err := h.runner.Acceptances(ctx, appID, rec.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		return len(m)
	}

	// 2. an operator who is not authorised cannot accept
	if _, err := accept(AcceptRequest{OperatorID: "mallory", Reason: "looks fine"}); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("an unauthorised operator: %v, want permission_denied", err)
	}
	// ... nor can nobody, nor can one with no reason
	if _, err := accept(AcceptRequest{OperatorID: "", Reason: "x"}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Errorf("no operator: %v", err)
	}
	if _, err := accept(AcceptRequest{OperatorID: "alice", Reason: "  "}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Errorf("no reason: %v", err)
	}
	if _, err := accept(AcceptRequest{OperatorID: "alice", Reason: "x", FindingKey: "0000000000000000"}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Errorf("a key that is in no report: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d acceptances were recorded by refused requests", n)
	}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err == nil {
		t.Fatalf("still not accepted: must not activate")
	}

	// 3. only a warning can be accepted
	for _, f := range rep.Findings {
		if f.Severity == contracts.SeverityInfo {
			if _, err := accept(AcceptRequest{OperatorID: "alice", Reason: "x", FindingKey: f.Key}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("an informational finding has nothing to accept: %v", err)
			}
			break
		}
	}

	// 4. an authorised operator with a reason
	h.clk.Advance(5 * time.Second)
	acc, err := accept(AcceptRequest{OperatorID: "alice", Reason: "the operation is an internal export we do not use"})
	if err != nil {
		t.Fatalf("AcceptWarning: %v", err)
	}
	if !acc.Created || acc.OperatorID != "alice" || acc.FindingKey != key || acc.CheckID != "surface.undocumented_operation" ||
		acc.RunID != run.RunID || !acc.AcceptedAt.Equal(t0.Add(5*time.Second)) || acc.Reason == "" {
		t.Errorf("acceptance = %+v", acc)
	}
	// idempotent: the first acceptance stands, whoever asks again
	again, err := accept(AcceptRequest{OperatorID: "alice", Reason: "a second reason"})
	if err != nil || again.Created || again.Reason != acc.Reason {
		t.Errorf("a second acceptance returns the first: %+v %v", again, err)
	}
	if n := count(); n != 1 {
		t.Errorf("%d acceptances, want 1", n)
	}
	// who, when and which finding are recorded, and in the audit log
	events := h.events(appID)
	want := []string{"audit_run.started|", "audit_run.completed|", "audit_run.warning_accepted|alice"}
	if diff := cmp.Diff(want, events); diff != "" {
		t.Errorf("audit log (-want +got):\n%s", diff)
	}

	// 5. now it activates
	act, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID, OperatorID: "alice"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if h.state(rec) != apppkg.StateActive || act.Record.AuditReportID != run.RunID || act.RunID != run.RunID {
		t.Errorf("activation = %+v, state %s", act, h.state(rec))
	}
	if len(act.AcceptedWarnings) != 1 || act.AcceptedWarnings[0].OperatorID != "alice" {
		t.Errorf("the activation records the warnings it relied on: %+v", act.AcceptedWarnings)
	}
	last := h.events(appID)
	if last[len(last)-1] != "package.activated|alice" {
		t.Errorf("audit log tail = %v", last)
	}
	if _, err := h.reg.Info(ctx, appID); err != nil {
		t.Errorf("an activated package is served at once: %v", err)
	}

	// 6. a critical finding is never accepted, by anyone
	h2 := newHarness(t)
	h2.install(validMinimalDir)
	blocked := h2.audit(newStub(t, func(s *stub) { s.desc.Capabilities = s.desc.Capabilities[:1] }))
	for _, f := range blocked.Signed.Report.Findings {
		if f.Severity == contracts.SeverityCritical {
			if _, err := h2.runner.AcceptWarning(ctx, AcceptRequest{ApplicationID: appID, FindingKey: f.Key, OperatorID: "alice", Reason: "ship it"}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("a critical finding cannot be accepted: %v", err)
			}
		}
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// An acceptance belongs to one package: the same warning in another package, or
// the same package with a different finding, needs its own.
func TestAcceptanceIsBoundToPackage(t *testing.T) {
	h := newHarness(t)
	first := h.install(validMinimalDir)
	adapter := func() *stub { return newStub(t, func(s *stub) { s.desc.Unmapped = []string{"admin.purge_all"} }) }
	run := h.audit(adapter())
	acceptAll(t, h, run)
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}

	// version 1.1.0: the same warning, a different package
	dir := copyDir(t, validMinimalDir)
	editFile(t, dir, "alixai.yaml", "version: 1.0.0\n  description: 'Initial", "version: 1.1.0\n  description: 'Initial")
	resign(t, dir, appID, "1.1.0")
	second := h.install(dir)
	if second.ManifestHash == first.ManifestHash {
		t.Fatal("fixture: the packages must differ")
	}
	h.clk.Advance(time.Minute)
	run2 := h.audit(adapter())
	if run2.Record.ManifestHash != second.ManifestHash {
		t.Fatalf("the audit must pick the newest quarantined package")
	}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); reason(err) != ReasonWarningsPending {
		t.Fatalf("the first package's acceptance must not carry over: %v", err)
	}
	acceptAll(t, h, run2)
	act, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
	if err != nil {
		t.Fatal(err)
	}
	// and the first one is superseded in the same step
	if act.Superseded != first.ManifestHash || h.state(first) != apppkg.StateSuperseded || h.state(second) != apppkg.StateActive {
		t.Errorf("activation = %+v; first %s second %s", act, h.state(first), h.state(second))
	}
	info, err := h.reg.Info(ctx, appID)
	if err != nil || info.PackageVersion != "1.1.0" {
		t.Errorf("the registry serves the new package at once: %+v %v", info, err)
	}
	if ev := h.events(appID); !containsPrefix(ev, "package.deactivated|") {
		t.Errorf("superseding is recorded: %v", ev)
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// SEC-061: accepting a warning never widens the tools or the scope the model
// receives.
func TestAcceptedWarningDoesNotExpandScope(t *testing.T) {
	scope := func() contracts.Scope {
		return contracts.Scope{
			DeploymentID: "dep_test", ApplicationID: appID, TenantID: "library-network-1", WorkspaceID: "branch-1",
			PrincipalID: "usr_member_1", Roles: []string{"member"}, SessionID: "ses_test",
			CapabilityScopes: []string{"catalog.find_title", "reservations.create"},
		}
	}
	// what the model may be shown, and what is registered, once active
	observe := func(t *testing.T, h *harness) (capabilities []string, visible []string, hash string) {
		t.Helper()
		info, err := h.reg.Info(ctx, appID)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		recs, _ := h.inst.List(ctx, appID)
		pkg, rec, err := h.inst.Open(ctx, appID, recs[0].ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if pkg.Status() != policy.StatusActive {
			t.Fatalf("package is %s", pkg.Status())
		}
		gw := policy.NewGateway(h.clk)
		sc := scope()
		ex := gw.Expose(ctx, policy.ExposeRequest{Scope: sc, Package: pkg, Binding: policy.BindingOf(sc)})
		for _, d := range ex.Visible {
			visible = append(visible, d.Record.CapabilityID)
		}
		return info.Capabilities, visible, rec.ManifestHash
	}

	// A: an application that is exactly what the package declares.
	a := newHarness(t)
	a.install(validMinimalDir)
	runA := a.audit(newStub(t))
	if runA.Disposition() != DispositionClear {
		t.Fatalf("fixture: the faithful application must be clear: %s\n%s", runA.Disposition(), reportJSON(runA.Signed.Report))
	}
	if _, err := a.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	capsA, visA, hashA := observe(t, a)

	// B: an application that exposes much more, with every warning accepted.
	b := newHarness(t)
	b.install(validMinimalDir)
	runB := b.audit(newStub(t, func(s *stub) {
		s.desc.Unmapped = []string{"admin.purge_all", "export_everything"}
		s.desc.Capabilities = append(s.desc.Capabilities,
			adapters.CapabilityOps{CapabilityID: "admin.export", Operations: opsOf(adapters.OpRead, adapters.OpCommit)})
		s.desc.Capabilities[0].Operations = opsOf(adapters.OpRead, adapters.OpValidate) // an extra read-only operation
	}))
	if runB.Disposition() != DispositionNeedsAcceptance {
		t.Fatalf("fixture: want warnings only, got %s\n%s", runB.Disposition(), reportJSON(runB.Signed.Report))
	}
	warnings := 0
	for _, f := range runB.Signed.Report.Findings {
		if f.Severity == contracts.SeverityWarning {
			warnings++
		}
	}
	if warnings < 4 {
		t.Fatalf("fixture: want at least 4 warnings (2 undocumented, 1 undeclared capability, 1 over-broad), got %d", warnings)
	}
	mustNotServe(t, b, "before acceptance")
	acceptAll(t, b, runB)
	if _, err := b.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	capsB, visB, hashB := observe(t, b)

	if hashA != hashB {
		t.Fatalf("fixture: same bytes, same hash")
	}
	if diff := cmp.Diff(capsA, capsB); diff != "" {
		t.Errorf("accepting warnings changed the registered capabilities (-clean +accepted):\n%s", diff)
	}
	if diff := cmp.Diff(visA, visB); diff != "" {
		t.Errorf("accepting warnings changed what the policy gateway exposes (-clean +accepted):\n%s", diff)
	}
	want := []string{"catalog.find_title", "reservations.create"}
	if diff := cmp.Diff(want, capsB); diff != "" {
		t.Errorf("the registry holds exactly the package's declared user capabilities (-want +got):\n%s", diff)
	}
	for _, name := range []string{"admin.purge_all", "export_everything", "admin.export"} {
		for _, list := range [][]string{capsB, visB} {
			for _, got := range list {
				if got == name {
					t.Errorf("%s, which the package does not declare, became visible", name)
				}
			}
		}
	}
	// an acceptance row holds no member any capability or scope decision reads
	rows, err := b.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('integrationaudit_acceptances')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		cols = append(cols, n)
	}
	wantCols := []string{"application_id", "manifest_hash", "finding_key", "check_id", "capability", "operator_id", "reason", "run_id", "accepted_at"}
	if diff := cmp.Diff(wantCols, cols); diff != "" {
		t.Errorf("the acceptance record grew a column (-want +got):\n%s", diff)
	}
}

// An earlier clean report never outlives a later audit that found problems.
func TestActivationUsesNewestReport(t *testing.T) {
	h := newHarness(t)
	rec := h.install(validMinimalDir)
	clean := h.audit(newStub(t))
	if clean.Disposition() != DispositionClear {
		t.Fatalf("fixture")
	}
	h.clk.Advance(time.Second)
	bad := h.audit(newStub(t, func(s *stub) { s.desc.Capabilities = s.desc.Capabilities[:1] }))
	if bad.Disposition() != DispositionBlocked {
		t.Fatalf("fixture")
	}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); reason(err) != ReasonCriticalFindings {
		t.Fatalf("Activate = %v: the newest report is blocked, the older clean one must not be used", err)
	}
	if h.state(rec) != apppkg.StateQuarantined {
		t.Errorf("must stay quarantined")
	}
	// a later clean audit makes it activatable again
	h.clk.Advance(time.Second)
	h.audit(newStub(t))
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Errorf("a newer clean report activates: %v", err)
	}
}

func TestStaleReportRefused(t *testing.T) {
	h := newHarness(t, func(o *hopts) { o.maxAge = time.Hour })
	rec := h.install(validMinimalDir)
	h.audit(newStub(t))
	h.clk.Advance(61 * time.Minute)
	_, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
	if !contracts.HasCode(err, contracts.ErrQuarantined) || reason(err) != ReasonStaleReport {
		t.Fatalf("Activate = %v (%s), want stale_report", err, reason(err))
	}
	if h.state(rec) != apppkg.StateQuarantined {
		t.Errorf("must stay quarantined")
	}
	h.audit(newStub(t))
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Errorf("a fresh audit activates: %v", err)
	}
}

// A quarantined package needs a new audit: a report made before it was
// quarantined is not evidence about what it is now.
func TestQuarantineStopsServingAndNeedsNewAudit(t *testing.T) {
	h := newHarness(t)
	rec := h.install(validMinimalDir)
	h.audit(newStub(t))
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.Info(ctx, appID); err != nil {
		t.Fatalf("active package must be served: %v", err)
	}

	if err := h.runner.Quarantine(ctx, appID, rec.ManifestHash, "", "alice"); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Errorf("quarantining needs a reason: %v", err)
	}
	h.clk.Advance(2 * time.Second)
	if err := h.runner.Quarantine(ctx, appID, rec.ManifestHash, "a critical change was detected", "alice"); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if h.state(rec) != apppkg.StateQuarantined {
		t.Errorf("state = %s", h.state(rec))
	}
	got, _ := h.inst.Get(ctx, appID, rec.ManifestHash)
	if got.AuditReportID != "" || got.StatusReason != "a critical change was detected" {
		t.Errorf("record after quarantine = %+v", got)
	}
	mustNotServe(t, h, "after quarantine: traffic stops at once, not after the cache expires")
	if err := h.runner.Quarantine(ctx, appID, rec.ManifestHash, "again", "alice"); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Errorf("a package that is not active cannot be quarantined: %v", err)
	}
	if ev := h.events(appID); !containsPrefix(ev, "package.quarantined|alice") {
		t.Errorf("quarantine is recorded: %v", ev)
	}

	// the old report predates the quarantine
	_, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
	if reason(err) != ReasonStaleReport {
		t.Fatalf("Activate on the old report = %v (%s), want stale_report", err, reason(err))
	}
	h.audit(newStub(t))
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Errorf("a new audit reactivates: %v", err)
	}
}

// The registry caches an application with no active package for 30 seconds; the
// audit refreshes it on every activation change instead of waiting.
func TestActivationRefreshesCapabilityRegistry(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	if _, err := h.reg.Info(ctx, appID); err == nil {
		t.Fatal("fixture: nothing is active")
	} // the registry now remembers "absent"
	h.audit(newStub(t))
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.Info(ctx, appID); err != nil {
		t.Errorf("the registry must serve the package at once, without waiting for its cache: %v", err)
	}

	// without a registry the activation still happens (a process that serves no traffic)
	h2 := newHarness(t, func(o *hopts) { o.noRegistry = true })
	h2.install(validMinimalDir)
	h2.audit(newStub(t))
	if _, err := h2.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Errorf("Activate without a registry: %v", err)
	}
}

// A stored report that was tampered with, or signed by anyone but the platform
// key, never activates a package.
func TestActivationRefusesForgedStoredReport(t *testing.T) {
	t.Run("a blocked report edited to clear", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		run := h.audit(newStub(t, func(s *stub) { s.desc.Capabilities = s.desc.Capabilities[:1] }))
		forged := run.Signed
		forged.Report.Summary = Summary{Outcome: DispositionClear}
		forged.Report.Findings = []KeyedFinding{}
		doc, err := forged.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		// someone with write access to the database removes the append-only guard and rewrites the run
		h.exec(`DROP TRIGGER integrationaudit_runs_no_update`)
		h.exec(`UPDATE integrationaudit_runs SET document = ?, disposition = 'clear' WHERE run_id = ?`, string(doc), run.RunID)
		_, err = h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID})
		if err == nil || !contracts.HasCode(err, contracts.ErrUnauthenticated) {
			t.Fatalf("Activate = %v, want unauthenticated", err)
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
		mustNotServe(t, h, "forged report")
	})

	t.Run("a report signed by a key of the host application", func(t *testing.T) {
		h := newHarness(t)
		rec := h.install(validMinimalDir)
		run := h.audit(newStub(t, func(s *stub) { s.desc.Capabilities = s.desc.Capabilities[:1] }))
		forged := run.Signed
		forged.Report.Summary = Summary{Outcome: DispositionClear}
		forged.Report.Findings = []KeyedFinding{}
		sha, _ := Digest(forged.Report)
		forged.Attestation.ReportSHA256 = sha
		sig, err := trust.SignPayload(forged.Attestation.payload(), devkeys.KeyID, devkeys.PrivateKey(), t0, t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		forged.Attestation.Signatures = []trust.Signature{sig}
		doc, _ := forged.Marshal()
		h.exec(`DROP TRIGGER integrationaudit_runs_no_update`)
		h.exec(`UPDATE integrationaudit_runs SET document = ?, report_sha256 = ?, disposition = 'clear' WHERE run_id = ?`, string(doc), sha, run.RunID)
		if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); !contracts.HasCode(err, contracts.ErrUnauthenticated) {
			t.Fatalf("Activate = %v, want unauthenticated", err)
		}
		if h.state(rec) != apppkg.StateQuarantined {
			t.Errorf("must stay quarantined")
		}
	})

	t.Run("a platform key revoked after the audit", func(t *testing.T) {
		h := newHarness(t)
		h.install(validMinimalDir)
		h.audit(newStub(t))
		if err := h.trust.RevokeKey(ctx, trust.PlatformApplicationID, platformKeyID, "leaked"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); !contracts.HasCode(err, contracts.ErrUnauthenticated) {
			t.Fatalf("Activate = %v, want unauthenticated", err)
		}
	})
}

// A rejected package is final; an active one cannot be activated twice.
// raceClock runs race once, the first time the time is read, before reading it.
type raceClock struct {
	*clock.Fake
	race func()
}

func (c *raceClock) Now() time.Time {
	if f := c.race; f != nil {
		c.race = nil
		f()
	}
	return c.Fake.Now()
}

// A quarantine that lands while an activation is checking the package (here
// another operator activates it and it is quarantined again) stops that
// activation: the report it relies on predates the newer quarantine.
func TestActivationRefusesAQuarantineThatLandedMeanwhile(t *testing.T) {
	h := newHarness(t)
	rec := h.install(validMinimalDir)
	h.audit(newStub(t))
	h.runner.clk = &raceClock{Fake: h.clk, race: func() {
		if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(2 * time.Second)
		if err := h.runner.Quarantine(ctx, appID, rec.ManifestHash, "a critical change was detected", "alice"); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Fatalf("Activate over a newer quarantine = %v, want conflict", err)
	}
	if h.state(rec) != apppkg.StateQuarantined {
		t.Errorf("state = %s, want quarantined", h.state(rec))
	}
	mustNotServe(t, h, "a package quarantined after its report was made")
}

func TestActivateStateGuards(t *testing.T) {
	h := newHarness(t)
	rec := h.install(validMinimalDir)
	h.audit(newStub(t))
	if err := h.inst.Reject(ctx, appID, rec.ManifestHash, "operator decided against it"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: appID, ManifestHash: rec.ManifestHash}); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Errorf("a rejected package is final: %v", err)
	}
	if _, err := h.runner.Audit(ctx, RunRequest{ApplicationID: appID, ManifestHash: rec.ManifestHash}); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Errorf("a rejected package is not audited: %v", err)
	}
	if _, err := h.runner.Activate(ctx, ActivateRequest{ApplicationID: "no-such-app"}); err == nil {
		t.Errorf("an application with nothing installed has nothing to activate")
	}

	h2 := newHarness(t)
	h2.install(validMinimalDir)
	h2.audit(newStub(t))
	if _, err := h2.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.runner.Activate(ctx, ActivateRequest{ApplicationID: appID, ManifestHash: h2.onlyHash()}); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Errorf("an active package is already active: %v", err)
	}
}
