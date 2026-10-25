package integrationaudit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// customHarness is a harness whose runner uses exactly the given checks.
func customHarness(t *testing.T, checks ...Check) *harness {
	t.Helper()
	h := newHarness(t)
	reg := NewRegistry()
	for _, c := range checks {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	cfg := h.cfg
	cfg.Checks = reg
	var err error
	if h.runner, err = NewRunner(cfg); err != nil {
		t.Fatal(err)
	}
	h.install(validMinimalDir)
	return h
}

func probeCheck(id string, section int, stage Stage, run func(context.Context, *Input) (*Outcome, error)) Check {
	return NewCheck(Spec{ID: id, Title: "test check " + id, Section: section, Stage: stage, Target: TargetApplication}, run)
}

func TestRegistryRefusesInvalidAndDuplicateChecks(t *testing.T) {
	ok := func(context.Context, *Input) (*Outcome, error) { return &Outcome{}, nil }
	r := NewRegistry()
	if err := r.Register(probeCheck("probes.alpha", 2, StageResults, ok)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Check{
		"duplicate":      probeCheck("probes.alpha", 2, StageResults, ok),
		"not dotted":     probeCheck("alpha", 2, StageResults, ok),
		"upper case":     probeCheck("Probes.Alpha", 2, StageResults, ok),
		"bad section":    probeCheck("probes.beta", 11, StageResults, ok),
		"bad stage":      probeCheck("probes.gamma", 2, Stage(99), ok),
		"nil":            nil,
		"no title":       NewCheck(Spec{ID: "probes.delta", Section: 2, Stage: StageResults, Target: TargetApplication}, ok),
		"unknown target": NewCheck(Spec{ID: "probes.eps", Title: "t", Section: 2, Stage: StageResults, Target: "elsewhere"}, ok),
	} {
		if err := r.Register(c); err == nil {
			t.Errorf("%s: the registry accepted an invalid check", name)
		}
	}
	// a check cannot be replaced by registering over it, which would silence it
	if len(r.Checks()) != 1 {
		t.Errorf("registry holds %d checks, want 1", len(r.Checks()))
	}
	// Default holds the built-in checks, none registered twice
	got := map[string]bool{}
	for _, c := range Default.Checks() {
		got[c.ID()] = true
	}
	for _, c := range Builtins() {
		if !got[c.ID()] {
			t.Errorf("the default registry lacks built-in check %s", c.ID())
		}
	}
}

// Checks run in the order of master plan 9.4, whatever order they registered in.
func TestChecksRunInStageOrder(t *testing.T) {
	var order []string
	rec := func(id string, section int, stage Stage) Check {
		return probeCheck(id, section, stage, func(context.Context, *Input) (*Outcome, error) {
			order = append(order, id)
			return &Outcome{}, nil
		})
	}
	h := customHarness(t,
		rec("probes.smoke", SectionRuntimeSmoke, StageBenchmark),
		rec("probes.safety", SectionSafeExecution, StageSafety),
		rec("probes.rag", SectionRAGInventory, StageRAG),
		rec("probes.scope", SectionScope, StageScope),
		rec("probes.results", SectionDeterministic, StageResults),
		rec("probes.docs", SectionDocumentation, StageDocumentation),
		rec("probes.inventory", SectionControlSurface, StageInventory),
		rec("probes.signature", SectionAuthenticity, StageSignature),
	)
	h.audit(nil)
	want := []string{"probes.signature", "probes.docs", "probes.inventory", "probes.results", "probes.scope", "probes.rag", "probes.safety", "probes.smoke"}
	if diff := cmp.Diff(want, order); diff != "" {
		t.Errorf("order of checks (-want +got):\n%s", diff)
	}
}

// A check that cannot run, panics or returns something unreportable is a
// critical finding, never a pass and never silence.
func TestFailingCheckIsCritical(t *testing.T) {
	h := customHarness(t,
		probeCheck("probes.errors", SectionDeterministic, StageResults, func(context.Context, *Input) (*Outcome, error) {
			return nil, errors.New("the probe could not reach the disposable tenant: secret-detail-123")
		}),
		probeCheck("probes.panics", SectionScope, StageScope, func(context.Context, *Input) (*Outcome, error) {
			panic("boom")
		}),
		probeCheck("probes.nothing", SectionSafeExecution, StageSafety, func(context.Context, *Input) (*Outcome, error) {
			return nil, nil
		}),
		probeCheck("probes.invalid_finding", SectionDegradation, StageSafety, func(context.Context, *Input) (*Outcome, error) {
			return (&Outcome{}).Add(contracts.Finding{CheckID: "probes.x", Severity: contracts.SeverityWarning, Message: "no remediation"}), nil
		}),
		probeCheck("probes.float", SectionRuntimeSmoke, StageBenchmark, func(context.Context, *Input) (*Outcome, error) {
			return &Outcome{Extension: map[string]any{"p95_ms": 1.5}}, nil
		}),
		probeCheck("probes.badtext", SectionRuntimeSmoke, StageBenchmark, func(context.Context, *Input) (*Outcome, error) {
			return &Outcome{Probes: []ProbeResult{{Kind: ProbeSmoke, Subject: "s", Expected: "e", Observed: string([]byte{0xff, 0xfe}) + " not text"}}}, nil
		}),
	)
	rep := h.audit(nil).Signed.Report
	failed := findings(rep, "audit.check_failed")
	if len(failed) != 3 {
		t.Fatalf("want 3 check_failed findings (error, panic, nil), got %d:\n%s", len(failed), reportJSON(rep))
	}
	for _, f := range failed {
		if f.Severity != contracts.SeverityCritical {
			t.Errorf("a check that could not run is critical: %+v", f)
		}
		if strings.Contains(f.Message, "secret-detail-123") {
			t.Errorf("the error text of a check must not reach the report: %q", f.Message)
		}
	}
	if len(findings(rep, "audit.check_invalid")) != 3 {
		t.Errorf("want 3 check_invalid findings (invalid finding, float, invalid text):\n%s", reportJSON(rep))
	}
	if rep.Summary.Outcome != DispositionBlocked {
		t.Errorf("a check that failed blocks: %s", rep.Summary.Outcome)
	}
	for _, n := range []int{SectionDeterministic, SectionScope, SectionSafeExecution, SectionDegradation, SectionRuntimeSmoke} {
		if section(rep, n).Status != StatusFail {
			t.Errorf("section %d = %s, want fail", n, section(rep, n).Status)
		}
	}
}

// Unverified is never pass, however the check reports the rest.
func TestUnverifiedItemNeverPasses(t *testing.T) {
	h := customHarness(t,
		probeCheck("probes.partial", SectionScope, StageScope, func(_ context.Context, in *Input) (*Outcome, error) {
			o := &Outcome{Covered: in.Package.SortedCapabilityIDs()}
			o.Probes = []ProbeResult{{Kind: ProbePositiveScope, Capability: "catalog.find_title", Subject: "own tenant", Expected: "allowed", Observed: "allowed", Passed: true}}
			return o.Unverify(Unverified{Subject: "probe:cross_tenant", Capability: "reservations.create", Section: SectionScope, Target: TargetApplication, Reason: "no disposable tenant is declared"}), nil
		}),
	)
	rep := h.audit(nil).Signed.Report
	if got := section(rep, SectionScope).Status; got != StatusUnverified {
		t.Errorf("section 6 = %s, want unverified", got)
	}
	if capabilityStatus(rep, "reservations.create") != CapabilityUnverified {
		t.Errorf("the capability the probe could not check is unverified, got %s", capabilityStatus(rep, "reservations.create"))
	}
	if capabilityStatus(rep, "catalog.find_title") != CapabilityPassed {
		t.Errorf("the other one passed, got %s", capabilityStatus(rep, "catalog.find_title"))
	}
	var found bool
	for _, u := range rep.Unverified {
		found = found || u.Subject == "probe:cross_tenant"
	}
	if !found {
		t.Errorf("the item must be listed in the report")
	}
}

// A section nothing implements is not run: a warning, and not a pass. The
// warning goes away by registering the check, without editing the framework.
func TestSectionNotRunIsWarning(t *testing.T) {
	h := newHarness(t, func(o *hopts) { o.probes = false })
	h.install(validMinimalDir)
	rep := h.audit(newStub(t)).Signed.Report
	missing := findings(rep, "audit.section_not_run")
	if len(missing) != 5 {
		t.Fatalf("want 5 sections not run (2, 6, 8, 9, 10), got %d:\n%s", len(missing), reportJSON(rep))
	}
	for _, f := range missing {
		if f.Severity != contracts.SeverityWarning {
			t.Errorf("a section that was not run is a warning: %+v", f)
		}
	}
	for _, n := range []int{SectionDeterministic, SectionScope, SectionSafeExecution, SectionDegradation, SectionRuntimeSmoke} {
		if section(rep, n).Status != StatusNotRun {
			t.Errorf("section %d = %s, want not_run", n, section(rep, n).Status)
		}
	}
	if rep.Summary.Outcome != DispositionNeedsAcceptance {
		t.Errorf("an audit that did not run every required check needs acceptance: %s", rep.Summary.Outcome)
	}

	// register the missing checks through the extension point
	h2 := newHarness(t)
	h2.install(validMinimalDir)
	rep2 := h2.audit(newStub(t)).Signed.Report
	if n := len(findings(rep2, "audit.section_not_run")); n != 0 {
		t.Errorf("%d sections still not run with every check registered", n)
	}
	if rep2.Summary.Outcome != DispositionClear {
		t.Errorf("outcome = %s, want clear", rep2.Summary.Outcome)
	}
}

// What an extension adds reaches the signed report: findings, probes, coverage,
// measurements, verdicts and its own data; and coverage of one name adds up.
func TestExtensionPointAddsChecks(t *testing.T) {
	h := customHarness(t,
		probeCheck("probes.scope", SectionScope, StageScope, func(_ context.Context, in *Input) (*Outcome, error) {
			if in.Adapter == nil || in.Scope.ApplicationID != appID || in.Scope.Validate() != nil || in.Clock == nil {
				return nil, errors.New("the input must carry the adapter, a valid audit scope and the clock")
			}
			o := &Outcome{Covered: in.Package.SortedCapabilityIDs()}
			o.Probes = []ProbeResult{
				{Kind: ProbeNegativeScope, Capability: "reservations.create", Subject: "tenant-b reads tenant-a", Expected: "scope_violation", Observed: "ok", Passed: false},
				{Kind: ProbeNegativeScope, Capability: "catalog.find_title", Subject: "tenant-b reads tenant-a", Expected: "scope_violation", Observed: "scope_violation", Passed: true},
			}
			o.Coverage = []CoverageItem{{Name: "scope_probes", Covered: 1, Total: 2}}
			o.Measurements = []Measurement{{Name: "probe_p95", Unit: "ms_bucket_10", Value: 40}}
			o.Verdicts = []CapabilityVerdict{{CapabilityID: "reservations.create", Status: CapabilityQuarantined, Reason: "a cross-tenant read succeeded"}}
			o.Extension = map[string]any{"disposable_tenant": "tnt-audit-1", "probes": 2, "cleaned_up": true}
			o.Add(contracts.Finding{CheckID: "scope.cross_tenant_leak", Severity: contracts.SeverityCritical, Capability: "reservations.create",
				Message: "a request in another tenant read a record", Remediation: "Fix the host's tenant filter."})
			return o, nil
		}),
		probeCheck("probes.rag", SectionRAGInventory, StageRAG, func(_ context.Context, in *Input) (*Outcome, error) {
			o := &Outcome{Coverage: []CoverageItem{{Name: "scope_probes", Covered: 3, Total: 3}}}
			o.Sources = []SourceResult{{ID: "loan-policies", Type: "documentation_package", Verified: "live"}}
			return o, nil
		}),
	)
	rep := h.audit(newStub(t)).Signed.Report

	if !hasFinding(rep, "scope.cross_tenant_leak", contracts.SeverityCritical) || rep.Summary.Outcome != DispositionBlocked {
		t.Errorf("the extension's finding blocks:\n%s", reportJSON(rep))
	}
	if capabilityStatus(rep, "reservations.create") != CapabilityFailed {
		t.Errorf("a critical finding on a capability fails it, whatever its explicit verdict: %s", capabilityStatus(rep, "reservations.create"))
	}
	if len(rep.Probes) != 2 || rep.Probes[0].Capability != "catalog.find_title" {
		t.Errorf("probes are sorted and kept: %+v", rep.Probes)
	}
	var cov CoverageItem
	for _, c := range rep.Coverage {
		if c.Name == "scope_probes" {
			cov = c
		}
	}
	if cov.Covered != 4 || cov.Total != 5 {
		t.Errorf("coverage of one name adds up across checks: %+v", cov)
	}
	if len(rep.Measurements) != 1 || rep.Measurements[0].Value != 40 {
		t.Errorf("measurements = %+v", rep.Measurements)
	}
	var ext *Extension
	for i := range rep.Extensions {
		if rep.Extensions[i].Check == "probes.scope" {
			ext = &rep.Extensions[i]
		}
	}
	if ext == nil || ext.Data["disposable_tenant"] != "tnt-audit-1" || ext.Data["cleaned_up"] != true {
		t.Errorf("the extension's own data is in the report: %+v", rep.Extensions)
	}
	if len(rep.RAGSources) != 1 || rep.RAGSources[0].Verified != "live" {
		t.Errorf("a source the RAG probes verified live is recorded as such: %+v", rep.RAGSources)
	}
	// the static RAG check no longer claims the live part is unverified once a
	// section-3 check exists: here the built-in one is not registered at all, so
	// the section simply has the one check
	if section(rep, SectionRAGInventory).Status != StatusPass {
		t.Errorf("section 3 = %s", section(rep, SectionRAGInventory).Status)
	}
	// and the whole report still verifies
	if _, err := VerifyReport(ctx, h.cfg.Verifier, h.mustLatest().Signed); err != nil {
		t.Errorf("the report with extension data must verify: %v", err)
	}
}

func (h *harness) mustLatest() StoredRun {
	h.t.Helper()
	run, ok, err := h.runner.LatestRun(ctx, appID, h.onlyHash())
	if err != nil || !ok {
		h.t.Fatalf("LatestRun: %v %v", ok, err)
	}
	return run
}

// With a live RAG check registered the static one stops reporting the live
// verification as unverified (the other check owns it).
func TestRAGLiveVerificationIsLeftToTheLiveCheck(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	rep := h.audit(newStub(t)).Signed.Report
	var unverified bool
	for _, u := range rep.Unverified {
		unverified = unverified || u.Subject == "rag_sources:live"
	}
	if !unverified {
		t.Errorf("with no live RAG check the audit must say the sources were not verified live")
	}

	regs := newChecks(t, true)
	live := probeCheck("rag.live", SectionRAGInventory, StageRAG, func(context.Context, *Input) (*Outcome, error) { return &Outcome{}, nil })
	if err := regs.Register(live); err != nil {
		t.Fatal(err)
	}
	cfg := h.cfg
	cfg.Checks = regs
	h.runner, _ = NewRunner(cfg)
	h.clk.Advance(1)
	rep = h.audit(newStub(t)).Signed.Report
	for _, u := range rep.Unverified {
		if u.Subject == "rag_sources:live" {
			t.Errorf("a live RAG check is registered: the static one must not claim the part it does not do: %+v", u)
		}
	}
	if section(rep, SectionRAGInventory).Status != StatusPass {
		t.Errorf("section 3 = %s", section(rep, SectionRAGInventory).Status)
	}
}

// A package that does not verify is not audited beyond saying so.
func TestUnloadablePackageRunsOnlyAuthenticity(t *testing.T) {
	var ran []string
	spy := func(id string, section int) Check {
		return probeCheck(id, section, StageResults, func(context.Context, *Input) (*Outcome, error) {
			ran = append(ran, id)
			return &Outcome{}, nil
		})
	}
	h := customHarness(t, append(Builtins(), spy("probes.spy", SectionDeterministic))...)
	if err := h.trust.RevokeKey(ctx, appID, "alixai-test-only-publisher", "revoked"); err != nil {
		t.Fatal(err)
	}
	rep := h.audit(newStub(t)).Signed.Report
	if len(ran) != 0 {
		t.Errorf("a check ran against a package that does not verify: %v", ran)
	}
	if got := section(rep, SectionDeterministic).Status; got != StatusUnverified {
		t.Errorf("section 2 = %s, want unverified", got)
	}
	if got := section(rep, SectionAuthenticity).Status; got != StatusFail {
		t.Errorf("section 1 = %s, want fail", got)
	}
	if len(rep.Capabilities) != 0 {
		t.Errorf("an unverified package declares no capability to the report: %+v", rep.Capabilities)
	}
	if rep.Subject.ManifestHash == "" || rep.Subject.ApplicationID != appID {
		t.Errorf("the subject still names the package: %+v", rep.Subject)
	}
}

// A capability a check fails by an explicit verdict, with no finding (verdicts
// are for what findings cannot express), blocks the report like a critical
// finding would.
func TestFailedVerdictWithoutAFindingBlocks(t *testing.T) {
	h := customHarness(t,
		probeCheck("probes.verdict", SectionScope, StageScope, func(_ context.Context, in *Input) (*Outcome, error) {
			o := &Outcome{Covered: in.Package.SortedCapabilityIDs()}
			o.Verdicts = []CapabilityVerdict{{CapabilityID: "reservations.create", Status: CapabilityFailed, Reason: "the host answered a forged confirmation"}}
			return o, nil
		}),
	)
	rep := h.audit(newStub(t)).Signed.Report
	if capabilityStatus(rep, "reservations.create") != CapabilityFailed {
		t.Fatalf("verdict = %s, want failed", capabilityStatus(rep, "reservations.create"))
	}
	if rep.Summary.Outcome != DispositionBlocked {
		t.Errorf("outcome = %s, want blocked:\n%s", rep.Summary.Outcome, reportJSON(rep))
	}
	accepted := map[string]Acceptance{}
	for _, f := range rep.Findings {
		accepted[f.Key] = Acceptance{FindingKey: f.Key}
	}
	if _, err := gate(rep, accepted); reason(err) != ReasonCriticalFindings {
		t.Errorf("activation over a failed capability, every warning accepted: %v", err)
	}
}
