package probes

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func finding(o *integrationaudit.Outcome, id string) *contracts.Finding {
	for i := range o.Findings {
		if o.Findings[i].CheckID == id {
			return &o.Findings[i]
		}
	}
	return nil
}

// After principals of another scope were refused a commit of the owner's preparation,
// only the no-record marker shows that nothing was applied; any other Status answer
// leaves it unverified.
func TestScopeProbeNeedsTheNoRecordMarkerAfterAForeignCommit(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.statusOddUnknown = true })
	out := e.run(integrationaudit.SectionScope)
	found := false
	for _, u := range out.Unverified {
		found = found || (u.Capability == capUpdate && strings.HasPrefix(u.Subject, "scope_probe:derived-foreign-commit-"))
	}
	if !found {
		t.Errorf("no unverified item for the foreign commits whose effect Status did not rule out:\n%s", dump(out))
	}
}

// Derived probes the audit's bound cut off were not run: unverified, never passed.
func TestScopeSweepCutByItsBoundIsUnverified(t *testing.T) {
	e := newEnv(t)
	opts := e.opts()
	opts.MaxSweep = 1
	var out *integrationaudit.Outcome
	for _, c := range Checks(opts) {
		if c.Section() == integrationaudit.SectionScope {
			var err error
			if out, err = c.Run(context.Background(), e.in); err != nil {
				t.Fatal(err)
			}
		}
	}
	cut := map[string]bool{}
	for _, u := range out.Unverified {
		if strings.Contains(u.Reason, "bound") {
			cut[u.Capability] = true
		}
	}
	if !cut[capGet] || !cut[capUpdate] {
		t.Errorf("cut = %v, want the derived probes of %s and %s that did not run unverified:\n%s", cut, capGet, capUpdate, dump(out))
	}
}

// A capability the suite names no principal for is probed by its declared probes only:
// the derived ones it could not have are unverified, never silently skipped.
func TestScopeDerivedProbesWithoutAnOwnerAreUnverified(t *testing.T) {
	e := newEnv(t)
	suite := &e.pkg.Tests[0]
	var probes []ap.PackageTestsScopeProbe
	for _, p := range suite.ScopeProbes {
		if !(p.CapabilityID != nil && string(*p.CapabilityID) == capUpdate && p.Expect == ap.PackageTestsScopeProbeExpectAllow) {
			probes = append(probes, p)
		}
	}
	suite.ScopeProbes, suite.SafetyProbes, suite.Smoke.Paths = probes, nil, suite.Smoke.Paths[:2]
	out := e.run(integrationaudit.SectionScope)
	found := false
	for _, u := range out.Unverified {
		found = found || (u.Capability == capUpdate && u.Subject == "scope_probe:derived-foreign-commit-notes_update")
	}
	if !found {
		t.Errorf("no unverified item for the foreign commits of %s, which have no owner:\n%s", capUpdate, dump(out))
	}
}

// TestScopeProbesPassForACorrectHost: the positive and negative probes of the
// suite, and the probes derived from its records, all hold for a host that
// enforces tenant, workspace, principal and role scope, and nothing is left behind.
func TestScopeProbesPassForACorrectHost(t *testing.T) {
	e := newEnv(t)
	out := e.run(integrationaudit.SectionScope)
	if hasCritical(out) || len(findingIDs(out, contracts.SeverityWarning)) > 0 {
		t.Fatalf("a correct host must pass:\n%s", dump(out))
	}
	var pos, neg int
	for _, p := range out.Probes {
		if !p.Passed {
			t.Errorf("probe failed on a correct host: %+v", p)
		}
		switch p.Kind {
		case integrationaudit.ProbePositiveScope:
			pos++
		case integrationaudit.ProbeNegativeScope:
			neg++
		}
	}
	if pos < 3 || neg < 10 {
		t.Errorf("only %d positive and %d negative probes ran", pos, neg)
	}
	if len(out.Covered) != 3 {
		t.Errorf("covered = %v, want the three user capabilities", out.Covered)
	}
	// the retrieval probe belongs to the RAG probes: unverified, never a pass or a failure
	var retrieval bool
	for _, u := range out.Unverified {
		if u.Subject == "scope_probe:editor-retrieves-policies" {
			retrieval = true
		}
	}
	if !retrieval || len(out.Unverified) != 1 {
		t.Errorf("unverified = %+v, want exactly the retrieval probe", out.Unverified)
	}
	if e.sim.tenantCount() != 0 {
		t.Errorf("tenants left behind: %d", e.sim.tenantCount())
	}
	// the generated identifiers never reach the report
	for _, p := range out.Probes {
		if strings.Contains(p.Subject+p.Expected+p.Observed, "note_") || strings.Contains(p.Subject, "tnt_") {
			t.Errorf("a generated identifier is in a probe result: %+v", p)
		}
	}
}

