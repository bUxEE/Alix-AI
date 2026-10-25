package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/apps/convstage"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// closingAdapter is a fake adapter that counts how often it was closed.
type transportBuildClosing struct {
	adapters.AppAdapter
	closed *atomic.Int64
}

func (c transportBuildClosing) Close() error {
	c.closed.Add(1)
	return nil
}

func transportBuildPackage(kinds ...string) *apppkg.Package {
	eps := map[string]ap.ManifestEndpointValue{}
	for i, k := range kinds {
		eps[fmt.Sprintf("ep-%d-%s", i, k)] = transportsEndpointOf(k)
	}
	return &apppkg.Package{Manifest: transportsManifest(eps)}
}

// The builder is the code path serve and audit use: the factory registered for the declared kind is
// called once with the package, its record and its absolute directory, and what comes back is that
// factory's own adapter (not the scope-checking wrapper), closed exactly once by Close.
func TestBuildPackageAdapterGoesThroughTheRegisteredTransport(t *testing.T) {
	transportsSwapDefault(t)
	var closed atomic.Int64
	rec := &transportsRecorder{}
	for _, kind := range transportsOperationKinds {
		app := fake.NewApp(fake.WithApplicationVersion("v-" + kind))
		inner := rec.factory(app)
		RegisterTransport(kind, func(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
			ad, err := inner(ctx, req)
			return transportBuildClosing{AppAdapter: ad, closed: &closed}, err
		})
	}
	dir := t.TempDir()
	for _, kind := range transportsOperationKinds {
		t.Run(kind, func(t *testing.T) {
			closed.Store(0)
			built, err := BuildPackageAdapter(context.Background(), BuildRequest{
				ApplicationID: "demo-library", Package: transportBuildPackage(kind, "events"), ManifestHash: "h-" + kind, Dir: dir,
			})
			if err != nil {
				t.Fatal(err)
			}
			if built.Kind() != kind {
				t.Errorf("kind = %q, want %q", built.Kind(), kind)
			}
			req := rec.calls[len(rec.calls)-1]
			if req.Kind != kind || req.ApplicationID != "demo-library" || req.Record.ManifestHash != "h-"+kind || req.Record.ApplicationID != "demo-library" {
				t.Errorf("request = %+v", req)
			}
			if want, _ := filepath.Abs(dir); req.Dir != want {
				t.Errorf("the factory was given directory %q, want the absolute %q", req.Dir, want)
			}
			// the transport's own adapter: Describe answers what the factory's adapter says
			if d, err := built.Adapter().Describe(context.Background()); err != nil || d.ApplicationVersion != "v-"+kind {
				t.Errorf("Describe = %+v, %v: not the %s factory's adapter", d, err, kind)
			}
			if _, wrapped := built.Adapter().(transportsClosing); wrapped {
				t.Error("Adapter returned the wrapped adapter; the caller adds adapters.Wrap itself")
			}
			if closed.Load() != 0 {
				t.Error("the adapter was closed before Close")
			}
			if err := built.Close(); err != nil || closed.Load() != 1 {
				t.Errorf("Close = %v, closed %d time(s), want once", err, closed.Load())
			}
			if err := built.Close(); err != nil || closed.Load() != 1 {
				t.Errorf("a second Close = %v, closed %d time(s), want still once", err, closed.Load())
			}
			if _, err := built.Provisioner(context.Background()); err == nil {
				t.Error("a closed package adapter offered a provisioner")
			}
		})
	}
}

// A package this build cannot serve is said so, in the terms serve and audit use; a transport that is
// registered and cannot be opened is a different answer.
func TestBuildPackageAdapterSaysWhyThereIsNoAdapter(t *testing.T) {
	transportsSwapDefault(t)
	RegisterTransport(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		return nil, errors.New("connection refused")
	})
	build := func(pkg *apppkg.Package) error {
		_, err := BuildPackageAdapter(context.Background(), BuildRequest{ApplicationID: "demo-library", Package: pkg, ManifestHash: "h", Dir: t.TempDir()})
		return err
	}
	for name, tc := range map[string]struct {
		pkg       *apppkg.Package
		noAdapter bool
		reason    string
	}{
		"no operation endpoint":        {transportBuildPackage("events"), true, transportsReasonNoEndpoint},
		"several kinds":                {transportBuildPackage(TransportKindMCP, TransportKindNative), true, transportsReasonMixedKinds},
		"a kind nobody registered":     {transportBuildPackage(TransportKindOpenAPI), true, transportsReasonNotRegistered},
		"a transport that cannot open": {transportBuildPackage(TransportKindMCP), false, transportsReasonOpenFailed},
	} {
		t.Run(name, func(t *testing.T) {
			err := build(tc.pkg)
			if err == nil {
				t.Fatal("no error")
			}
			if TransportNoAdapter(err) != tc.noAdapter {
				t.Errorf("TransportNoAdapter(%v) = %v, want %v", err, !tc.noAdapter, tc.noAdapter)
			}
			if reason, _, ok := transportsUnavailableReason(err); !ok || reason != tc.reason || contracts.CodeOf(err) != contracts.ErrUnavailable {
				t.Errorf("error = %v, want unavailable with transport_reason %q", err, tc.reason)
			}
		})
	}
	for name, req := range map[string]BuildRequest{
		"no package":     {ApplicationID: "a", Dir: "d"},
		"no application": {Package: transportBuildPackage(TransportKindMCP), Dir: "d"},
		"no directory":   {ApplicationID: "a", Package: transportBuildPackage(TransportKindMCP)},
	} {
		if _, err := BuildPackageAdapter(context.Background(), req); contracts.CodeOf(err) != contracts.ErrInternal {
			t.Errorf("%s: %v, want an internal (programming) error", name, err)
		}
	}
}

