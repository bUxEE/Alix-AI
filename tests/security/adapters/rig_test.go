package adapterssec

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mcpfixture "github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	openapifixture "github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The synthetic application the coordinator rig serves. The capabilities are the two
// the in-memory host (internal/adapters/fake) offers.
const (
	demoApp   = "demo-app"
	tenantA   = "tenant-a"
	tenantB   = "tenant-b"
	ws1       = "ws-1"
	capRead   = fake.ReadCapability
	capCreate = fake.ActionCapability
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

// level describes how the package declares items.create: the effect class fixes the
// platform risk floor, and level 1 with no confirmation commits straight from prepared.
type level struct {
	effect  policy.EffectClass
	risk    contracts.RiskLevel
	confirm policy.ConfirmationMode
}

var level1 = level{policy.EffectReversible, contracts.RiskReversible, policy.ConfirmNone}

const policyJSON = `{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

// newPackage builds an active package for an application that declares the two
// capabilities of the fake host.
func newPackage(t testing.TB, appID string, lv level) *policy.Package {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(policyJSON))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID,
		Version:       "1.0.0",
		Status:        policy.StatusActive,
		Roles:         []string{"member", "visitor", "staff"},
		Capabilities: []policy.CapabilityDecl{
			{ID: capRead, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
			{ID: capCreate, Audience: policy.AudienceUser, EffectClass: lv.effect, RiskLevel: lv.risk,
				Confirmation: lv.confirm, Fields: []policy.FieldDecl{{Name: "name", Required: true}}},
		},
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	return p
}

// scopeFor is a valid scope of a member of a tenant of an application.
func scopeFor(appID, tenant, principal, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep-1",
		ApplicationID:    appID,
		TenantID:         tenant,
		WorkspaceID:      ws1,
		PrincipalID:      principal,
		Roles:            []string{"member"},
		CapabilityScopes: []string{capRead, capCreate},
		SessionID:        session,
	}
}

func ctxb(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// randomHex returns a fresh random token. Canaries and credentials are made per run:
// none is a literal in the source.
func randomHex(t testing.TB) string {
	t.Helper()
	b := make([]byte, 10)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// ---- loopback egress ---------------------------------------------------------

// loopbackDialer allows exactly the host and port of an http URL.
func loopbackDialer(t testing.TB, rawURL string) *egress.Dialer {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portText)
	al, err := egress.NewAllowlist(egress.Destination{Host: host, Ports: []int{port}, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	return egress.NewDialer(al)
}

// ---- the hostile man in the middle -------------------------------------------

// forged is one reply as a hostile server could send it: the honest members of the
// DeterministicResult, as the application produced them, for a rule to edit, replace,
// reduce to prose or send with another idempotency key.
type forged struct {
	// Op is the adapter operation the reply answers.
	Op adapters.Operation
	// Object is the honest reply's members. A rule edits it in place.
	Object map[string]any
	// Echo is the idempotency key the honest reply carried back ("" when none). A rule
	// sets EchoSet to send another one (or none).
	Echo    string
	EchoSet bool

	// Replace, when Replaced, is sent instead of the object: an array, a string, a number.
	Replace  any
	Replaced bool
	// Prose, when not empty, makes the reply text only.
	Prose string
	// Truncate cuts a REST body in half.
	Truncate bool
	// Status and ContentType override a REST reply's HTTP status and content type.
	Status      int
	ContentType string
}

func (f *forged) setEcho(k string) { f.Echo, f.EchoSet = k, true }
func (f *forged) replace(v any)    { f.Replace, f.Replaced = v, true }

// rule is one forgery armed for the next reply of an operation, optionally for one tenant.
type rule struct {
	op     adapters.Operation
	tenant string
	apply  func(*forged)
	fired  bool
}

// mitm is an HTTP reverse proxy between an adapter and the real host application
// behind its real wire (the MCP fixture server or the OpenAPI fixture host). It
// forwards every request unchanged and, for the next reply of an operation that a
// rule is armed for, rewrites the host's reply: the adapter and everything above it
// then deal with a hostile server that answers like the real one except where it
// lies. It speaks the wire of its kind, so the adapters under test read the forged
// reply through their real parsers.
type mitm struct {
	kind string // "mcp" or "openapi"
	srv  *httptest.Server

	mu    sync.Mutex
	rules []*rule
	fired int
}

func newMITM(t testing.TB, kind, upstream string) *mitm {
	t.Helper()
	target, err := url.Parse("http://" + upstream)
	if err != nil {
		t.Fatal(err)
	}
	m := &mitm{kind: kind}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			op, tenant := m.classify(pr.In)
			if pr.In.Body != nil {
				b, _ := io.ReadAll(pr.In.Body)
				pr.Out.Body = io.NopCloser(bytes.NewReader(b))
				pr.Out.ContentLength = int64(len(b))
				op, tenant = m.classifyBody(pr.In, b)
			}
			pr.Out.Header.Set("X-Mitm-Op", string(op))
			pr.Out.Header.Set("X-Mitm-Tenant", tenant)
		},
		ModifyResponse: m.modify,
	}
	m.srv = httptest.NewServer(rp)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mitm) URL() string  { return m.srv.URL }
func (m *mitm) Addr() string { return m.srv.Listener.Addr().String() }

// arm forges the next reply of an operation, for any tenant.
func (m *mitm) arm(op adapters.Operation, apply func(*forged)) { m.armFor(op, "", apply) }

// armFor forges the next reply of an operation asked for by one tenant.
func (m *mitm) armFor(op adapters.Operation, tenant string, apply func(*forged)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append(m.rules, &rule{op: op, tenant: tenant, apply: apply})
}

// pending is how many armed rules have not fired.
func (m *mitm) pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rules {
		if !r.fired {
			n++
		}
	}
	return n
}

// disarm drops every rule that has not fired.
func (m *mitm) disarm() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = nil
}

// classify names the adapter operation and the tenant of a request, from what the
// wire carries.
func (m *mitm) classify(r *http.Request) (adapters.Operation, string) {
	if m.kind != "openapi" {
		return "", "" // MCP: decided from the body
	}
	var op adapters.Operation
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/items/"):
		op = adapters.OpRead
	case r.Method == http.MethodPost && r.URL.Path == "/item-drafts/validate":
		op = adapters.OpValidate
	case r.Method == http.MethodPost && r.URL.Path == "/item-drafts/prepare":
		op = adapters.OpPrepare
	case r.Method == http.MethodPost && r.URL.Path == "/items":
		op = adapters.OpCommit
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/commits/"):
		op = adapters.OpStatus
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/commits/"):
		op = adapters.OpCancel
	}
	var id adapters.Identity
	if raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get(openapifixture.IdentityHdr)); err == nil {
		_ = json.Unmarshal(raw, &id)
	}
	return op, id.TenantID
}

func (m *mitm) classifyBody(r *http.Request, body []byte) (adapters.Operation, string) {
	if m.kind != "mcp" {
		return m.classify(r)
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Identity adapters.Identity `json:"identity"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &rpc) != nil || rpc.Method != "tools/call" {
		return "", ""
	}
	ops := map[string]adapters.Operation{
		mcpfixture.ToolGet: adapters.OpRead, mcpfixture.ToolValidate: adapters.OpValidate, mcpfixture.ToolPrepare: adapters.OpPrepare,
		mcpfixture.ToolCommit: adapters.OpCommit, mcpfixture.ToolStatus: adapters.OpStatus, mcpfixture.ToolCancel: adapters.OpCancel,
	}
	return ops[rpc.Params.Name], rpc.Params.Arguments.Identity.TenantID
}

