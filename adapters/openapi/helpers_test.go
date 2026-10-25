package openapi

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
)

const (
	testApp  = "app_demo"
	testEP   = "fixture-api"
	testSize = 64 << 10
	testRef  = "cred:fixture"
	// testToken is the credential the fixture application requires. Tests that
	// look for it in errors and logs use a value no other string contains.
	testToken = "tok_Zx9-live-SECRET-42"
)

// loopbackDialer allows the one loopback endpoint of the fixture: the egress
// dialer refuses loopback unless it is listed.
func loopbackDialer(t testing.TB, h *fixture.Host, opts ...egress.Option) *egress.Dialer {
	t.Helper()
	_, portText, _ := splitHostPort(h.Addr)
	port, _ := strconv.Atoi(portText)
	allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{port}, Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	return egress.NewDialer(allow, opts...)
}

func splitHostPort(addr string) (host, port string, ok bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], true
		}
	}
	return addr, "", false
}

// fixtureCapabilities is the mapping of the fixture package: the two capabilities
// of fake.App on the operations of the fixture document. item_id travels as the
// path parameter id (a rename). cancel is wired in code, as a package cannot.
func fixtureCapabilities() []Capability {
	bind := func(id string) Binding { return Binding{Endpoint: testEP, OperationID: id} }
	get := bind(fixture.OpGet)
	get.Arguments = map[string]string{"item_id": "id"}
	return []Capability{
		{ID: fake.ReadCapability, Operations: map[adapters.Operation]Binding{adapters.OpRead: get}},
		{ID: fake.ActionCapability, Operations: map[adapters.Operation]Binding{
			adapters.OpValidate: bind(fixture.OpValidate), adapters.OpPrepare: bind(fixture.OpPrepare), adapters.OpCommit: bind(fixture.OpCommit),
			adapters.OpStatus: bind(fixture.OpStatus), adapters.OpCancel: bind(fixture.OpCancel),
		}},
	}
}

func fixtureSettings(t testing.TB) AdapterConfig {
	t.Helper()
	s, err := ParseAdapterConfig(fixture.AdapterConfig, testEP)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fixtureConfig is the configuration of an adapter over the fixture host: its
// document, its adapter configuration, a credential and the loopback allowlist.
func fixtureConfig(t testing.TB, h *fixture.Host) Config {
	t.Helper()
	return Config{
		ApplicationID: testApp,
		Endpoints: []Endpoint{{
			ID: testEP, BaseURL: h.URL, Spec: fixture.Spec, Settings: fixtureSettings(t), CredentialRef: testRef,
		}},
		Capabilities:     fixtureCapabilities(),
		Dialer:           loopbackDialer(t, h),
		Secrets:          StaticSecrets{testRef: testToken},
		MaxResponseBytes: testSize,
	}
}

// newAdapter builds the raw adapter and closes it when the test ends.
func newAdapter(t testing.TB, cfg Config) *Adapter {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// wrapped is the adapter the way production wires it.
func wrapped(t testing.TB, a *Adapter) adapters.AppAdapter {
	t.Helper()
	w, err := adapters.Wrap(a, adapters.Options{ApplicationID: testApp})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// subject is an adapter over a fresh fixture host, with the credential required.
func subject(t *testing.T) adaptersuite.Subject {
	t.Helper()
	app := fake.NewApp(fake.WithOversizeBytes(4 * testSize))
	h := fixture.Start(t, app)
	h.RequireBearer(testToken)
	a := newAdapter(t, fixtureConfig(t, h))
	return adaptersuite.Subject{Adapter: wrapped(t, a), Fixture: h, ApplicationID: testApp, MaxResponseBytes: testSize}
}

// testScope is a valid scope for the fixture application.
func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_t", ApplicationID: testApp, TenantID: "ten_a", WorkspaceID: "wks_a", PrincipalID: "usr_alice",
		Roles: []string{"member"}, CapabilityScopes: []string{fake.ReadCapability, fake.ActionCapability}, SessionID: "ses_alice",
	}
}

// logBuffer is a log destination that tests read back.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// debugLogger logs everything at debug level into the buffer.
func debugLogger() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// seen is one request a test API received.
type seen struct {
	Method string
	Path   string // escaped
	Query  string
	Header http.Header
	Body   []byte
	Close  bool
}

// testAPI is a scriptable HTTP server on a loopback port for the tests whose
// document and answers are written by the test itself.
type testAPI struct {
	URL, Addr string

	mu      sync.Mutex
	reqs    []seen
	handle  func(w http.ResponseWriter, r *http.Request, body []byte)
	accepts atomic.Int64
	srv     *http.Server
}

// startAPI serves handle on 127.0.0.1 until the test ends.
func startAPI(t testing.TB, handle func(w http.ResponseWriter, r *http.Request, body []byte)) *testAPI {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &testAPI{Addr: ln.Addr().String(), handle: handle}
	a.URL = "http://" + a.Addr
	a.srv = &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				a.accepts.Add(1)
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			a.mu.Lock()
			a.reqs = append(a.reqs, seen{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body, Close: r.Close})
			h := a.handle
			a.mu.Unlock()
			h(w, r, body)
		}),
	}
	go func() { _ = a.srv.Serve(ln) }()
	t.Cleanup(func() { _ = a.srv.Close() })
	return a
}

