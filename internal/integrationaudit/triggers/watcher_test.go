package triggers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// The variants of the demo library package the tests upload.
var (
	// a capability contract changed: documentation of what is allowed, not who may
	capabilityEdit = func(t testing.TB) func(dir string) {
		return func(d string) {
			editFile(t, d, "capabilities/reservations-create.yaml", "ttl_seconds: 600", "ttl_seconds: 300")
		}
	}
	// a source definition changed in a way that moves no scope
	sourceLimitEdit = func(t testing.TB) func(dir string) {
		return func(d string) { editFile(t, d, "rag-sources/loan-policies.yaml", "max_files: 500", "max_files: 400") }
	}
	// a source definition whose tenant and workspace binding changed: a scope change
	sourceScopeEdit = func(t testing.TB) func(dir string) {
		return func(d string) {
			editFile(t, d, "rag-sources/loan-policies.yaml",
				"  tenant:\n    mode: shared\n  workspace:\n    mode: shared", "  tenant:\n    mode: request\n  workspace:\n    mode: request")
		}
	}
	// a role the application asserts that the package did not know: the permission model
	roleEdit = func(t testing.TB) func(dir string) {
		return func(d string) {
			editFile(t, d, "alixai.yaml",
				"      description: An unregistered visitor who can only search the catalogue.\n",
				"      description: An unregistered visitor who can only search the catalogue.\n    staff:\n      description: Library staff who handle the desk.\n")
		}
	}
	// a policy reworded and nothing else
	policyWordingEdit = func(t testing.TB) func(dir string) {
		return func(d string) {
			editFile(t, d, "policies/default-access.yaml",
				"Members search and reserve; visitors only search.", "Members may search and reserve; visitors may only search.")
		}
	}
)

func kinds(cs []Change) []string {
	var out []string
	for _, c := range cs {
		crit := ""
		if c.Critical {
			crit = "!"
		}
		out = append(out, string(c.Kind)+"/"+c.Subject+"="+c.Detail+crit)
	}
	return out
}

func hasChange(cs []Change, kind integrationaudit.TriggerKind, critical bool) bool {
	for _, c := range cs {
		if c.Kind == kind && c.Critical == critical {
			return true
		}
	}
	return false
}

// servedDuring records what the registry serves each time the application's
// adapter is asked to describe itself, which the re-audit does in the middle of
// its work. "stopped" means the registry served nothing.
func servedDuring(h *harness, live apppkg.Record) *[]observation {
	var seen []observation
	h.app.onDescribe = func() {
		o := observation{served: "stopped", live: mustGet(h.t, h, live).State}
		if info, err := h.reg.Info(ctx, appID); err == nil {
			o.served = info.ContentHash
		}
		seen = append(seen, o)
	}
	return &seen
}

// observation is what was true when the application was asked to describe itself.
type observation struct {
	// served is the hash the registry served, or "stopped" when it served nothing.
	served string
	// live is the state of the package that was live before the change.
	live apppkg.State
}

// mustHaveStoppedFirst requires that every look the application got after the live
// package was quarantined found the registry serving nothing, and that there was
// at least one: the audit ran, and it ran with traffic already stopped.
func mustHaveStoppedFirst(t *testing.T, seen []observation) {
	t.Helper()
	after := 0
	for _, o := range seen {
		if o.live != apppkg.StateQuarantined {
			continue
		}
		after++
		if o.served != "stopped" {
			t.Errorf("an audit ran while the registry still served %q: traffic must stop first", o.served)
		}
	}
	if after == 0 {
		t.Errorf("fixture: no audit ran after the quarantine (%d looks)", len(seen))
	}
}

