package probes

import (
	"context"
	"sort"
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func unverifiedSubjects(o *integrationaudit.Outcome) []string {
	var out []string
	for _, u := range o.Unverified {
		out = append(out, u.Subject)
	}
	sort.Strings(out)
	return out
}

func TestSmokeRunsTheTextAndActionPathsAndChecksTheAuditEvents(t *testing.T) {
	e := newEnv(t)
	out := e.run(integrationaudit.SectionRuntimeSmoke)
	if hasCritical(out) || len(findingIDs(out, contracts.SeverityWarning)) > 0 {
		t.Fatalf("a correct host must pass:\n%s", dump(out))
	}
	for _, p := range out.Probes {
		if !p.Passed {
			t.Errorf("probe failed on a correct host: %+v", p)
		}
	}
	// what the audit's pipeline cannot produce is unverified, never present and never missing:
	// the RAG path, and the event the dialogue controller writes
	got := unverifiedSubjects(out)
	want := []string{"audit_event:dialogue.turn (smoke-text)", "smoke_path:smoke-rag"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("unverified = %v, want %v", got, want)
	}
	for _, name := range []string{
		"smoke-text: audit event policy.decision", "smoke-action: audit event action.prepared",
		"smoke-action: audit event action.confirmed", "smoke-action: audit event action.committed",
	} {
		if p := probeNamed(out, name); p == nil || !p.Passed {
			t.Errorf("probe %q = %+v", name, p)
		}
	}
	if n := e.sim.effectCount(); n != 1 {
		t.Errorf("the action path applied the effect %d times, want once", n)
	}
	if len(out.Covered) != 2 {
		t.Errorf("covered = %v, want the two capabilities of the paths", out.Covered)
	}
}

// TestSmokeCleansUpDisposableTenant: the disposable tenants are removed when the
// smoke test is done, and the audit proves it by asking the application what is
// left and by cleaning up a second time. A tenant that is left behind, in either
// way, is critical.
func TestSmokeCleansUpDisposableTenant(t *testing.T) {
	t.Run("a correct host is left with nothing", func(t *testing.T) {
		e := newEnv(t)
		out := e.run(integrationaudit.SectionRuntimeSmoke)
		if e.sim.tenantCount() != 0 || e.sim.noteCount() != 0 {
			t.Fatalf("%d tenants and %d notes remain", e.sim.tenantCount(), e.sim.noteCount())
		}
		p := probeNamed(out, "sim-safety: the disposable tenants are removed")
		if p == nil || !p.Passed || p.Observed != "nothing left" {
			t.Errorf("cleanup probe = %+v", p)
		}
		if e.sim.created < 4 || e.sim.cleaned != e.sim.created {
			t.Errorf("created %d tenants and cleaned %d: every tenant made must be removed", e.sim.created, e.sim.cleaned)
		}
		if hasFinding(out, "probes.tenant_left_behind") {
			t.Errorf("nothing was left behind:\n%s", dump(out))
		}
	})

	t.Run("an application whose cleanup leaves records behind", func(t *testing.T) {
		e := newEnv(t, func(k *simKnobs) { k.leftBehind = true })
		out := e.run(integrationaudit.SectionRuntimeSmoke)
		f := finding(out, "probes.tenant_left_behind")
		if f == nil || f.Severity != contracts.SeverityCritical {
			t.Fatalf("want a critical probes.tenant_left_behind:\n%s", dump(out))
		}
		if e.sim.noteCount() == 0 {
			t.Errorf("the broken host left nothing behind: the probe found a failure that did not happen")
		}
		p := probeNamed(out, "sim-safety: the disposable tenants are removed")
		if p == nil || p.Passed {
			t.Errorf("cleanup probe = %+v, want a failed probe", p)
		}
	})

	t.Run("an application that reports an empty tenant that is still there", func(t *testing.T) {
		e := newEnv(t, func(k *simKnobs) { k.staysAfterCleanup = true })
		out := e.run(integrationaudit.SectionRuntimeSmoke)
		f := finding(out, "probes.tenant_left_behind")
		if f == nil || f.Severity != contracts.SeverityCritical {
			t.Fatalf("a second cleanup finds the tenant: want a critical probes.tenant_left_behind:\n%s", dump(out))
		}
		if e.sim.tenantCount() == 0 {
			t.Errorf("the broken host removed its tenants: the probe found a failure that did not happen")
		}
	})

	t.Run("cleanup happens when a path fails", func(t *testing.T) {
		e := newEnv(t, func(k *simKnobs) { k.prose = true })
		out := e.run(integrationaudit.SectionRuntimeSmoke)
		if f := finding(out, "smoke.path_failed"); f == nil || f.Severity != contracts.SeverityCritical {
			t.Fatalf("want a critical smoke.path_failed:\n%s", dump(out))
		}
		if e.sim.tenantCount() != 0 {
			t.Errorf("%d tenants left behind after a failed path", e.sim.tenantCount())
		}
	})

	t.Run("cleanup happens when the provisioner panics", func(t *testing.T) {
		e := newEnv(t)
		opts := e.opts()
		opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) {
			return &panicOnSecondSeed{Provisioner: e.sim}, nil
		}
		var check integrationaudit.Check
		for _, c := range Checks(opts) {
			if c.Section() == integrationaudit.SectionRuntimeSmoke {
				check = c
			}
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic was swallowed: the framework must see it")
				}
			}()
			_, _ = check.Run(context.Background(), e.in)
		}()
		if e.sim.tenantCount() != 0 {
			t.Errorf("%d tenants left behind after a panic in the provisioner", e.sim.tenantCount())
		}
	})

	t.Run("cleanup is not cut short by the audit's own cancellation", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		opts := e.opts()
		opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) {
			return &cancelAfterSeed{Provisioner: e.sim, cancel: cancel}, nil
		}
		var check integrationaudit.Check
		for _, c := range Checks(opts) {
			if c.Section() == integrationaudit.SectionRuntimeSmoke {
				check = c
			}
		}
		_, _ = check.Run(ctx, e.in)
		if e.sim.tenantCount() != 0 {
			t.Errorf("%d tenants left behind after the audit was cancelled", e.sim.tenantCount())
		}
	})
}