// Set replaces the handler.
func (a *testAPI) Set(handle func(w http.ResponseWriter, r *http.Request, body []byte)) {
	a.mu.Lock()
	a.handle = handle
	a.mu.Unlock()
}

// Requests returns every request received, oldest first.
func (a *testAPI) Requests() []seen {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]seen(nil), a.reqs...)
}

// Accepts is how many connections the server accepted.
func (a *testAPI) Accepts() int { return int(a.accepts.Load()) }

// Port is the port the server listens on.
func (a *testAPI) Port() int {
	_, p, _ := splitHostPort(a.Addr)
	n, _ := strconv.Atoi(p)
	return n
}

// Dialer allows exactly this server (and any extra ports on the loopback
// address).
func (a *testAPI) Dialer(t testing.TB, extra ...int) *egress.Dialer {
	t.Helper()
	allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: append([]int{a.Port()}, extra...), Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	return egress.NewDialer(allow)
}

// okResult is a well-formed DeterministicResult body of a successful read.
const okResult = `{"status":"ok","identifiers":[],"values":[{"field":"name","value":{"kind":"text","value":"Widget"}}],"errors":[],"allowed_next_actions":[],"freshness":{"as_of":"2026-10-09T08:00:00Z","version":"v1"}}`

// writeBody answers with a status, a content type and a body.
func writeBody(w http.ResponseWriter, status int, contentType, body string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// miniSpec is a document with a read at GET /items/{id} and a commit at POST
// /items, whose Result schema is the one given. Replies with the statuses 200 and
// 422 are described, and 202 is described without a body.
func miniSpec(resultSchema string) []byte {
	return []byte(`openapi: 3.0.3
info: {title: mini, version: "1"}
paths:
  /items/{id}:
    get:
      operationId: getItem
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses:
        "200": {$ref: "#/components/responses/Result"}
        "422": {$ref: "#/components/responses/Result"}
        "500": {$ref: "#/components/responses/Result"}
        "202": {description: accepted}
  /items:
    post:
      operationId: commitItem
      requestBody:
        required: true
        content:
          application/json:
            schema: {type: object}
      responses:
        "200": {$ref: "#/components/responses/Result"}
        "422": {$ref: "#/components/responses/Result"}
        "500": {$ref: "#/components/responses/Result"}
components:
  responses:
    Result:
      description: result
      content:
        application/json:
          schema: {$ref: "#/components/schemas/Result"}
  schemas:
    Result:
` + resultSchema)
}

// miniCapabilities maps items.get to getItem (item_id is the path parameter id)
// and items.create's commit to commitItem.
func miniCapabilities() []Capability {
	return []Capability{
		{ID: fake.ReadCapability, Operations: map[adapters.Operation]Binding{
			adapters.OpRead: {Endpoint: testEP, OperationID: "getItem", Arguments: map[string]string{"item_id": "id"}},
		}},
		{ID: fake.ActionCapability, Operations: map[adapters.Operation]Binding{
			adapters.OpCommit: {Endpoint: testEP, OperationID: "commitItem"},
		}},
	}
}

// miniConfig is the configuration of an adapter over a test API and a document.
// It has no credential and no management operations.
func miniConfig(t testing.TB, api *testAPI, spec []byte, caps []Capability) Config {
	t.Helper()
	return Config{
		ApplicationID: testApp,
		Endpoints: []Endpoint{{
			ID: testEP, BaseURL: api.URL, Spec: spec, Settings: mustSettings(t, "server: "+testEP+"\n"),
		}},
		Capabilities:     caps,
		Dialer:           api.Dialer(t),
		MaxResponseBytes: testSize,
	}
}

func mustSettings(t testing.TB, yaml string) AdapterConfig {
	t.Helper()
	s, err := ParseAdapterConfig([]byte(yaml), testEP)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// preparedFor builds the PreparedAction the core would hold for a key.
func preparedFor(capability string, key actions.IdempotencyKey) actions.PreparedAction {
	return actions.PreparedAction{
		PreparationID: actions.PreparationID("prep_" + string(key)), CapabilityID: common.CapabilityID(capability),
		State: actions.PreparedActionStateCommitting, Summary: "Create an item", Effects: []actions.Effect{}, RiskLevel: 1,
		DataVersions: []actions.DataVersion{}, PreparedAt: "2026-10-09T08:00:00Z", ExpiresAt: "2026-10-09T08:10:00Z",
		InvalidationConditions: []actions.PreparedActionInvalidationConditionsItem{actions.PreparedActionInvalidationConditionsItemDataVersionChanged},
		Confirmation:           actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone, Bindings: []actions.ConfirmationRequirementBindingsItem{}},
		IdempotencyKey:         key,
	}
}

func newKey() actions.IdempotencyKey { return actions.IdempotencyKey(ids.New("idem")) }

// newLoopbackDialer allows 127.0.0.1 on the given ports, over http.
func newLoopbackDialer(t testing.TB, ports ...int) *egress.Dialer {
	t.Helper()
	allow, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: ports, Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	return egress.NewDialer(allow)
}