// With nothing changed the watcher audits nothing, says nothing and leaves the
// package serving: a comparison, not a poll that re-runs the audit.
func TestNothingChangedAuditsNothing(t *testing.T) {
	h := newHarness(t)
	live := h.goLive(validMinimalDir)
	h.clk.Advance(time.Hour)
	runs, events := h.totalRuns(), len(h.events())

	res, err := h.w.Check(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 || len(res.Audits) != 0 || len(res.Quarantined) != 0 || len(res.Unverified) != 0 {
		t.Errorf("nothing changed, got %+v", res)
	}
	if h.totalRuns() != runs || len(h.events()) != events || len(h.notices.all()) != 0 {
		t.Errorf("an unchanged application must cause no audit run, no event and no notice")
	}
	h.mustServe(live.ManifestHash)
}

// SEC-062: a new package after activation is audited again at once, and the
// package that was activated stays in force while that happens.
func TestPackageChangeTriggersReaudit(t *testing.T) {
	h := newHarness(t)
	live := h.goLive(validMinimalDir)
	h.clk.Advance(2 * time.Second)

	up := h.install(variant(t, "1.0.1", capabilityEdit(t)))
	during := servedDuring(h, live)
	res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}

	if h.runs(up.ManifestHash) != 1 || len(res.Audits) != 1 || res.Audits[0].ManifestHash != up.ManifestHash {
		t.Fatalf("the uploaded package must be audited once: runs=%d audits=%+v", h.runs(up.ManifestHash), res.Audits)
	}
	if !hasChange(res.Changes, integrationaudit.TriggerPackage, false) || !hasChange(res.Changes, integrationaudit.TriggerCapability, false) {
		t.Errorf("the changes must name the package and the capability: %v", kinds(res.Changes))
	}
	if anyCritical(res.Changes) || len(res.Quarantined) != 0 {
		t.Errorf("a changed contract is not a critical change: %v", kinds(res.Changes))
	}
	// the previous activation stayed in force while the audit ran, and after it
	if len(*during) == 0 {
		t.Fatal("fixture: the audit never asked the application to describe itself")
	}
	for _, o := range *during {
		if got := o.served; got != live.ManifestHash {
			t.Errorf("while the re-audit ran the registry served %q, want the activated package", got)
		}
	}
	h.mustServe(live.ManifestHash)
	if h.state(live) != apppkg.StateActive || h.state(up) != apppkg.StateQuarantined {
		t.Errorf("states: live %s, upload %s", h.state(live), h.state(up))
	}
	ev := h.events()
	if count(ev, "reaudit.triggered") != 1 || count(ev, "reaudit.completed") != 1 || count(ev, "reaudit.quarantined") != 0 || count(ev, "package.quarantined") != 0 {
		t.Errorf("ops events: %v", ev)
	}
	if got := h.notices.of("reaudit.completed"); len(got) != 1 || got[0].RunID != res.Audits[0].RunID {
		t.Errorf("the operator is told which run: %+v", got)
	}

	// the same upload again, and the same bytes installed again, change nothing
	runs := h.totalRuns()
	if _, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash); err != nil {
		t.Fatal(err)
	}
	again, err := h.inst.Install(ctx, apppkg.FromDir(variant(t, "1.0.1", capabilityEdit(t))))
	if err != nil || again.Created {
		t.Fatalf("fixture: the same bytes are one package: %+v %v", again, err)
	}
	if _, err := h.w.Check(ctx, appID); err != nil {
		t.Fatal(err)
	}
	if h.totalRuns() != runs {
		t.Errorf("a package that was audited is not audited again")
	}
}

// With a job queue the upload hook does the part that must be immediate and queues
// the audit; the job body then does it.
func TestUploadQueuesTheReauditAndTheJobRunsIt(t *testing.T) {
	h := newHarness(t, func(o *hopts) { o.queue = true })
	live := h.goLive(validMinimalDir)
	h.clk.Advance(2 * time.Second)
	up := h.install(variant(t, "1.0.1", capabilityEdit(t)))

	res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	jobs := h.enq.all()
	if !res.Deferred || len(jobs) != 1 || h.runs(up.ManifestHash) != 0 {
		t.Fatalf("the audit is queued, not run: deferred=%v jobs=%d runs=%d", res.Deferred, len(jobs), h.runs(up.ManifestHash))
	}
	if err := jobs[0].scope.Validate(); err != nil || jobs[0].scope.ApplicationID != appID {
		t.Errorf("the job's scope must be valid and about the application: %v", err)
	}
	if err := h.w.HandleJob(ctx, ServiceIdentity, jobs[0].payload); err != nil {
		t.Fatal(err)
	}
	if h.runs(up.ManifestHash) != 1 {
		t.Errorf("the job must audit the upload")
	}
	h.mustServe(live.ManifestHash)
}

// SEC-062: a changed source definition is audited again; a source whose scope
// moved also stops traffic.
func TestSourceDefinitionChangeTriggersReaudit(t *testing.T) {
	t.Run("a limit changed", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		up := h.install(variant(t, "1.0.1", sourceLimitEdit(t)))

		res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, integrationaudit.TriggerSource, false) {
			t.Errorf("the changes must name the source: %v", kinds(res.Changes))
		}
		if anyCritical(res.Changes) {
			t.Errorf("a limit is not a scope: %v", kinds(res.Changes))
		}
		if h.runs(up.ManifestHash) != 1 {
			t.Errorf("the changed source must be audited again")
		}
		h.mustServe(live.ManifestHash)
	})

	t.Run("a scope binding changed", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		up := h.install(variant(t, "1.0.1", sourceScopeEdit(t)))
		during := servedDuring(h, live)

		res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, KindScope, true) || !hasChange(res.Changes, integrationaudit.TriggerSource, false) {
			t.Fatalf("a moved scope is a critical change of kind scope: %v", kinds(res.Changes))
		}
		h.mustNotServe("a scope change stops traffic at once, with the registry's cache still fresh")
		if h.state(live) != apppkg.StateQuarantined || h.runs(up.ManifestHash) != 1 {
			t.Errorf("live is %s, upload runs %d", h.state(live), h.runs(up.ManifestHash))
		}
		mustHaveStoppedFirst(t, *during)
	})
}