type panicOnSecondSeed struct {
	Provisioner
	seeds int
}

func (p *panicOnSecondSeed) Seed(ctx context.Context, s Seed) (SeedResult, error) {
	p.seeds++
	if p.seeds == 2 {
		panic("the provisioner failed")
	}
	return p.Provisioner.Seed(ctx, s)
}

type cancelAfterSeed struct {
	Provisioner
	cancel context.CancelFunc
}

func (c *cancelAfterSeed) Seed(ctx context.Context, s Seed) (SeedResult, error) {
	res, err := c.Provisioner.Seed(ctx, s)
	c.cancel()
	return res, err
}

func TestSmokeFindsAnAuditEventThePathMustProduce(t *testing.T) {
	e := newEnv(t)
	paths := e.pkg.Tests[0].Smoke.Paths
	paths[2].Expect.AuditEvents = append(paths[2].Expect.AuditEvents, "action.expired") // never produced by a commit
	out := e.run(integrationaudit.SectionRuntimeSmoke)
	f := finding(out, "smoke.audit_event_missing")
	if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capUpdate {
		t.Fatalf("want a critical smoke.audit_event_missing on %s:\n%s", capUpdate, dump(out))
	}
}

func TestSmokeVoiceAndRAGPathsAreUnverifiedUnlessAnotherCheckCoversThem(t *testing.T) {
	e := newEnv(t)
	paths := append(e.pkg.Tests[0].Smoke.Paths, ap.PackageTestsSmokePath{
		ID: "smoke-voice", Path: ap.PackageTestsSmokePathPathVoice, Principal: "viewer-a1", Utterance: "What notes do I have?",
		Expect: ap.PackageTestsSmokePathExpect{AuditEvents: []ap.DottedID{"dialogue.turn"}},
	})
	e.pkg.Tests[0].Smoke.Paths = paths
	out := e.run(integrationaudit.SectionRuntimeSmoke)
	got := unverifiedSubjects(out)
	for _, want := range []string{"smoke_path:smoke-rag", "smoke_path:smoke-voice"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("unverified = %v, want %s among them", got, want)
		}
	}
	// with the RAG probes registered the RAG path is theirs, not an unverified item of this check
	e.in.Registered = []integrationaudit.CheckInfo{{ID: "rag.smoke", Section: integrationaudit.SectionRuntimeSmoke}}
	out = e.run(integrationaudit.SectionRuntimeSmoke)
	for _, g := range unverifiedSubjects(out) {
		if g == "smoke_path:smoke-rag" {
			t.Errorf("the RAG path is left to the RAG probes that are registered")
		}
	}
}

func TestSmokeLifecycleOrder(t *testing.T) {
	if !indexOrder([]string{"policy.decision", "action.prepared", "policy.decision", "action.confirmed", "action.committing", "action.committed"},
		"action.prepared", "action.confirmed", "action.committing", "action.committed") {
		t.Error("the lifecycle in order must pass")
	}
	if indexOrder([]string{"action.committed", "action.prepared"}, "action.prepared", "action.committed") {
		t.Error("a commit before its preparation must fail")
	}
}
