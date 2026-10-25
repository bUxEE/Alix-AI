package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	testApp   = "app_ledger"
	testEP    = "ledger-bridge"
	testKeyID = "k1"
)

var testKey = bytes.Repeat([]byte{0x5a}, 40)

func testIdentity() Identity {
	return Identity{ApplicationID: testApp, TenantID: "ten_a", WorkspaceID: "wks_a", PrincipalID: "usr_alice", Roles: []string{"member"}, SessionID: "ses_alice"}
}

// harness is a bridge serving two declared operations and one management
// operation on a loopback port, and how many times each handler ran.
type harness struct {
	srv   *Server
	addr  string
	reads atomic.Int32
	seen  chan Call
}

func startHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{seen: make(chan Call, 16)}
	cfg := Config{ApplicationID: testApp, EndpointID: testEP, Keys: map[string][]byte{testKeyID: testKey}, ApplicationVersion: "1.2.3", HandshakeTimeout: 2 * time.Second}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	read := func(_ context.Context, c Call) (Reply, error) {
		h.reads.Add(1)
		select {
		case h.seen <- c:
		default:
		}
		return Reply{Result: map[string]any{"status": "ok"}, IdempotencyKey: c.IdempotencyKey}, nil
	}
	for _, m := range []string{"items_get", "items_create"} {
		if err := srv.Handle(m, read); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.HandleManagement("seed", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"seeded": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	l, err := ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.addr = srv, l.Addr().String()
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return h
}

var errAuth = &authError{}

type authError struct{}

func (*authError) Error() string { return "authentication failed" }

func sendRequest(c net.Conn, id int, method string, params any) error {
	p, _ := json.Marshal(params)
	b, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: p})
	return writeFrame(c, b, 1<<20)
}

func sendSealed(c net.Conn, ks []byte, id int, method string, params any) error {
	p, _ := json.Marshal(params)
	b, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: p})
	return writeSealed(c, ks, labelRequest, b, 1<<20)
}

func recvSealed(c net.Conn, ks []byte) (rpcResponse, error) {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := readSealed(c, ks, labelReply, 1<<20)
	if err != nil {
		return rpcResponse{}, err
	}
	var r rpcResponse
	return r, json.Unmarshal(b, &r)
}

func recvResponse(c net.Conn) (rpcResponse, error) {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := readFrame(c, 1<<20)
	if err != nil {
		return rpcResponse{}, err
	}
	var r rpcResponse
	return r, json.Unmarshal(b, &r)
}

// clientHandshake is written against the protocol and nothing of the bridge's
// own code but the framing: the adapter has the real client.
func clientHandshake(c net.Conn, app, ep, keyID string, key []byte) ([]byte, error) {
	cn := make([]byte, nonceLen)
	_, _ = rand.Read(cn)
	if err := sendRequest(c, 1, methodHello, helloParams{Version: 1, ApplicationID: app, EndpointID: ep, KeyID: keyID, Nonce: b64.EncodeToString(cn)}); err != nil {
		return nil, err
	}
	resp, err := recvResponse(c)
	if err != nil || resp.Error != nil {
		return nil, errAuth
	}
	var hello struct{ Nonce, Proof string }
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		return nil, err
	}
	sn, _ := b64.DecodeString(hello.Nonce)
	t := transcript(app, ep, keyID, cn, sn)
	if got, _ := b64.DecodeString(hello.Proof); !bytes.Equal(got, proof(key, labelServer, t...)) {
		return nil, errAuth // the bridge is not the application
	}
	if err := sendRequest(c, 2, methodAuth, authParams{Proof: b64.EncodeToString(proof(key, labelClient, t...))}); err != nil {
		return nil, err
	}
	ks := sessionKey(key, t)
	if resp, err = recvSealed(c, ks); err != nil || resp.Error != nil {
		return nil, errAuth
	}
	return ks, nil
}

