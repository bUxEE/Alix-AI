package native

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/bridge"
)

// realBridge is the Go bridge (pkg/bridge) with fake.App behind it: the other
// half of the protocol, as a host application embeds it. handled counts the
// calls that reached a handler.
type realBridge struct {
	srv     *bridge.Server
	app     *fake.App
	network string
	addr    string
	handled atomic.Int32
}

func appHandler(b *realBridge, op adapters.Operation, capID string) bridge.Handler {
	return func(ctx context.Context, c bridge.Call) (bridge.Reply, error) {
		b.handled.Add(1)
		rep := b.app.Handle(ctx, fake.Request{
			Operation: op,
			Identity: adapters.Identity{
				ApplicationID: c.Identity.ApplicationID, TenantID: c.Identity.TenantID, WorkspaceID: c.Identity.WorkspaceID,
				PrincipalID: c.Identity.PrincipalID, Roles: c.Identity.Roles, SessionID: c.Identity.SessionID,
			},
			CapabilityID: capID, Fields: c.Fields, Prepared: c.Prepared, IdempotencyKey: c.IdempotencyKey,
		})
		if rep.Action != fake.Deliver {
			return bridge.Reply{}, bridge.ErrDropConnection
		}
		return bridge.Reply{Result: json.RawMessage(rep.Body), IdempotencyKey: rep.EchoedKey}, nil
	}
}

func startBridge(t *testing.T, network string, mutate ...func(*bridge.Config)) *realBridge {
	t.Helper()
	b := &realBridge{app: fake.NewApp(), network: network}
	cfg := bridge.Config{
		ApplicationID: testApp, EndpointID: testEP, Keys: map[string][]byte{testKeyID: testKey}, ApplicationVersion: "1.0.0",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := bridge.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range fixtureMethods {
		if err := srv.Handle(m.method, appHandler(b, m.op, m.cap)); err != nil {
			t.Fatal(err)
		}
	}
	// A method the host exposes that no capability of the package maps.
	if err := srv.Handle("admin_dump", func(context.Context, bridge.Call) (bridge.Reply, error) {
		b.handled.Add(1)
		return bridge.Reply{Result: map[string]any{"status": "ok"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var seeded sync.Map
	if err := srv.HandleManagement("seed", func(_ context.Context, args map[string]any) (any, error) {
		seeded.Store("last", args["name"])
		b.app.Seed("ten_a", "wks_a", "item_1", "Widget")
		return map[string]any{"seeded": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	addr := wireAddr(t, network)
	l, err := listen(t, network, addr)
	if err != nil {
		t.Fatal(err)
	}
	b.srv, b.addr = srv, l.Addr().String()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); wg.Wait() })
	return b
}

// rawConn dials a bridge without the adapter.
func (b *realBridge) rawConn(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial(b.network, b.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
