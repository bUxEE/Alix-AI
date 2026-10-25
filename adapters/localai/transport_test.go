package localai

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-065
// AlixAI must not send prompts and the API key to a non-loopback
// runtime endpoint over plaintext, whether the endpoint comes from a file, an
// environment variable or a literal.
func TestRejectsNonLoopbackWithoutTLS(t *testing.T) {
	const apiKey = "sk-live-must-not-appear"
	refused := []string{
		"http://192.168.1.10:8080",
		"http://10.0.0.5",
		"http://172.16.0.9:8080",
		"http://169.254.169.254",
		"http://example.com:8080",
		"http://localai:8080",
		"http://localai.internal",
		"http://0.0.0.0:8080",
		"http://[::]:8080",
		"http://[fe80::1%25eth0]:8080",
		"http://[2001:db8::1]:8080",
		"http://localhost.evil.com:8080",
		"http://127.0.0.1.evil.com:8080",
		"http://evil.com#@127.0.0.1",
		"http://127.0.0.1@evil.com:8080",
		"http://user:" + apiKey + "@127.0.0.1:8080",
		"http://127.1:8080",
		"http://0177.0.0.1:8080",
		"http://224.0.0.1:8080",
		"ftp://127.0.0.1:8080",
		"127.0.0.1:8080",
		"http://",
		"https://",
		"http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080?x=1",
		"unix://somehost/run/x.sock",
		"unix:relative.sock",
		"unix://",
	}
	for _, ep := range refused {
		t.Run("refuse "+ep, func(t *testing.T) {
			cfg := Config{Endpoint: ep, Models: ModelSet{Controller: "m"}}
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate accepted %q (config check must refuse it)", ep)
			} else if strings.Contains(err.Error(), apiKey) {
				t.Errorf("error repeats a credential: %v", err)
			}
			c, err := New(cfg, WithAPIKey(NewSecret(apiKey)))
			if err == nil {
				t.Fatalf("New accepted %q", ep)
			}
			if c != nil {
				t.Error("New returned a client alongside an error")
			}
			wantCode(t, err, contracts.ErrValidationFailed)
			if strings.Contains(err.Error(), apiKey) {
				t.Errorf("error repeats a credential: %v", err)
			}
		})
	}

	accepted := []string{
		"http://127.0.0.1:8080",
		"http://127.0.0.1",
		"http://127.5.5.5:1234",
		"http://[::1]:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"http://localhost:8080",
		"http://LOCALHOST:8080",
		"http://localhost.:8080",
		"http://127.0.0.1:8080/",
		"https://localai.example.com:8443",
		"https://10.0.0.5:8443",
		"https://127.0.0.1:8443",
		"unix:///run/alixai/localai.sock",
	}
	for _, ep := range accepted {
		t.Run("accept "+ep, func(t *testing.T) {
			cfg := Config{Endpoint: ep, Models: ModelSet{Controller: "m"}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%q): %v", ep, err)
			}
			if _, err := New(cfg); err != nil {
				t.Fatalf("New(%q): %v", ep, err)
			}
		})
	}

	t.Run("an https endpoint stays https", func(t *testing.T) {
		spec, err := parseEndpoint("https://localai.example.com:8443")
		if err != nil || spec.scheme != "https" || spec.loopback {
			t.Fatalf("spec = %+v, err = %v", spec, err)
		}
	})
	t.Run("tls settings need an https endpoint", func(t *testing.T) {
		cfg := Config{Endpoint: "http://127.0.0.1:8080", TLS: TLSConfig{CAFile: "ca.pem"}, Models: ModelSet{Controller: "m"}}
		if cfg.Validate() == nil {
			t.Error("tls settings on a plaintext endpoint were accepted")
		}
		cfg = Config{Endpoint: "https://host:8443", TLS: TLSConfig{CertFile: "c.pem"}, Models: ModelSet{Controller: "m"}}
		if cfg.Validate() == nil {
			t.Error("a client certificate without its key was accepted")
		}
	})
	t.Run("the default endpoint is loopback", func(t *testing.T) {
		if err := (Config{Models: ModelSet{Controller: "m"}}).Validate(); err != nil {
			t.Fatal(err)
		}
		spec, _ := parseEndpoint(DefaultEndpoint)
		if !spec.loopback {
			t.Error("DefaultEndpoint is not loopback")
		}
	})
}

