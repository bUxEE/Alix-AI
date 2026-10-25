package probes

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func TestFailureProbesPassForACorrectHost(t *testing.T) {
	e := newEnv(t)
	out := e.run(integrationaudit.SectionDegradation)
	if hasCritical(out) || len(findingIDs(out, contracts.SeverityWarning)) > 0 {
		t.Fatalf("a correct host must pass:\n%s", dump(out))
	}
	if len(out.Unverified) != 0 {
		t.Errorf("nothing is unverified for a host that can fail on request:\n%s", dump(out))
	}
	if len(out.Covered) != 3 {
		t.Errorf("covered = %v, want the three user capabilities", out.Covered)
	}
	want := map[string]bool{
		"update without its required fields": false, "a successful commit is on record": false,
		"status for a key the application never saw": false, "commit with a dependency down applies nothing": false,
		"a commit whose reply was lost is recovered": false,
	}
	for _, p := range out.Probes {
		if !p.Passed {
			t.Errorf("probe failed on a correct host: %+v", p)
		}
		for k := range want {
			if p.Subject == k {
				want[k] = true
			}
		}
	}
	for k, seen := range want {
		if !seen && k != "update without its required fields" {
			t.Errorf("no probe result %q", k)
		}
	}
	// the record probe and the lost-reply probe apply an effect each; nothing else does
	if n := e.sim.effectCount(); n != 2 {
		t.Errorf("effects = %d, want 2", n)
	}
	if e.sim.tenantCount() != 0 {
		t.Errorf("tenants left behind: %d", e.sim.tenantCount())
	}
}

// SEC-047
// TestGenericSuccessOnFailureDetected: an application that answers ok to a request
// that cannot complete, to a status for a key it never saw, to a commit it did not
// record or to a dependency that is down, or that answers in a success-looking
// generic message that is not a typed result at all, is found.
func TestGenericSuccessOnFailureDetected(t *testing.T) {
	cases := []struct {
		name string
		knob func(*simKnobs)
		want string
		cap  string
	}{
		{"ok to a value outside the declared bounds", func(k *simKnobs) { k.genericSuccess = true }, "degradation.generic_success", capUpdate},
		{"ok to a status for a key it never saw", func(k *simKnobs) { k.statusOKUnknown = true }, "degradation.generic_success", capUpdate},
		{"a commit answered ok that has no record", func(k *simKnobs) { k.successNoRecord = true }, "degradation.success_without_record", capUpdate},
		{"ok while a dependency is down", func(k *simKnobs) { k.faultAnswersOK = true }, "degradation.generic_success", capGet},
		{"a bare {status: ok, message} object is not a typed result", func(k *simKnobs) { k.genericBlob = true }, "probes.untyped_reply", capUpdate},
		{"a preparation answered in prose is not a typed result", func(k *simKnobs) { k.prose = true }, "probes.untyped_reply", capUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.knob)
			out := e.run(integrationaudit.SectionDegradation)
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
			if e.sim.tenantCount() != 0 {
				t.Errorf("tenants left behind after a failing probe: %d", e.sim.tenantCount())
			}
		})
	}
}

func TestFailureProbesFindATypedFailureThatIsNotRecoverable(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.lostReplyNoRecord = true })
	out := e.run(integrationaudit.SectionDegradation)
	f := finding(out, "degradation.lost_reply_unrecoverable")
	if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capUpdate {
		t.Fatalf("want a critical degradation.lost_reply_unrecoverable on %s:\n%s", capUpdate, dump(out))
	}
}

func TestFailureProbesAnswerOfAnUnknownKeyThatIsNotTheMarkerIsAWarning(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.statusOddUnknown = true })
	out := e.run(integrationaudit.SectionDegradation)
	f := finding(out, "degradation.no_record_not_marked")
	if f == nil || f.Severity != contracts.SeverityWarning {
		t.Fatalf("want a warning that the no-record marker is not used:\n%s", dump(out))
	}
	if hasCritical(out) {
		t.Errorf("a safe but unrecoverable answer is not critical:\n%s", dump(out))
	}
}

