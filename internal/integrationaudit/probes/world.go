package probes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// runCounter keeps the identifiers of two worlds made in the same instant apart.
var runCounter atomic.Uint64

// newRunID returns the suffix of the identifiers generated for one disposable
// world: short, lower case, and different for every world even when the clock
// does not move. It never appears in the report.
func newRunID(appID string, clk clock.Clock) string {
	n := runCounter.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", appID, clk.Now().UnixNano(), n)))
	return hex.EncodeToString(sum[:])[:10]
}

// principalIDOf is how the identifier of a fixture principal is generated for a run.
// Nothing outside the probes may rely on its shape: a Provisioner that needs the
// principal a reference names reads Principal.Ref (the tests swap this function to
// prove it).
var principalIDOf = func(ref, suffix string) string { return "usr_" + slug(ref) + "_" + suffix }

// slug turns a logical reference into the lower-case, underscore-separated form
// used inside generated identifiers.
func slug(ref string) string { return strings.ReplaceAll(ref, "-", "_") }

type wPrincipal struct {
	ref          string
	id           string
	tenantRef    string
	workspaceRef string
	roles        []string
	service      bool
}

type wRecord struct {
	ref          string
	entityType   string
	tenantRef    string
	workspaceRef string
	id           string
}

// world is one provisioned set of disposable tenants: the logical references of
// a suite's fixtures mapped to the identifiers generated for this run and the
// identifiers the application gave the seeded records.
type world struct {
	suite      ap.PackageTests
	suffix     string
	deployment string
	appID      string
	capIDs     []string
	prov       Provisioner

	tenants    map[string]string // tenant ref -> generated tenant id
	workspaces map[string]string // workspace ref -> generated workspace id
	principals map[string]*wPrincipal
	records    map[string]*wRecord

	created  []string // generated tenant ids that exist, or may
	sessions int
}

// provision creates and seeds every tenant of the suite's fixtures. On any
// failure it removes what it created and returns the error, with what that
// removal left behind.
func provision(ctx context.Context, prov Provisioner, suite ap.PackageTests, base contracts.Scope, capIDs []string, clk clock.Clock) (w *world, left []leftover, err error) {
	w = &world{
		suite: suite, suffix: newRunID(base.ApplicationID, clk), deployment: base.DeploymentID, appID: base.ApplicationID,
		capIDs: capIDs, prov: prov,
		tenants: map[string]string{}, workspaces: map[string]string{},
		principals: map[string]*wPrincipal{}, records: map[string]*wRecord{},
	}
	// A panic in the provisioner must not leave the tenants created so far behind.
	defer func() {
		if p := recover(); p != nil {
			w.abandon(ctx)
			panic(p)
		}
	}()
	fx := suite.Fixtures
	for _, t := range fx.Tenants {
		w.tenants[string(t.Ref)] = "tnt_" + slug(string(t.Ref)) + "_" + w.suffix
		for _, ws := range t.Workspaces {
			w.workspaces[string(ws)] = "ws_" + slug(string(ws)) + "_" + w.suffix
		}
	}
	for _, p := range fx.Principals {
		roles := make([]string, len(p.Roles))
		for i, r := range p.Roles {
			roles[i] = string(r)
		}
		w.principals[string(p.Ref)] = &wPrincipal{
			ref: string(p.Ref), id: principalIDOf(string(p.Ref), w.suffix),
			tenantRef: string(p.Tenant), workspaceRef: string(p.Workspace), roles: roles,
			service: p.Kind == ap.PackageTestsPrincipalKindService,
		}
	}
	for _, r := range fx.Records {
		w.records[string(r.Ref)] = &wRecord{
			ref: string(r.Ref), entityType: string(r.EntityType), tenantRef: string(r.Tenant), workspaceRef: string(r.Workspace),
		}
	}
	dataFile := ""
	if fx.Provisioning.SeedData != nil {
		dataFile = *fx.Provisioning.SeedData
	}
	for _, t := range fx.Tenants {
		tref := string(t.Ref)
		tenant := Tenant{ID: w.tenants[tref]}
		for _, ws := range t.Workspaces {
			tenant.Workspaces = append(tenant.Workspaces, w.workspaces[string(ws)])
		}
		for _, pref := range sortedKeys(w.principals) {
			if p := w.principals[pref]; p.tenantRef == tref {
				tenant.Principals = append(tenant.Principals, Principal{
					Ref: p.ref, ID: p.id, WorkspaceID: w.workspaces[p.workspaceRef], Roles: slices.Clone(p.roles), Service: p.service,
				})
			}
		}
		// The tenant is listed before it is asked for: a creation that ran out of time or
		// whose reply could not be read may have happened all the same.
		w.created = append(w.created, tenant.ID)
		if err := prov.CreateTenant(ctx, tenant); err != nil {
			if !outage(err) && !untyped(err) {
				// refused: nothing was created, and a provisioner need not remove a
				// tenant it never created
				w.created = w.created[:len(w.created)-1]
			}
			return nil, w.abandon(ctx), fmt.Errorf("create_tenant for fixture tenant %s: %w", quote(tref), errKind(err))
		}
		seed := Seed{TenantID: tenant.ID, DataFile: dataFile}
		for _, rref := range sortedKeys(w.records) {
			if r := w.records[rref]; r.tenantRef == tref {
				seed.Records = append(seed.Records, Record{Ref: r.ref, EntityType: r.entityType, WorkspaceID: w.workspaces[r.workspaceRef]})
			}
		}
		res, err := prov.Seed(ctx, seed)
		if err != nil {
			return nil, w.abandon(ctx), fmt.Errorf("seed for fixture tenant %s: %w", quote(tref), errKind(err))
		}
		for _, rec := range seed.Records {
			id := res.Entities[rec.Ref]
			if id == "" {
				return nil, w.abandon(ctx), fmt.Errorf("seed for fixture tenant %s returned no identifier for record %s", quote(tref), quote(rec.Ref))
			}
			w.records[rec.Ref].id = id
		}
	}
	return w, nil, nil
}

