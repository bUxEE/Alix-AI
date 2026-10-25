package probes

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// hangingProvisioner never answers a seed: it waits for its context.
type hangingProvisioner struct{ Provisioner }

func (hangingProvisioner) Seed(ctx context.Context, _ Seed) (SeedResult, error) {
	<-ctx.Done()
	return SeedResult{}, ctx.Err()
}

// A provisioner that hangs must not hang the audit: every call to it has a deadline,
// the fixture that could not be provisioned is recorded, and what the check could not
// probe is critical for what changes data.
func TestAProvisionerThatHangsDoesNotHangTheAudit(t *testing.T) {
	old := provisionTimeout
	provisionTimeout = 50 * time.Millisecond
	t.Cleanup(func() { provisionTimeout = old })

	e := newEnv(t)
	opts := e.opts()
	opts.Source = func(context.Context, *integrationaudit.Input) (Provisioner, error) {
		return hangingProvisioner{e.sim}, nil
	}
	var check integrationaudit.Check
	for _, c := range Checks(opts) {
		if c.Section() == integrationaudit.SectionDeterministic {
			check = c
		}
	}
	start := time.Now()
	out, err := check.Run(context.Background(), e.in)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the check took %v with a provisioner that hangs", took)
	}
	f := finding(out, "probes.capability_not_probed")
	if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capUpdate {
		t.Fatalf("want a critical probes.capability_not_probed on %s:\n%s", capUpdate, dump(out))
	}
	if e.sim.tenantCount() != 0 {
		t.Errorf("the tenant created before the seed hung was left behind: %d", e.sim.tenantCount())
	}
}

// timingOutCreate creates the tenant and then reports that the call ran out of time,
// as a provisioner whose reply was lost does.
type timingOutCreate struct{ *sim }

func (p timingOutCreate) CreateTenant(ctx context.Context, t Tenant) error {
	if err := p.sim.CreateTenant(ctx, t); err != nil {
		return err
	}
	return contracts.New(contracts.ErrTimeout, "no reply in time")
}

// failingSeedAndCleanup can create a tenant, but neither seed nor remove one.
type failingSeedAndCleanup struct{ *sim }

func (failingSeedAndCleanup) Seed(context.Context, Seed) (SeedResult, error) {
	return SeedResult{}, contracts.New(contracts.ErrValidationFailed, "seed refused")
}

func (failingSeedAndCleanup) Cleanup(context.Context, string) (CleanupReport, error) {
	return CleanupReport{}, contracts.New(contracts.ErrUnavailable, "cleanup is down")
}

// refusingCreate refuses every tenant and, as a provisioner that only acts on what it
// created does, refuses to remove one it never created.
type refusingCreate struct{ *sim }

func (refusingCreate) CreateTenant(context.Context, Tenant) error {
	return contracts.New(contracts.ErrConflict, "the tenant is refused")
}

func (refusingCreate) Cleanup(context.Context, string) (CleanupReport, error) {
	return CleanupReport{}, contracts.New(contracts.ErrPermissionDenied, "not a tenant of this audit")
}

// A world that could not be provisioned is removed as far as it got, including a tenant
// whose creation failed without saying whether it happened, and what its removal left
// behind is reported like the leftovers of any other world. A creation the application
// refused created nothing, so nothing is reported for it.
func TestAWorldThatCouldNotBeProvisionedIsRemoved(t *testing.T) {
	t.Run("a tenant whose creation timed out", func(t *testing.T) {
		e := newEnv(t)
		out := runDegradation(t, e, timingOutCreate{e.sim})
		if n := e.sim.tenantCount(); n != 0 {
			t.Errorf("%d tenant(s) whose creation timed out were left behind", n)
		}
		if hasFinding(out, "probes.tenant_left_behind") {
			t.Errorf("everything was removed, yet something is reported left behind:\n%s", dump(out))
		}
	})
	t.Run("a cleanup that fails", func(t *testing.T) {
		e := newEnv(t)
		out := runDegradation(t, e, failingSeedAndCleanup{e.sim})
		if !hasFinding(out, "probes.tenant_left_behind") {
			t.Errorf("the tenant the cleanup could not remove is not reported:\n%s", dump(out))
		}
	})
	t.Run("a creation that was refused", func(t *testing.T) {
		e := newEnv(t)
		out := runDegradation(t, e, refusingCreate{e.sim})
		if hasFinding(out, "probes.tenant_left_behind") {
			t.Errorf("a tenant that was never created is reported left behind:\n%s", dump(out))
		}
	})
}

// SEC-081
// The throwaway store the platform's pipeline runs on is created owner-only and is
// removed when the check is done, whatever the check found.
func TestSecRigStoreIsOwnerOnlyAndRemoved(t *testing.T) {
	e := newEnv(t)
	scratch := e.opts().ScratchDir
	g, err := newRig(context.Background(), Options{ScratchDir: scratch}, e.pkg, e.guard, simT0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Errorf("the rig directory is %v, want owner-only", st.Mode().Perm())
	}
	if filepath.Dir(g.dir) != filepath.Clean(scratch) {
		t.Errorf("the rig directory %s is not under the scratch directory %s", g.dir, scratch)
	}
	g.close()
	if _, err := os.Stat(g.dir); !os.IsNotExist(err) {
		t.Errorf("the rig directory is still there after close: %v", err)
	}

	// and after a whole check
	for _, section := range []int{integrationaudit.SectionSafeExecution, integrationaudit.SectionRuntimeSmoke} {
		e.run(section)
		left, err := os.ReadDir(scratch)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("section %d left %d entries in the scratch directory", section, len(left))
		}
	}
}