// SEC-062: a changed permission model stops live traffic immediately and is
// audited again; a reworded policy is only audited again.
func TestPermissionModelChangeTriggersReaudit(t *testing.T) {
	t.Run("a new role", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second) // the report that activated live predates the quarantine below
		up := h.install(variant(t, "1.0.1", roleEdit(t)))
		during := servedDuring(h, live)

		res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, integrationaudit.TriggerPermissionModel, true) {
			t.Fatalf("a changed permission model is a critical change: %v", kinds(res.Changes))
		}
		// traffic stopped at once: the clock never moved, the registry's entry was fresh
		h.mustNotServe("after a permission model change")
		if h.state(live) != apppkg.StateQuarantined {
			t.Errorf("the live package is %s, want quarantined", h.state(live))
		}
		rec, _ := h.inst.Get(ctx, appID, live.ManifestHash)
		if !strings.HasPrefix(rec.StatusReason, ReasonPrefix) || !strings.Contains(rec.StatusReason, "permission_model") {
			t.Errorf("the quarantine says why: %q", rec.StatusReason)
		}
		// stopped before anything was audited, and the re-audit ran
		mustHaveStoppedFirst(t, *during)
		if h.runs(up.ManifestHash) != 1 {
			t.Errorf("the uploaded package must be audited")
		}
		ev := h.events()
		if i, j := indexOf(ev, "package.quarantined"), indexOf(ev, "reaudit.quarantined"); i < 0 || j < i {
			t.Errorf("the quarantine is recorded, then the ops event: %v", ev)
		}
		if got := h.notices.of("reaudit.quarantined"); len(got) != 1 || got[0].Severity != SeverityCritical || !anyCritical(got[0].Changes) {
			t.Errorf("the operator is told, critically: %+v", got)
		}
		d := h.eventData("reaudit.quarantined")
		if d.ManifestHash != live.ManifestHash || !d.Critical || !strings.HasPrefix(d.Reason, ReasonPrefix) {
			t.Errorf("event data: %+v", d)
		}

		// the quarantined package was audited again too, after the quarantine, so a
		// report postdates it: the operator may reactivate it. The watcher never does.
		if h.runs(live.ManifestHash) != 2 {
			t.Fatalf("the quarantined package needs a report that postdates the quarantine: runs=%d", h.runs(live.ManifestHash))
		}
		h.mustNotServe("the watcher never reactivates a package")
		if _, err := h.runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: appID, ManifestHash: live.ManifestHash, OperatorID: "alice"}); err != nil {
			t.Errorf("the operator can activate on the report the watcher made: %v", err)
		}
	})

	t.Run("a reworded policy", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		up := h.install(variant(t, "1.0.1", policyWordingEdit(t)))

		res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if anyCritical(res.Changes) || len(res.Quarantined) != 0 {
			t.Errorf("wording grants nothing and stops nothing: %v", kinds(res.Changes))
		}
		if h.runs(up.ManifestHash) != 1 {
			t.Errorf("a changed policy file is audited again")
		}
		h.mustServe(live.ManifestHash)
	})
}