// SEC-029
// TestNegativeScopeProbeDetectsLeak: each way an application can let a record of
// another scope reach the caller is found by the probe that exists for it, as a
// critical finding on the capability.
func TestNegativeScopeProbeDetectsLeak(t *testing.T) {
	cases := []struct {
		name string
		knob func(*simKnobs)
		want string
		cap  string
	}{
		{"a read finds a record of another tenant", func(k *simKnobs) { k.leakTenant = true }, "scope.cross_scope_allowed", capGet},
		{"a listing contains records of every workspace and tenant", func(k *simKnobs) { k.leakList = true }, "scope.foreign_data_in_result", capList},
		{"a refusal carries the identifier of the record it refuses", func(k *simKnobs) { k.disclose = true }, "scope.leak", capGet},
		{"a prepare for a record of another tenant is accepted", func(k *simKnobs) { k.leakTenant = true }, "scope.cross_scope_allowed", capUpdate},
		{"a principal of another tenant commits a preparation it does not own", func(k *simKnobs) { k.commitForeign = true }, "scope.cross_scope_allowed", capUpdate},
		{"a principal of another tenant reads the status of a commit it does not own", func(k *simKnobs) { k.statusCrossTenant = true }, "scope.cross_scope_allowed", capUpdate},
		{"an application that answers ok whoever asks", func(k *simKnobs) { k.unauthorised = true }, "scope.cross_scope_allowed", capUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.knob)
			out := e.run(integrationaudit.SectionScope)
			var hit *contracts.Finding
			for i := range out.Findings {
				if out.Findings[i].CheckID == tc.want && out.Findings[i].Capability == tc.cap {
					hit = &out.Findings[i]
				}
			}
			if hit == nil {
				t.Fatalf("no %s finding on %s:\n%s", tc.want, tc.cap, dump(out))
			}
			if hit.Severity != contracts.SeverityCritical {
				t.Errorf("finding = %s, want critical", hit)
			}
			var failedProbe bool
			for _, p := range out.Probes {
				if !p.Passed && p.Capability == tc.cap {
					failedProbe = true
				}
			}
			if !failedProbe && !strings.Contains(tc.name, "commit") && !strings.Contains(tc.name, "status") {
				t.Errorf("no failed probe is recorded for %s", tc.cap)
			}
			if e.sim.tenantCount() != 0 {
				t.Errorf("tenants left behind after a failing probe: %d", e.sim.tenantCount())
			}
		})
	}
}

// A probe that finds nothing wrong with a refusal that holds must not be moved
// by the probes derived from the suite: the same host without the leak passes.
func TestNegativeScopeProbeLeavesACorrectRefusalAlone(t *testing.T) {
	e := newEnv(t)
	out := e.run(integrationaudit.SectionScope)
	for _, id := range []string{"scope.leak", "scope.cross_scope_allowed", "scope.foreign_data_in_result"} {
		if f := finding(out, id); f != nil {
			t.Errorf("unexpected %s", f)
		}
	}
}

func TestScopeProbeFindsADifferentDenialCode(t *testing.T) {
	e := newEnv(t)
	// the suite says the other tenant is refused with permission_denied; the host says scope_violation
	e.pkg.Tests[0].ScopeProbes[3].ErrorCode = ptr(ap.ErrorCode("permission_denied"))
	out := e.run(integrationaudit.SectionScope)
	f := finding(out, "scope.denial_code_differs")
	if f == nil || f.Severity != contracts.SeverityWarning {
		t.Fatalf("want a warning that the code differs:\n%s", dump(out))
	}
	if hasCritical(out) {
		t.Errorf("the isolation held, so nothing may be critical:\n%s", dump(out))
	}
}

func TestScopeProbeFindsAnAllowedRequestRefused(t *testing.T) {
	e := newEnv(t)
	// editor-b1 is a principal of another tenant: the suite wrongly declares it allowed to read a note of org-a
	e.pkg.Tests[0].ScopeProbes[0].Principal = "editor-b1"
	out := e.run(integrationaudit.SectionScope)
	f := finding(out, "scope.allow_refused")
	if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capGet {
		t.Fatalf("want a critical scope.allow_refused on %s:\n%s", capGet, dump(out))
	}
}

func TestScopeProbeFindsAnExistenceOracle(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.oracle = true })
	out := e.run(integrationaudit.SectionScope)
	f := finding(out, "scope.existence_disclosed")
	if f == nil || f.Severity != contracts.SeverityWarning {
		t.Fatalf("want a warning that existence is disclosed:\n%s", dump(out))
	}
}

// A package whose application can not be reached, or that names no provisioner,
// is unverified, and critical for what changes data.
func TestScopeProbesWithoutADisposableTenantAreUnverifiedAndCriticalForConsequentialCapabilities(t *testing.T) {
	e := newEnv(t)
	e.in.Adapter = nil
	out := e.run(integrationaudit.SectionScope)
	if len(out.Covered) != 0 {
		t.Errorf("nothing was probed, covered = %v", out.Covered)
	}
	var crit, warn []string
	for _, f := range out.Findings {
		switch f.Severity {
		case contracts.SeverityCritical:
			crit = append(crit, f.Capability)
		case contracts.SeverityWarning:
			warn = append(warn, f.Capability)
		}
	}
	if len(crit) != 1 || crit[0] != capUpdate || len(warn) != 2 {
		t.Errorf("critical on %v, warnings on %v; want critical on the consequential capability and a warning for each read-only one", crit, warn)
	}
	if len(out.Unverified) != 3 {
		t.Errorf("unverified = %d items, want one per capability", len(out.Unverified))
	}
	for _, v := range out.Verdicts {
		if v.Status != integrationaudit.CapabilityUnverified {
			t.Errorf("verdict = %+v", v)
		}
	}
}
