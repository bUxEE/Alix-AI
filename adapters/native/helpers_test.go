package native

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
)

const (
	testApp    = "app_demo"
	testEP     = "fixture-bridge"
	testKeyID  = "k1"
	testSize   = 64 << 10
	testSecret = "fixture-application-key-0123456789-abcdef"
)

var testKey = []byte(testSecret)

// The method names the fixture package maps for fake.App.
var fixtureMethods = []struct {
	method string
	op     adapters.Operation
	cap    string
}{
	{"items_get", adapters.OpRead, fake.ReadCapability},
	{"items_create_validate", adapters.OpValidate, fake.ActionCapability},
	{"items_create_prepare", adapters.OpPrepare, fake.ActionCapability},
	{"items_create_commit", adapters.OpCommit, fake.ActionCapability},
	{"items_create_status", adapters.OpStatus, fake.ActionCapability},
	{"items_create_cancel", adapters.OpCancel, fake.ActionCapability},
}

func fixtureCapabilities() []Capability {
	byCap := map[string]*Capability{}
	var out []*Capability
	for _, m := range fixtureMethods {
		c := byCap[m.cap]
		if c == nil {
			c = &Capability{ID: m.cap, Operations: map[adapters.Operation]Binding{}}
			byCap[m.cap] = c
			out = append(out, c)
		}
		b := Binding{Endpoint: testEP, Method: m.method}
		if m.op == adapters.OpRead {
			b.Arguments = map[string]string{"item_id": "item_id"}
		}
		c.Operations[m.op] = b
	}
	caps := make([]Capability, 0, len(out))
	for _, c := range out {
		caps = append(caps, *c)
	}
	return caps
}

func shortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("", "nx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// wireAddr picks an address for the wire: a socket path in a short private
// directory (sun_path is about 100 bytes, which a test name can exceed), or the
// loopback port 0 of the TCP fallback.
func wireAddr(t testing.TB, network string) string {
	t.Helper()
	if network == NetworkUnix {
		return filepath.Join(shortDir(t), "run", "b.sock")
	}
	return "127.0.0.1:0"
}

// listen opens the listener of a wire the way a bridge does.
func listen(t testing.TB, network, addr string) (net.Listener, error) {
	t.Helper()
	if network == NetworkUnix {
		return bridge.ListenUnix(addr)
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(p)
	return bridge.ListenLoopback(port)
}

// dialerFor allows the one loopback destination of a TCP wire: the egress dialer
// refuses loopback unless it is listed.
func dialerFor(t testing.TB, network, addr string) *egress.Dialer {
	t.Helper()
	if network != NetworkTCP {
		return nil
	}
	host, port, err := loopbackAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	allow, err := egress.NewAllowlist(egress.Destination{Host: host, Ports: []int{port}, Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	return egress.NewDialer(allow)
}

func fixtureConfig(t testing.TB, network, addr string) Config {
	t.Helper()
	return Config{
		ApplicationID: testApp,
		Endpoints:     []Endpoint{{ID: testEP, Network: network, Address: addr, KeyID: testKeyID, Key: NewSecret(testKey)}},
		Capabilities:  fixtureCapabilities(),
		Dialer:        dialerFor(t, network, addr),

		MaxResponseBytes: testSize,
	}
}

func newAdapter(t testing.TB, cfg Config) *Adapter {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func wrapped(t testing.TB, a *Adapter) adapters.AppAdapter {
	t.Helper()
	w, err := adapters.Wrap(a, adapters.Options{ApplicationID: testApp})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

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

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ---- a host speaking the protocol at wire level, over fake.App ---------------

// host is a bridge written for the tests: the server half of the protocol in
// about a hundred lines, with fake.App behind it. Unlike pkg/bridge it can put
// bytes on the wire that no correct bridge would (a truncated reply, prose, an
// oversize frame, a dropped connection), which is what the conformance suite
// needs of a host.
type host struct {
	t       testing.TB
	app     *fake.App
	network string
	addr    string
	key     []byte
	limit   int
	// script, when set, answers a request instead of fake.App; it reports whether it did.
	script func(c net.Conn, ks []byte, id int, method string) bool

	mu    sync.Mutex
	l     net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func startHost(t testing.TB, network string, app *fake.App) *host {
	t.Helper()
	h := &host{t: t, app: app, network: network, addr: wireAddr(t, network), key: testKey, limit: 1 << 20, conns: map[net.Conn]struct{}{}}
	h.up()
	t.Cleanup(h.down)
	return h
}

// up starts listening (again, after down) on the same address.
func (h *host) up() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.l != nil {
		return
	}
	l, err := listen(h.t, h.network, h.addr)
	if err != nil {
		h.t.Fatalf("host cannot listen: %v", err)
	}
	h.addr = l.Addr().String()
	if h.network == NetworkUnix {
		h.addr = l.Addr().String()
	}
	h.l = l
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conns[c] = struct{}{}
			h.mu.Unlock()
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				defer func() {
					h.mu.Lock()
					delete(h.conns, c)
					h.mu.Unlock()
					_ = c.Close()
				}()
				h.serve(c)
			}()
		}
	}()
}

// down stops listening and drops every connection: the host is gone, and a
// connection attempt is refused.
func (h *host) down() {
	h.mu.Lock()
	l := h.l
	h.l = nil
	conns := make([]net.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	h.wg.Wait()
}

type wireParams struct {
	Identity       adapters.Identity       `json:"identity"`
	Fields         map[string]any          `json:"fields"`
	IdempotencyKey string                  `json:"idempotency_key"`
	Prepared       *actions.PreparedAction `json:"prepared"`
	Aspects        []adapters.Aspect       `json:"aspects"`
}

func rawFrame(c net.Conn, payload []byte) error {
	var hdr [4]byte
	n := len(payload)
	hdr[0], hdr[1], hdr[2], hdr[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	_, err := c.Write(append(hdr[:], payload...))
	return err
}

// envelope puts a result, valid JSON or not, in a JSON-RPC response by concatenation,
// so that a host can send bytes no correct bridge would.
func envelope(id int, result []byte, echoed string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"jsonrpc":"2.0","id":%d,"result":`, id)
	b.Write(result)
	if echoed != "" {
		fmt.Fprintf(&b, `,"meta":{"idempotency_key":%q}`, echoed)
	}
	b.WriteString("}")
	return b.Bytes()
}

// respond sends a handshake response, which is not sealed yet.
func (h *host) respond(c net.Conn, id int, result []byte, echoed string) error {
	return rawFrame(c, envelope(id, result, echoed))
}

// respondSealed sends a reply sealed under the session key, whatever its size.
func (h *host) respondSealed(c net.Conn, ks []byte, id int, result []byte, echoed string) error {
	return writeSealed(c, ks, labelReply, envelope(id, result, echoed), 16<<20)
}

// serve is one connection: the handshake, then one request.
func (h *host) serve(c net.Conn) {
	type req struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	read := func(limit int) (req, bool) {
		b, err := readFrame(c, limit)
		var r req
		return r, err == nil && json.Unmarshal(b, &r) == nil
	}
	hr, ok := read(4096)
	var hello helloParams
	if !ok || hr.Method != methodHello || json.Unmarshal(hr.Params, &hello) != nil || hello.KeyID != testKeyID {
		return
	}
	cn, _ := b64.DecodeString(hello.Nonce)
	sn := make([]byte, nonceLen)
	_, _ = rand.Read(sn)
	t := transcript(hello.ApplicationID, hello.EndpointID, hello.KeyID, cn, sn)
	res, _ := json.Marshal(helloResult{Version: 1, Nonce: b64.EncodeToString(sn), Proof: b64.EncodeToString(proof(h.key, labelServer, t...))})
	if h.respond(c, hr.ID, res, "") != nil {
		return
	}
	ar, ok := read(4096)
	var auth struct{ Proof string }
	_ = json.Unmarshal(ar.Params, &auth)
	if got, _ := b64.DecodeString(auth.Proof); !ok || ar.Method != methodAuth || !bytes.Equal(got, proof(h.key, labelClient, t...)) {
		_ = rawFrame(c, []byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32001,"message":"authentication failed"}}`))
		return
	}
	ks := proof(h.key, labelSession, t...)
	if h.respondSealed(c, ks, ar.ID, []byte(`{"authenticated":true}`), "") != nil {
		return
	}

	b, err := readSealed(c, ks, labelRequest, h.limit)
	var rr req
	if err != nil || json.Unmarshal(b, &rr) != nil {
		return
	}
	if h.script != nil && h.script(c, ks, rr.ID, rr.Method) {
		return
	}
	var p wireParams
	_ = json.Unmarshal(rr.Params, &p)
	fr := fake.Request{Identity: p.Identity, Fields: p.Fields, Prepared: p.Prepared, IdempotencyKey: p.IdempotencyKey, Aspects: p.Aspects}
	switch rr.Method {
	case methodDescribe:
		fr.Operation = adapters.OpDescribe
	case methodHealth:
		fr.Operation = adapters.OpHealth
	case methodInspect:
		fr.Operation = adapters.OpInspect
	default:
		found := false
		for _, m := range fixtureMethods {
			if m.method == rr.Method {
				fr.Operation, fr.CapabilityID, found = m.op, m.cap, true
			}
		}
		if !found {
			_ = writeSealed(c, ks, labelReply, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"method not found"}}`, rr.ID)), 1<<16)
			return
		}
	}
	rep := h.app.Handle(context.Background(), fr)
	switch rep.Action {
	case fake.Hang:
		_, _ = io.Copy(io.Discard, c) // until the adapter gives up or the host goes down
	case fake.Deliver:
		_ = h.respondSealed(c, ks, rr.ID, rep.Body, rep.EchoedKey)
	}
	// Drop and Refuse: the connection closes without a reply.
}

// nativeFixture is the host application behind the adapter under test: fake.App,
// with the wire taken down and brought back with it, so that "the host is down"
// means that the connection is refused before anything is sent.
type nativeFixture struct {
	*fake.App
	host *host
}

func (f *nativeFixture) SetAvailable(up bool) {
	f.App.SetAvailable(up)
	if up {
		f.host.up()
	} else {
		f.host.down()
	}
}

var _ adaptersuite.Fixture = (*nativeFixture)(nil)

// subject builds the adapter the way production wires one (inside adapters.Wrap)
// over fake.App behind the wire-level host, on one of the two wires.
func subject(t *testing.T, network string) adaptersuite.Subject {
	t.Helper()
	app := fake.NewApp(fake.WithOversizeBytes(4 * testSize))
	h := startHost(t, network, app)
	a := newAdapter(t, fixtureConfig(t, network, h.addr))
	return adaptersuite.Subject{Adapter: wrapped(t, a), Fixture: &nativeFixture{App: app, host: h}, ApplicationID: testApp, MaxResponseBytes: testSize}
}