// A provisioning bridge exists for every operation transport and the builder asks each of them: a new
// transport that adds a bridge without adding it here would leave its packages with no disposable tenants
// in the conformance stage, which then does not run.
func TestPackageAdapterAsksTheBridgeOfEveryTransport(t *testing.T) {
	if len(transportsBuildProvisionSources) != len(transportsOperationKinds) {
		t.Fatalf("%d provisioning sources for %d transport kinds (%v)", len(transportsBuildProvisionSources), len(transportsOperationKinds), transportsOperationKinds)
	}
	// an adapter that no bridge recognises is a package with no provisioner, not an error
	transportsSwapDefault(t)
	RegisterTransport(TransportKindNative, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		return fake.NewAdapter(fake.NewApp()), nil
	})
	built, err := BuildPackageAdapter(context.Background(), BuildRequest{
		ApplicationID: "demo-library", Package: transportBuildPackage(TransportKindNative), ManifestHash: "h", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer built.Close()
	if p, err := built.Provisioner(context.Background()); p != nil || err != nil {
		t.Errorf("Provisioner = %v, %v, want nil, nil for an adapter no bridge is for", p, err)
	}
}

// transportBuildReplay runs the conversation stage of alixai-conformance over the package, through the
// exported builder and the provisioning bridge of the transport the package declares, and holds the
// result to what a stage that ran means: every item passes (a warning or an unverified capability is a
// failure here, since the two reference packages replay clean). knownFailing lists the items of the
// package's own content that fail the stage, by item id with the reason, checked both ways (an unlisted
// failure fails the test, and so does a listed one that has stopped failing: the entry must go); it is
// empty for both packages today.
func transportBuildReplay(t *testing.T, pkg *apppkg.Package, dir, wantKind string, knownFailing map[string]string) convstage.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	app := string(pkg.Manifest.Application.ID)
	built, err := BuildPackageAdapter(ctx, BuildRequest{ApplicationID: app, Package: pkg, ManifestHash: "h-conversation", Dir: dir})
	if err != nil {
		t.Fatalf("the adapter of the package: %v", err)
	}
	defer built.Close()
	if built.Kind() != wantKind {
		t.Fatalf("the package is served over %q, want %q", built.Kind(), wantKind)
	}
	prov, err := built.Provisioner(ctx)
	if err != nil || prov == nil {
		t.Fatalf("the provisioning bridge of the %s transport: %v, %v", wantKind, prov, err)
	}
	res := convstage.Replay(ctx, convstage.Subject{
		ApplicationID: app, Package: pkg, Adapter: built.Adapter(), Provisioner: prov, ScratchDir: t.TempDir(),
	})

	var report strings.Builder
	by := map[string]convstage.Item{}
	for _, it := range res.Items {
		by[it.ID] = it
		if it.Status != convstage.StatusPass {
			fmt.Fprintf(&report, "  %-10s %s: %s\n", it.Status, it.Name, it.Message)
		}
		// a warning (an example the router does not shortlist, names that are not in the seed data) and an
		// unverified capability are the package's own to fix: the reference packages replay with none
		if it.Status != convstage.StatusPass {
			if _, known := knownFailing[it.ID]; !known {
				t.Errorf("%s (%s) is %s: %s (details: %v)", it.ID, it.Name, it.Status, it.Message, it.Details)
			}
		}
	}
	for id, why := range knownFailing {
		if it, ok := by[id]; !ok || it.Status != convstage.StatusFail {
			t.Errorf("%s is listed as failing (%s) and is now %+v: remove it from the list", id, why, it)
		}
	}
	if _, skipped := by["conversation.transport"]; skipped {
		t.Errorf("the stage says it does not drive the %s transport: %+v", wantKind, by["conversation.transport"])
	}
	if res.Declared == 0 || res.Replayed != res.Declared {
		t.Errorf("%d example(s) declared and %d replayed; every one must be replayed", res.Declared, res.Replayed)
	}
	for _, id := range []string{"conversation.demonstrated", "conversation.cleanup", "conversation.examples"} {
		if it, ok := by[id]; !ok || it.Status != convstage.StatusPass {
			t.Errorf("%s = %+v, want pass", id, it)
		}
	}
	t.Logf("%s: %d example(s) replayed, %d item(s); not passing:\n%s", wantKind, res.Replayed, len(res.Items), report.String())
	return res
}

// The conversation stage of alixai-conformance used to build only the MCP adapter, so a package served
// over OpenAPI or a native bridge ended in not_run. It now runs over both, with the real reference
// applications: the ledger (Go, native bridge, in process) and facility-maintenance (Python, OpenAPI,
// a subprocess; skipped only where no interpreter has its dependencies).
func TestConformanceConversationStageRunsOverOpenAPIAndNative(t *testing.T) {
	t.Run("native: the ledger", func(t *testing.T) {
		_, w := nativeProvisionLedgerWorld(t, nil)
		transportBuildReplay(t, w.pkg, w.dir, TransportKindNative, nil)
	})
	t.Run("openapi: facility-maintenance", func(t *testing.T) {
		app := transportStepUpStartFacility(t)
		in := transportStepUpFacilityInput(t, app.baseURL, nil)
		dir, err := filepath.Abs(filepath.Join(transportStepUpFacilityDir, "package"))
		if err != nil {
			t.Fatal(err)
		}
		transportBuildReplay(t, in.Package, dir, TransportKindOpenAPI, nil)
	})
}