// callOnce opens an authenticated connection, sends one call and returns the answer.
func (h *harness) callOnce(t *testing.T, method string, params any) rpcResponse {
	t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ks, err := clientHandshake(c, testApp, testEP, testKeyID, testKey)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := sendSealed(c, ks, 3, method, params); err != nil {
		t.Fatal(err)
	}
	r, err := recvSealed(c, ks)
	if err != nil {
		t.Fatalf("%q: %v", method, err)
	}
	return r
}

func errCode(r rpcResponse) int {
	if r.Error == nil {
		return 0
	}
	return r.Error.Code
}

// SEC-038: the host exposes exactly the operations it declared, to a peer that
// proved it holds the application key, and nothing else: not an undeclared
// method, not a spelling variant of a declared one, not a reserved session
// method after the handshake, not an undeclared management operation, and not a
// request bound to another application.
func TestBridgeServesDeclaredOperationsOnly(t *testing.T) {
	h := startHarness(t)
	args := map[string]any{"identity": testIdentity(), "fields": map[string]any{"item_id": "item_1"}}

	r := h.callOnce(t, "items_get", args)
	if r.Error != nil || !json.Valid(r.Result) {
		t.Fatalf("a declared operation failed: %+v", r)
	}
	got := <-h.seen
	if got.Method != "items_get" || got.Fields["item_id"] != "item_1" || got.Identity.PrincipalID != "usr_alice" || got.Identity.TenantID != "ten_a" {
		t.Errorf("the handler saw %+v", got)
	}
	r = h.callOnce(t, "items_create", map[string]any{"identity": testIdentity(), "idempotency_key": "idem_0123456789abcdef"})
	if r.Error != nil || r.Meta == nil || r.Meta.IdempotencyKey != "idem_0123456789abcdef" {
		t.Errorf("the idempotency key is not echoed: %+v", r)
	}
	before := h.reads.Load()

	for _, method := range []string{
		"items_delete", "ITEMS_GET", "items_get ", " items_get", "items-get", "items_get\x00", "rpc.discover", "items_",
		"session.hello", "session.auth", "alixai.shell", "alixai.", "",
	} {
		if r := h.callOnce(t, method, args); errCode(r) != codeMethodNotFound && errCode(r) != codeInvalidRequest {
			t.Errorf("method %q: %+v, want method not found", method, r)
		}
	}
	if h.reads.Load() != before {
		t.Fatalf("an undeclared method reached a handler (%d calls)", h.reads.Load()-before)
	}

	// The list of operations is the declared set and no more.
	r = h.callOnce(t, methodDescribe, map[string]any{})
	var d struct {
		Versions []int    `json:"contract_versions"`
		Methods  []string `json:"methods"`
		Version  string   `json:"application_version"`
	}
	if err := json.Unmarshal(r.Result, &d); err != nil || !slices.Equal(d.Methods, []string{"items_create", "items_get"}) || d.Version != "1.2.3" || !slices.Equal(d.Versions, []int{1}) {
		t.Errorf("describe = %s (%v)", r.Result, err)
	}

	// Management is a separate, closed set.
	if r := h.callOnce(t, methodManage, map[string]any{"name": "seed", "args": map[string]any{}}); r.Error != nil {
		t.Errorf("a declared management operation failed: %+v", r)
	}
	for _, name := range []string{"drop_database", "Seed", "items_get"} {
		if r := h.callOnce(t, methodManage, map[string]any{"name": name}); errCode(r) != codeMethodNotFound {
			t.Errorf("management %q: %+v, want method not found", name, r)
		}
	}

	// A request for another application, and one missing a level of the context,
	// never reach the host.
	other := testIdentity()
	other.ApplicationID = "app_other"
	noTenant := testIdentity()
	noTenant.TenantID = ""
	wild := testIdentity()
	wild.PrincipalID = "*"
	for name, id := range map[string]Identity{"another application": other, "no tenant": noTenant, "wildcard principal": wild} {
		if r := h.callOnce(t, "items_get", map[string]any{"identity": id}); errCode(r) != codeInvalidParams {
			t.Errorf("%s: %+v, want invalid params", name, r)
		}
	}
	if r := h.callOnce(t, "items_get", map[string]any{"identity": testIdentity(), "extra": 1}); errCode(r) != codeInvalidParams {
		t.Errorf("an unknown param member: %+v, want invalid params", r)
	}
	if h.reads.Load() != before {
		t.Fatalf("a request that is not valid reached a handler")
	}
}