// SEC-049: a tool whose pinned schema changed quarantines the application: live
// traffic stops before the call returns, however fresh the registry's cache, and
// the quarantine is confirmed against the registry.
func TestMCPSchemaDriftQuarantines(t *testing.T) {
	drift := Drift{Endpoint: "library-mcp", Tool: "reserve_title", Kind: "schema_changed", Pinned: "aaaaaaaaaaaaaaaa", Observed: "bbbbbbbbbbbbbbbb"}

	t.Run("pushed by the adapter", func(t *testing.T) {
		h := newHarness(t, func(o *hopts) { o.queue = true })
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)

		res, err := h.w.ReportDrift(ctx, appID, drift)
		if err != nil {
			t.Fatal(err)
		}
		// no time has passed since the registry loaded the package: it would serve it
		// for another 28 seconds if nothing evicted it
		h.mustNotServe("after an MCP schema drift")
		if h.state(live) != apppkg.StateQuarantined || len(res.Quarantined) != 1 || res.Quarantined[0] != live.ManifestHash {
			t.Errorf("live is %s, result %+v", h.state(live), res)
		}
		if !hasChange(res.Changes, KindMCPSchema, true) {
			t.Errorf("a drift is a critical change of kind mcp_schema: %v", kinds(res.Changes))
		}
		ev := h.events()
		if i, j := indexOf(ev, "package.quarantined"), indexOf(ev, "reaudit.quarantined"); i < 0 || j < i {
			t.Errorf("the quarantine is recorded, then the ops event: %v", ev)
		}
		if got := h.notices.of("reaudit.quarantined"); len(got) != 1 || got[0].Severity != SeverityCritical || got[0].Changes[0].Subject != "library-mcp/reserve_title" {
			t.Errorf("the operator is told which tool: %+v", got)
		}
		// the audit is not run on the adapter's call path: it is queued
		jobs := h.enq.all()
		if !res.Deferred || len(jobs) != 1 || h.runs(live.ManifestHash) != 1 {
			t.Fatalf("the re-audit is queued, not run: deferred=%v jobs=%d runs=%d", res.Deferred, len(jobs), h.runs(live.ManifestHash))
		}

		// a second report of the same drift stops nothing more
		if _, err := h.w.ReportDrift(ctx, appID, drift); err != nil {
			t.Fatal(err)
		}
		if count(h.events(), "package.quarantined") != 1 {
			t.Errorf("the package is quarantined once: %v", h.events())
		}
		h.mustNotServe("after a second report")

		// the job audits the quarantined package; its report postdates the quarantine,
		// so the operator may reactivate it, and only the operator does
		if err := h.w.HandleJob(ctx, ServiceIdentity, jobs[0].payload); err != nil {
			t.Fatal(err)
		}
		if h.runs(live.ManifestHash) != 2 {
			t.Fatalf("the quarantined package needs a report that postdates the quarantine: runs=%d", h.runs(live.ManifestHash))
		}
		h.mustNotServe("an audit is not an activation")
		if _, err := h.runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: appID, ManifestHash: live.ManifestHash, OperatorID: "alice"}); err != nil {
			t.Errorf("the operator can activate on the new report: %v", err)
		}
		h.mustServe(live.ManifestHash)
	})

	t.Run("polled from the adapters", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		h.drifts = []Drift{drift}

		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		h.mustNotServe("after a polled drift")
		if h.state(live) != apppkg.StateQuarantined || !hasChange(res.Changes, KindMCPSchema, true) {
			t.Errorf("live is %s: %v", h.state(live), kinds(res.Changes))
		}
	})

	t.Run("every kind of drift is critical", func(t *testing.T) {
		for _, kind := range []string{"schema_changed", "tool_missing", "ambiguous_name", "contract_hash", "a kind from a newer adapter", ""} {
			h := newHarness(t)
			live := h.goLive(validMinimalDir)
			d := drift
			d.Kind = kind
			if _, err := h.w.ReportDrift(ctx, appID, d); err != nil {
				t.Fatalf("%q: %v", kind, err)
			}
			if h.state(live) != apppkg.StateQuarantined {
				t.Errorf("drift %q left the package %s", kind, h.state(live))
			}
			h.mustNotServe("drift " + kind)
		}
	})

	t.Run("a registry that still serves is not a quarantine", func(t *testing.T) {
		// Nothing evicts: not the Runner (no registry of its own here) and not the
		// stand-in the watcher is given. The database says quarantined and the
		// registry, trusting its cache, still serves the package.
		h := newHarness(t, func(o *hopts) {
			o.watcherRegistry = func(r *capabilities.Registry) Registry { return stuckRegistry{r} }
		})
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)

		_, err := h.w.ReportDrift(ctx, appID, drift)
		if err == nil || !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("a stop the registry does not confirm must be an error the queue retries, got %v", err)
		}
		if h.state(live) != apppkg.StateQuarantined {
			t.Errorf("the package is still quarantined in the store: %s", h.state(live))
		}
		ev := h.events()
		if count(ev, "reaudit.failed") != 1 || count(ev, "reaudit.quarantined") != 0 {
			t.Errorf("the failure is recorded and no success is claimed: %v", ev)
		}
		if got := h.notices.of("reaudit.failed"); len(got) != 1 || got[0].Severity != SeverityCritical {
			t.Errorf("the operator is told, critically: %+v", got)
		}

		// the retry confirms the stop again before it does anything else, and fails
		// again for as long as the registry serves; nothing is audited meanwhile
		runs := h.totalRuns()
		if _, err := h.w.Check(ctx, appID); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("the retry must not pass while the registry still serves: %v", err)
		}
		if h.totalRuns() != runs || count(h.events(), "reaudit.failed") != 2 {
			t.Errorf("the retry fails again and audits nothing: runs %d -> %d, events %v", runs, h.totalRuns(), h.events())
		}
		// once the registry's own cache has expired it serves nothing and the retry goes on
		h.clk.Advance(31 * time.Second)
		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatalf("the retry after the cache expired: %v", err)
		}
		if len(res.Audits) != 1 || res.Audits[0].ManifestHash != live.ManifestHash {
			t.Errorf("the quarantined package is audited again once the stop is confirmed: %+v", res.Audits)
		}
	})

	t.Run("the refresh of the registry is what stops it", func(t *testing.T) {
		// Only the refresh reaches the registry (the admin route of a running server):
		// nothing evicts by name and the Runner has no registry of its own, so a
		// watcher that did not refresh would leave the cached entry serving.
		h := newHarness(t, func(o *hopts) {
			o.watcherRegistry = func(r *capabilities.Registry) Registry { return refreshOnlyRegistry{r} }
		})
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		if _, err := h.w.ReportDrift(ctx, appID, drift); err != nil {
			t.Fatal(err)
		}
		h.mustNotServe("after the refresh")
		if h.state(live) != apppkg.StateQuarantined {
			t.Errorf("state %s", h.state(live))
		}
	})

	t.Run("nothing live, nothing to stop", func(t *testing.T) {
		h := newHarness(t)
		h.install(validMinimalDir) // quarantined, never activated
		res, err := h.w.ReportDrift(ctx, appID, drift)
		if err != nil || len(res.Quarantined) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		if count(h.events(), "package.quarantined") != 0 || count(h.events(), "reaudit.triggered") != 1 {
			t.Errorf("only the operator is told: %v", h.events())
		}
		if _, err := h.w.ReportDrift(ctx, "", drift); !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Errorf("a drift is about an application: %v", err)
		}
	})
}

// stuckRegistry is a registry that cannot be made to forget: refreshing and
// deactivating do nothing, so the entry it loaded stays until its cache expires.
type stuckRegistry struct{ r *capabilities.Registry }

func (s stuckRegistry) Refresh(context.Context, string) error { return nil }
func (s stuckRegistry) Deactivate(string)                     {}
func (s stuckRegistry) Info(c context.Context, app string) (capabilities.Info, error) {
	return s.r.Info(c, app)
}

// refreshOnlyRegistry is a registry that can only be told to refresh: it ignores
// Deactivate.
type refreshOnlyRegistry struct{ r *capabilities.Registry }

