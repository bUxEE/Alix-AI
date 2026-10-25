package probes

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// plainProvisioner hides the optional abilities of a provisioner: an application
// that can create, seed and remove a tenant and nothing else.
type plainProvisioner struct{ Provisioner }

func probeNamed(o *integrationaudit.Outcome, subjectPrefix string) *integrationaudit.ProbeResult {
	for i := range o.Probes {
		if strings.HasPrefix(o.Probes[i].Subject, subjectPrefix) {
			return &o.Probes[i]
		}
	}
	return nil
}

func TestSafeExecutionProbesPassForACorrectHost(t *testing.T) {
	e := newEnv(t)
	out := e.run(integrationaudit.SectionSafeExecution)
	if hasCritical(out) || len(findingIDs(out, contracts.SeverityWarning)) > 0 || len(findingIDs(out, contracts.SeverityInfo)) > 0 {
		t.Fatalf("a correct host must pass:\n%s", dump(out))
	}
	if len(out.Unverified) != 0 {
		t.Errorf("nothing is unverified for a host that can change a record on request:\n%s", dump(out))
	}
	if len(out.Covered) != 1 || out.Covered[0] != capUpdate {
		t.Errorf("covered = %v, want only the consequential capability", out.Covered)
	}
	var cov *integrationaudit.CoverageItem
	for i := range out.Coverage {
		if out.Coverage[i].Name == "safe_execution_probe_kinds" {
			cov = &out.Coverage[i]
		}
	}
	if cov == nil || cov.Covered != 7 || cov.Total != 7 {
		t.Errorf("coverage = %+v, want 7 of 7 kinds", cov)
	}
	kinds := map[string]bool{}
	for _, p := range out.Probes {
		if !p.Passed {
			t.Errorf("probe failed on a correct host: %+v", p)
		}
		kinds[p.Subject] = true
	}
	for _, want := range []string{
		"update-confirmed (confirmed_commit) as editor-a1", "update-unconfirmed (unconfirmed_commit) as editor-a1",
		"update-replayed (replayed_commit) as editor-a1", "update-expired (expired_preparation) as editor-a1",
		"update-stale (stale_record) as editor-a1", "update-cancelled (cancelled_preparation) as editor-a1",
		"update-uncertain (uncertain_commit_recovery) as editor-a1",
	} {
		if !kinds[want] {
			t.Errorf("no probe result %q", want)
		}
	}
	if n := e.sim.effectCount(); n != 1 {
		t.Errorf("the effect was applied %d times, want once: the refused commits must apply nothing", n)
	}
	if e.sim.tenantCount() != 0 {
		t.Errorf("tenants left behind: %d", e.sim.tenantCount())
	}
	// the read-only capabilities have nothing to commit, and the check says so
	skipped := 0
	for _, v := range out.Verdicts {
		if v.Status == integrationaudit.CapabilitySkipped {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("skipped verdicts = %d, want the two read-only capabilities", skipped)
	}
}

// SEC-041
// TestSafeExecutionProbeDetectsNonIdempotentCommit: an application whose commit
// applies its effect every time it is received, whatever the idempotency key, is
// found by the replay probe, and the double application is real.
func TestSafeExecutionProbeDetectsNonIdempotentCommit(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.nonIdempotent = true })
	out := e.run(integrationaudit.SectionSafeExecution)
	f := finding(out, "safety.replay_not_idempotent")
	if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capUpdate {
		t.Fatalf("want a critical safety.replay_not_idempotent on %s:\n%s", capUpdate, dump(out))
	}
	if n := e.sim.effectCount(); n < 2 {
		t.Errorf("the broken host applied the effect %d times: the probe found a failure that did not happen", n)
	}
	p := probeNamed(out, "update-replayed (replayed_commit)")
	if p == nil || p.Passed {
		t.Errorf("the replay probe must fail: %+v", p)
	}
	if p := probeNamed(out, "update-confirmed (confirmed_commit)"); p == nil || p.Passed {
		t.Errorf("a commit that applies twice is not committed once: %+v", p)
	}
	if e.sim.tenantCount() != 0 {
		t.Errorf("tenants left behind")
	}
}

