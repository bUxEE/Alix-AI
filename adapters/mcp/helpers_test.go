package mcp

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	testApp  = "app_demo"
	testEP   = "fake-mcp"
	testSize = 64 << 10 // the reply size limit of the conformance subject
)

// TestMain lets the test binary act as the stdio fixture server: the tests that
// need a real subprocess start os.Args[0] with ALIXAI_MCP_FIXTURE=stdio.
func TestMain(m *testing.M) {
	if os.Getenv("ALIXAI_MCP_FIXTURE") == "stdio" {
		runStdioFixture()
		return
	}
	os.Exit(m.Run())
}

// fakeMapping maps the two capabilities of fake.App to the fixture's tools.
func fakeMapping(ep string, withCancel bool) []Capability {
	b := func(tool string) Binding { return Binding{Endpoint: ep, Tool: tool} }
	ops := map[adapters.Operation]Binding{
		adapters.OpValidate: b(fixture.ToolValidate),
		adapters.OpPrepare:  b(fixture.ToolPrepare),
		adapters.OpCommit:   b(fixture.ToolCommit),
		adapters.OpStatus:   b(fixture.ToolStatus),
	}
	if withCancel {
		ops[adapters.OpCancel] = b(fixture.ToolCancel)
	}
	return []Capability{
		{ID: fake.ReadCapability, Operations: map[adapters.Operation]Binding{adapters.OpRead: b(fixture.ToolGet)}},
		{ID: fake.ActionCapability, Operations: ops},
	}
}

// loopbackDialer allows exactly the host and port of an http URL.
func loopbackDialer(t testing.TB, rawURL string, opts ...egress.Option) *egress.Dialer {
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
	return egress.NewDialer(al, opts...)
}

// httpConfig is a Config for one HTTP endpoint serving the fake mapping.
func httpConfig(t testing.TB, h *fixture.HTTPServer) Config {
	t.Helper()
	return Config{
		ApplicationID:    testApp,
		Endpoints:        []Endpoint{{ID: testEP, URL: h.URL}},
		Capabilities:     fakeMapping(testEP, true),
		Dialer:           loopbackDialer(t, h.URL),
		MaxResponseBytes: testSize,
		SurfaceRecheck:   -1, // check the tools before every call, so tests never wait on a timer
	}
}

// newHTTPAdapter starts a fixture over fake.App and an adapter for it. mutate
// edits the Config before the adapter is built.
func newHTTPAdapter(t *testing.T, mutate ...func(*Config)) (*Adapter, *fixture.HTTPServer, *fake.App) {
	t.Helper()
	app := fake.NewApp(fake.WithOversizeBytes(4 * testSize))
	h := fixture.StartHTTP(t, fixture.New(app))
	cfg := httpConfig(t, h)
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, h, app
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: testApp, TenantID: "ten_a", WorkspaceID: "wks_a",
		PrincipalID: "usr_alice", Roles: []string{"member"},
		CapabilityScopes: []string{fake.ReadCapability, fake.ActionCapability}, SessionID: "ses_alice",
	}
}

func wrapped(t testing.TB, a *Adapter) adapters.AppAdapter {
	t.Helper()
	w, err := adapters.Wrap(a, adapters.Options{ApplicationID: testApp})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testContext(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var fakeClock = func() *clock.Fake { return clock.NewFake(time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)) }

// pipeConnect is a transport factory that serves srv over an in-process pipe, so
// the stdio framing can be tested without a process.
func pipeConnect(srv *fixture.Server, refuse func() bool) func(context.Context, *endpointConn) (sdkTransport, error) {
	return func(_ context.Context, ep *endpointConn) (sdkTransport, error) {
		if refuse != nil && refuse() {
			return nil, &net.OpError{Op: "dial", Net: "pipe", Err: os.ErrNotExist}
		}
		c1, c2 := net.Pipe()
		go srv.ServeStream(c2)
		return &mcp.IOTransport{Reader: &lineLimitReader{rc: c1, limit: transportCap(ep.a.cfg.MaxResponseBytes), trips: &ep.trips}, Writer: c1}, nil
	}
}