func (s refreshOnlyRegistry) Refresh(c context.Context, app string) error { return s.r.Refresh(c, app) }
func (s refreshOnlyRegistry) Deactivate(string)                           {}
func (s refreshOnlyRegistry) Info(c context.Context, app string) (capabilities.Info, error) {
	return s.r.Info(c, app)
}

// SEC-061: accepting warnings widens neither the tools nor the scope the model
// receives, and the watcher neither accepts anything nor treats an acceptance as
// a reason to stop watching.
func TestWarningAcceptanceDoesNotExpandScope(t *testing.T) {
	scope := contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: appID, TenantID: "library-network-1", WorkspaceID: "branch-1",
		PrincipalID: "usr_member_1", Roles: []string{"member"}, SessionID: "ses_test",
		CapabilityScopes: []string{"catalog.find_title", "reservations.create"},
	}
	// what the model may be shown, and what is registered, for the active package
	observe := func(t *testing.T, h *harness) (registered, visible []string) {
		t.Helper()
		info, err := h.reg.Info(ctx, appID)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		pkg, _, err := h.inst.Open(ctx, appID, info.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		ex := policy.NewGateway(h.clk).Expose(ctx, policy.ExposeRequest{Scope: scope, Package: pkg, Binding: policy.BindingOf(scope)})
		for _, d := range ex.Visible {
			visible = append(visible, d.Record.CapabilityID)
		}
		return info.Capabilities, visible
	}
	// an application that exposes much more than the package declares
	overbroad := func(s *stub) {
		s.desc.Unmapped = []string{"admin.purge_all", "export_everything"}
		s.desc.Capabilities = append(s.desc.Capabilities,
			adapters.CapabilityOps{CapabilityID: "admin.export", Operations: []adapters.Operation{adapters.OpRead, adapters.OpCommit}})
		s.desc.Capabilities[0].Operations = []adapters.Operation{adapters.OpRead, adapters.OpValidate}
	}

	// upgrade runs one flow: the demo library is live, 1.0.1 is uploaded and audited
	// by the watcher against the given application, and the operator accepts every
	// warning and activates it.
	upgrade := func(t *testing.T, mutate func(*stub)) (*harness, apppkg.Record, apppkg.Record, *integrationaudit.Run) {
		t.Helper()
		h := newHarness(t)
		first := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		if mutate != nil {
			mutate(h.app)
		}
		up := h.install(variant(t, "1.0.1", capabilityEdit(t)))
		h.mustServe(first.ManifestHash)
		res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
		audited := false
		for _, a := range res.Audits {
			audited = audited || a.ManifestHash == up.ManifestHash
		}
		if err != nil || !audited {
			t.Fatalf("PackageInstalled: %+v %v", res, err)
		}
		// the audit by the watcher changed nothing that is served
		h.mustServe(first.ManifestHash)
		run, ok, err := h.runner.LatestRun(ctx, appID, up.ManifestHash)
		if err != nil || !ok {
			t.Fatalf("LatestRun: %v %v", ok, err)
		}
		return h, first, up, &integrationaudit.Run{RunID: run.RunID, Signed: run.Signed, Record: mustGet(t, h, up)}
	}

	clean, _, cleanUp, cleanRun := upgrade(t, nil)
	if cleanRun.Disposition() != integrationaudit.DispositionClear {
		t.Fatalf("fixture: the faithful application is clear, got %s", cleanRun.Disposition())
	}
	if _, err := clean.runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: appID, ManifestHash: cleanUp.ManifestHash}); err != nil {
		t.Fatal(err)
	}
	capsClean, visClean := observe(t, clean)

	warned, first, up, run := upgrade(t, overbroad)
	if run.Disposition() != integrationaudit.DispositionNeedsAcceptance {
		t.Fatalf("fixture: want warnings only, got %s", run.Disposition())
	}
	warnings := 0
	for _, f := range run.Signed.Report.Findings {
		if f.Severity == contracts.SeverityWarning {
			warnings++
		}
	}
	if warnings < 4 {
		t.Fatalf("fixture: want at least 4 warnings, got %d", warnings)
	}
	// a re-audit with warnings the operator has not accepted activates nothing
	if _, err := warned.runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: appID, ManifestHash: up.ManifestHash}); err == nil {
		t.Fatal("warnings that nobody accepted must block the activation")
	}
	warned.mustServe(first.ManifestHash)

	warned.acceptAll(run)
	if _, err := warned.runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: appID, ManifestHash: up.ManifestHash, OperatorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	capsWarned, visWarned := observe(t, warned)

	if diff := cmp.Diff(capsClean, capsWarned); diff != "" {
		t.Errorf("accepting warnings changed the registered capabilities (-clean +accepted):\n%s", diff)
	}
	if diff := cmp.Diff(visClean, visWarned); diff != "" {
		t.Errorf("accepting warnings changed what the policy gateway exposes (-clean +accepted):\n%s", diff)
	}
	for _, name := range []string{"admin.purge_all", "export_everything", "admin.export"} {
		for _, list := range [][]string{capsWarned, visWarned} {
			for _, got := range list {
				if got == name {
					t.Errorf("%s, which the package does not declare, became visible", name)
				}
			}
		}
	}

	// the acceptance is not a blind spot: the watcher compares against the report
	// of the active package, not against what an operator said yes to
	accepted, err := warned.runner.Acceptances(ctx, appID, up.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := warned.w.Check(ctx, appID); err != nil || len(res.Changes) != 0 || len(res.Audits) != 0 {
		t.Fatalf("after the upgrade nothing differs from its own report: %+v %v", res, err)
	}
	warned.clk.Advance(time.Second)
	warned.app.desc.Unmapped = append(warned.app.desc.Unmapped, "dump_audit_log") // one more over-broad operation
	res, err := warned.w.Check(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasChange(res.Changes, integrationaudit.TriggerAdapterSurface, false) || len(res.Audits) != 1 {
		t.Fatalf("a new operation after the acceptance is a new change that is audited again: %v %+v", kinds(res.Changes), res.Audits)
	}
	if len(res.Quarantined) != 0 {
		t.Errorf("a new warning is not a critical change: the previous activation stays in force")
	}
	warned.mustServe(up.ManifestHash)
	// the finding the new operation raised is not covered by the old acceptances,
	// and the watcher accepted nothing
	after, err := warned.runner.Acceptances(ctx, appID, up.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(accepted, after); diff != "" {
		t.Errorf("the watcher must not accept anything (-before +after):\n%s", diff)
	}
	latest, _, _ := warned.runner.LatestRun(ctx, appID, up.ManifestHash)
	pending := 0
	for _, f := range latest.Signed.Report.Findings {
		if _, ok := after[f.Key]; f.Severity == contracts.SeverityWarning && !ok {
			pending++
		}
	}
	if pending == 0 {
		t.Errorf("the warning of the new operation needs its own acceptance")
	}
	caps2, vis2 := observe(t, warned)
	if diff := cmp.Diff(capsWarned, caps2); diff != "" {
		t.Errorf("a re-audit changed the registered capabilities:\n%s", diff)
	}
	if diff := cmp.Diff(visWarned, vis2); diff != "" {
		t.Errorf("a re-audit changed what the policy gateway exposes:\n%s", diff)
	}

	// accepting warnings never clears a quarantine
	if _, err := warned.w.ReportDrift(ctx, appID, Drift{Endpoint: "e", Tool: "t", Kind: "schema_changed"}); err != nil {
		t.Fatal(err)
	}
	warned.mustNotServe("after the drift")
	if _, err := warned.w.Check(ctx, appID); err != nil {
		t.Fatal(err)
	}
	latest, _, _ = warned.runner.LatestRun(ctx, appID, up.ManifestHash)
	warned.acceptAllReport(t, latest.Signed.Report, up.ManifestHash)
	warned.mustNotServe("accepting warnings of a quarantined package does not serve it")
}

func mustGet(t *testing.T, h *harness, rec apppkg.Record) apppkg.Record {
	t.Helper()
	got, err := h.inst.Get(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// acceptAllReport accepts every warning of a report of a quarantined package.
func (h *harness) acceptAllReport(t *testing.T, rep integrationaudit.Report, hash string) {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Severity != contracts.SeverityWarning {
			continue
		}
		if _, err := h.runner.AcceptWarning(ctx, integrationaudit.AcceptRequest{
			ApplicationID: appID, ManifestHash: hash, FindingKey: f.Key, OperatorID: "alice", Reason: "reviewed in the test",
		}); err != nil {
			t.Fatalf("AcceptWarning(%s): %v", f.CheckID, err)
		}
	}
}

// A package edited on disk, or whose publisher key was revoked, no longer
// verifies: that is a change of signature and stops traffic.
func TestLivePackageThatStopsVerifyingIsQuarantined(t *testing.T) {
	t.Run("a file edited under the data directory", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		rec := mustGet(t, h, live)
		editFile(t, filepath.Join(h.dataDir, filepath.FromSlash(rec.Path)), "documentation/index.yaml", "title:", "title: tampered\n  x-title:")

		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, KindSignature, true) {
			t.Errorf("a package that does not verify is a critical change of kind signature: %v", kinds(res.Changes))
		}
		h.mustNotServe("after the live package was edited")
		if h.state(live) != apppkg.StateQuarantined {
			t.Errorf("state %s", h.state(live))
		}
	})

	t.Run("a package whose files are gone", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		rec := mustGet(t, h, live)
		if err := os.Remove(filepath.Join(h.dataDir, filepath.FromSlash(rec.Path), "alixai.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.w.Check(ctx, appID); err != nil {
			t.Fatal(err)
		}
		h.mustNotServe("after the live package lost a file")
		if h.state(live) != apppkg.StateQuarantined {
			t.Errorf("state %s", h.state(live))
		}
	})

	t.Run("a revoked publisher key", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		if err := h.trust.RevokeKey(ctx, appID, devkeys.KeyID, "compromised"); err != nil {
			t.Fatal(err)
		}
		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, KindSignature, true) {
			t.Errorf("a revoked key is a critical change of kind signature: %v", kinds(res.Changes))
		}
		h.mustNotServe("after the publisher key was revoked")
		if h.state(live) != apppkg.StateQuarantined {
			t.Errorf("state %s", h.state(live))
		}
	})

	t.Run("a package signed by another key", func(t *testing.T) {
		h := newHarness(t)
		h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		k := genKey(t)
		if _, err := h.trust.RegisterKey(ctx, appID, "second-publisher", k.pub, "second publisher"); err != nil {
			t.Fatal(err)
		}
		up := h.install(variantSigned(t, "1.0.1", func(d string) {
			editFile(t, d, "alixai.yaml", "  publisher_key_ids:\n  - alixai-test-only-publisher\n", "  publisher_key_ids:\n  - second-publisher\n")
		}, trust.SigningKey{KeyID: "second-publisher", Key: k.priv}))
		res, err := h.w.PackageInstalled(ctx, appID, up.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, KindSignature, true) {
			t.Fatalf("another signer is a critical change: %v", kinds(res.Changes))
		}
		h.mustNotServe("after the signer changed")
	})
}

