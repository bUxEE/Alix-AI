package ledger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The ledger is exercised the way AlixAI reaches it: through the real native adapter,
// configured from the shipped package, over loopback, to the real host bridge of
// pkg/bridge. Every result is also validated against the result schema the package
// declares for the operation, because that is what the integration audit holds the
// application to.

var epoch = time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC)

// testKey is a throwaway application key (never a fixture secret on disk).
var testKey = []byte("ledger-test-application-key-0123456789abcdef")

type world struct {
	t       *testing.T
	clk     *clock.Fake
	app     *ledger.Ledger
	raw     *native.Adapter
	ad      adapters.AppAdapter
	pkg     *apppkg.Package
	schemas map[string]*jsonschema.Schema
	fx      *fixtures
}

func packageDir() string { return "package" }

func loadPackage(t testing.TB) *apppkg.Package {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(packageDir()), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if res.Package == nil {
		for _, f := range res.Findings {
			t.Errorf("package finding: %s %s: %s", f.Severity, f.CheckID, f.Message)
		}
		t.Fatal("the package does not load")
	}
	return res.Package
}

// newWorld starts a ledger, serves its bridge on loopback and connects the native
// adapter to it as the package says: capability methods and resolvers from the
// capability files, the application key through the reference of the adapter
// configuration, and the audit's provisioning operations the configuration approves.
func newWorld(t *testing.T, mutate ...func(*ledger.Config)) *world {
	t.Helper()
	w := &world{t: t, clk: clock.NewFake(epoch), pkg: loadPackage(t), schemas: map[string]*jsonschema.Schema{}}
	cfg := ledger.Config{Clock: w.clk.Now, TestSupport: true}
	for _, m := range mutate {
		m(&cfg)
	}
	w.app = ledger.New(cfg)

	raw, err := os.ReadFile(filepath.Join(packageDir(), "schemas", "adapters", "ledger-bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := native.ParseAdapterConfig(raw, ledger.EndpointID)
	if err != nil {
		t.Fatalf("the package's adapter configuration is refused by the native adapter: %v", err)
	}
	t.Setenv("LEDGER_BRIDGE_KEY", string(testKey))
	key, err := native.ResolveKey(settings.Key.Ref)
	if err != nil {
		t.Fatalf("the key reference of the adapter configuration does not resolve: %v", err)
	}

	srv, err := w.app.NewBridge(ledger.BridgeConfig{Keys: map[string][]byte{settings.Key.ID: testKey}})
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
	allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{portOf(t, addr)}, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	ncfg := native.Config{
		ApplicationID: ledger.ApplicationID,
		Endpoints: []native.Endpoint{{
			ID: ledger.EndpointID, Network: native.NetworkTCP, Address: addr, KeyID: settings.Key.ID, Key: key,
			Provisioning: settings.Management.Provisioning,
		}},
		Capabilities: capabilitiesOf(t, w.pkg),
		Dialer:       egress.NewDialer(allow),
	}
	w.raw, err = native.New(ncfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.raw.Close() })
	w.ad, err = adapters.Wrap(w.raw, adapters.Options{ApplicationID: ledger.ApplicationID})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func portOf(t testing.TB, addr string) int {
	t.Helper()
	i := strings.LastIndex(addr, ":")
	var p int
	for _, c := range addr[i+1:] {
		p = p*10 + int(c-'0')
	}
	return p
}

// capabilitiesOf is what a package becomes for the native adapter: the native_method of
// every operation and of every entity resolver (internal/cli does the same for `serve`).
func capabilitiesOf(t testing.TB, pkg *apppkg.Package) []native.Capability {
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- scopes and calls --------------------------------------------------------------

func (w *world) scope(tenantID, team, principal, role string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: ledger.ApplicationID, TenantID: tenantID, WorkspaceID: team,
		PrincipalID: principal, Roles: []string{role}, CapabilityScopes: sortedKeys(w.pkg.Capabilities), SessionID: "ses_" + principal,
	}
}

// schemaFor compiles the result schema of the package once.
func (w *world) schemaFor(path string) *jsonschema.Schema {
	w.t.Helper()
	if s := w.schemas[path]; s != nil {
		return s
	}
	f, err := os.Open(filepath.Join(packageDir(), filepath.FromSlash(path)))
	if err != nil {
		w.t.Fatal(err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		w.t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("urn:ledger:"+path, doc); err != nil {
		w.t.Fatal(err)
	}
	s, err := c.Compile("urn:ledger:" + path)
	if err != nil {
		w.t.Fatalf("result schema %s does not compile: %v", path, err)
	}
	w.schemas[path] = s
	return s
}

// conforms checks a result against the result schema the package names for it: the
// host members only, since operation and capability_id belong to AlixAI.
func (w *world) conforms(res actions.DeterministicResult, schemaPath string) {
	w.t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		w.t.Fatal(err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		w.t.Fatal(err)
	}
	delete(m, "operation")
	delete(m, "capability_id")
	if err := w.schemaFor(schemaPath).Validate(m); err != nil {
		w.t.Errorf("a %s result does not match %s: %v\n%s", res.Operation, schemaPath, err, b)
	}
}

func (w *world) binding(capID string, op adapters.Operation, resolve string) *ap.CapabilityOperationBinding {
	c := w.pkg.Capabilities[capID]
	if resolve != "" {
		return c.EntityTypes[resolve].Resolve
	}
	switch op {
	case adapters.OpRead:
		return c.Operations.Read
	case adapters.OpValidate:
		return c.Operations.Validate
	case adapters.OpPrepare:
		return c.Operations.Prepare
	case adapters.OpCommit:
		return c.Operations.Commit
	}
	return c.Operations.Status
}

func (w *world) must(res actions.DeterministicResult, err error, capID string, op adapters.Operation, resolve string) actions.DeterministicResult {
	w.t.Helper()
	if err != nil {
		w.t.Fatalf("%s %s: %v", capID, op, err)
	}
	w.conforms(res, string(w.binding(capID, op, resolve).ResultSchema))
	return res
}

func (w *world) read(sc contracts.Scope, capID string, fields map[string]any) actions.DeterministicResult {
	w.t.Helper()
	res, err := w.ad.Read(context.Background(), sc, adapters.ReadRequest{CapabilityID: capID, Fields: fields})
	resolve := ""
	if et, ok := fields["entity_type"].(string); ok {
		resolve = et
	}
	return w.must(res, err, capID, adapters.OpRead, resolve)
}

func (w *world) validate(sc contracts.Scope, capID string, fields map[string]any) actions.DeterministicResult {
	w.t.Helper()
	res, err := w.ad.Validate(context.Background(), sc, adapters.ValidateRequest{CapabilityID: capID, Fields: fields})
	return w.must(res, err, capID, adapters.OpValidate, "")
}

func (w *world) prepare(sc contracts.Scope, capID string, fields map[string]any) actions.DeterministicResult {
	w.t.Helper()
	res, err := w.ad.Prepare(context.Background(), sc, adapters.PrepareRequest{CapabilityID: capID, Fields: fields})
	return w.must(res, err, capID, adapters.OpPrepare, "")
}

// preparedFrom builds the PreparedAction the coordinator would hold for a successful
// Prepare result, with the expiry the host stated.
func (w *world) preparedFrom(capID string, res actions.DeterministicResult) actions.PreparedAction {
	w.t.Helper()
	if res.Preparation == nil {
		w.t.Fatalf("no preparation in %+v", res)
	}
	p := res.Preparation
	mode := actions.ConfirmationRequirementMode(w.pkg.Capabilities[capID].Confirmation.Mode)
	bindings := []actions.ConfirmationRequirementBindingsItem{}
	if mode != actions.ConfirmationRequirementModeNone {
		bindings = []actions.ConfirmationRequirementBindingsItem{
			actions.ConfirmationRequirementBindingsItemPreparationID, actions.ConfirmationRequirementBindingsItemPrincipal,
			actions.ConfirmationRequirementBindingsItemSession, actions.ConfirmationRequirementBindingsItemDataVersions,
		}
	}
	ttl := time.Duration(*p.ValidForSeconds) * time.Second
	return actions.PreparedAction{
		PreparationID: actions.PreparationID(ids.New("prep")), CapabilityID: res.CapabilityID, State: actions.PreparedActionStateCommitting,
		Summary: p.Summary, Effects: p.Effects, RiskLevel: p.DeclaredRiskLevel, DataVersions: p.DataVersions,
		PreparedAt: contractsTime(w.clk.Now()), ExpiresAt: contractsTime(w.clk.Now().Add(ttl)),
		InvalidationConditions: []actions.PreparedActionInvalidationConditionsItem{actions.PreparedActionInvalidationConditionsItemDataVersionChanged},
		Confirmation:           actions.ConfirmationRequirement{Mode: mode, Bindings: bindings},
		IdempotencyKey:         actions.IdempotencyKey(ids.New("idem")),
	}
}

func contractsTime(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format(time.RFC3339))
}

func (w *world) commit(sc contracts.Scope, capID string, p actions.PreparedAction) (actions.DeterministicResult, error) {
	w.t.Helper()
	return w.ad.Commit(context.Background(), sc, adapters.CommitRequest{CapabilityID: capID, IdempotencyKey: p.IdempotencyKey, Prepared: p})
}

func (w *world) committed(sc contracts.Scope, capID string, p actions.PreparedAction) actions.DeterministicResult {
	w.t.Helper()
	res, err := w.commit(sc, capID, p)
	return w.must(res, err, capID, adapters.OpCommit, "")
}

func (w *world) status(sc contracts.Scope, capID string, key actions.IdempotencyKey) actions.DeterministicResult {
	w.t.Helper()
	res, err := w.ad.Status(context.Background(), sc, adapters.StatusRequest{CapabilityID: capID, IdempotencyKey: key})
	return w.must(res, err, capID, adapters.OpStatus, "")
}

// ---- reading results ---------------------------------------------------------------

func rejectedWith(t testing.TB, res actions.DeterministicResult, code string) {
	t.Helper()
	if res.Status != actions.DeterministicResultStatusRejected || len(res.Errors) != 1 || string(res.Errors[0].Code) != code {
		t.Fatalf("want a result rejected with %s, got %s %+v", code, res.Status, res.Errors)
	}
	if len(res.Values) != 0 || len(res.Identifiers) != 0 {
		t.Fatalf("a rejected result discloses something: %+v", res)
	}
}

func isOK(t testing.TB, res actions.DeterministicResult) {
	t.Helper()
	if res.Status != actions.DeterministicResultStatusOk {
		t.Fatalf("want ok, got %s %+v", res.Status, res.Errors)
	}
}

// value finds a fact by field name.
func value(t testing.TB, res actions.DeterministicResult, field string) actions.CanonicalValueValue {
	t.Helper()
	for _, f := range res.Values {
		if string(f.Field) == field {
			return f.Value.Value
		}
	}
	t.Fatalf("no fact %q in %+v", field, res.Values)
	return nil
}

// money renders a money fact as "12.34 EUR".
func money(t testing.TB, res actions.DeterministicResult, field string) string {
	t.Helper()
	m, ok := value(t, res, field).(actions.MoneyValue)
	if !ok {
		t.Fatalf("fact %q is not a money value: %+v", field, value(t, res, field))
	}
	return m.Value + " " + m.Currency
}

func text(t testing.TB, res actions.DeterministicResult, field string) string {
	t.Helper()
	switch v := value(t, res, field).(type) {
	case actions.TextValue:
		return v.Value
	case actions.DateValue:
		return v.Value
	case actions.IdentifierValue:
		return string(v.Value)
	}
	t.Fatalf("fact %q is not text: %+v", field, value(t, res, field))
	return ""
}

func integer(t testing.TB, res actions.DeterministicResult, field string) int64 {
	t.Helper()
	v, ok := value(t, res, field).(actions.IntegerValue)
	if !ok {
		t.Fatalf("fact %q is not an integer", field)
	}
	return v.Value
}

func hasFact(res actions.DeterministicResult, field string) bool {
	for _, f := range res.Values {
		if string(f.Field) == field {
			return true
		}
	}
	return false
}

// ---- fixtures, provisioned the way the integration audit does ---------------------

// fixtures are the disposable organisations of the package's own test suite: the
// tenants, teams, principals and records it declares, created and seeded through the
// management operations with generated identifiers.
type fixtures struct {
	tenant, team, principal, record map[string]string // logical ref -> real id
	role, teamOf, tenantOf          map[string]string // principal ref -> role, team id, tenant id
}

func (w *world) manage(op string, args map[string]any) map[string]any {
	w.t.Helper()
	out, err := w.raw.Manage(context.Background(), ledger.EndpointID, op, args)
	if err != nil {
		w.t.Fatalf("manage %s: %v", op, err)
	}
	return out
}

func (w *world) provision() *fixtures {
	w.t.Helper()
	suite := w.pkg.Tests[0]
	fx := &fixtures{tenant: map[string]string{}, team: map[string]string{}, principal: map[string]string{}, record: map[string]string{},
		role: map[string]string{}, teamOf: map[string]string{}, tenantOf: map[string]string{}}
	const suffix = "a1b2c3d4"
	name := func(prefix, ref string) string {
		return prefix + "_" + strings.ReplaceAll(ref, "-", "_") + "_" + suffix
	}

	seedRaw, err := os.ReadFile(filepath.Join(packageDir(), "tests", "seed", "ledger.json"))
	if err != nil {
		w.t.Fatal(err)
	}
	var seed struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(seedRaw, &seed); err != nil {
		w.t.Fatal(err)
	}
	seedBy := map[string]map[string]any{}
	for _, r := range seed.Records {
		seedBy[r["ref"].(string)] = r
	}

	for _, tn := range suite.Fixtures.Tenants {
		tid := name("tnt", string(tn.Ref))
		fx.tenant[string(tn.Ref)] = tid
		var teams []any
		for _, ws := range tn.Workspaces {
			wid := name("ws", string(ws))
			fx.team[string(ws)] = wid
			teams = append(teams, map[string]any{"workspace_id": wid})
		}
		var principals []any
		for _, p := range suite.Fixtures.Principals {
			if p.Tenant != tn.Ref {
				continue
			}
			pid := name("usr", string(p.Ref))
			fx.principal[string(p.Ref)] = pid
			fx.role[string(p.Ref)] = string(p.Roles[0])
			fx.teamOf[string(p.Ref)] = fx.team[string(p.Workspace)]
			fx.tenantOf[string(p.Ref)] = tid
			principals = append(principals, map[string]any{"principal_id": pid, "workspace_id": fx.team[string(p.Workspace)], "roles": []any{string(p.Roles[0])}})
		}
		created := w.manage("create_tenant", map[string]any{"tenant_id": tid, "workspaces": teams, "principals": principals})
		if created["status"] != "ok" {
			w.t.Fatalf("create_tenant %s: %v", tid, created)
		}
		var records []any
		var order []string
		for _, r := range suite.Fixtures.Records {
			if r.Tenant != tn.Ref {
				continue
			}
			rec := map[string]any{"ref": string(r.Ref), "entity_type": string(r.EntityType), "workspace_id": fx.team[string(r.Workspace)]}
			for k, v := range seedBy[string(r.Ref)] {
				switch k {
				case "workspace", "principal", "entity_type", "tenant", "tenant_id", "workspace_id":
				default:
					rec[k] = v
				}
			}
			records = append(records, rec)
			order = append(order, string(r.Ref))
		}
		seeded := w.manage("seed", map[string]any{"tenant_id": tid, "records": records})
		if seeded["status"] != "ok" {
			w.t.Fatalf("seed %s: %v", tid, seeded)
		}
		for i, rr := range seeded["records"].([]any) {
			m := rr.(map[string]any)
			if m["ref"] != order[i] {
				w.t.Fatalf("seed answered %v for %s", m["ref"], order[i])
			}
			fx.record[order[i]] = m["entity_id"].(string)
		}
	}
	w.fx = fx
	return fx
}

// as returns the scope of a fixture principal.
func (w *world) as(ref string) contracts.Scope {
	w.t.Helper()
	return w.scope(w.fx.tenantOf[ref], w.fx.teamOf[ref], w.fx.principal[ref], w.fx.role[ref])
}

func jsonRoundTrip(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}