// take returns the first armed rule that matches a reply.
func (m *mitm) take(op adapters.Operation, tenant string) *rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rules {
		if !r.fired && r.op == op && (r.tenant == "" || r.tenant == tenant) {
			r.fired = true
			m.fired++
			return r
		}
	}
	return nil
}

func (m *mitm) modify(resp *http.Response) error {
	op := adapters.Operation(resp.Request.Header.Get("X-Mitm-Op"))
	if op == "" {
		return nil
	}
	r := m.take(op, resp.Request.Header.Get("X-Mitm-Tenant"))
	if r == nil {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	out, err := m.rewrite(resp, op, body, r.apply)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// rewrite applies a forgery to the honest reply body and returns the body to send.
func (m *mitm) rewrite(resp *http.Response, op adapters.Operation, honest []byte, apply func(*forged)) ([]byte, error) {
	f := &forged{Op: op}
	if m.kind == "openapi" {
		_ = json.Unmarshal(bytes.TrimRight(honest, " "), &f.Object)
		f.Echo = resp.Header.Get(openapifixture.KeyHdr)
		apply(f)
		var body []byte
		ct := "application/json"
		switch {
		case f.Prose != "":
			body, ct = []byte(f.Prose), "text/plain"
		case f.Replaced:
			body, _ = json.Marshal(f.Replace)
		default:
			body, _ = json.Marshal(f.Object)
		}
		if f.Truncate {
			body = body[:len(body)/2]
		}
		if f.ContentType != "" {
			ct = f.ContentType
		}
		if f.Status != 0 {
			resp.StatusCode, resp.Status = f.Status, strconv.Itoa(f.Status)+" "+http.StatusText(f.Status)
		}
		resp.Header.Set("Content-Type", ct)
		if f.EchoSet {
			if f.Echo == "" {
				resp.Header.Del(openapifixture.KeyHdr)
			} else {
				resp.Header.Set(openapifixture.KeyHdr, f.Echo)
			}
		}
		return body, nil
	}

	// MCP: a JSON-RPC response whose result carries the tool result.
	var env map[string]any
	if err := json.Unmarshal(honest, &env); err != nil {
		return nil, fmt.Errorf("the MCP reply is not JSON: %w", err)
	}
	result, _ := env["result"].(map[string]any)
	if result == nil {
		return honest, nil
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		f.Object = sc
	}
	if meta, ok := result["_meta"].(map[string]any); ok {
		f.Echo, _ = meta[mcpfixture.MetaKey].(string)
	}
	apply(f)
	text := func(v any) {
		b, _ := json.Marshal(v)
		result["content"] = []any{map[string]any{"type": "text", "text": string(b)}}
	}
	switch {
	case f.Prose != "":
		delete(result, "structuredContent")
		result["content"] = []any{map[string]any{"type": "text", "text": f.Prose}}
	case f.Replaced:
		result["structuredContent"] = f.Replace
		text(f.Replace)
	default:
		result["structuredContent"] = f.Object
		text(f.Object)
	}
	if f.EchoSet {
		if f.Echo == "" {
			delete(result, "_meta")
		} else {
			result["_meta"] = map[string]any{mcpfixture.MetaKey: f.Echo}
		}
	}
	return json.Marshal(env)
}

// ---- the three wires ---------------------------------------------------------

// wire is a host application behind a transport adapter.
type wire struct {
	kind string // "mcp", "openapi" or "fake"
	app  *fake.App
	// adapter is the raw transport adapter, before adapters.Wrap.
	adapter adapters.AppAdapter
	// proxy is the hostile man in the middle; nil on the fake wire.
	proxy *mitm
	// mcp is the fixture MCP server, for the tool calls it served.
	mcp *mcpfixture.HTTPServer
	// rest is the fixture HTTP host.
	rest *openapifixture.Host
}

// newWire starts the host application and a transport adapter for it. appID is the
// application the adapter is configured for.
func newWire(t *testing.T, kind, appID string, clk clock.Clock) *wire {
	t.Helper()
	w := &wire{kind: kind, app: fake.NewApp(fake.WithClock(clk))}
	switch kind {
	case "fake":
		w.adapter = fake.NewAdapter(w.app)
	case "mcp":
		w.mcp = mcpfixture.StartHTTP(t, mcpfixture.New(w.app))
		w.proxy = newMITM(t, "mcp", w.mcp.Addr)
		ep := w.proxy.URL() + "/mcp"
		b := func(tool string) mcptransport.Binding { return mcptransport.Binding{Endpoint: "fake-mcp", Tool: tool} }
		a, err := mcptransport.New(mcptransport.Config{
			ApplicationID: appID,
			Endpoints:     []mcptransport.Endpoint{{ID: "fake-mcp", URL: ep}},
			Capabilities: []mcptransport.Capability{
				{ID: capRead, Operations: map[adapters.Operation]mcptransport.Binding{adapters.OpRead: b(mcpfixture.ToolGet)}},
				{ID: capCreate, Operations: map[adapters.Operation]mcptransport.Binding{
					adapters.OpValidate: b(mcpfixture.ToolValidate), adapters.OpPrepare: b(mcpfixture.ToolPrepare),
					adapters.OpCommit: b(mcpfixture.ToolCommit), adapters.OpStatus: b(mcpfixture.ToolStatus), adapters.OpCancel: b(mcpfixture.ToolCancel),
				}},
			},
			Dialer:           loopbackDialer(t, ep),
			MaxResponseBytes: 64 << 10,
			SurfaceRecheck:   -1, // list the tools before every call, so no test waits on a timer
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		w.adapter = a
	case "openapi":
		w.rest = openapifixture.Start(t, w.app)
		token := "tok_" + randomHex(t)
		w.rest.RequireBearer(token)
		w.proxy = newMITM(t, "openapi", w.rest.Addr)
		settings, err := openapitransport.ParseAdapterConfig(openapifixture.AdapterConfig, "fixture-api")
		if err != nil {
			t.Fatal(err)
		}
		bind := func(id string) openapitransport.Binding {
			return openapitransport.Binding{Endpoint: "fixture-api", OperationID: id}
		}
		get := bind(openapifixture.OpGet)
		get.Arguments = map[string]string{"item_id": "id"}
		a, err := openapitransport.New(openapitransport.Config{
			ApplicationID: appID,
			Endpoints: []openapitransport.Endpoint{{
				ID: "fixture-api", BaseURL: w.proxy.URL(), Spec: openapifixture.Spec, Settings: settings, CredentialRef: "cred:fixture",
			}},
			Capabilities: []openapitransport.Capability{
				{ID: capRead, Operations: map[adapters.Operation]openapitransport.Binding{adapters.OpRead: get}},
				{ID: capCreate, Operations: map[adapters.Operation]openapitransport.Binding{
					adapters.OpValidate: bind(openapifixture.OpValidate), adapters.OpPrepare: bind(openapifixture.OpPrepare),
					adapters.OpCommit: bind(openapifixture.OpCommit), adapters.OpStatus: bind(openapifixture.OpStatus), adapters.OpCancel: bind(openapifixture.OpCancel),
				}},
			},
			Dialer:           loopbackDialer(t, w.proxy.URL()),
			Secrets:          openapitransport.StaticSecrets{"cred:fixture": token},
			MaxResponseBytes: 64 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		w.adapter = a
	default:
		t.Fatalf("unknown wire %q", kind)
	}
	return w
}

// hostCalls counts the requests of an operation the host application received.
func (w *wire) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range w.app.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

// ---- the coordinator rig -----------------------------------------------------

type policyRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (r *policyRecorder) Emit(_ context.Context, rec policy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

type tee []policy.Sink

func (t tee) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// stack is the real coordinator over a real store, audit log and policy gateway, with
// the adapter of a wire behind adapters.Wrap, exactly as the composition root wires
// them. The package and the adapter are resolved per application, as in production.
type stack struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	gw    *policy.Gateway
	co    *actions.Coordinator
	wires map[string]*wire // by application
	pkgs  map[string]policy.ActivePackage
	guard map[string]adapters.AppAdapter
	// forcedAdapter and forcedPackage, when set, answer for every application: a
	// wiring mistake (or an attack on the wiring) the defence in depth must survive.
	forcedAdapter adapters.AppAdapter
	forcedPackage policy.ActivePackage
	mu            sync.Mutex
}

// newStack builds the rig for the applications given: each gets its own host and its
// own adapter of the wire kind. The first application's wire is s.wire().
func newStack(t *testing.T, kind string, lv level, apps ...string) *stack {
	t.Helper()
	if len(apps) == 0 {
		apps = []string{demoApp}
	}
	s := &stack{t: t, clk: clock.NewFake(t0), wires: map[string]*wire{}, pkgs: map[string]policy.ActivePackage{}, guard: map[string]adapters.AppAdapter{}}
	s.db = storetest.Open(t, nil)
	alog, err := audit.New(s.db, audit.Options{Clock: s.clk})
	if err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		sub := r.Subject
		return audit.ForIDs(sub.ApplicationID, sub.TenantID, sub.WorkspaceID, sub.PrincipalID, sub.SessionID)
	})
	s.gw = policy.NewGateway(s.clk, policy.WithSink(tee{sink, &policyRecorder{}}))
	for _, app := range apps {
		w := newWire(t, kind, app, s.clk)
		s.wires[app] = w
		g, err := adapters.Wrap(w.adapter, adapters.Options{ApplicationID: app})
		if err != nil {
			t.Fatal(err)
		}
		s.guard[app] = g
		s.pkgs[app] = newPackage(t, app, lv)
	}
	s.co, err = actions.New(actions.Deps{
		DB: s.db, Policy: s.gw, Audit: alog, Clock: s.clk,
		Packages: actions.PackageSourceFunc(func(_ context.Context, app string) (policy.ActivePackage, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.forcedPackage != nil {
				return s.forcedPackage, nil
			}
			p, ok := s.pkgs[app]
			if !ok {
				return nil, contracts.New(contracts.ErrUnavailable, "no package for this application")
			}
			return p, nil
		}),
		Adapters: actions.AdapterSourceFunc(func(_ context.Context, app string) (adapters.AppAdapter, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.forcedAdapter != nil {
				return s.forcedAdapter, nil
			}
			g, ok := s.guard[app]
			if !ok {
				return nil, contracts.New(contracts.ErrUnavailable, "no adapter for this application")
			}
			return g, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wire returns the wire of the (first) application.
func (s *stack) wire(app string) *wire { return s.wires[app] }

// ---- steps ----

func (s *stack) prepare(sc contracts.Scope, name string) (actions.Prepared, error) {
	s.t.Helper()
	return s.co.Prepare(ctxb(s.t), sc, actions.PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": name}})
}

func (s *stack) mustPrepare(sc contracts.Scope, name string) actions.Prepared {
	s.t.Helper()
	p, err := s.prepare(sc, name)
	if err != nil {
		s.t.Fatalf("Prepare: %v", err)
	}
	return p
}

func (s *stack) commit(sc contracts.Scope, p actions.Prepared) (contracts.ActionReceipt, error) {
	s.t.Helper()
	return s.co.Commit(ctxb(s.t), sc, string(p.Action.PreparationID))
}

func (s *stack) read(sc contracts.Scope, itemID string) (actions.ReadOutcome, error) {
	s.t.Helper()
	return s.co.Read(ctxb(s.t), sc, actions.ReadInput{Session: sc, CapabilityID: capRead, Fields: map[string]any{"item_id": itemID}})
}

// ---- what the store holds ----------------------------------------------------

func (s *stack) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (s *stack) state(id string) act.PreparedActionState {
	s.t.Helper()
	var st string
	if err := s.db.QueryRowContext(context.Background(), `SELECT state FROM action_preparations WHERE id = ?`, id).Scan(&st); err != nil {
		s.t.Fatalf("state of %s: %v", id, err)
	}
	return act.PreparedActionState(st)
}

func (s *stack) preparations() int { return s.count(`SELECT COUNT(*) FROM action_preparations`) }
func (s *stack) receipts() int     { return s.count(`SELECT COUNT(*) FROM action_receipts`) }
func (s *stack) events(prefix string) int {
	return s.count(`SELECT COUNT(*) FROM audit_events WHERE event_type LIKE ?`, prefix+"%")
}

// dbContains reports whether any text column of any table of the store holds needle.
// It is how the suite proves a forged value was never persisted, and it is run
// against a value that WAS accepted to prove it can see one.
func (s *stack) dbContains(needle string) bool {
	s.t.Helper()
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		s.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			s.t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	if len(tables) < 5 {
		s.t.Fatalf("the store lists only %d tables: the dump would look at nothing", len(tables))
	}
	found := false
	for _, tbl := range tables {
		trs, err := s.db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(tbl, `"`, `""`)+`"`)
		if err != nil {
			s.t.Fatalf("dumping %s: %v", tbl, err)
		}
		cols, _ := trs.Columns()
		for trs.Next() {
			vals := make([]sql.RawBytes, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := trs.Scan(ptrs...); err != nil {
				s.t.Fatalf("dumping %s: %v", tbl, err)
			}
			for _, v := range vals {
				if bytes.Contains(v, []byte(needle)) {
					found = true
				}
			}
		}
		_ = trs.Close()
	}
	return found
}
