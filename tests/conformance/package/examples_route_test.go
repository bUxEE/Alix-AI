package conformancepkg

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The three reference applications of the integration success test (master plan 15.5), each
// as its own package ships.
var referencePackages = []struct{ name, dir string }{
	{"clinic-booking", "../../../examples/reference-apps/clinic-booking/package"},
	{"facility-maintenance", "../../../examples/reference-apps/facility-maintenance/package"},
	{"ledger", "../../../examples/reference-apps/ledger/package"},
}

// exampleRouting is the router's answer for one example, for one role.
type exampleRouting struct {
	capability, example, language, role, utterance string
	shortlist                                      []string
}

func (r exampleRouting) routed() bool {
	for _, id := range r.shortlist {
		if id == r.capability {
			return true
		}
	}
	return false
}

func (r exampleRouting) String() string {
	return fmt.Sprintf("%s / %s (%s, as %s) %q: shortlist %v", r.capability, r.example, r.language, r.role, r.utterance, r.shortlist)
}

// routeExamples runs every example of every user capability of the package at dir through the
// deterministic, rules-only router (no embedder, no model: the router has neither), once for
// each role the host policy grants the capability, and reports what it shortlisted. It is
// what the conformance conversation stage's "unrouted" warning is made of: an example whose
// capability is not on the shortlist never reaches the model.
func routeExamples(t testing.TB, dir string) []exampleRouting {
	t.Helper()
	ctx := context.Background()
	src, err := apppkg.FromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(ctx, src, apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	pkg := activePackage{capabilities.FromApppkg(res.Package)}
	app := string(res.Package.Manifest.Application.ID)

	clk := clock.NewFake(worldNow)
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
	if err := reg.Activate(pkg); err != nil {
		t.Fatalf("activate %s: %v", app, err)
	}
	router, err := capabilities.NewRouter(reg, policy.NewGateway(clk))
	if err != nil {
		t.Fatal(err)
	}

	var user []string
	for id, c := range res.Package.Capabilities {
		if c.Audience == ap.CapabilityAudienceUser {
			user = append(user, id)
		}
	}
	sort.Strings(user)
	roles := map[string][]string{} // capability -> roles the host policy grants it to
	for _, p := range res.Package.Policies {
		for _, g := range p.RoleGrants {
			for _, c := range g.Capabilities {
				roles[c] = append(roles[c], g.Role)
			}
		}
	}

	var out []exampleRouting
	for _, id := range user {
		for _, ex := range res.Package.Capabilities[id].Examples {
			for _, role := range roles[id] {
				sc := contracts.Scope{
					DeploymentID: "alixai-conformance", ApplicationID: app, TenantID: "ten-routing", WorkspaceID: "wks-routing",
					PrincipalID: "usr-routing", Roles: []string{role}, CapabilityScopes: user, SessionID: "ses-routing",
				}
				got, err := router.Route(ctx, capabilities.Request{
					Scope: sc, Binding: policy.BindingOf(sc), Utterance: ex.Utterance, Language: string(ex.Language),
				})
				if err != nil {
					t.Fatalf("%s %s: route: %v", id, ex.ID, err)
				}
				out = append(out, exampleRouting{
					capability: id, example: string(ex.ID), language: string(ex.Language), role: role,
					utterance: ex.Utterance, shortlist: got.IDs(),
				})
			}
		}
	}
	return out
}

// routingFailures holds the router's answers for one package to the bar of this suite: every
// example routes for every role that may use its capability, and no capability is left without an
// example that routes for each of its roles. There is no list of tolerated gaps: a package whose
// own words do not cover its own examples fails.
func routingFailures(rows []exampleRouting) []string {
	var out []string
	routedFor := map[string]bool{} // capability/role has an example that routes
	roles := map[string]bool{}
	for _, r := range rows {
		cr := r.capability + "/" + r.role
		roles[cr] = true
		if r.routed() {
			routedFor[cr] = true
			continue
		}
		out = append(out, "does not route: "+r.String())
	}
	for cr := range roles {
		if !routedFor[cr] {
			out = append(out, "no example of "+cr+" routes: the capability is not reachable from its own examples")
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryReferencePackageExampleRoutes holds each reference package to what the router can
// do for it with no model: every example of every user capability, for every role that may use
// the capability, puts that capability on the shortlist from the package's terminology and
// titles alone (the router reads nothing else, and an example is test data, never policy).
// An example that does not route is a package whose own words do not cover its own examples;
// the fix is in the package, never in the router.
func TestEveryReferencePackageExampleRoutes(t *testing.T) {
	for _, rp := range referencePackages {
		t.Run(rp.name, func(t *testing.T) {
			rows := routeExamples(t, rp.dir)
			if len(rows) == 0 {
				t.Fatalf("%s: no example of any user capability was routed", rp.name)
			}
			for _, f := range routingFailures(rows) {
				t.Error(f)
			}
		})
	}
}

// TestUnroutedExampleFailsTheSuite is the control of the one above: the check really fails
// on an example that does not route (and on a capability none of whose examples routes, once
// more), and passes a package where everything does, with no list of tolerated gaps to hide
// behind.
func TestUnroutedExampleFailsTheSuite(t *testing.T) {
	row := func(capability, example, role string, shortlist ...string) exampleRouting {
		return exampleRouting{capability: capability, example: example, language: "en", role: role, utterance: "u", shortlist: shortlist}
	}
	tests := []struct {
		name  string
		rows  []exampleRouting
		fails int
	}{
		{"everything routes", []exampleRouting{row("a.b", "x", "r", "a.b", "c.d"), row("c.d", "y", "r", "c.d")}, 0},
		{"one example unrouted", []exampleRouting{row("a.b", "x", "r", "a.b"), row("a.b", "z", "r")}, 1},
		{"routed only to another capability", []exampleRouting{row("a.b", "x", "r", "a.b"), row("a.b", "z", "r", "c.d")}, 1},
		{"unrouted for one role only", []exampleRouting{row("a.b", "x", "r", "a.b"), row("a.b", "x", "s")}, 2},
		{"a capability with no routed example", []exampleRouting{row("a.b", "x", "r")}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routingFailures(tt.rows); len(got) != tt.fails {
				t.Errorf("want %d failure(s), got %d: %v", tt.fails, len(got), got)
			}
		})
	}
}
