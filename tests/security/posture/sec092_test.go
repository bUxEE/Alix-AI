package posturesec

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/gateway"
)

// addrListener reports another address than the one it is bound to: a listener a
// careless deployment could hand to Serve.
type addrListener struct {
	net.Listener
	addr net.Addr
}

func (l addrListener) Addr() net.Addr { return l.addr }

// SEC-092: no forbidden surface is exposed (browser CORS proxy, arbitrary MCP
// registration, agent hub, shell or code execution, model apply or gallery, an
// administration UI, profiling, metrics), every registered route is on the OpenAPI
// document, no response carries a CORS header, and the unauthenticated dev spike
// endpoint is absent unless explicitly enabled and never served beyond loopback. The
// suite builds the real gateway with every route area mounted and asks it.
//
// SEC-092
func TestSecNoForbiddenRoutes(t *testing.T) {
	docs := newSpecDocs(t)
	ops := docs.operations()
	specList := specRoutes(ops)
	inSpec := set(specList...)

	t.Run("the detectors see what they must", func(t *testing.T) {
		planted := map[string]string{
			"GET /cors-proxy": "browser CORS proxy", "POST /api/cors-proxy": "browser CORS proxy", "POST /exec": "code or command execution",
			"POST /shell": "code or command execution", "POST /v1/agents": "agent hub", "POST /v1/mcp/servers": "MCP or tool registration",
			"POST /v1/tools": "MCP or tool registration", "POST /models/apply": "model apply, install or gallery",
			"POST /v1/models/install": "model apply, install or gallery", "GET /models/gallery": "model apply, install or gallery",
			"GET /admin": "admin or web UI", "GET /webui": "admin or web UI", "GET /swagger": "admin or web UI",
			"GET /debug/pprof/heap": "debug or profiling", "GET /metrics": "metrics",
			"POST /v1/chat/completions": "OpenAI-compatible or LocalAI surface", "POST /v0/dev/propose": "development route",
		}
		for route, rule := range planted {
			found := false
			for _, got := range forbidden(route, inSpec) {
				found = found || got == rule
			}
			if !found {
				t.Errorf("the deny-list does not flag %s as %q (flags %v)", route, rule, forbidden(route, inSpec))
			}
		}
		for _, route := range []string{
			"GET /healthz", "GET /readyz", "POST /v1/applications", "GET /v1/applications/{application_id}", "GET /v1/agui/capabilities",
			"POST /v1/sessions/{session_id}/turns", "GET /v1/rag/sources", "POST /v1/rag/search", "PUT /v1/settings/user",
			"POST /v1/voice/sessions", "GET /v1/ops/queue", "POST /v1/history/export", "GET /v1/capabilities",
		} {
			if got := forbidden(route, inSpec); len(got) != 0 {
				t.Errorf("the deny-list flags the clean route %s: %v", route, got)
			}
		}
		// a metrics route that the specification lists is a decision, not a leak
		if got := forbidden("GET /metrics", set("GET /metrics")); len(got) != 0 {
			t.Errorf("a listed metrics route is flagged: %v", got)
		}

		notInSpec, noRoute := routeDiff([]string{"GET /a", "GET /b", "POST /exec"}, []string{"GET /a", "GET /c"})
		if strings.Join(notInSpec, ",") != "GET /b,POST /exec" || strings.Join(noRoute, ",") != "GET /c" {
			t.Errorf("the specification comparison reports %v / %v", notInSpec, noRoute)
		}

		// CORS: a wildcard, a reflected origin and a preflight answer are all flagged; a plain handler is not.
		plain := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
		wildcard := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			_, _ = w.Write([]byte("ok"))
		})
		reflect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			_, _ = w.Write([]byte("ok"))
		})
		preflight := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte("ok"))
		})
		probes := []probe{{method: http.MethodGet, path: "/x"}, {method: http.MethodOptions, path: "/x"}}
		if v, sent, _ := corsViolations(plain, probes); len(v) != 0 || sent != 2 {
			t.Errorf("the CORS sweep flags a plain handler: %v (sent %d)", v, sent)
		}
		for name, h := range map[string]http.Handler{"a wildcard": wildcard, "a reflected origin": reflect, "a preflight answer": preflight} {
			if v, _, _ := corsViolations(h, probes); len(v) == 0 {
				t.Errorf("the CORS sweep passes %s", name)
			}
		}
	})

	t.Run("planted routes on the real server are found by every detector", func(t *testing.T) {
		bad := newRig(t, withPlanted)
		routes := bad.srv.Routes()
		notInSpec, _ := routeDiff(routes, specList)
		if len(notInSpec) != 6 {
			t.Errorf("the specification comparison finds %v, want the 6 planted routes", notInSpec)
		}
		flagged := forbiddenRoutes(routes, inSpec)
		if len(flagged) < 6 {
			t.Errorf("the deny-list finds %v, want at least the 6 planted routes", flagged)
		}
		var probes []probe
		for _, rt := range routes {
			m, p, _ := strings.Cut(rt, " ")
			probes = append(probes, probe{method: m, path: substitute(p)}, probe{method: http.MethodOptions, path: substitute(p)})
		}
		v, _, _ := corsViolations(bad.srv.Handler(), probes)
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "/api/cors-proxy") {
			t.Errorf("the CORS sweep does not find the planted wildcard route: %v", v)
		}
	})

	r := newRig(t)
	r.requireEveryAreaMounted()
	routes := r.srv.Routes()

	t.Run("every registered route is on the OpenAPI document, and every operation has a route", func(t *testing.T) {
		notInSpec, noRoute := routeDiff(routes, specList)
		if len(notInSpec) != 0 {
			t.Errorf("routes registered but not on the OpenAPI document: %v", notInSpec)
		}
		if len(noRoute) != 0 {
			t.Errorf("operations on the OpenAPI document with no route: %v", noRoute)
		}
		if len(routes) < 40 {
			t.Fatalf("only %d routes are registered", len(routes))
		}
	})

	t.Run("the deny-list matches nothing", func(t *testing.T) {
		if got := forbiddenRoutes(routes, inSpec); len(got) != 0 {
			t.Fatalf("routes on the deny-list: %v", got)
		}
		for _, o := range ops {
			if strings.HasPrefix(o.Path, "/v0/") {
				t.Errorf("%s: a development route is on the public API", o.Route())
			}
		}
	})

	t.Run("no response carries an Access-Control header", func(t *testing.T) {
		// Register a package and activate it, so that the sweep also reaches the
		// handlers that answer 2xx.
		if rec := r.do(r.token(libraryApp, gateway.CapabilityRegisterApplication), http.MethodPost, "/v1/applications", "application/gzip", packArchive(t)); rec.Code != http.StatusCreated {
			t.Fatalf("registration = %d", rec.Code)
		}
		if _, err := r.db.ExecContext(context.Background(),
			`UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_sec_092' WHERE application_id = ? AND status = 'quarantined'`, libraryApp); err != nil {
			t.Fatal(err)
		}
		tok := func() string { return r.token(libraryApp, capsForTraffic...) }
		var probes []probe
		for _, rt := range routes {
			m, p, _ := strings.Cut(rt, " ")
			p = substitute(p)
			body, ct := []byte(nil), ""
			if m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch {
				body, ct = []byte(`{}`), "application/json"
			}
			for _, method := range []string{m, http.MethodOptions} {
				probes = append(probes,
					probe{method: method, path: p, body: body, contentType: ct},
					probe{method: method, path: p, body: body, contentType: ct, token: tok})
			}
			if m == http.MethodGet {
				probes = append(probes, probe{method: http.MethodHead, path: p, token: tok})
			}
		}
		// Error paths: unknown path, wrong method, malformed and oversized bodies, bad token.
		huge := []byte(strings.Repeat("a", 1<<20))
		probes = append(probes,
			probe{method: http.MethodGet, path: "/nope"}, probe{method: http.MethodGet, path: "/v1/nope", token: tok},
			probe{method: http.MethodDelete, path: "/healthz"}, probe{method: http.MethodPost, path: "/readyz"},
			probe{method: http.MethodPost, path: "/v1/sessions", body: []byte(`{`), contentType: "application/json", token: tok},
			probe{method: http.MethodPost, path: "/v1/sessions", body: huge, contentType: "application/json", token: tok},
			probe{method: http.MethodPost, path: "/v1/sessions", body: []byte(`{}`), contentType: "text/plain", token: tok},
			probe{method: http.MethodGet, path: "/v1/capabilities", token: func() string { return "not.a.token" }},
			probe{method: http.MethodOptions, path: "*"}, probe{method: http.MethodOptions, path: "/"},
		)
		violations, sent, codes := corsViolations(r.srv.Handler(), probes)
		t.Logf("status codes of the sweep: %v", codes)
		// The sweep reached the handlers, not only the middleware: successes, validation
		// refusals, scope refusals and the stand-in services (which answer 500 or 503).
		if codes[200] < 5 || codes[400] < 10 || codes[403] < 5 || codes[500]+codes[503] < 5 {
			t.Fatalf("the sweep did not reach the handlers: status codes %v", codes)
		}
		if sent < 2*len(routes) {
			t.Fatalf("the sweep sent only %d requests for %d routes", sent, len(routes))
		}
		if len(violations) != 0 {
			t.Fatalf("responses with CORS headers (a permissive default would be a browser-reachable API):\n%s", strings.Join(violations, "\n"))
		}
	})

	t.Run("the dev spike endpoint", func(t *testing.T) {
		if gateway.DefaultDevConfig().SpikeEndpoint || gateway.DevSection().SpikeEndpoint {
			t.Fatal("dev.spike_endpoint defaults to true")
		}
		for _, rt := range routes {
			if strings.Contains(rt, "/v0/") {
				t.Errorf("route %s is mounted while the spike endpoint is off", rt)
			}
		}
		if rec := r.do("", http.MethodPost, "/v0/dev/propose", "application/json", []byte(`{}`)); rec.Code != http.StatusNotFound {
			t.Fatalf("POST /v0/dev/propose with the flag off = %d, want 404", rec.Code)
		}
		offPolicy := r.srv.Policy()
		if offPolicy.SpikeEndpoint {
			t.Fatal("the default server's listener policy says the spike endpoint is on")
		}

		// Explicitly enabled: exactly one more route, and it answers without a token.
		on := newRig(t, func(o *rigOptions) { o.dev = gateway.DevConfig{SpikeEndpoint: true} })
		notInSpec, noRoute := routeDiff(on.srv.Routes(), specList)
		if len(noRoute) != 0 || strings.Join(notInSpec, ",") != "POST /v0/dev/propose" {
			t.Fatalf("with the flag on the server adds %v (missing %v), want exactly POST /v0/dev/propose", notInSpec, noRoute)
		}
		if rec := on.do("", http.MethodPost, "/v0/dev/propose", "application/json", []byte(`{}`)); rec.Code == http.StatusNotFound || rec.Code == http.StatusUnauthorized {
			t.Fatalf("control: POST /v0/dev/propose with the flag on = %d: the route is not reached, so the refusals below prove nothing", rec.Code)
		}
		pol := on.srv.Policy()
		if !pol.SpikeEndpoint {
			t.Fatal("the listener policy does not know the spike endpoint is on")
		}

		// It is never reachable on a listener beyond loopback, whatever address it is given.
		for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0", "192.0.2.1:7470", "10.0.0.5:7470", "localhost.example.com:7470"} {
			ln, err := gateway.Listen(addr, pol)
			if err == nil {
				_ = ln.Close()
				t.Errorf("Listen(%s) with the spike endpoint on succeeded", addr)
				continue
			}
			if !errors.Is(err, gateway.ErrSpikeNeedsLoopback) {
				t.Errorf("Listen(%s) = %v, want ErrSpikeNeedsLoopback", addr, err)
			}
		}
		// A listener that is handed in already bound is checked again by Serve.
		inner, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		beyond := addrListener{Listener: inner, addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 7470}}
		if err := on.srv.Serve(ctx, beyond); !errors.Is(err, gateway.ErrSpikeNeedsLoopback) {
			t.Errorf("Serve on a listener beyond loopback with the spike endpoint on = %v, want ErrSpikeNeedsLoopback", err)
		}
		// and the server without the flag still refuses a listener beyond loopback without TLS
		inner2, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		beyond2 := addrListener{Listener: inner2, addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 7470}}
		if err := r.srv.Serve(ctx, beyond2); !errors.Is(err, gateway.ErrInsecureListener) {
			t.Errorf("Serve on a listener beyond loopback without TLS = %v, want ErrInsecureListener", err)
		}

		// Control: on loopback the same server serves, and the endpoint answers there.
		ln, err := gateway.Listen("127.0.0.1:0", pol)
		if err != nil {
			t.Fatalf("control: Listen on loopback: %v", err)
		}
		serveCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- on.srv.Serve(serveCtx, ln) }()
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post("http://"+ln.Addr().String()+"/v0/dev/propose", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("control: the loopback server does not answer: %v", err)
		}
		drain(resp.Body)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("control: POST /v0/dev/propose on loopback = %d", resp.StatusCode)
		}
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve on loopback returned %v after a clean stop", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the loopback server did not stop")
		}
	})
}