// A change of the adapter surface is audited again while the previous activation
// stays in force; one that makes the audit critical stops traffic.
func TestAdapterSurfaceChange(t *testing.T) {
	t.Run("a new host operation", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		h.app.desc.Unmapped = []string{"admin.purge_all"}
		during := servedDuring(h, live)

		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, integrationaudit.TriggerAdapterSurface, false) || len(res.Audits) != 1 || res.Audits[0].ManifestHash != live.ManifestHash {
			t.Fatalf("the live package is audited again: %v %+v", kinds(res.Changes), res.Audits)
		}
		for _, o := range *during {
			if got := o.served; got != live.ManifestHash {
				t.Errorf("the previous activation stays in force during the audit, registry served %q", got)
			}
		}
		h.mustServe(live.ManifestHash)
		if res.Audits[0].Outcome != integrationaudit.DispositionNeedsAcceptance {
			t.Errorf("an undocumented host operation is a warning: %s", res.Audits[0].Outcome)
		}
		// the new report is the baseline now: the same state is not audited again
		res2, err := h.w.Check(ctx, appID)
		if err != nil || len(res2.Changes) != 0 || len(res2.Audits) != 0 {
			t.Errorf("a change is audited once: %+v %v", res2, err)
		}
	})

	t.Run("the host application changed version", func(t *testing.T) {
		h := newHarness(t)
		h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		h.app.desc.ApplicationVersion = "2.5.0"
		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasChange(res.Changes, integrationaudit.TriggerApplicationVersion, false) || len(res.Audits) != 1 {
			t.Errorf("an upgraded application is audited again: %v", kinds(res.Changes))
		}
	})

	t.Run("a re-audit that comes back blocked", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		// commit is gone: the audit finds what the loader, which looks at the package
		// alone, cannot
		h.app.desc.Capabilities[1].Operations = []adapters.Operation{adapters.OpValidate, adapters.OpPrepare, adapters.OpStatus}

		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Audits) != 1 || res.Audits[0].Outcome != integrationaudit.DispositionBlocked {
			t.Fatalf("fixture: want a blocked audit, got %+v", res.Audits)
		}
		h.mustNotServe("a re-audit with critical findings stops the activation it contradicts")
		if h.state(live) != apppkg.StateQuarantined || len(res.Quarantined) != 1 {
			t.Errorf("live is %s: %+v", h.state(live), res)
		}
		ev := h.events()
		if i, j := indexOf(ev, "reaudit.completed"), indexOf(ev, "reaudit.quarantined"); i < 0 || j < i {
			t.Errorf("the blocked report, then the quarantine: %v", ev)
		}
	})

	t.Run("an application that cannot be reached is not an unchanged one", func(t *testing.T) {
		h := newHarness(t)
		live := h.goLive(validMinimalDir)
		h.clk.Advance(2 * time.Second)
		h.app.descErr = contracts.New(contracts.ErrUnavailable, "down")

		res, err := h.w.Check(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Unverified) == 0 || len(res.Audits) != 0 || len(res.Quarantined) != 0 {
			t.Errorf("an unreachable adapter is unverified and neither audits nor stops traffic: %+v", res)
		}
		h.mustServe(live.ManifestHash)
	})
}

