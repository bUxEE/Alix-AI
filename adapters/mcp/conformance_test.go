package mcp

import (
	"os"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
)

// dropSession closes the endpoint's current session as a lost connection would.
func (a *Adapter) dropSession() {
	for _, e := range a.eps {
		e.mu.Lock()
		s := e.sess
		e.sess = nil
		e.mu.Unlock()
		if s != nil {
			s.force()
		}
	}
}

// closeIdleConnections drops the HTTP connections kept for reuse, which is what
// a client that noticed a server going away would have done.
func (a *Adapter) closeIdleConnections() {
	for _, e := range a.eps {
		if e.http != nil {
			e.http.CloseIdleConnections()
		}
	}
}

// mcpFixture is the host application behind the adapter under test: fake.App,
// with the wire taken down and brought back with it, so that "the host is down"
// means that the connection is refused before anything is sent.
type mcpFixture struct {
	*fake.App
	adapter *Adapter
	http    *fixture.HTTPServer
	t       testing.TB
	down    atomic.Bool
}

func (f *mcpFixture) SetAvailable(up bool) {
	f.App.SetAvailable(up)
	f.down.Store(!up)
	switch {
	case f.http != nil && !up:
		f.http.Down()
		f.adapter.closeIdleConnections()
	case f.http != nil:
		f.http.Up(f.t)
	case !up:
		f.adapter.dropSession()
	}
}

var _ adaptersuite.Fixture = (*mcpFixture)(nil)

// subject builds the adapter the way production wires one (inside adapters.Wrap)
// over fake.App behind the fixture MCP server, on one of the two wires.
func subject(t *testing.T, wire string) adaptersuite.Subject {
	t.Helper()
	app := fake.NewApp(fake.WithOversizeBytes(4 * testSize))
	srv := fixture.New(app)
	fx := &mcpFixture{App: app, t: t}
	var cfg Config
	switch wire {
	case "http":
		fx.http = fixture.StartHTTP(t, srv)
		cfg = httpConfig(t, fx.http)
	case "stream":
		cfg = Config{
			ApplicationID:    testApp,
			Endpoints:        []Endpoint{{ID: testEP, Command: &Command{Path: os.Args[0]}}},
			Capabilities:     fakeMapping(testEP, true),
			MaxResponseBytes: testSize,
			SurfaceRecheck:   -1,
			connect:          pipeConnect(srv, fx.down.Load),
		}
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	fx.adapter = a
	guarded, err := adapters.Wrap(a, adapters.Options{ApplicationID: testApp})
	if err != nil {
		t.Fatal(err)
	}
	return adaptersuite.Subject{Adapter: guarded, Fixture: fx, ApplicationID: testApp, MaxResponseBytes: testSize}
}

// TestConformanceMCPAdapter runs the shared adapter conformance suite
// (tests/conformance/adapters) against the MCP adapter, over streamable HTTP and
// over a newline-delimited JSON stream (the framing of stdio), with fake.App
// behind a fixture MCP server. The suite records the identity the host received
// on every read, prepare and commit, denies a commit and checks that nothing is
// retried around the denial, and feeds hung, oversize, malformed and forged
// results to the adapter.
//
// SEC-044
// SEC-050
// SEC-051
func TestConformanceMCPAdapter(t *testing.T) {
	for _, wire := range []string{"http", "stream"} {
		t.Run(wire, func(t *testing.T) {
			adaptersuite.RunAdapterSuite(t, func(t *testing.T) adaptersuite.Subject { return subject(t, wire) })
		})
	}
}