// SEC-042
// TestSafeExecutionProbeDetectsMissingExpiry: an application that does not say how
// long it honours a preparation, that honours one longer than the capability
// declares, or that commits a preparation whose expiry has passed, is found.
func TestSafeExecutionProbeDetectsMissingExpiry(t *testing.T) {
	cases := []struct {
		name string
		knob func(*simKnobs)
		want string
	}{
		{"prepare does not say how long the preparation is honoured", func(k *simKnobs) { k.noExpiry = true }, "safety.expiry_missing"},
		{"the application honours a preparation longer than the capability declares", func(k *simKnobs) { k.longExpiry = true }, "safety.expiry_too_long"},
		{"the application commits a preparation whose expiry has passed", func(k *simKnobs) { k.noExpiryEnforce = true }, "safety.expired_commit_accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.knob)
			out := e.run(integrationaudit.SectionSafeExecution)
			f := finding(out, tc.want)
			if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capUpdate {
				t.Fatalf("want a critical %s on %s:\n%s", tc.want, capUpdate, dump(out))
			}
			if p := probeNamed(out, "update-expired (expired_preparation)"); p == nil || p.Passed {
				t.Errorf("the expiry probe must fail: %+v", p)
			}
			if tc.want == "safety.expired_commit_accepted" && e.sim.effectCount() < 1 {
				t.Errorf("the host did not apply the expired commit: the probe found a failure that did not happen")
			}
		})
	}
}

func TestSafeExecutionProbeDetectsAStaleCommit(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.ignoreStale = true })
	out := e.run(integrationaudit.SectionSafeExecution)
	if f := finding(out, "safety.stale_commit_accepted"); f == nil || f.Severity != contracts.SeverityCritical {
		t.Fatalf("want a critical safety.stale_commit_accepted:\n%s", dump(out))
	}
	if p := probeNamed(out, "update-stale (stale_record)"); p == nil || p.Passed {
		t.Errorf("the stale probe must fail: %+v", p)
	}
}

// The M1-12 contract case: Validate on unchanged data reports the snapshot
// version Prepare reported. A host that reports a per-response version, or one
// that cannot see a record has changed, breaks it.
func TestSafeExecutionProbeHoldsValidateToThePrepareSnapshotVersion(t *testing.T) {
	t.Run("a per-response version", func(t *testing.T) {
		e := newEnv(t, func(k *simKnobs) { k.etagVersion = true })
		out := e.run(integrationaudit.SectionSafeExecution)
		if f := finding(out, "results.validate_version_mismatch"); f == nil || f.Severity != contracts.SeverityCritical {
			t.Fatalf("want a critical results.validate_version_mismatch:\n%s", dump(out))
		}
	})
	t.Run("a version that does not move when a record changes", func(t *testing.T) {
		e := newEnv(t, func(k *simKnobs) { k.blindVersion = true })
		out := e.run(integrationaudit.SectionSafeExecution)
		if f := finding(out, "safety.validate_blind_to_change"); f == nil || f.Severity != contracts.SeverityCritical {
			t.Fatalf("want a critical safety.validate_blind_to_change:\n%s", dump(out))
		}
	})
	t.Run("without a way to change a record that part is unverified, not passed", func(t *testing.T) {
		e := newEnv(t)
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
		out, err := check.Run(context.Background(), e.in)
		if err != nil {
			t.Fatal(err)
		}
		if hasCritical(out) {
			t.Fatalf("an application that cannot change a record is not failed for it:\n%s", dump(out))
		}
		if len(out.Unverified) != 1 || out.Unverified[0].Subject != "safety:validate_notices_change" || out.Unverified[0].Capability != capUpdate {
			t.Errorf("unverified = %+v", out.Unverified)
		}
	})
}