// The comparison needs a report it can trust: without one nothing can be said to be
// unchanged, so the package is audited again, and never silently passed.
func TestUnverifiableBaselineIsAChange(t *testing.T) {
	h := newHarness(t)
	h.goLive(validMinimalDir)
	h.clk.Advance(2 * time.Second)
	// a watcher whose trust store does not know the platform key
	other := newHarness(t)
	w, err := NewWatcher(Config{
		Packages: h.inst, Auditor: h.runner, Verifier: other.w.cfg.Verifier, Registry: h.reg, Log: h.log, Clock: h.clk,
		Adapter: func(context.Context, string) (adapters.AppAdapter, func(), error) { return h.app, nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Check(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasChange(res.Changes, KindBaseline, false) || len(res.Audits) != 1 {
		t.Errorf("an unverifiable baseline is audited again: %v %+v", kinds(res.Changes), res.Audits)
	}
}

// A core upgrade since the last audit is a change.
func TestCoreVersionChangeIsATrigger(t *testing.T) {
	h := newHarness(t, func(o *hopts) { o.coreVersion = "0.2.0" })
	h.goLive(validMinimalDir)
	h.clk.Advance(2 * time.Second)
	res, err := h.w.Check(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasChange(res.Changes, integrationaudit.TriggerCoreVersion, false) {
		t.Errorf("%v", kinds(res.Changes))
	}
}

// The watcher can audit and stop, nothing else: its only handle on the Runner has
// no Activate and no AcceptWarning, so nothing it does can start traffic or accept
// a warning, however clean a report it makes.
func TestWatcherCannotActivateOrAccept(t *testing.T) {
	typ := reflect.TypeOf((*Auditor)(nil)).Elem()
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	if diff := cmp.Diff([]string{"Audit", "LatestRun", "Quarantine"}, got); diff != "" {
		t.Errorf("the Auditor the watcher holds must be able to audit, read and quarantine only (-want +got):\n%s", diff)
	}
	// and the registry it confirms against is the reader's, not the activator's
	rt := reflect.TypeOf((*Registry)(nil)).Elem()
	for i := 0; i < rt.NumMethod(); i++ {
		if n := rt.Method(i).Name; n == "Activate" {
			t.Errorf("Registry has %s", n)
		}
	}
}

// After a critical change the registry check is not skipped by a quarantine the
// Runner could not confirm: the events the watcher writes are all registered on the
// application chain, and an unknown application is refused.
func TestEventsAreRegisteredAndCheckNeedsAnApplication(t *testing.T) {
	for _, name := range []audit.EventType{EventTriggered.Name(), EventCompleted.Name(), EventQuarantined.Name(), EventFailed.Name()} {
		spec, ok := audit.DefaultRegistry.Lookup(name)
		if !ok || spec.Levels != audit.LevelApplication {
			t.Errorf("%s must be registered on the application chain: %+v", name, spec)
		}
	}
	h := newHarness(t)
	if _, err := h.w.Check(ctx, ""); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Errorf("Check needs an application: %v", err)
	}
	if res, err := h.w.Check(ctx, "no-such-app"); err != nil || len(res.Changes) != 0 {
		t.Errorf("an application nothing was ever live for is not the watcher's business: %+v %v", res, err)
	}
}

func TestNewWatcherRefusesMissingParts(t *testing.T) {
	h := newHarness(t)
	full := h.w.cfg
	for name, mutate := range map[string]func(*Config){
		"packages": func(c *Config) { c.Packages = nil },
		"auditor":  func(c *Config) { c.Auditor = nil },
		"verifier": func(c *Config) { c.Verifier = nil },
		"registry": func(c *Config) { c.Registry = nil },
		"log":      func(c *Config) { c.Log = nil },
	} {
		cfg := full
		mutate(&cfg)
		if _, err := NewWatcher(cfg); !contracts.HasCode(err, contracts.ErrInternal) {
			t.Errorf("without %s: %v", name, err)
		}
	}
}

// A package installed before the live one was activated was never a change after
// activation: leaving an old candidate in quarantine, whatever it declares, must not
// take the live application down.
func TestOlderQuarantinedPackageIsNotAChange(t *testing.T) {
	h := newHarness(t)
	h.install(variant(t, "0.9.0", roleEdit(t))) // another permission model, never activated
	h.clk.Advance(time.Minute)
	live := h.goLive(validMinimalDir)
	h.clk.Advance(2 * time.Second)

	res, err := h.w.Check(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 || len(res.Audits) != 0 || len(res.Quarantined) != 0 {
		t.Errorf("an older candidate is not a change: %+v", res)
	}
	h.mustServe(live.ManifestHash)
}

// failingOpen is the installer with Open of one package made to fail.
type failingOpen struct {
	Packages
	hash string
	err  error
}

func (f failingOpen) Open(c context.Context, app, hash string) (*apppkg.Package, apppkg.Record, error) {
	if hash == f.hash {
		return nil, apppkg.Record{}, f.err
	}
	return f.Packages.Open(c, app, hash)
}

// A live package that cannot be opened is a critical change unless the reason is
// that the store is unavailable for a moment: then nothing is known, the watcher
// says so and tries again, and the package that was cleared is not stopped by a
// busy database, a trust store that could not be consulted or a cancelled check.
func TestLivePackageThatCannotBeOpened(t *testing.T) {
	refused := func(checkID string) error {
		return &apppkg.RefusedError{Reason: "the installed package no longer passes its checks", Findings: []contracts.Finding{
			{CheckID: checkID, Severity: contracts.SeverityCritical, Path: trust.SignatureManifestPath, Message: "refused", Remediation: "fix it"},
		}}
	}
	for name, tc := range map[string]struct {
		err         error
		quarantined bool
	}{
		"the store is unavailable":               {contracts.New(contracts.ErrUnavailable, "database busy"), false},
		"the trust store could not be consulted": {refused("signature.unverifiable"), false},
		"the check was cancelled":                {context.Canceled, false},
		"the check ran out of time":              {context.DeadlineExceeded, false},
		"the package no longer verifies":         {refused("signature.invalid"), true},
		"any other reason":                       {contracts.New(contracts.ErrInternal, "cannot read the package"), true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			live := h.goLive(validMinimalDir)
			h.clk.Advance(2 * time.Second)
			cfg := h.w.cfg
			cfg.Packages = failingOpen{h.inst, live.ManifestHash, tc.err}
			w, err := NewWatcher(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.Check(ctx, appID)
			if tc.quarantined {
				if err != nil {
					t.Fatal(err)
				}
				h.mustNotServe("a package that cannot be opened is not served")
				if h.state(live) != apppkg.StateQuarantined {
					t.Errorf("state %s", h.state(live))
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("a passing failure is an error to retry, got %v", err)
			}
			h.mustServe(live.ManifestHash)
			if h.state(live) != apppkg.StateActive {
				t.Errorf("state %s", h.state(live))
			}
		})
	}
}
