package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// fakeListener is a listener that claims an address without binding it, so the
// refusal of a non-loopback listener is tested without opening a port on a real
// interface.
type fakeListener struct {
	addr   net.Addr
	closed bool
}

func (f *fakeListener) Accept() (net.Conn, error) { return nil, errors.New("fake: no connections") }
func (f *fakeListener) Close() error              { f.closed = true; return nil }
func (f *fakeListener) Addr() net.Addr            { return f.addr }

func metricsTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(CollectorFunc("demo", func(context.Context) ([]Family, error) {
		return []Family{{Name: "alixai_demo_gauge", Help: "A demo.", Type: Gauge, Samples: []Sample{{Value: 7}}}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	return reg
}

// request builds a request as the server would see it: the peer, the local
// address (which only a real connection supplies) and the Host.
func metricsRequest(method, target, remote, host string, local net.Addr) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
	if host != "" {
		r.Host = host
	}
	if local != nil {
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
	}
	return r
}

// SEC-093: Rule 3 and the "loopback-only metrics" deliverable: the listener cannot be
// bound beyond the machine, a listener that is not loopback is refused and
// closed, a request that is not from the machine itself gets a 403 whatever it
// asks for, and a scrape from the machine works.
func TestMetricsLoopbackOnly(t *testing.T) {
	t.Run("Listen refuses everything but a literal loopback address", func(t *testing.T) {
		for _, addr := range []string{
			"0.0.0.0:0", "[::]:0", ":9464", "192.0.2.10:9464", "10.0.0.5:9464", "[2001:db8::1]:9464",
			"example.com:9464", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1:http", "[::1%lo]:9464",
		} {
			ln, err := Listen(addr)
			if err == nil {
				_ = ln.Close()
				t.Errorf("Listen(%q) bound %s", addr, ln.Addr())
			}
		}
		for _, addr := range []string{"", "127.0.0.1:0", "localhost:0"} {
			ln, err := Listen(addr)
			if err != nil {
				t.Errorf("Listen(%q): %v", addr, err)
				continue
			}
			if !LoopbackListener(ln) {
				t.Errorf("Listen(%q) bound %s, which is not loopback", addr, ln.Addr())
			}
			_ = ln.Close()
		}
	})

	t.Run("Serve refuses and closes a listener that is not loopback", func(t *testing.T) {
		for _, addr := range []*net.TCPAddr{
			{IP: net.ParseIP("0.0.0.0"), Port: 9464},
			{IP: net.ParseIP("192.0.2.10"), Port: 9464},
			{IP: net.ParseIP("2001:db8::1"), Port: 9464},
		} {
			fl := &fakeListener{addr: addr}
			err := Serve(context.Background(), fl, Handler(metricsTestRegistry(t)))
			if !errors.Is(err, ErrNotLoopback) {
				t.Errorf("Serve on %s: err = %v, want ErrNotLoopback", addr, err)
			}
			if !fl.closed {
				t.Errorf("Serve on %s left the refused listener open", addr)
			}
		}
		if err := Serve(context.Background(), &fakeListener{addr: &net.UnixAddr{Name: "/tmp/m.sock", Net: "unix"}}, http.NotFoundHandler()); !errors.Is(err, ErrNotLoopback) {
			t.Errorf("Serve on a unix socket: err = %v, want ErrNotLoopback", err)
		}
		if err := Serve(context.Background(), nil, http.NotFoundHandler()); !errors.Is(err, ErrNotLoopback) {
			t.Errorf("Serve on a nil listener: err = %v, want ErrNotLoopback", err)
		}
	})

	t.Run("a request must come from the machine, to the machine, for the machine", func(t *testing.T) {
		h := Handler(metricsTestRegistry(t))
		lo := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9464}
		cases := []struct {
			name   string
			remote string
			host   string
			local  net.Addr
			want   int
		}{
			{"loopback peer, loopback local, loopback host", "127.0.0.1:40000", "127.0.0.1:9464", lo, http.StatusOK},
			{"IPv6 loopback peer", "[::1]:40000", "localhost:9464", lo, http.StatusOK},
			{"a LAN peer", "192.0.2.50:40000", "127.0.0.1:9464", lo, http.StatusForbidden},
			{"a public peer", "203.0.113.9:40000", "127.0.0.1:9464", lo, http.StatusForbidden},
			{"the httptest default peer", "192.0.2.1:1234", "127.0.0.1:9464", lo, http.StatusForbidden},
			{"a non-loopback local address", "127.0.0.1:40000", "127.0.0.1:9464", &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 9464}, http.StatusForbidden},
			{"no local address in the context", "127.0.0.1:40000", "127.0.0.1:9464", nil, http.StatusForbidden},
			{"a DNS-rebinding Host", "127.0.0.1:40000", "attacker.example:9464", lo, http.StatusForbidden},
			{"an unparseable peer", "not-an-address", "127.0.0.1:9464", lo, http.StatusForbidden},
		}
		for _, c := range cases {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, metricsRequest(http.MethodGet, "/metrics", c.remote, c.host, c.local))
			if rec.Code != c.want {
				t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
			}
			if c.want == http.StatusForbidden && strings.Contains(rec.Body.String(), "alixai_") {
				t.Errorf("%s: a refused request still received metrics", c.name)
			}
		}
		// The refusal is the same for every path and method: nothing is learned
		// about what exists.
		for _, p := range []string{"/metrics", "/", "/nope", "/debug/pprof/"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, metricsRequest(http.MethodPost, p, "203.0.113.9:40000", "127.0.0.1:9464", lo))
			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s from a public peer: status %d, want 403", p, rec.Code)
			}
		}
	})

	t.Run("a scrape over a real loopback connection", func(t *testing.T) {
		ln, err := Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, ln, Handler(metricsTestRegistry(t))) }()
		defer func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve ended with %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Serve did not stop after its context ended")
			}
		}()

		base := "http://" + ln.Addr().String()
		resp, err := http.Get(base + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ContentType {
			t.Fatalf("scrape: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		for _, want := range []string{"# TYPE alixai_demo_gauge gauge\n", "alixai_demo_gauge 7\n", `alixai_scrape_collector_up{collector="demo"} 1` + "\n"} {
			if !strings.Contains(string(body), want) {
				t.Errorf("scrape lacks %q:\n%s", want, body)
			}
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
		}

		// A different Host header on a real connection (what a rebinding page
		// sends) is refused even though the peer is the machine.
		req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
		req.Host = "attacker.example"
		resp2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusForbidden {
			t.Errorf("a foreign Host header got %d, want 403", resp2.StatusCode)
		}
		resp3, err := http.Post(base+"/metrics", "text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		resp3.Body.Close()
		if resp3.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST /metrics got %d, want 405", resp3.StatusCode)
		}
	})
}

