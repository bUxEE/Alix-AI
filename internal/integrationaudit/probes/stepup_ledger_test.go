package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The ledger reference application is audited the way AlixAI reaches it: the real
// application in this process, served on loopback by its host bridge, reached through the
// native adapter configured from its shipped package. Its level 3 capability is
// invoices.void: the application's own interface (Ledger.ApproveVoid) records an approval
// for the void reference the prepared action shows, and only then does a commit apply the
// effect. There is no native provisioner in the composition root yet, so this file is the
// Provisioner (the three management operations of the package) and the StepUpCompleter
// (the manager's approval in the application's own interface).

const (
	ledgerDir     = "../../../examples/reference-apps/ledger/package"
	ledgerCap     = "invoices.void"
	ledgerVoidRef = "void_reference"
)

var ledgerEpoch = time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC)

// ledgerHost is the ledger, its native adapter, and the provisioner over its management
// operations.
type ledgerHost struct {
	t        *testing.T
	app      *ledger.Ledger
	raw      *native.Adapter
	ad       adapters.AppAdapter
	clk      *clock.Fake
	seedBy   map[string]map[string]any
	approved int
}

func newLedgerHost(t *testing.T, pkg *apppkg.Package) *ledgerHost {
	t.Helper()
	h := &ledgerHost{t: t, clk: clock.NewFake(ledgerEpoch), seedBy: map[string]map[string]any{}}
	h.app = ledger.New(ledger.Config{Clock: h.clk.Now, TestSupport: true})

	raw, err := os.ReadFile(filepath.Join(ledgerDir, "schemas", "adapters", "ledger-bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := native.ParseAdapterConfig(raw, ledger.EndpointID)
	if err != nil {
		t.Fatalf("the package's adapter configuration is refused by the native adapter: %v", err)
	}
	key := []byte("ledger-audit-application-key-0123456789abcdef")
	t.Setenv("LEDGER_BRIDGE_KEY", string(key))
	resolved, err := native.ResolveKey(settings.Key.Ref)
	if err != nil {
		t.Fatalf("the key reference of the adapter configuration does not resolve: %v", err)
	}
	srv, err := h.app.NewBridge(ledger.BridgeConfig{Keys: map[string][]byte{settings.Key.ID: key}})
	if err != nil {
		t.Fatal(err)
	}
	l, err := bridge.ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	addr := l.Addr().String()
	var port int
	if _, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port); err != nil {
		t.Fatal(err)
	}
	allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{port}, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	h.raw, err = native.New(native.Config{
		ApplicationID: ledger.ApplicationID,
		Endpoints: []native.Endpoint{{
			ID: ledger.EndpointID, Network: native.NetworkTCP, Address: addr, KeyID: settings.Key.ID, Key: resolved,
			Provisioning: settings.Management.Provisioning,
		}},
		Capabilities: ledgerCapabilities(t, pkg),
		Dialer:       egress.NewDialer(allow),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.raw.Close() })
	if h.ad, err = adapters.Wrap(h.raw, adapters.Options{ApplicationID: ledger.ApplicationID}); err != nil {
		t.Fatal(err)
	}

	seed, err := os.ReadFile(filepath.Join(ledgerDir, "tests", "seed", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(seed, &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc.Records {
		h.seedBy[fmt.Sprint(r["ref"])] = r
	}
	return h
}

// ledgerCapabilities is what a package becomes for the native adapter: the native method
// of every operation and of every entity resolver.
func ledgerCapabilities(t testing.TB, pkg *apppkg.Package) []native.Capability {
	t.Helper()
	var out []native.Capability
	for _, id := range sortedKeys(pkg.Capabilities) {
		c := pkg.Capabilities[id]
		nc := native.Capability{ID: id, Operations: map[adapters.Operation]native.Binding{}, Resolve: map[string]native.Binding{}}
		bind := func(b *ap.CapabilityOperationBinding) native.Binding {
			if b.NativeMethod == nil || string(b.Endpoint) != ledger.EndpointID {
				t.Fatalf("capability %s maps a method that is not a native method of %s", id, ledger.EndpointID)
			}
			out := native.Binding{Endpoint: string(b.Endpoint), Method: string(*b.NativeMethod)}
			if b.TimeoutMS != nil {
				out.Timeout = time.Duration(*b.TimeoutMS) * time.Millisecond
			}
			return out
		}
		for op, b := range map[adapters.Operation]*ap.CapabilityOperationBinding{
			adapters.OpRead: c.Operations.Read, adapters.OpValidate: c.Operations.Validate, adapters.OpPrepare: c.Operations.Prepare,
			adapters.OpCommit: c.Operations.Commit, adapters.OpStatus: c.Operations.Status,
		} {
			if b != nil {
				nc.Operations[op] = bind(b)
			}
		}
		for typ, et := range c.EntityTypes {
			if et.Resolve != nil {
				nc.Resolve[typ] = bind(et.Resolve)
			}
		}
		out = append(out, nc)
	}
	return out
}

func (h *ledgerHost) manage(ctx context.Context, op string, args map[string]any) (map[string]any, error) {
	return h.raw.Manage(ctx, ledger.EndpointID, op, args)
}

func (h *ledgerHost) CreateTenant(ctx context.Context, t Tenant) error {
	var teams, principals []any
	for _, w := range t.Workspaces {
		teams = append(teams, map[string]any{"workspace_id": w})
	}
	for _, p := range t.Principals {
		roles := make([]any, len(p.Roles))
		for i, r := range p.Roles {
			roles[i] = r
		}
		principals = append(principals, map[string]any{"principal_id": p.ID, "workspace_id": p.WorkspaceID, "roles": roles})
	}
	out, err := h.manage(ctx, "create_tenant", map[string]any{"tenant_id": t.ID, "workspaces": teams, "principals": principals})
	if err != nil {
		return err
	}
	if out["status"] != "ok" {
		return contracts.New(contracts.ErrConflict, "the tenant was not created")
	}
	return nil
}

func (h *ledgerHost) Seed(ctx context.Context, sd Seed) (SeedResult, error) {
	var records []any
	var order []string
	for _, r := range sd.Records {
		rec := map[string]any{"ref": r.Ref, "entity_type": r.EntityType, "workspace_id": r.WorkspaceID}
		for k, v := range h.seedBy[r.Ref] {
			switch k {
			case "workspace", "principal", "entity_type", "tenant", "tenant_id", "workspace_id", "ref":
			default:
				rec[k] = v
			}
		}
		records = append(records, rec)
		order = append(order, r.Ref)
	}
	out, err := h.manage(ctx, "seed", map[string]any{"tenant_id": sd.TenantID, "records": records})
	if err != nil {
		return SeedResult{}, err
	}
	list, _ := out["records"].([]any)
	if out["status"] != "ok" || len(list) != len(order) {
		return SeedResult{}, contracts.New(contracts.ErrValidationFailed, "the tenant was not seeded")
	}
	res := SeedResult{Entities: map[string]string{}}
	for i, raw := range list {
		m, _ := raw.(map[string]any)
		if m["ref"] != order[i] {
			return SeedResult{}, contracts.New(contracts.ErrValidationFailed, "the seed answered another record")
		}
		res.Entities[order[i]] = fmt.Sprint(m["entity_id"])
	}
	return res, nil
}

func (h *ledgerHost) Cleanup(ctx context.Context, tenantID string) (CleanupReport, error) {
	out, err := h.manage(ctx, "cleanup", map[string]any{"tenant_id": tenantID})
	if err != nil {
		return CleanupReport{}, err
	}
	rep := CleanupReport{Remaining: map[string]int{}}
	rep.Existed, _ = out["existed"].(bool)
	if rem, ok := out["remaining"].(map[string]any); ok {
		for k, v := range rem {
			if n, ok := v.(float64); ok {
				rep.Remaining[k] = int(n)
			}
		}
	}
	return rep, nil
}

// CompleteStepUp is the manager in the ledger's own authenticated application: it quotes
// the void reference the prepared action showed.
func (h *ledgerHost) CompleteStepUp(_ context.Context, su StepUp) error {
	ref := ""
	for _, f := range su.Result.Values {
		if tv, ok := f.Value.Value.(act.TextValue); ok && string(f.Field) == ledgerVoidRef {
			ref = tv.Value
		}
	}
	if ref == "" {
		return contracts.New(contracts.ErrValidationFailed, "the prepared action shows no void reference")
	}
	h.approved++
	return h.app.ApproveVoid(bridge.Identity{
		ApplicationID: su.Scope.ApplicationID, TenantID: su.Scope.TenantID, WorkspaceID: su.Scope.WorkspaceID,
		PrincipalID: su.Scope.PrincipalID, Roles: su.Scope.Roles, SessionID: su.Scope.SessionID,
	}, ref)
}

func newLedgerEnv(t *testing.T, withStepUp bool) (*suEnv, *ledgerHost) {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(ledgerDir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if res.Package == nil {
		t.Fatalf("the ledger package did not load: %v", res.Findings)
	}
	h := newLedgerHost(t, res.Package)
	pkg := prunedTo(t, res.Package, ledgerCap)
	desc, err := h.ad.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := &suEnv{t: t, pkg: pkg, prov: h}
	if !withStepUp {
		e.prov = noStepUp{h}
	}
	e.in = &integrationaudit.Input{
		Description: &desc, Package: pkg, Adapter: h.ad, Clock: h.clk, CoreVersion: "0.1.0",
		Scope: contracts.Scope{
			DeploymentID: "alixai-audit", ApplicationID: ledger.ApplicationID, TenantID: "alixai-audit", WorkspaceID: "alixai-audit",
			PrincipalID: "alixai-audit", Roles: []string{"auditor"}, CapabilityScopes: []string{"audit.inspect"}, SessionID: "audit-run",
		},
	}
	return e, h
}

func TestLedgerAuditsWithoutCriticalStepUpFindings(t *testing.T) {
	for _, withStepUp := range []bool{true, false} {
		name := "with the manager's approval in the application"
		if !withStepUp {
			name = "without a way to complete the step-up"
		}
		t.Run(name, func(t *testing.T) {
			e, h := newLedgerEnv(t, withStepUp)
			var safety *integrationaudit.Outcome
			for _, s := range suSections {
				out := e.run(s)
				if s == integrationaudit.SectionSafeExecution {
					safety = out
				}
				for _, f := range out.Findings {
					if f.Severity == contracts.SeverityCritical {
						t.Errorf("check %d: critical finding %s for %q: %s", s, f.CheckID, f.Capability, f.Message)
					}
				}
				if !covered(out, ledgerCap) {
					t.Errorf("check %d did not probe %s:\n%s", s, ledgerCap, dump(out))
				}
			}
			for _, p := range safety.Probes {
				if !p.Passed {
					t.Errorf("probe %s %q failed: want %s, got %s", p.Kind, p.Subject, p.Expected, p.Observed)
				}
			}
			found, passed := probePassed(safety, "the effect is there after the application's own step-up")
			switch {
			case withStepUp && (!found || !passed):
				t.Errorf("the void was not shown to apply after the manager's approval (found %v):\n%s", found, dump(safety))
			case !withStepUp && (found || !hasUnverified(safety, "safety:step_up_completion")):
				t.Errorf("without a completer the audit must say the step-up is unverified:\n%s", dump(safety))
			}
			if withStepUp && h.approved == 0 {
				t.Errorf("the step-up was never completed")
			}
		})
	}
}