// A declaration is a programming act: bad, reserved and repeated names, and
// weak configuration, are errors.
func TestBridgeDeclarationErrors(t *testing.T) {
	srv, err := New(Config{ApplicationID: testApp, Keys: map[string][]byte{testKeyID: testKey}})
	if err != nil {
		t.Fatal(err)
	}
	h := func(context.Context, Call) (Reply, error) { return Reply{}, nil }
	for _, name := range []string{"", "1abc", "has space", "session.x", "alixai.describe", "alixai.manage", "a/b"} {
		if err := srv.Handle(name, h); err == nil {
			t.Errorf("Handle(%q) succeeded", name)
		}
	}
	if err := srv.Handle("ok", h); err != nil {
		t.Fatal(err)
	}
	if err := srv.Handle("ok", h); err == nil {
		t.Error("a method declared twice")
	}
	if err := srv.Handle("late", nil); err == nil {
		t.Error("a nil handler")
	}
	for _, c := range []Config{
		{ApplicationID: "", Keys: map[string][]byte{"k": testKey}},
		{ApplicationID: testApp},
		{ApplicationID: testApp, Keys: map[string][]byte{"k": []byte("short")}},
		{ApplicationID: testApp, Keys: map[string][]byte{"bad id": testKey}},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v) succeeded", c)
		}
	}
}

// Nobody reaches a handler without the application key, whatever they send first.
func TestBridgeRefusesPeersWithoutTheKey(t *testing.T) {
	h := startHarness(t)

	raw := func(frame []byte) rpcResponse {
		c, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := writeFrame(c, frame, 1<<20); err != nil {
			t.Fatal(err)
		}
		r, _ := recvResponse(c)
		return r
	}
	p, _ := json.Marshal(map[string]any{"identity": testIdentity()})
	call, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "items_get", Params: p})
	if r := raw(call); errCode(r) != codeAuthFailed {
		t.Errorf("a call with no handshake: %+v", r)
	}
	if r := raw([]byte("not json")); errCode(r) != codeParse {
		t.Errorf("garbage: %+v", r)
	}

	for name, try := range map[string]func(net.Conn) error{
		"wrong key": func(c net.Conn) error {
			_, err := clientHandshake(c, testApp, testEP, testKeyID, bytes.Repeat([]byte{1}, 40))
			return err
		},
		"unknown key id": func(c net.Conn) error {
			_, err := clientHandshake(c, testApp, testEP, "k2", testKey)
			return err
		},
		"other app": func(c net.Conn) error {
			_, err := clientHandshake(c, "app_other", testEP, testKeyID, testKey)
			return err
		},
		"other endpoint": func(c net.Conn) error {
			_, err := clientHandshake(c, testApp, "other-bridge", testKeyID, testKey)
			return err
		},
	} {
		c, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := try(c); err == nil {
			t.Errorf("%s: the handshake succeeded", name)
		}
		_ = c.Close()
	}
	if h.reads.Load() != 0 {
		t.Fatalf("a handler ran for a peer that was not authenticated")
	}
}

// A frame above the limit is refused from its length, before its body is read,
// and its call never reaches a handler.
func TestBridgeFrameLimit(t *testing.T) {
	h := startHarness(t, func(c *Config) { c.MaxRequestBytes = 512 })
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ks, err := clientHandshake(c, testApp, testEP, testKeyID, testKey)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 512+macLen+1)
	if _, err := c.Write(hdr[:]); err != nil { // the header alone: the body is never sent
		t.Fatal(err)
	}
	r, err := recvSealed(c, ks)
	if err != nil || errCode(r) != codeInvalidRequest {
		t.Fatalf("an oversize frame: %+v, %v", r, err)
	}
	if h.reads.Load() != 0 {
		t.Fatal("a handler ran")
	}
}

