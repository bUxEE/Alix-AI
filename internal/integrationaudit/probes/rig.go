package probes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	coordinator "github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// rig is the platform's own pipeline wired over the application's adapter: the
// real policy gateway, the real prepare/confirm/commit coordinator and the real
// audit log, over a throwaway store that exists only for one check. The audit
// uses it where the core decides, and to produce the preparations a Commit needs
// (a PreparedAction is the core's object, not the host's).
//
// The package under audit is quarantined, and the gateway only acts for an active
// package, so the rig builds its own policy view of the package with the status
// active. That view exists only inside the rig; it never reaches the capability
// registry, and nothing here activates anything.
//
// The rig has a clock of its own (a fake one), so a preparation can be made to
// expire without waiting. It starts at the audit's time and is moved only by a
// probe that means to.
type rig struct {
	dir   string
	db    *store.DB
	log   *audit.Log
	co    *coordinator.Coordinator
	clk   *clock.Fake
	count *countingAdapter
}

// countingAdapter counts what the coordinator sends to the application, so a probe
// can prove that a commit the core had to refuse never reached it.
type countingAdapter struct {
	adapters.AppAdapter
	mu    sync.Mutex
	calls map[adapters.Operation]int
}

func (c *countingAdapter) bump(op adapters.Operation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[op]++
}

func (c *countingAdapter) sent(op adapters.Operation) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[op]
}

func (c *countingAdapter) Read(ctx context.Context, s contracts.Scope, r adapters.ReadRequest) (act.DeterministicResult, error) {
	c.bump(adapters.OpRead)
	return c.AppAdapter.Read(ctx, s, r)
}

func (c *countingAdapter) Validate(ctx context.Context, s contracts.Scope, r adapters.ValidateRequest) (act.DeterministicResult, error) {
	c.bump(adapters.OpValidate)
	return c.AppAdapter.Validate(ctx, s, r)
}

func (c *countingAdapter) Prepare(ctx context.Context, s contracts.Scope, r adapters.PrepareRequest) (act.DeterministicResult, error) {
	c.bump(adapters.OpPrepare)
	return c.AppAdapter.Prepare(ctx, s, r)
}

func (c *countingAdapter) Commit(ctx context.Context, s contracts.Scope, r adapters.CommitRequest) (act.DeterministicResult, error) {
	c.bump(adapters.OpCommit)
	return c.AppAdapter.Commit(ctx, s, r)
}

func (c *countingAdapter) Status(ctx context.Context, s contracts.Scope, r adapters.StatusRequest) (act.DeterministicResult, error) {
	c.bump(adapters.OpStatus)
	return c.AppAdapter.Status(ctx, s, r)
}

func (c *countingAdapter) Cancel(ctx context.Context, s contracts.Scope, r adapters.CancelRequest) (act.DeterministicResult, error) {
	c.bump(adapters.OpCancel)
	return c.AppAdapter.Cancel(ctx, s, r)
}

// newRig builds the pipeline over ad and a throwaway store. The caller closes it.
func newRig(ctx context.Context, o Options, pkg *apppkg.Package, ad adapters.AppAdapter, start time.Time) (*rig, error) {
	g := &rig{clk: clock.NewFake(start), count: &countingAdapter{AppAdapter: ad, calls: map[adapters.Operation]int{}}}
	var err error
	if g.dir, err = os.MkdirTemp(o.ScratchDir, "alixai-audit-probe-*"); err != nil {
		return nil, fmt.Errorf("the throwaway store directory could not be created")
	}
	fail := func(what string) (*rig, error) {
		g.close()
		return nil, fmt.Errorf("%s", what)
	}
	if g.db, err = store.Open(ctx, store.Options{Path: filepath.Join(g.dir, "probe.db"), Clock: g.clk}); err != nil {
		return fail("the throwaway store could not be opened")
	}
	if g.log, err = audit.New(g.db, audit.Options{Clock: g.clk}); err != nil {
		return fail("the throwaway audit log could not be opened")
	}
	view, err := activeView(pkg)
	if err != nil {
		return fail("the policy view of the package could not be built")
	}
	sink := audit.NewRecordSink[policy.Record](g.log, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(g.clk, policy.WithSink(sink))
	g.co, err = coordinator.New(coordinator.Deps{
		DB: g.db, Policy: gw, Audit: g.log,
		Packages: coordinator.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return view, nil }),
		Adapters: coordinator.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return g.count, nil }),
		Clock:    g.clk,
	})
	if err != nil {
		return fail("the coordinator could not be built")
	}
	return g, nil
}

