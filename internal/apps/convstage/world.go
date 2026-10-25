package convstage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The conversation stage needs real records for its examples to name: the disposable tenants of the
// package's own test suite (tests/*.yaml, fixtures:), created, seeded and removed through the
// provisioning bridge of the application's transport (probes.Provisioner), the very bridge the
// integration audit's runtime probes use, so the wire convention of docs/integration/application-package.md
// section 12 is the transport's business and the same for MCP tools, OpenAPI operations and native
// methods. Identifiers are generated for this run; the suite's logical references never reach the
// application. Only tenants this run created are ever seeded or removed, which the bridge also
// enforces.

// worldPrincipal is a synthetic principal of a disposable tenant.
type worldPrincipal struct {
	Ref         string
	ID          string
	TenantRef   string
	TenantID    string
	WorkspaceID string
	Roles       []string
	Service     bool
}

// world is what the conversation stage runs against.
type world struct {
	suite      ap.PackageTests
	prov       probes.Provisioner
	principals []worldPrincipal
	// tenants are the tenant identifiers this run created, by the suite's reference.
	tenants map[string]string
	// order is the creation order of the tenants.
	order []string
	// records holds the application's identifier of each seeded record, by reference.
	records map[string]string
}

func worldRandomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failed: nothing here can be trusted
	}
	return hex.EncodeToString(b)
}

func worldSlug(ref string) string { return strings.ReplaceAll(ref, "-", "_") }

// suiteWithProvisioning returns the first test suite that names its provisioning calls: all three
// of create_tenant, seed and cleanup, each bound to an mcp tool, an openapi operation or a native
// method (which of the three is the transport's business, not this stage's).
func suiteWithProvisioning(suites []ap.PackageTests) (ap.PackageTests, bool) {
	named := func(c ap.AdapterCall) bool {
		return c.MCPTool != nil || c.OpenAPIOperationID != nil || c.NativeMethod != nil
	}
	for _, s := range suites {
		p := s.Fixtures.Provisioning
		if named(p.CreateTenant) && named(p.Seed) && named(p.Cleanup) {
			return s, true
		}
	}
	return ap.PackageTests{}, false
}

// newWorld creates and seeds the disposable tenants of suite through the provisioner. On any error
// the tenants already created are removed before it returns.
func newWorld(ctx context.Context, prov probes.Provisioner, suite ap.PackageTests) (*world, error) {
	w := &world{suite: suite, prov: prov, tenants: map[string]string{}, records: map[string]string{}}
	suffix := worldRandomSuffix()
	workspaces := map[string]string{}
	for _, t := range suite.Fixtures.Tenants {
		for _, ws := range t.Workspaces {
			workspaces[string(ws)] = "ws_" + worldSlug(string(ws)) + "_" + suffix
		}
	}
	fail := func(err error) (*world, error) {
		_ = w.cleanup()
		return nil, err
	}
	for _, t := range suite.Fixtures.Tenants {
		ref := string(t.Ref)
		tenant := probes.Tenant{ID: "tnt_" + worldSlug(ref) + "_" + suffix}
		for _, ws := range t.Workspaces {
			tenant.Workspaces = append(tenant.Workspaces, workspaces[string(ws)])
		}
		for _, p := range suite.Fixtures.Principals {
			if string(p.Tenant) != ref {
				continue
			}
			roles := make([]string, len(p.Roles))
			for i, r := range p.Roles {
				roles[i] = string(r)
			}
			wp := worldPrincipal{
				Ref: string(p.Ref), ID: "usr_" + worldSlug(string(p.Ref)) + "_" + suffix, TenantRef: ref, TenantID: tenant.ID,
				WorkspaceID: workspaces[string(p.Workspace)], Roles: roles, Service: p.Kind == ap.PackageTestsPrincipalKindService,
			}
			if wp.WorkspaceID == "" {
				return fail(fmt.Errorf("principal %s names a workspace tenant %s does not have", p.Ref, ref))
			}
			w.principals = append(w.principals, wp)
			tenant.Principals = append(tenant.Principals, probes.Principal{
				Ref: wp.Ref, ID: wp.ID, WorkspaceID: wp.WorkspaceID, Roles: roles, Service: wp.Service,
			})
		}
		// recorded before the call: a reply that was lost may still have created it
		w.tenants[ref] = tenant.ID
		w.order = append(w.order, ref)
		if err := w.prov.CreateTenant(ctx, tenant); err != nil {
			return fail(fmt.Errorf("create tenant %s: %w", ref, err))
		}
	}

	dataFile := ""
	if suite.Fixtures.Provisioning.SeedData != nil {
		dataFile = *suite.Fixtures.Provisioning.SeedData
	}
	for _, ref := range w.order {
		seed := probes.Seed{TenantID: w.tenants[ref], DataFile: dataFile}
		for _, r := range suite.Fixtures.Records {
			if string(r.Tenant) == ref {
				seed.Records = append(seed.Records, probes.Record{Ref: string(r.Ref), EntityType: string(r.EntityType), WorkspaceID: workspaces[string(r.Workspace)]})
			}
		}
		if len(seed.Records) == 0 {
			continue
		}
		res, err := w.prov.Seed(ctx, seed)
		if err != nil {
			return fail(fmt.Errorf("seed tenant %s: %w", ref, err))
		}
		for _, r := range seed.Records {
			id := res.Entities[r.Ref]
			if id == "" {
				return fail(fmt.Errorf("seed of tenant %s returned no identifier for record %s", ref, r.Ref))
			}
			w.records[r.Ref] = id
		}
	}
	return w, nil
}

// cleanup removes every tenant this run created, on a context of its own (a run that was
// cancelled still cleans up) and reports what could not be removed or was left behind.
func (w *world) cleanup() error {
	var errs []error
	for _, ref := range w.order {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rep, err := w.prov.Cleanup(ctx, w.tenants[ref])
		cancel()
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("remove tenant %s: %w", ref, err))
		default:
			for kind, n := range rep.Remaining {
				if n > 0 {
					errs = append(errs, fmt.Errorf("remove tenant %s: %d %s left", ref, n, kind))
				}
			}
		}
	}
	w.order = nil
	return errors.Join(errs...)
}