// A handler's failure is reported without its text, and a hang-up cancels it.
func TestBridgeHandlerFailuresAndHangups(t *testing.T) {
	srv, err := New(Config{ApplicationID: testApp, Keys: map[string][]byte{testKeyID: testKey}, HandshakeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}), make(chan error, 1)
	_ = srv.Handle("boom", func(context.Context, Call) (Reply, error) {
		return Reply{}, os.ErrPermission // text the adapter must never see
	})
	_ = srv.Handle("hang", func(ctx context.Context, _ Call) (Reply, error) {
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return Reply{}, ctx.Err()
	})
	l, err := ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); wg.Wait() })
	h := &harness{addr: l.Addr().String()}

	// This server declares no endpoint, so any endpoint ID is accepted.
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := clientHandshake(c, testApp, testEP, testKeyID, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := sendSealed(c, ks, 3, "boom", map[string]any{"identity": testIdentity()}); err != nil {
		t.Fatal(err)
	}
	r, err := recvSealed(c, ks)
	_ = c.Close()
	if err != nil || errCode(r) != codeInternal || bytes.Contains([]byte(r.Error.Message), []byte("permission")) {
		t.Errorf("a handler error: %+v, %v", r, err)
	}

	c, err = net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	ks, err = clientHandshake(c, testApp, testEP, testKeyID, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := sendSealed(c, ks, 3, "hang", map[string]any{"identity": testIdentity()}); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = c.Close()
	select {
	case err := <-ended:
		if err == nil {
			t.Error("the handler's context ended without an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler was not cancelled when the peer hung up")
	}
}

// fdStarvedListener fails its first accepts the way a process that is out of
// file descriptors does (EMFILE), then accepts normally.
type fdStarvedListener struct {
	net.Listener
	fails atomic.Int32
}

func (l *fdStarvedListener) Accept() (net.Conn, error) {
	if l.fails.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Addr: l.Addr(), Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

// Running out of file descriptors for a moment does not stop the bridge for
// good: it backs off and accepts again.
func TestBridgeKeepsAcceptingAfterATemporaryError(t *testing.T) {
	srv, err := New(Config{ApplicationID: testApp, Keys: map[string][]byte{testKeyID: testKey}, HandshakeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	l, err := ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	fl := &fdStarvedListener{Listener: l}
	fl.fails.Store(3)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(fl) }()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := clientHandshake(c, testApp, testEP, testKeyID, testKey); err != nil {
		t.Fatalf("handshake after temporary accept errors: %v", err)
	}
	select {
	case err := <-served:
		t.Fatalf("Serve returned %v on a temporary accept error", err)
	default:
	}
	_ = srv.Close()
	if err := <-served; err != nil {
		t.Errorf("Serve after Close = %v, want nil", err)
	}
}

func TestReadFrameBounds(t *testing.T) {
	frame := func(n uint32, body []byte) *bytes.Reader {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], n)
		return bytes.NewReader(append(hdr[:], body...))
	}
	if b, err := readFrame(frame(3, []byte("abc")), 3); err != nil || string(b) != "abc" {
		t.Errorf("a frame at the limit: %q, %v", b, err)
	}
	if _, err := readFrame(frame(4, []byte("abcd")), 3); err != errFrameTooLarge {
		t.Errorf("a frame over the limit: %v", err)
	}
	if _, err := readFrame(frame(0, nil), 3); err != errFrameEmpty {
		t.Errorf("an empty frame: %v", err)
	}
	if _, err := readFrame(frame(3, []byte("ab")), 8); err == nil {
		t.Error("a truncated frame was accepted")
	}
	if _, err := readFrame(bytes.NewReader(nil), 8); err == nil {
		t.Error("no frame was accepted")
	}
	if err := writeFrame(&bytes.Buffer{}, make([]byte, 9), 8); err == nil {
		t.Error("an oversize frame was written")
	}
}

// SEC-038, SEC-081: the policy for a socket and its directory, per platform.
func TestSocketPermsPolicy(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		file, dir fs.FileMode
		checkFile bool
		ok        bool
	}{
		{"owner only", "linux", 0o600, 0o700, true, true},
		{"group readable dir is fine", "linux", 0o600, 0o750, true, true},
		{"group-accessible socket", "linux", 0o660, 0o700, true, false},
		{"world-accessible socket", "linux", 0o666, 0o700, true, false},
		{"world-writable dir", "linux", 0o600, 0o777, true, false},
		{"group-writable dir", "darwin", 0o600, 0o770, true, false},
		{"sticky shared dir", "linux", 0o600, 0o777 | fs.ModeSticky, true, true},
		{"socket not yet created", "linux", 0o666, 0o700, false, true},
		{"windows has no bits", "windows", 0o666, 0o777, true, true},
	}
	for _, tc := range tests {
		if err := socketPermsOK(tc.file, tc.dir, tc.goos, tc.checkFile); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "bx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// ListenUnix makes an owner-only socket in an owner-only directory, replaces a
// stale socket and never a file that is not one, and removes the socket on close.
func TestListenUnix(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	path := filepath.Join(dir, "b.sock")
	l, err := ListenUnix(path)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("socket mode %v, %v; want 0600", fi.Mode().Perm(), err)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("directory mode %v, %v; want 0700", fi.Mode().Perm(), err)
		}
	}
	if _, err := ListenUnix(path); err == nil {
		t.Error("a second listener took a live socket")
	}
	_ = l.Close()

	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUnix(path); err == nil {
		t.Error("a regular file was replaced by a socket")
	}
	if got, _ := os.ReadFile(path); string(got) != "data" {
		t.Error("the regular file was damaged")
	}
	if _, err := ListenUnix("relative.sock"); err == nil {
		t.Error("a relative path")
	}
	if _, err := ListenUnix(filepath.Join(dir, string(bytes.Repeat([]byte("x"), 120)))); err == nil {
		t.Error("a path longer than sun_path")
	}
}

// SEC-038: once the handshake is done every frame is sealed with the session key.
// A request that is not sealed, is sealed for the other direction or under
// another session, or was altered after it was sealed (a relay that holds no key
// rewriting the tenant) is dropped without a handler running or a reply.
func TestBridgeDropsRequestsThatAreNotSealedForTheSession(t *testing.T) {
	h := startHarness(t)
	args := map[string]any{"identity": testIdentity()}
	body := func(method string) []byte {
		p, _ := json.Marshal(args)
		b, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: method, Params: p})
		return b
	}
	for name, send := range map[string]func(c net.Conn, ks []byte) error{
		"an unsealed request": func(c net.Conn, _ []byte) error { return writeFrame(c, body("items_get"), 1<<20) },
		"a request sealed for the reply direction": func(c net.Conn, ks []byte) error {
			return writeSealed(c, ks, labelReply, body("items_get"), 1<<20)
		},
		"a request sealed under another session": func(c net.Conn, _ []byte) error {
			return writeSealed(c, bytes.Repeat([]byte{3}, 32), labelRequest, body("items_get"), 1<<20)
		},
		"a request altered after it was sealed": func(c net.Conn, ks []byte) error {
			var buf bytes.Buffer
			_ = writeSealed(&buf, ks, labelRequest, body("items_get"), 1<<20)
			b := buf.Bytes()
			b[4+bytes.Index(b[4:], []byte("ten_a"))+4] = 'b' // ten_a -> ten_b, the length unchanged
			_, err := c.Write(b)
			return err
		},
	} {
		c, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatal(err)
		}
		ks, err := clientHandshake(c, testApp, testEP, testKeyID, testKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := send(c, ks); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r, err := recvSealed(c, ks); err == nil {
			t.Errorf("%s was answered: %+v", name, r)
		}
		_ = c.Close()
	}
	if h.reads.Load() != 0 {
		t.Fatalf("a handler ran for a frame that was not sealed for the session")
	}
}