// errKind describes an error by its typed code only: what the application said
// is untrusted text and never reaches a message.
func errKind(err error) error {
	if code := contracts.CodeOf(err); code != "" {
		return fmt.Errorf("error code %s", code)
	}
	return fmt.Errorf("an untyped error")
}

// abandon removes the tenants created so far after a provisioning failure and
// returns what it could not remove, which is reported as cleanup leaves it.
func (w *world) abandon(ctx context.Context) []leftover {
	cctx := context.WithoutCancel(ctx)
	refOf := map[string]string{}
	for ref, id := range w.tenants {
		refOf[id] = ref
	}
	var out []leftover
	for _, id := range w.created {
		rep, err := w.prov.Cleanup(cctx, id)
		if err != nil || rep.Left() > 0 {
			out = append(out, leftover{tenantRef: refOf[id], remaining: rep.Left(), err: err})
		}
	}
	w.created = nil
	return out
}

// leftover is what cleanup found still in place.
type leftover struct {
	tenantRef  string
	remaining  int
	stillThere bool
	err        error
}

// cleanup removes every tenant of the world and verifies it is gone: the
// application's own report of what remains must be empty, and a second cleanup
// of the same tenant must find nothing to remove. It runs on a context
// detached from the caller's.
func (w *world) cleanup(ctx context.Context) []leftover {
	cctx := context.WithoutCancel(ctx)
	refOf := map[string]string{}
	for ref, id := range w.tenants {
		refOf[id] = ref
	}
	var out []leftover
	for _, id := range w.created {
		l := leftover{tenantRef: refOf[id]}
		rep, err := w.prov.Cleanup(cctx, id)
		switch {
		case err != nil:
			l.err = err
		default:
			l.remaining = rep.Left()
			// A second cleanup proves the tenant is gone; if it cannot answer
			// that proves nothing either way, so only a clear "it still
			// exists" counts.
			if again, err2 := w.prov.Cleanup(cctx, id); err2 == nil && again.Existed {
				l.stillThere = true
			}
		}
		if l.err != nil || l.remaining > 0 || l.stillThere {
			out = append(out, l)
		}
	}
	w.created = nil
	sort.Slice(out, func(i, j int) bool { return out[i].tenantRef < out[j].tenantRef })
	return out
}