// The configuration cannot name a non-loopback address, and the listener is off
// until an operator turns it on.
func TestMetricsConfigIsLoopbackAndOffByDefault(t *testing.T) {
	def := DefaultConfig()
	if def.Enabled {
		t.Error("metrics are enabled by default")
	}
	if err := def.Validate(); err != nil {
		t.Errorf("the default does not validate: %v", err)
	}
	for _, listen := range []string{"0.0.0.0:9464", ":9464", "192.0.2.4:9464", "metrics.example:9464", ""} {
		c := Config{Enabled: true, Listen: listen}
		if err := c.Validate(); err == nil {
			t.Errorf("listen %q was accepted", listen)
		}
	}
	// Through the real loader: a public address stops the load.
	r := config.NewRegistry()
	c := DefaultConfig()
	r.Register(SectionName, &c)
	path := t.TempDir() + "/alixai.yaml"
	writeTestFile(t, path, "metrics:\n  enabled: true\n  listen: 0.0.0.0:9464\n")
	if err := r.Load(path); err == nil {
		t.Error("the loader accepted a metrics listener on 0.0.0.0")
	}
	writeTestFile(t, path, "metrics:\n  enabled: true\n  listen: 127.0.0.1:19464\n")
	if err := r.Load(path); err != nil || !c.Enabled || c.Listen != "127.0.0.1:19464" {
		t.Errorf("loopback config: err=%v cfg=%+v", err, c)
	}
}