// A commit answered unavailable applied nothing only if Status for its key is the
// no-record marker; any other answer leaves it unverified, never passed.
func TestFailureProbesNeedTheNoRecordMarkerAfterAnUnavailableCommit(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.statusOddUnknown = true })
	out := e.run(integrationaudit.SectionDegradation)
	if p := probeNamed(out, "commit with a dependency down applies nothing"); p != nil && p.Passed {
		t.Errorf("an unavailable commit whose Status is not the no-record marker passed: %+v", p)
	}
	found := false
	for _, u := range out.Unverified {
		found = found || (u.Capability == capUpdate && u.Subject == "degradation:dependency_failure")
	}
	if !found {
		t.Errorf("no unverified item for the unavailable commit:\n%s", dump(out))
	}
}

// A failed record probe is a finding, never a silent failed probe: Status that answers
// neither the commit's outcome nor the no-record marker disagrees with the commit.
func TestFailureProbeOfACommitThatStatusDoesNotReportIsAFinding(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.statusUnavailable = true })
	out := e.run(integrationaudit.SectionDegradation)
	if p := probeNamed(out, "a successful commit is on record"); p == nil || p.Passed {
		t.Fatalf("the record probe = %+v, want it failed:\n%s", p, dump(out))
	}
	if f := finding(out, "results.status_disagrees"); f == nil || f.Capability != capUpdate || f.Severity != contracts.SeverityCritical {
		t.Errorf("the failed record probe has no finding:\n%s", dump(out))
	}
}

func TestFailureProbesFindAnUndocumentedFailureCode(t *testing.T) {
	e := newEnv(t)
	c := e.pkg.Capabilities[capUpdate]
	c.FailureStates = failure("scope_violation", "permission_denied", "stale", "expired", "conflict", "unavailable", "uncertain_outcome") // no missing_fields, no validation_failed
	e.pkg.Capabilities[capUpdate] = c
	out := e.run(integrationaudit.SectionDegradation)
	f := finding(out, "degradation.undeclared_failure_code")
	if f == nil || f.Severity != contracts.SeverityWarning || f.Capability != capUpdate {
		t.Fatalf("want a warning for an undocumented failure code on %s:\n%s", capUpdate, dump(out))
	}
}

// Without a way to make the application fail, the typed handling of unavailable
// and lost-reply outcomes is unverified: recorded, never passed and never failed.
func TestFailureProbesWithoutFaultInjectionAreUnverified(t *testing.T) {
	e := newEnv(t)
	opts := e.opts()
	opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) {
		return plainProvisioner{e.sim}, nil
	}
	var check integrationaudit.Check
	for _, c := range Checks(opts) {
		if c.Section() == integrationaudit.SectionDegradation {
			check = c
		}
	}
	out, err := check.Run(context.Background(), e.in)
	if err != nil {
		t.Fatal(err)
	}
	if hasCritical(out) {
		t.Fatalf("an application that cannot be made to fail is not failed for it:\n%s", dump(out))
	}
	if len(out.Unverified) != 3 {
		t.Fatalf("unverified = %+v, want one per capability", out.Unverified)
	}
	for _, u := range out.Unverified {
		if u.Subject != "degradation:dependency_failure" {
			t.Errorf("unverified = %+v", u)
		}
	}
}

// refusingInjector is the sim behind a provisioner that offers fault injection and then
// refuses every fault it is asked for.
type refusingInjector struct{ *sim }

func (refusingInjector) InjectFault(context.Context, Fault) error {
	return contracts.New(contracts.ErrPermissionDenied, "no fault is armed")
}

// lateProvisionFailure is the sim behind a provisioner that can provision no further
// disposable tenant once the audit has asked for a failure at commit.
type lateProvisionFailure struct {
	*sim
	broken bool
}

func (p *lateProvisionFailure) InjectFault(ctx context.Context, f Fault) error {
	if f.Operation == adapters.OpCommit {
		p.broken = true
	}
	return p.sim.InjectFault(ctx, f)
}

func (p *lateProvisionFailure) CreateTenant(ctx context.Context, t Tenant) error {
	if p.broken {
		return contracts.New(contracts.ErrUnavailable, "the provisioner is gone")
	}
	return p.sim.CreateTenant(ctx, t)
}