func TestSafeExecutionProbeDetectsAStatusThatDoesNotShowTheCommit(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.statusLies = true })
	out := e.run(integrationaudit.SectionSafeExecution)
	if f := finding(out, "safety.status_disagrees_with_commit"); f == nil || f.Severity != contracts.SeverityCritical {
		t.Fatalf("want a critical safety.status_disagrees_with_commit:\n%s", dump(out))
	}
	if p := probeNamed(out, "update-uncertain (uncertain_commit_recovery)"); p == nil || p.Passed || !strings.HasPrefix(p.Observed, "lost_or_duplicated") {
		t.Errorf("an uncertain commit that cannot be recovered through Status must fail: %+v", p)
	}
}

// Only the no-record marker proves that a refused commit applied nothing: a refusal
// after which Status answers anything else is not passed as rejected_no_effect.
func TestSafeExecutionProbeNeedsTheNoRecordMarkerToShowNothingWasApplied(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.statusOddUnknown = true })
	out := e.run(integrationaudit.SectionSafeExecution)
	if p := probeNamed(out, "update-stale (stale_record)"); p != nil && p.Passed {
		t.Errorf("a refusal whose Status is not the no-record marker passed: %+v", p)
	}
	for _, id := range out.Covered {
		if id == capUpdate {
			t.Errorf("%s is covered although nothing showed that its refused commits applied nothing:\n%s", capUpdate, dump(out))
		}
	}
}

func TestSafeExecutionProbeDetectsACancelThatMisbehaves(t *testing.T) {
	for name, knob := range map[string]func(*simKnobs){
		"cancel reverses an applied update":           func(k *simKnobs) { k.cancelReverses = true },
		"a key commits after the cancel was accepted": func(k *simKnobs) { k.cancelThenCommit = true },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, knob)
			out := e.run(integrationaudit.SectionSafeExecution)
			if f := finding(out, "safety.cancel_misbehaves"); f == nil || f.Severity != contracts.SeverityCritical {
				t.Fatalf("want a critical safety.cancel_misbehaves:\n%s", dump(out))
			}
		})
	}
}

// The outcome a probe observes is compared with the outcome the suite declares.
func TestSafeExecutionProbeComparesWithTheDeclaredOutcome(t *testing.T) {
	t.Run("another outcome than declared is critical", func(t *testing.T) {
		e := newEnv(t)
		for i, p := range e.pkg.Tests[0].SafetyProbes {
			if p.Kind == kindStale {
				e.pkg.Tests[0].SafetyProbes[i].Expect = ap.PackageTestsSafetyExpect{Outcome: ap.PackageTestsSafetyExpectOutcomeCommittedOnce}
			}
		}
		out := e.run(integrationaudit.SectionSafeExecution)
		if f := finding(out, "safety.outcome_differs_from_package"); f == nil || f.Severity != contracts.SeverityCritical {
			t.Fatalf("want a critical safety.outcome_differs_from_package:\n%s", dump(out))
		}
	})
	t.Run("another error code than declared, with nothing applied, is information", func(t *testing.T) {
		e := newEnv(t)
		for i, p := range e.pkg.Tests[0].SafetyProbes {
			if p.Kind == kindStale {
				e.pkg.Tests[0].SafetyProbes[i].Expect.ErrorCode = ptr(ap.ErrorCode("conflict"))
			}
		}
		out := e.run(integrationaudit.SectionSafeExecution)
		f := finding(out, "safety.error_code_differs")
		if f == nil || f.Severity != contracts.SeverityInfo {
			t.Fatalf("want an informational safety.error_code_differs:\n%s", dump(out))
		}
		if hasCritical(out) {
			t.Errorf("the refusal held, so nothing is critical:\n%s", dump(out))
		}
	})
}
