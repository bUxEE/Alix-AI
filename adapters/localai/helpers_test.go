package localai

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// suiteLog records every request any test server in this package received, so
// TestMain can assert that the whole suite stayed on the endpoint allowlist
// (SEC-067). Every httptest server in this package is created through
// newRuntime / newTLSRuntime / newUnixRuntime in this file;
// TestAllTestServersAreRecorded enforces that.
var suiteLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func recordSuiteRequest(r *http.Request) {
	suiteLog.mu.Lock()
	defer suiteLog.mu.Unlock()
	if suiteLog.seen == nil {
		suiteLog.seen = map[string]int{}
	}
	suiteLog.seen[r.Method+" "+r.URL.Path]++
}

func suiteViolations() (bad []string, total int) {
	suiteLog.mu.Lock()
	defer suiteLog.mu.Unlock()
	allowed := map[string]bool{}
	for _, e := range AllowedEndpoints() {
		allowed[e] = true
	}
	for k, n := range suiteLog.seen {
		total += n
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad, total
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bad, _ := suiteViolations(); code == 0 && len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: the suite sent requests to paths outside the allowlist: %v\n", bad)
		code = 1
	}
	os.Exit(code)
}

// newTestKey returns a fresh random API key for one test. It is a TEST-ONLY
// value: generated per run, never a real credential, never committed.
func newTestKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "TESTONLY-" + hex.EncodeToString(b)
}

type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// stubServer is a recording stand-in for LocalAI.
type stubServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func (rt *stubServer) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		recordSuiteRequest(r)
		rt.mu.Lock()
		rt.reqs = append(rt.reqs, recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		rt.mu.Unlock()
		h(w, r)
	}
}

func newRuntime(t testing.TB, h http.HandlerFunc) *stubServer {
	t.Helper()
	rt := &stubServer{}
	rt.Server = httptest.NewServer(rt.wrap(h))
	t.Cleanup(rt.Close)
	return rt
}

// requests returns a copy of what the server received so far.
func (rt *stubServer) requests() []recordedRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]recordedRequest(nil), rt.reqs...)
}

func (rt *stubServer) paths() []string {
	var out []string
	for _, r := range rt.requests() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func (rt *stubServer) only(t testing.TB) recordedRequest {
	t.Helper()
	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("server received %d requests, want 1: %v", len(reqs), rt.paths())
	}
	return reqs[0]
}

// testConfig is a valid configuration with every role configured.
func testConfig(endpoint string) Config {
	return Config{
		Endpoint: endpoint,
		Models:   ModelSet{Controller: "ctl-model", Router: "router-model", Embedder: "embed-model"},
		Profiles: map[string]ModelSet{"lite": {Controller: "ctl-lite"}},
	}
}

func (rt *stubServer) client(t testing.TB, mutate ...func(*Config)) (*Client, string) {
	t.Helper()
	cfg := testConfig(rt.URL)
	for _, m := range mutate {
		m(&cfg)
	}
	key := newTestKey(t)
	c, err := New(cfg, WithAPIKey(NewSecret(key)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, key
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_test",
		ApplicationID:    "app_test",
		TenantID:         "ten_test",
		WorkspaceID:      "wsp_test",
		PrincipalID:      "usr_test",
		Roles:            []string{"member"},
		CapabilityScopes: []string{"cap.read"},
		SessionID:        "ses_test",
	}
}

func chatReq(user string) providers.ChatRequest {
	return providers.ChatRequest{Messages: []providers.Message{
		{Role: providers.MsgSystem, Content: "You propose typed tasks.", Trusted: true},
		{Role: providers.MsgUser, Content: user},
	}}
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", code)
	}
	if !contracts.HasCode(err, code) {
		t.Fatalf("error code = %s (%v), want %s", contracts.CodeOf(err), err, code)
	}
}

var bg = context.Background()

func embedReq() providers.EmbedRequest { return providers.EmbedRequest{Inputs: []string{"a"}} }

// newUnixRuntime serves h on a unix socket. The socket lives in a short
// temporary directory (socket paths are length-limited) and the test is
// skipped where the platform cannot listen on one.
func newUnixRuntime(t testing.TB, h http.HandlerFunc) (*stubServer, string) {
	t.Helper()
	ln, sock := listenUnix(t)
	rt := &stubServer{}
	rt.Server = httptest.NewUnstartedServer(rt.wrap(h))
	rt.Listener.Close()
	rt.Listener = ln
	rt.Start()
	t.Cleanup(rt.Close)
	return rt, sock
}

func listenUnix(t testing.TB) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ai")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot listen on a unix socket here: %v", err)
	}
	return ln, sock
}

// unixEndpoint formats a socket path as a unix:// endpoint.
func unixEndpoint(sock string) string {
	p := filepath.ToSlash(sock)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/dir/x.sock -> /C:/dir/x.sock
	}
	return "unix://" + p
}

func decodeJSONBody(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// newTLSRuntime serves h over TLS, with a throwaway self-signed certificate
// for 127.0.0.1, ::1 and localhost, on a unix socket. A socket and not TCP
// because some developer machines run a TLS-intercepting security product
// that replaces the certificate of every loopback TCP connection, which
// would make a certificate-pinning test depend on the machine. The client
// reaches the socket through the unexported dial hook (see dialerTo), and
// still negotiates TLS for an https://127.0.0.1 endpoint.
// rt.Certificate() is the certificate a client must trust.
func newTLSRuntime(t testing.TB, h http.HandlerFunc) (*stubServer, string) {
	t.Helper()
	ln, sock := listenUnix(t)
	rt := &stubServer{}
	rt.Server = httptest.NewUnstartedServer(rt.wrap(h))
	rt.Listener.Close()
	rt.Listener = ln
	rt.TLS = &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
	rt.StartTLS()
	t.Cleanup(rt.Close)
	return rt, sock
}

// dialerTo returns the dial hook that connects every request to sock.
func dialerTo(sock string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
}

func selfSignedCert(t testing.TB) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "alixai-localai-test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