// close removes the throwaway store.
func (g *rig) close() {
	if g == nil {
		return
	}
	if g.db != nil {
		_ = g.db.Close()
	}
	if g.dir != "" {
		_ = os.RemoveAll(g.dir)
	}
}

// activeView is the policy view of the package with the status active, built from
// its typed contents (the same declarations the loader hands the gateway).
func activeView(pkg *apppkg.Package) (*policy.Package, error) {
	roles := make([]string, 0, len(pkg.Manifest.Tenancy.Roles))
	for r := range pkg.Manifest.Tenancy.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	decls := make([]policy.CapabilityDecl, 0, len(pkg.Capabilities))
	for _, id := range pkg.SortedCapabilityIDs() {
		c := pkg.Capabilities[id]
		risk, err := contracts.ParseRiskLevel(int(c.RiskLevel))
		if err != nil {
			return nil, err
		}
		d := policy.CapabilityDecl{
			ID: string(c.ID), Audience: policy.Audience(c.Audience), EffectClass: policy.EffectClass(c.EffectClass),
			RiskLevel: risk, Confirmation: policy.ConfirmationMode(c.Confirmation.Mode),
		}
		for _, f := range c.Fields {
			d.Fields = append(d.Fields, policy.FieldDecl{Name: string(f.Name), Required: f.Required, Sensitive: f.Sensitive != nil && *f.Sensitive})
		}
		decls = append(decls, d)
	}
	return policy.NewPackage(policy.PackageSpec{
		ApplicationID: string(pkg.Manifest.Application.ID),
		Version:       string(pkg.Manifest.Package.Version),
		Status:        policy.StatusActive,
		Roles:         roles,
		Capabilities:  decls,
		Policies:      pkg.Policies,
	})
}

// taskFields names the sensitive fields a capability requires: the fields the
// task needs, which deterministic code (not the model) allows through.
func taskFields(c ap.Capability) []string {
	var out []string
	for _, f := range c.Fields {
		if f.Required && f.Sensitive != nil && *f.Sensitive {
			out = append(out, string(f.Name))
		}
	}
	return out
}

// prepared is a preparation the coordinator made.
type prepared struct {
	action contracts.PreparedAction
	result contracts.DeterministicResult // the host's own Prepare result
	token  coordinator.ConfirmationToken
}

// prepare asks the coordinator to prepare an action in the session of sc. The
// error is the coordinator's: when the host refused, result holds its typed issues.
func (g *rig) prepare(ctx context.Context, sc contracts.Scope, c ap.Capability, fields map[string]any) (prepared, error) {
	p, err := g.co.Prepare(ctx, sc, coordinator.PrepareInput{
		Session: sc, CapabilityID: string(c.ID), Fields: fields, TaskFields: taskFields(c),
	})
	return prepared{action: p.Action, result: p.Result, token: p.Confirmation}, err
}

func (g *rig) confirm(ctx context.Context, sc contracts.Scope, p prepared) (contracts.PreparedAction, error) {
	return g.co.Confirm(ctx, sc, coordinator.ConfirmInput{PreparationID: string(p.action.PreparationID), Token: p.token})
}

func (g *rig) commit(ctx context.Context, sc contracts.Scope, p prepared) (contracts.ActionReceipt, error) {
	return g.co.Commit(ctx, sc, string(p.action.PreparationID))
}

func (g *rig) reject(ctx context.Context, sc contracts.Scope, p prepared) (contracts.PreparedAction, error) {
	return g.co.Reject(ctx, sc, string(p.action.PreparationID))
}

func (g *rig) read(ctx context.Context, sc contracts.Scope, c ap.Capability, fields map[string]any) (coordinator.ReadOutcome, error) {
	return g.co.Read(ctx, sc, coordinator.ReadInput{Session: sc, CapabilityID: string(c.ID), Fields: fields, TaskFields: taskFields(c)})
}

// auditEvent is one event of the rig's log, as the probes check it.
type auditEvent struct {
	Type        string
	Application string
	Tenant      string
	Workspace   string
	Principal   string
	Session     string
}

// events lists the events recorded for one session, in order.
func (g *rig) events(ctx context.Context, sessionID string) ([]auditEvent, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT event_type, application_id, tenant_id, workspace_id, principal_id, session_id
		   FROM audit_events WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditEvent
	for rows.Next() {
		var e auditEvent
		if err := rows.Scan(&e.Type, &e.Application, &e.Tenant, &e.Workspace, &e.Principal, &e.Session); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// advance moves the rig's clock forward, and back returns it to a time.
func (g *rig) advance(d time.Duration) { g.clk.Advance(d) }
func (g *rig) setTime(t time.Time)     { g.clk.Set(t) }
func (g *rig) now() time.Time          { return g.clk.Now() }
