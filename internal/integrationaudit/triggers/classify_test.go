package triggers

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/integrationaudit"
)

// What stops traffic is decided by the kind of change and nothing else: this is the
// whole table, so a kind added later has to be put on one side of it.
func TestCriticalKinds(t *testing.T) {
	want := map[integrationaudit.TriggerKind]bool{
		KindSignature:                           true,
		KindScope:                               true,
		KindMCPSchema:                           true,
		KindAuditBlocked:                        true,
		integrationaudit.TriggerPermissionModel: true,

		integrationaudit.TriggerPackage:            false,
		integrationaudit.TriggerDocumentation:      false,
		integrationaudit.TriggerContract:           false,
		integrationaudit.TriggerCapability:         false,
		integrationaudit.TriggerSource:             false,
		integrationaudit.TriggerAdapterSurface:     false,
		integrationaudit.TriggerApplicationVersion: false,
		integrationaudit.TriggerCoreVersion:        false,
		KindBaseline:                               false,
		KindQuarantined:                            false,
	}
	for k, crit := range want {
		if got := newChange(k, "x", "y"); got.Critical != crit {
			t.Errorf("%s: critical = %v, want %v", k, got.Critical, crit)
		}
	}
	// a kind nobody listed is not critical by accident of the zero value: it is
	// reported as unknown here so the table above is extended on purpose
	if criticalKind("something_new") {
		t.Errorf("an unknown kind is not critical; add it to the table on purpose")
	}
}

// diffOf opens two variants of the demo library through the installer, as the
// watcher does, and returns what differs between them.
func diffOf(t *testing.T, edit func(dir string)) []string {
	t.Helper()
	h := newHarness(t)
	live := h.install(validMinimalDir)
	other := h.install(variant(t, "1.0.1", edit))
	a, _, err := h.inst.Open(ctx, appID, live.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := h.inst.Open(ctx, appID, other.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	fa, err := facetsOf(a, live)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := facetsOf(b, other)
	if err != nil {
		t.Fatal(err)
	}
	return kinds(diffPackages(fa, fb))
}

// Each edit moves exactly the facets it should: a reworded policy moves no
// permission, a limit of a source moves no scope, and nothing moves anything it
// did not touch.
func TestDiffNamesWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(testing.TB) func(string)
		want []string
	}{
		"nothing but the package": {
			edit: func(testing.TB) func(string) { return nil },
			want: []string{"package/demo-library=changed"},
		},
		"a capability contract": {
			edit: capabilityEdit,
			want: []string{"capability/reservations.create=changed", "package/demo-library=changed"},
		},
		"a source limit": {
			edit: sourceLimitEdit,
			want: []string{"package/demo-library=changed", "source_definition/loan-policies=changed"},
		},
		"a source scope": {
			edit: sourceScopeEdit,
			want: []string{"package/demo-library=changed", "scope/scope=changed!", "source_definition/loan-policies=changed"},
		},
		"a new role": {
			edit: roleEdit,
			want: []string{"package/demo-library=changed", "permission_model/permissions=changed!"},
		},
		"a reworded policy": {
			edit: policyWordingEdit,
			want: []string{"package/demo-library=changed"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := diffOf(t, tc.edit(t))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("changes (-want +got):\n%s", diff)
			}
		})
	}
}

// The adapter-surface digest the watcher compares with is the one the audit
// recorded: byte for byte, whatever order the adapter lists things in.
func TestSurfaceHashMatchesTheReport(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	d := okDescription()
	// listed in the least convenient order, with a duplicate host operation
	d.Capabilities[0], d.Capabilities[1] = d.Capabilities[1], d.Capabilities[0]
	ops := d.Capabilities[0].Operations
	ops[0], ops[len(ops)-1] = ops[len(ops)-1], ops[0]
	d.Unmapped = []string{"z.op", "a.op", "z.op"}
	h.app.desc = d

	run, err := h.runner.Audit(ctx, integrationaudit.RunRequest{ApplicationID: appID, Adapter: h.app})
	if err != nil {
		t.Fatal(err)
	}
	var recorded *integrationaudit.Trigger
	for i, tr := range run.Signed.Report.ReauditTriggers {
		if tr.Kind == integrationaudit.TriggerAdapterSurface {
			recorded = &run.Signed.Report.ReauditTriggers[i]
		}
	}
	if recorded == nil {
		t.Fatal("the report records no adapter surface")
	}
	if got := SurfaceHash(d); got != recorded.Hash || recorded.Subject != d.Transport {
		t.Errorf("SurfaceHash = %s, the report recorded %s (%s)", got, recorded.Hash, recorded.Subject)
	}

	// and it moves with what matters only
	same := okDescription()
	same.Unmapped = []string{"a.op", "z.op"}
	if SurfaceHash(same) != SurfaceHash(d) {
		t.Errorf("the order of what the adapter lists must not move the hash")
	}
	more := okDescription()
	more.Unmapped = []string{"a.op", "z.op", "new.op"}
	if SurfaceHash(more) == SurfaceHash(same) {
		t.Errorf("a new host operation must move the hash")
	}
	fewer := okDescription()
	fewer.Unmapped = []string{"a.op", "z.op"}
	fewer.Capabilities[1].Operations = []adapters.Operation{adapters.OpValidate}
	if SurfaceHash(fewer) == SurfaceHash(same) {
		t.Errorf("a missing operation must move the hash")
	}
	versioned := okDescription()
	versioned.Unmapped = []string{"a.op", "z.op"}
	versioned.ApplicationVersion = "9.9.9"
	if SurfaceHash(versioned) != SurfaceHash(same) {
		t.Errorf("the application version is compared on its own, not through the surface hash")
	}
}