func runDegradation(t *testing.T, e *simEnv, prov Provisioner) *integrationaudit.Outcome {
	t.Helper()
	opts := e.opts()
	opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) { return prov, nil }
	for _, c := range Checks(opts) {
		if c.Section() == integrationaudit.SectionDegradation {
			out, err := c.Run(context.Background(), e.in)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatal("no degradation check")
	return nil
}

// An application that offers fault injection but does not arm the fault it is asked for is
// unverified for it, as one that offers none is: nothing was made to fail.
func TestFailureProbesWhoseFaultIsRefusedAreUnverified(t *testing.T) {
	e := newEnv(t)
	out := runDegradation(t, e, refusingInjector{e.sim})
	for _, id := range []string{capGet, capList, capUpdate} {
		found := false
		for _, u := range out.Unverified {
			found = found || (u.Capability == id && u.Subject == "degradation:dependency_failure")
		}
		if !found {
			t.Errorf("%s: no unverified dependency failure although no fault could be armed:\n%s", id, dump(out))
		}
	}
	for _, p := range out.Probes {
		if strings.Contains(p.Subject, "dependency down") || strings.Contains(p.Subject, "lost") {
			t.Errorf("a fault probe was recorded although no fault was armed: %+v", p)
		}
	}
}

// A provisioner that fails while the fault probes need another disposable tenant leaves the
// capability not probed: the lost-reply probe never ran, so it is not covered.
func TestFailureProbesCutShortByProvisioningAreNotProbed(t *testing.T) {
	e := newEnv(t)
	out := runDegradation(t, e, &lateProvisionFailure{sim: e.sim})
	if slices.Contains(out.Covered, capUpdate) {
		t.Errorf("%s is covered although its lost-reply probe never ran:\n%s", capUpdate, dump(out))
	}
	f := finding(out, "probes.capability_not_probed")
	if f == nil || f.Capability != capUpdate {
		t.Errorf("want a probes.capability_not_probed on %s:\n%s", capUpdate, dump(out))
	}
	if probeNamed(out, "a commit whose reply was lost is recovered") != nil {
		t.Errorf("the lost-reply probe cannot have run")
	}
}

// When the application stops answering, the check stops asking: each further call
// would wait out its own timeout. What it did not get to is not probed.
func TestFailureProbesStopWhenTheApplicationStopsAnswering(t *testing.T) {
	e := newEnv(t, func(k *simKnobs) { k.downAfter = 2 })
	out := e.run(integrationaudit.SectionDegradation)
	if e.sim.calls > 2+3 {
		t.Errorf("the host received %d calls after it went down at 2: the check kept asking", e.sim.calls)
	}
	var critical, warned bool
	for _, f := range out.Findings {
		if f.CheckID == "probes.capability_not_probed" && f.Capability == capUpdate {
			critical = f.Severity == contracts.SeverityCritical
		}
		if f.CheckID == "probes.capability_not_probed_minor" {
			warned = true
		}
	}
	if !critical {
		t.Errorf("the consequential capability that was not reached must be critical:\n%s", dump(out))
	}
	if !warned {
		t.Errorf("a read-only capability that was not reached is a warning:\n%s", dump(out))
	}
	if e.sim.tenantCount() != 0 {
		t.Errorf("the disposable tenants must be removed even when the application stops answering: %d left", e.sim.tenantCount())
	}
}

func TestFailureProbesBoundsOfAField(t *testing.T) {
	c := simCapabilities()[capUpdate]
	name, value, found := outOfBounds(c)
	if !found || name != "text" || len(value.(string)) != 201 {
		t.Errorf("outOfBounds = %q, %v, %v", name, value, found)
	}
	c.Fields = []ap.CapabilityField{{Name: "count", Type: ap.CapabilityFieldTypeInteger, Required: true, Maximum: ptr(float64(5))}}
	if name, value, found := outOfBounds(c); !found || name != "count" || value.(int64) != 6 {
		t.Errorf("outOfBounds = %q, %v, %v", name, value, found)
	}
}