// scopeOf returns a verified-looking scope for a fixture principal: the audit's
// deployment and application, the principal's generated tenant, workspace and
// identifier, its roles, every capability of the package as the token's
// capability scopes, and a session of its own.
func (w *world) scopeOf(ref string) (contracts.Scope, *wPrincipal, bool) {
	p, ok := w.principals[ref]
	if !ok {
		return contracts.Scope{}, nil, false
	}
	w.sessions++
	return contracts.Scope{
		DeploymentID:     w.deployment,
		ApplicationID:    w.appID,
		TenantID:         w.tenants[p.tenantRef],
		WorkspaceID:      w.workspaces[p.workspaceRef],
		PrincipalID:      p.id,
		Roles:            slices.Clone(p.roles),
		CapabilityScopes: slices.Clone(w.capIDs),
		SessionID:        fmt.Sprintf("ses_%s_%03d", w.suffix, w.sessions),
	}, p, true
}

// recordsOf lists the records of an entity type visible in a principal's own
// workspace, by reference.
func (w *world) recordsOf(entityType string, p *wPrincipal) []*wRecord {
	var out []*wRecord
	for _, ref := range sortedKeys(w.records) {
		r := w.records[ref]
		if r.entityType == entityType && r.tenantRef == p.tenantRef && r.workspaceRef == p.workspaceRef {
			out = append(out, r)
		}
	}
	return out
}

// outsideScope lists the records a principal must never see through a
// capability of the given scope level: those of another tenant always, those of
// another workspace of the same tenant unless the capability's scope is the whole
// tenant. A principal-scoped capability is held to the workspace boundary.
func (w *world) outsideScope(p *wPrincipal, level ap.CapabilityScopeLevel) []*wRecord {
	var out []*wRecord
	for _, ref := range sortedKeys(w.records) {
		r := w.records[ref]
		switch {
		case r.tenantRef != p.tenantRef:
			out = append(out, r)
		case level != ap.CapabilityScopeLevelTenant && r.workspaceRef != p.workspaceRef:
			out = append(out, r)
		}
	}
	return out
}

// allIDs returns the identifiers of records (for scanning a result for them).
func allIDs(rs []*wRecord) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.id != "" {
			out = append(out, r.id)
		}
	}
	return out
}

// pool hands out a clean world and replaces it after a probe changed what the
// application holds. Every world it made is removed by closeAll.
type pool struct {
	prov   Provisioner
	suite  ap.PackageTests
	base   contracts.Scope
	capIDs []string
	clk    clock.Clock

	cur   *world
	all   []*world
	left  []leftover // what the removal of a world that could not be provisioned left
	dirty bool
	made  int
}

func newPool(prov Provisioner, suite ap.PackageTests, base contracts.Scope, capIDs []string, clk clock.Clock) *pool {
	return &pool{prov: prov, suite: suite, base: base, capIDs: capIDs, clk: clk}
}

// get returns a provisioned world that no probe has changed.
func (p *pool) get(ctx context.Context) (*world, error) {
	if p.cur != nil && !p.dirty {
		return p.cur, nil
	}
	w, left, err := provision(ctx, p.prov, p.suite, p.base, p.capIDs, p.clk)
	p.left = append(p.left, left...)
	if err != nil {
		return nil, err
	}
	p.cur, p.dirty = w, false
	p.all = append(p.all, w)
	p.made++
	return w, nil
}

// markDirty records that a probe changed what the application holds: the next get
// provisions another world.
func (p *pool) markDirty() { p.dirty = true }

// closeAll removes every world made and returns what was left behind, with what
// the removal of the worlds that could not be provisioned left.
func (p *pool) closeAll(ctx context.Context) []leftover {
	out := p.left
	for _, w := range p.all {
		out = append(out, w.cleanup(ctx)...)
	}
	p.all, p.cur, p.left = nil, nil, nil
	return out
}