// A hostname such as localhost is trusted only if it really resolves to a
// loopback address when the connection is made.
func TestPlaintextDialRefusesNonLoopbackAddresses(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:80":          true,
		"127.9.9.9:80":          true,
		"[::1]:80":              true,
		"[::ffff:127.0.0.1]:80": true,
		"10.0.0.1:80":           false,
		"192.168.0.1:80":        false,
		"8.8.8.8:80":            false,
		"[2001:db8::1]:80":      false,
		"169.254.169.254:80":    false,
		"0.0.0.0:80":            false,
		"localhost:80":          false, // Control sees resolved addresses only
		"garbage":               false,
	} {
		err := loopbackOnly("tcp", addr, nil)
		if (err == nil) != ok {
			t.Errorf("loopbackOnly(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestTransportIgnoresProxyEnvironmentAndCompression(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	spec, _ := parseEndpoint("https://localai.example.com:8443")
	tr := newTransport(spec, nil)
	if tr.Proxy != nil {
		t.Error("the transport consults the proxy environment: prompts and the key could leave through it")
	}
	if !tr.DisableCompression {
		t.Error("compression must stay off so server-sent events are not buffered")
	}
}

// SEC-067
// SEC-069
// The client cannot reach administration, installation or
// model-management routes, and no test in this package ever did.
func TestOnlyAllowlistedEndpointsCalled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/version", serveJSON(200, `{"version":"v4.10.0 (7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672)"}`))
	mux.HandleFunc("/v1/models", serveJSON(200, `{"object":"list","data":[{"id":"ctl-model"},{"id":"router-model"},{"id":"embed-model"},{"id":"ctl-lite"}]}`))
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = decodeJSONBody(r, &probe)
		if probe.Stream {
			sseHandler(streamChunk("ok", ""), streamChunk("", "stop"), "[DONE]")(w, r)
			return
		}
		serveJSON(200, chatBody(`{"a":1}`, "stop"))(w, r)
	})
	mux.HandleFunc("/v1/embeddings", embedHandler(0))
	rt := newRuntime(t, mux.ServeHTTP)
	c, _ := rt.client(t)

	// Every public operation.
	if rep, err := c.Health(bg); err != nil || rep.Status != providers.HealthOK {
		t.Fatalf("Health = %+v, %v", rep, err)
	}
	if _, err := c.Version(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(bg); err != nil {
		t.Fatal(err)
	}
	for _, out := range []providers.OutputConstraint{
		{},
		providers.SchemaConstraint([]byte(`{"type":"object"}`)),
		providers.GrammarConstraint(`root ::= "ok"`),
	} {
		req := chatReq("hi")
		req.Output = out
		if _, err := c.Chat(bg, testScope(), req); err != nil {
			t.Fatalf("Chat(%q): %v", out.Kind(), err)
		}
	}
	s, err := c.ChatStream(bg, testScope(), chatReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := drain(t, s); err != io.EOF {
		t.Fatalf("stream: %v", err)
	}
	if _, err := c.Embed(bg, testScope(), embedReq()); err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{}
	for _, e := range AllowedEndpoints() {
		allowed[e] = true
	}
	got := map[string]bool{}
	for _, p := range rt.paths() {
		got[p] = true
		if !allowed[p] {
			t.Errorf("the client called %q, which is not on the allowlist", p)
		}
	}
	for e := range allowed {
		if !got[e] {
			t.Errorf("allowlisted endpoint %q is never exercised: the allowlist is wider than the code needs", e)
		}
	}

	// Forbidden requests are refused before any I/O.
	before := len(rt.requests())
	forbidden := []struct{ method, path string }{
		{"POST", "/models/apply"},
		{"POST", "/models/import"},
		{"POST", "/models/import-uri"},
		{"POST", "/models/edit/ctl-model"},
		{"POST", "/models/delete/ctl-model"},
		{"POST", "/models/reload"},
		{"GET", "/models/available"},
		{"GET", "/models/galleries"},
		{"POST", "/backends/apply"},
		{"POST", "/backends/delete/llama-cpp"},
		{"POST", "/backends/upgrade/llama-cpp"},
		{"GET", "/backends"},
		{"POST", "/backend/load"},
		{"POST", "/backend/shutdown"},
		{"GET", "/backend/monitor"},
		{"GET", "/api/settings"},
		{"POST", "/api/settings"},
		{"GET", "/api/agents"},
		{"POST", "/api/agents/x/chat"},
		{"GET", "/api/cors-proxy"},
		{"POST", "/v1/mcp/chat/completions"},
		{"GET", "/v1/mcp/servers/x"},
		{"GET", "/api/p2p/token"},
		{"GET", "/metrics"},
		{"GET", "/system"},
		{"GET", "/api/traces"},
		{"POST", "/api/fine-tuning/jobs"},
		{"GET", "/app"},
		{"GET", "/healthz"},
		{"POST", "/v1/completions"},
		{"POST", "/v1/audio/transcriptions"},
		{"GET", "/v1/models/capabilities"},
		// the allowed paths with the wrong method
		{"DELETE", "/v1/models"},
		{"POST", "/v1/models"},
		{"PUT", "/v1/chat/completions"},
		{"GET", "/v1/chat/completions"},
		{"GET", "/v1/embeddings"},
		{"POST", "/readyz"},
		{"POST", "/version"},
		// the allowed paths, altered
		{"GET", "/v1/models/"},
		{"GET", "/V1/models"},
		{"GET", "//v1/models"},
		{"GET", "/v1//models"},
		{"GET", "/v1/models/../models/apply"},
		{"POST", "/v1/chat/completions/../../models/apply"},
		{"GET", "/v1/models%2f..%2fmodels%2fapply"},
		{"GET", "/v1/%6dodels"},
		{"GET", "/v1/models?x=1"},
		{"GET", "/v1/models#frag"},
		{"GET", "/v1/models;x=1"},
		{"GET", "/readyz?"},
		{"GET", ""},
		{"GET", "/"},
	}
	for _, f := range forbidden {
		var payload any
		if f.method != "GET" {
			payload = map[string]string{"name": "evil"}
		}
		resp, err := c.exchange(bg, f.method, f.path, payload, "application/json")
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s %s was sent", f.method, f.path)
			continue
		}
		if !errors.Is(err, errForbiddenEndpoint) {
			t.Errorf("%s %s: error %v does not wrap the allowlist refusal", f.method, f.path, err)
		}
		wantCode(t, err, contracts.ErrInternal)
	}
	if after := len(rt.requests()); after != before {
		t.Errorf("%d forbidden requests reached the server", after-before)
	}

	// The allowlist itself holds nothing that administers the runtime.
	for _, e := range AllowedEndpoints() {
		for _, bad := range []string{"apply", "delete", "install", "import", "edit", "settings", "backend", "gallery", "agent", "mcp", "cors", "metrics", "p2p", "fine-tuning", "reload", "shutdown", "load"} {
			if strings.Contains(strings.ToLower(e), bad) {
				t.Errorf("allowlist entry %q contains %q", e, bad)
			}
		}
	}
	if len(AllowedEndpoints()) != 5 {
		t.Errorf("allowlist = %v: widening it is a security decision, update this test and docs/security with it", AllowedEndpoints())
	}

	t.Run("another scheme or host is refused before the key is attached", func(t *testing.T) {
		var sent *http.Request
		g := &guardTransport{
			next:   roundTripFunc(func(r *http.Request) (*http.Response, error) { sent = r; return nil, errors.New("must not be called") }),
			scheme: "http",
			host:   "127.0.0.1:8080",
			key:    NewSecret("k"),
		}
		for _, raw := range []string{"http://127.0.0.1:9999/v1/models", "http://evil.example/v1/models", "https://127.0.0.1:8080/v1/models", "http://127.0.0.1:8080@evil.example/v1/models"} {
			u, _ := url.Parse(raw)
			req := &http.Request{Method: "GET", URL: u, Header: http.Header{}}
			if _, err := g.RoundTrip(req.WithContext(context.Background())); !errors.Is(err, errForbiddenEndpoint) {
				t.Errorf("%s: err = %v", raw, err)
			}
		}
		if sent != nil {
			t.Error("a request to another host reached the wrapped transport")
		}
	})
	t.Run("the key is attached only on authenticated routes and overrides the caller's header", func(t *testing.T) {
		var got *http.Request
		g := &guardTransport{
			next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				got = r
				return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
			}),
			scheme: "http", host: "127.0.0.1:8080", key: NewSecret("the-key"),
		}
		for path, wantAuth := range map[string]string{"/readyz": "", "/version": "Bearer the-key", "/v1/models": "Bearer the-key"} {
			u, _ := url.Parse("http://127.0.0.1:8080" + path)
			h := http.Header{"Authorization": {"Bearer attacker-supplied"}}
			if _, err := g.RoundTrip(&http.Request{Method: "GET", URL: u, Header: h}); err != nil {
				t.Fatal(err)
			}
			if a := got.Header.Get("Authorization"); a != wantAuth {
				t.Errorf("%s: Authorization = %q, want %q", path, a, wantAuth)
			}
			if h.Get("Authorization") != "Bearer attacker-supplied" {
				t.Error("RoundTrip mutated the caller's request")
			}
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Every test server in this package is created by a helper in helpers_test.go
// that records its requests for TestMain's whole-suite allowlist check.
func TestAllTestServersAreRecorded(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files found: %v", err)
	}
	banned := map[string]map[string]bool{
		"httptest": {"NewServer": true, "NewTLSServer": true, "NewUnstartedServer": true},
		"net":      {"Listen": true, "ListenTCP": true, "ListenUnix": true},
	}
	for _, f := range files {
		if f == "helpers_test.go" {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && banned[id.Name][sel.Sel.Name] {
				t.Errorf("%s creates a server with %s.%s; use newRuntime, newTLSRuntime or newUnixRuntime so the request is recorded", f, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

func TestUnixSocketEndpoint(t *testing.T) {
	rt, sock := newUnixRuntime(t, serveJSON(200, chatBody("over a socket", "stop")))
	cfg := testConfig(unixEndpoint(sock))
	key := newTestKey(t)
	c, err := New(cfg, WithAPIKey(NewSecret(key)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Chat(bg, testScope(), chatReq("hi"))
	if err != nil || res.Text != "over a socket" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	got := rt.only(t)
	if got.Path != "/v1/chat/completions" || got.Header.Get("Authorization") != "Bearer "+key {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
}

func TestTLSEndpoint(t *testing.T) {
	rt, sock := newTLSRuntime(t, serveJSON(200, chatBody("secure", "stop")))
	const endpoint = "https://127.0.0.1:8443" // the certificate names 127.0.0.1; the dial hook reaches the socket
	key := newTestKey(t)

	t.Run("an untrusted certificate is refused", func(t *testing.T) {
		c, err := New(testConfig(endpoint), WithAPIKey(NewSecret(key)), withDialContext(dialerTo(sock)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Chat(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrUnavailable)
		if n := len(rt.requests()); n != 0 {
			t.Fatalf("the request was sent despite an unverifiable certificate (%d)", n)
		}
		if s := err.Error(); strings.Contains(s, key) {
			t.Errorf("error leaks the key: %q", s)
		}
	})

	t.Run("a pinned CA is accepted and the key is sent", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "ca.pem")
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rt.Certificate().Raw})
		if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(endpoint)
		cfg.TLS.CAFile = caFile
		c, err := New(cfg, WithAPIKey(NewSecret(key)), withDialContext(dialerTo(sock)))
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.Chat(bg, testScope(), chatReq("hi"))
		if err != nil || res.Text != "secure" {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if got := rt.only(t); got.Header.Get("Authorization") != "Bearer "+key {
			t.Error("the key was not sent over TLS")
		}
	})

	t.Run("a certificate for another name is refused", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "ca.pem")
		_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rt.Certificate().Raw}), 0o600)
		cfg := testConfig("https://10.9.9.9:8443") // the certificate does not name this address
		cfg.TLS.CAFile = caFile
		c, err := New(cfg, WithAPIKey(NewSecret(key)), withDialContext(dialerTo(sock)))
		if err != nil {
			t.Fatal(err)
		}
		before := len(rt.requests())
		_, err = c.Chat(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrUnavailable)
		if len(rt.requests()) != before {
			t.Fatal("the request was sent to a host the certificate does not name")
		}
	})

	t.Run("bad TLS files are refused at construction", func(t *testing.T) {
		dir := t.TempDir()
		junk := filepath.Join(dir, "junk.pem")
		_ = os.WriteFile(junk, []byte("not pem"), 0o600)
		for name, tc := range map[string]TLSConfig{
			"missing CA":     {CAFile: filepath.Join(dir, "absent.pem")},
			"junk CA":        {CAFile: junk},
			"junk client":    {CertFile: junk, KeyFile: junk},
			"missing client": {CertFile: filepath.Join(dir, "a"), KeyFile: filepath.Join(dir, "b")},
		} {
			cfg := testConfig("https://localai.example.com:8443")
			cfg.TLS = tc
			if _, err := New(cfg); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("TLS 1.2 is the floor and verification cannot be skipped", func(t *testing.T) {
		cfg, err := buildTLS(TLSConfig{})
		if err != nil || cfg.MinVersion < tls.VersionTLS12 || cfg.InsecureSkipVerify {
			t.Fatalf("tls config = %+v, err = %v", cfg, err)
		}
	})
}
