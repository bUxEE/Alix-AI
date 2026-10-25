package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/fsperm/fspermtest"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The admin interface of M1-24 is a handler of its own, never part of the
// public mux, so these tests drive it the way a client does: through its
// handler with a request that carries a peer and a local address, and over a
// real loopback socket.

const adminTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type adminCall struct{ op, app string }

// adminFake records what the routes asked for.
type adminFake struct {
	mu         sync.Mutex
	calls      []adminCall
	refreshErr error
}

func (f *adminFake) Refresh(_ context.Context, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, adminCall{"refresh", app})
	return f.refreshErr
}

func (f *adminFake) Deactivate(app string) {
	f.mu.Lock()
	f.calls = append(f.calls, adminCall{"deactivate", app})
	f.mu.Unlock()
}

func (f *adminFake) seen() []adminCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]adminCall(nil), f.calls...)
}

func newTestAdmin(t *testing.T, ctl AdminControl) (*AdminServer, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	adm, err := NewAdminServer(AdminOptions{
		Control: ctl, Token: adminTestToken, Clock: clock.NewFake(testNow()),
		Logger: alog.NewWithWriter(config.LogConfig{Level: "debug"}, logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	return adm, logs
}

// adminRequest builds a request as the server sees one from the loopback peer
// that holds the credential; mods change what a test needs to break.
func adminTestRequest(method, path, body string, mods ...func(*http.Request) *http.Request) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = "127.0.0.1:41000"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000})))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AdminTokenHeader, adminTestToken)
	for _, m := range mods {
		r = m(r)
	}
	return r
}

func adminServe(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func adminWithHeader(k, v string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
		return r
	}
}

func adminWithRemote(addr string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request { r.RemoteAddr = addr; return r }
}

func adminWithHost(h string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request { r.Host = h; return r }
}

func adminWithLocal(a net.Addr) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, a))
	}
}

var adminRoutes = []string{AdminPathRefresh, AdminPathDeactivate}

const adminBody = `{"application_id":"demo-library"}`

// Both routes are for the operator on this machine and nobody else: the
// credential is checked on every path, a /v1 context token is not it, the peer,
// the local address and the Host must all be loopback, a refusal never says
// which routes exist, and the public server has no such route at all.
func TestAdminRefreshRequiresOperatorAndLoopback(t *testing.T) {
	ctl := &adminFake{}
	adm, logs := newTestAdmin(t, ctl)
	h := adm.Handler()

	// a real /v1 context token: valid for the public API, no credential here
	iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: apiApp, Clock: clock.NewFake(testNow())})
	v1Token := iss.Token()

	t.Run("operator credential", func(t *testing.T) {
		for _, path := range adminRoutes {
			for name, mod := range map[string]func(*http.Request) *http.Request{
				"none":            adminWithHeader(AdminTokenHeader, ""),
				"wrong":           adminWithHeader(AdminTokenHeader, strings.Repeat("f", len(adminTestToken))),
				"prefix":          adminWithHeader(AdminTokenHeader, adminTestToken[:len(adminTestToken)-1]),
				"extended":        adminWithHeader(AdminTokenHeader, adminTestToken+"0"),
				"v1 token header": adminWithHeader(AdminTokenHeader, v1Token),
				"v1 token bearer": func(r *http.Request) *http.Request {
					r.Header.Del(AdminTokenHeader)
					r.Header.Set("Authorization", "Bearer "+v1Token)
					return r
				},
				"bearer of secret": func(r *http.Request) *http.Request {
					r.Header.Del(AdminTokenHeader)
					r.Header.Set("Authorization", "Bearer "+adminTestToken)
					return r
				},
			} {
				rec := adminServe(h, adminTestRequest(http.MethodPost, path, adminBody, mod))
				env := mustEnvelope(t, rec)
				if rec.Code != http.StatusUnauthorized || env.Code != string(contracts.ErrUnauthenticated) {
					t.Errorf("%s with %s credential = %d %s, want 401 unauthenticated", path, name, rec.Code, env.Code)
				}
			}
		}
		if got := ctl.seen(); len(got) != 0 {
			t.Fatalf("a request without the credential acted: %v", got)
		}
	})

	t.Run("loopback only", func(t *testing.T) {
		for name, mod := range map[string]func(*http.Request) *http.Request{
			"remote LAN peer":        adminWithRemote("192.0.2.1:4000"),
			"remote public peer":     adminWithRemote("203.0.113.9:4000"),
			"remote IPv6 global":     adminWithRemote("[2001:db8::1]:4000"),
			"remote without a port":  adminWithRemote("127.0.0.1"),
			"remote a name":          adminWithRemote("localhost.evil.example:4000"),
			"local address LAN":      adminWithLocal(&net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 8080}),
			"local address wildcard": adminWithLocal(&net.TCPAddr{IP: net.IPv4zero, Port: 8080}),
			"local address a unix":   adminWithLocal(&net.UnixAddr{Name: "/tmp/x.sock", Net: "unix"}),
			"host evil.example":      adminWithHost("evil.example"),
			"host a LAN name":        adminWithHost("192.168.1.5:41000"),
			"host rebinding name":    adminWithHost("localhost.evil.example"),
		} {
			for _, path := range adminRoutes {
				rec := adminServe(h, adminTestRequest(http.MethodPost, path, adminBody, mod))
				env := mustEnvelope(t, rec)
				if rec.Code != http.StatusForbidden || env.Code != string(contracts.ErrPermissionDenied) {
					t.Errorf("%s with the credential and %s = %d %s, want 403", path, name, rec.Code, env.Code)
				}
			}
		}
		// a request whose local address is unknown (not through a listener) is not loopback
		r := adminTestRequest(http.MethodPost, AdminPathRefresh, adminBody)
		r = r.WithContext(context.Background())
		if rec := adminServe(h, r); rec.Code != http.StatusForbidden {
			t.Errorf("a request with no local address = %d, want 403", rec.Code)
		}
		if got := ctl.seen(); len(got) != 0 {
			t.Fatalf("a request from beyond loopback acted: %v", got)
		}
	})

	t.Run("no route enumeration", func(t *testing.T) {
		// the guard is in front of the router: an unknown path, a wrong method and
		// a known route answer the same without the credential
		for _, tc := range [][2]string{
			{http.MethodPost, "/admin/registry/refresh"}, {http.MethodGet, "/admin/registry/refresh"},
			{http.MethodPost, "/admin/nothing"}, {http.MethodGet, "/"}, {http.MethodPost, "/v1/sessions"},
		} {
			rec := adminServe(h, adminTestRequest(tc[0], tc[1], adminBody, adminWithHeader(AdminTokenHeader, "")))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without the credential = %d, want 401", tc[0], tc[1], rec.Code)
			}
		}
	})

	t.Run("the operator", func(t *testing.T) {
		for _, tc := range []struct {
			path string
			op   string
		}{{AdminPathRefresh, "refresh"}, {AdminPathDeactivate, "deactivate"}} {
			rec := adminServe(h, adminTestRequest(http.MethodPost, tc.path, adminBody))
			var res AdminResult
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil ||
				res.ApplicationID != "demo-library" || res.Action != tc.op || res.Status != "ok" {
				t.Fatalf("%s = %d %s", tc.path, rec.Code, rec.Body.String())
			}
		}
		got := ctl.seen()
		if len(got) != 2 || got[0] != (adminCall{"refresh", "demo-library"}) || got[1] != (adminCall{"deactivate", "demo-library"}) {
			t.Fatalf("calls = %v", got)
		}
		// IPv6 loopback and the name localhost are the same machine
		for _, mod := range []func(*http.Request) *http.Request{
			adminWithRemote("[::1]:50000"), adminWithHost("localhost:41000"), adminWithHost("[::1]:41000"),
			adminWithLocal(&net.TCPAddr{IP: net.IPv6loopback, Port: 41000}),
		} {
			if rec := adminServe(h, adminTestRequest(http.MethodPost, AdminPathRefresh, adminBody, mod)); rec.Code != http.StatusOK {
				t.Errorf("a loopback variant = %d: %s", rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("the credential is never logged", func(t *testing.T) {
		if strings.Contains(logs.String(), adminTestToken) || strings.Contains(logs.String(), v1Token) {
			t.Errorf("a credential reached the log:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), `"path":"/admin/registry/refresh"`) {
			t.Errorf("the admin call is not in the access log:\n%s", logs.String())
		}
	})

	t.Run("not on the public server", func(t *testing.T) {
		// SEC-092: the public mux never carries an operator route, whatever the
		// caller holds; the admin handler is not reachable through it.
		e := newAPIEnv(t)
		before := len(ctl.seen())
		for _, path := range adminRoutes {
			for _, r := range e.srv.Routes() {
				if strings.Contains(r, "/admin") {
					t.Fatalf("the public server mounts %s", r)
				}
			}
			rec := e.callAs(e.token(), http.MethodPost, path, adminBody, AdminTokenHeader, adminTestToken)
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST %s on the public server = %d, want 404", path, rec.Code)
			}
		}
		if got := ctl.seen(); len(got) != before {
			t.Errorf("the public server reached the admin control: %v", got)
		}
	})
}

// The same, over real sockets: the listener is loopback and only that, a
// non-loopback listener is refused whatever it carries, and the client the CLI
// uses succeeds with the credential and fails without it.
func TestAdminListenerIsLoopbackOnly(t *testing.T) { // SEC-093
	ctl := &adminFake{}
	adm, _ := newTestAdmin(t, ctl)

	ln, err := AdminListen()
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.Equal(net.IPv4(127, 0, 0, 1)) || a.Port == 0 {
		t.Fatalf("the admin listener is %v, want 127.0.0.1 on an ephemeral port", ln.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adm.Serve(ctx, ln) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	}()

	ep := AdminEndpoint{URL: "http://" + ln.Addr().String(), Token: adminTestToken, PID: 1, StartedAt: time.Now().UTC()}
	cl, err := NewAdminClient(ep)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Refresh(context.Background(), "demo-library"); err != nil {
		t.Fatalf("Refresh over loopback: %v", err)
	}
	if err := cl.Deactivate(context.Background(), "demo-library"); err != nil {
		t.Fatalf("Deactivate over loopback: %v", err)
	}
	if got := ctl.seen(); len(got) != 2 {
		t.Fatalf("calls = %v", got)
	}
	bad := ep
	bad.Token = strings.Repeat("c", 64)
	wrong, _ := NewAdminClient(bad)
	if err := wrong.Refresh(context.Background(), "demo-library"); contracts.CodeOf(err) != contracts.ErrUnauthenticated {
		t.Fatalf("the wrong credential over a socket = %v", err)
	}
	// a request with a Host that is not this machine, over the same socket
	req, _ := http.NewRequest(http.MethodPost, ep.URL+AdminPathRefresh, strings.NewReader(adminBody))
	req.Host = "evil.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(AdminTokenHeader, adminTestToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host over a socket = %d, want 403", resp.StatusCode)
	}
	if got := ctl.seen(); len(got) != 2 {
		t.Fatalf("a refused request acted: %v", got)
	}

	// Serve refuses what is not a loopback TCP listener, and closes it
	wild, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot bind a wildcard listener here: %v", err)
	}
	err = adm.Serve(context.Background(), wild)
	if !errors.Is(err, ErrAdminNeedsLoopback) || contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("Serve on %v = %v, want the loopback refusal", wild.Addr(), err)
	}
	if c, derr := net.DialTimeout("tcp", wild.Addr().String(), 200*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Error("the refused wildcard listener is still open")
	}

	// nobody is listening at a recorded address that went away: no server
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadURL := "http://" + dead.Addr().String()
	_ = dead.Close()
	gone, _ := NewAdminClient(AdminEndpoint{URL: deadURL, Token: adminTestToken})
	if err := gone.Refresh(context.Background(), "demo-library"); !errors.Is(err, ErrAdminUnreachable) {
		t.Errorf("a recorded address with no listener = %v, want ErrAdminUnreachable", err)
	}
}

func TestAdminRequestValidation(t *testing.T) {
	ctl := &adminFake{}
	adm, _ := newTestAdmin(t, ctl)
	h := adm.Handler()

	for name, tc := range map[string]struct {
		req  *http.Request
		code int
	}{
		"get":                {adminTestRequest(http.MethodGet, AdminPathRefresh, ""), http.StatusMethodNotAllowed},
		"unknown route":      {adminTestRequest(http.MethodPost, "/admin/registry/activate", adminBody), http.StatusNotFound},
		"no content type":    {adminTestRequest(http.MethodPost, AdminPathRefresh, adminBody, adminWithHeader("Content-Type", "")), http.StatusUnsupportedMediaType},
		"text":               {adminTestRequest(http.MethodPost, AdminPathRefresh, adminBody, adminWithHeader("Content-Type", "text/plain")), http.StatusUnsupportedMediaType},
		"not json":           {adminTestRequest(http.MethodPost, AdminPathRefresh, `{`), http.StatusBadRequest},
		"empty":              {adminTestRequest(http.MethodPost, AdminPathRefresh, ``), http.StatusBadRequest},
		"unknown member":     {adminTestRequest(http.MethodPost, AdminPathRefresh, `{"application_id":"a","all":true}`), http.StatusBadRequest},
		"trailing value":     {adminTestRequest(http.MethodPost, AdminPathRefresh, adminBody+adminBody), http.StatusBadRequest},
		"no application":     {adminTestRequest(http.MethodPost, AdminPathRefresh, `{}`), http.StatusBadRequest},
		"wildcard":           {adminTestRequest(http.MethodPost, AdminPathDeactivate, `{"application_id":"*"}`), http.StatusBadRequest},
		"path in the id":     {adminTestRequest(http.MethodPost, AdminPathDeactivate, `{"application_id":"../x"}`), http.StatusBadRequest},
		"id with a space":    {adminTestRequest(http.MethodPost, AdminPathDeactivate, `{"application_id":"a b"}`), http.StatusBadRequest},
		"id too long":        {adminTestRequest(http.MethodPost, AdminPathDeactivate, `{"application_id":"`+strings.Repeat("a", 129)+`"}`), http.StatusBadRequest},
		"body too large":     {adminTestRequest(http.MethodPost, AdminPathRefresh, `{"application_id":"`+strings.Repeat("a", 5000)+`"}`), http.StatusRequestEntityTooLarge},
		"application number": {adminTestRequest(http.MethodPost, AdminPathRefresh, `{"application_id":7}`), http.StatusBadRequest},
	} {
		rec := adminServe(h, tc.req)
		if rec.Code != tc.code {
			t.Errorf("%s = %d, want %d: %s", name, rec.Code, tc.code, rec.Body.String())
			continue
		}
		mustEnvelope(t, rec)
	}
	if got := ctl.seen(); len(got) != 0 {
		t.Fatalf("an invalid request acted: %v", got)
	}

	// a Refresh that fails says unavailable, keeps the cause to itself, and
	// still names the application so the operator knows what is not served
	ctl.refreshErr = errors.New("open /srv/alixai/packages/demo-library: SECRET-CAUSE")
	rec := adminServe(h, adminTestRequest(http.MethodPost, AdminPathRefresh, adminBody))
	env := mustEnvelope(t, rec)
	if rec.Code != http.StatusServiceUnavailable || env.Code != string(contracts.ErrUnavailable) || env.Details["application_id"] != "demo-library" {
		t.Fatalf("a failing refresh = %d %+v", rec.Code, env)
	}
	if strings.Contains(rec.Body.String(), "SECRET-CAUSE") || strings.Contains(rec.Body.String(), "/srv/alixai") {
		t.Errorf("the cause reached the client: %s", rec.Body.String())
	}
}

func TestNewAdminServerRefusesWhatWouldBeOpen(t *testing.T) {
	if _, err := NewAdminServer(AdminOptions{Token: adminTestToken}); err == nil {
		t.Error("an admin server with nothing to act on was built")
	}
	var nilFake *adminFake
	if _, err := NewAdminServer(AdminOptions{Control: nilFake, Token: adminTestToken}); err == nil {
		t.Error("an admin server over a nil control was built")
	}
	for _, tok := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("a", 257)} {
		if _, err := NewAdminServer(AdminOptions{Control: &adminFake{}, Token: tok}); err == nil {
			t.Errorf("an admin server with a %d character credential was built", len(tok))
		}
	}
	a, err := NewAdminToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewAdminToken()
	if len(a) != 64 || a == b {
		t.Errorf("credentials %q and %q: want 64 hex characters, never repeated", a, b)
	}
}

// SEC-081: the credential is written owner-only, atomically and without a
// moment of exposure, refused when another user could read it, and a process
// that stops removes only its own record.
func TestAdminEndpointFileIsPrivateAndStrict(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ep := AdminEndpoint{URL: "http://127.0.0.1:43111", Token: adminTestToken, PID: 77, StartedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	if err := WriteAdminEndpoint(dir, ep); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, AdminEndpointFile)
	// Owner only on every platform: modes on POSIX, the ACL on Windows (M2).
	if err := fsperm.Check(path, false); err != nil {
		t.Errorf("the endpoint file is not owner only: %v", err)
	}
	if err := fsperm.Check(dir, true); err != nil {
		t.Errorf("the data directory it created is not owner only: %v", err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("the endpoint file mode = %v, %v; want 0600", st.Mode(), err)
		}
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("the data directory it created is %v, want 0700", st.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary file was left behind: %v", err)
	}
	got, err := ReadAdminEndpoint(dir)
	if err != nil || got.URL != ep.URL || got.Token != ep.Token || got.PID != 77 || got.Format != AdminEndpointFormat {
		t.Fatalf("read back %+v, %v", got, err)
	}

	// a second server replaces the record; the first one's stop leaves it alone
	second := ep
	second.Token = strings.Repeat("9", 64)
	second.URL = "http://127.0.0.1:43222"
	if err := WriteAdminEndpoint(dir, second); err != nil {
		t.Fatal(err)
	}
	RemoveAdminEndpoint(dir, ep.Token)
	if got, err := ReadAdminEndpoint(dir); err != nil || got.Token != second.Token {
		t.Fatalf("a stopping server removed the record of the one that replaced it: %+v, %v", got, err)
	}
	RemoveAdminEndpoint(dir, second.Token)
	if _, err := ReadAdminEndpoint(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record is still there after its own server stopped: %v", err)
	}

	// never written for an address that is not this machine, or without a credential
	for name, bad := range map[string]AdminEndpoint{
		"lan":        {URL: "http://192.168.1.5:80", Token: adminTestToken},
		"name":       {URL: "http://example.test:80", Token: adminTestToken},
		"https":      {URL: "https://127.0.0.1:80", Token: adminTestToken},
		"no port":    {URL: "http://127.0.0.1", Token: adminTestToken},
		"userinfo":   {URL: "http://u@127.0.0.1:80", Token: adminTestToken},
		"path":       {URL: "http://127.0.0.1:80/x", Token: adminTestToken},
		"no token":   {URL: "http://127.0.0.1:80"},
		"short":      {URL: "http://127.0.0.1:80", Token: "abc"},
		"query":      {URL: "http://127.0.0.1:80?x=1", Token: adminTestToken},
		"unspecific": {URL: "http://0.0.0.0:80", Token: adminTestToken},
	} {
		if err := WriteAdminEndpoint(t.TempDir(), bad); err == nil {
			t.Errorf("%s: a record that would send the credential off this machine was written", name)
		}
		if _, err := NewAdminClient(bad); err == nil {
			t.Errorf("%s: a client was built for it", name)
		}
	}

	// readers refuse a file with more members than the format has, and one that
	// other users can read (POSIX modes, or a Windows ACL that is not owner only)
	if err := os.WriteFile(path, []byte(`{"format":"alixai.admin-endpoint/v1","url":"http://127.0.0.1:1","token":"`+adminTestToken+`","x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAdminEndpoint(dir); err == nil {
		t.Error("a file with an unknown member was accepted")
	}
	if err := WriteAdminEndpoint(dir, ep); err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, path)
	_, err = ReadAdminEndpoint(dir)
	if err == nil || !errors.Is(err, fsperm.ErrNotOwnerOnly) || strings.Contains(err.Error(), adminTestToken) {
		t.Errorf("a credential file other users can read: %v", err)
	}
}

// ---- immediate effect ----

// adminSource is a capabilities.Source whose answer the test changes: the
// store, as the registry sees it.
type adminSource struct {
	mu          sync.Mutex
	pkg         *testPkg
	quarantined bool
	failing     bool
}

func (s *adminSource) set(f func(*adminSource)) {
	s.mu.Lock()
	f(s)
	s.mu.Unlock()
}

func (s *adminSource) ActiveHash(context.Context, string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return "", false, errors.New("the store is gone")
	}
	return s.pkg.hash, !s.quarantined, nil
}

func (s *adminSource) Open(_ context.Context, _, hash string) (capabilities.Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hash != s.pkg.hash {
		return nil, errors.New("no such package")
	}
	return s.pkg, nil
}

// adminRegistryControl is the registry behind the two routes, as the
// composition root makes it.
type adminRegistryControl struct{ reg *capabilities.Registry }

func (c adminRegistryControl) Refresh(ctx context.Context, app string) error {
	return c.reg.Refresh(ctx, app)
}

func (c adminRegistryControl) Deactivate(app string) { c.reg.Deactivate(app) }

// A quarantine changes the store at once and the registry's cache only when it
// ages out; the admin route closes that gap. The registry here has a one hour
// cache and a clock that does not move, so nothing but the route can change what
// it serves, and the control step shows it keeps serving without the route.
func TestAdminRefreshDeactivatesQuarantinedApplicationImmediately(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow())
	pkg := buildPackage(t, level2)
	src := &adminSource{pkg: pkg}
	reg := capabilities.NewRegistry(src, capabilities.WithClock(clk), capabilities.WithMaxAge(time.Hour))
	adm, _ := newTestAdmin(t, adminRegistryControl{reg})
	h := adm.Handler()
	post := func(path string) *httptest.ResponseRecorder {
		return adminServe(h, adminTestRequest(http.MethodPost, path, `{"application_id":"`+apiApp+`"}`))
	}
	listed := func() bool {
		for _, a := range reg.Applications() {
			if a == apiApp {
				return true
			}
		}
		return false
	}
	served := func() (capabilities.Info, error) { return reg.Info(ctx, apiApp) }

	if err := reg.Refresh(ctx, apiApp); err != nil {
		t.Fatal(err)
	}
	if info, err := served(); err != nil || info.ContentHash != pkg.hash || !listed() {
		t.Fatalf("the active package is not served: %+v, %v", info, err)
	}

	// the store quarantines the package; the registry, left alone, keeps serving
	// it from its cache (this is the problem)
	src.set(func(s *adminSource) { s.quarantined = true })
	clk.Advance(5 * time.Second)
	if _, err := served(); err != nil || !listed() {
		t.Fatalf("control failed: the registry stopped serving a quarantined package without being told (%v); the test would prove nothing", err)
	}

	// the operator's deactivate: gone now, with the clock where it was
	if rec := post(AdminPathDeactivate); rec.Code != http.StatusOK {
		t.Fatalf("deactivate = %d: %s", rec.Code, rec.Body.String())
	}
	if listed() {
		t.Fatal("the registry still holds the quarantined application after the route returned")
	}
	if _, err := served(); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("a quarantined application is still served: %v", err)
	}

	// it comes back only as the store says: reactivated there, an eviction never
	// loads anything, and the refresh an activation sends loads the store's
	// package (and clears the registry's note that there was none)
	src.set(func(s *adminSource) { s.quarantined = false })
	if rec := post(AdminPathDeactivate); rec.Code != http.StatusOK {
		t.Fatalf("deactivate = %d", rec.Code)
	}
	if listed() {
		t.Fatal("deactivate loaded a package")
	}
	if rec := post(AdminPathRefresh); rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d: %s", rec.Code, rec.Body.String())
	}
	if info, err := served(); err != nil || info.ContentHash != pkg.hash {
		t.Fatalf("a reactivated application is not served from the store: %+v, %v", info, err)
	}

	// refresh: the store has a new active package and the registry's one-hour
	// cache still holds the old one; the route reloads it at once
	next := *pkg
	next.hash = "00000000000000000000000000000000000000000000000000000000000000def"
	src.set(func(s *adminSource) { s.pkg = &next })
	if info, _ := served(); info.ContentHash != pkg.hash {
		t.Fatalf("control failed: the registry already serves the new package (%s)", info.ContentHash)
	}
	if rec := post(AdminPathRefresh); rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d: %s", rec.Code, rec.Body.String())
	}
	if info, err := served(); err != nil || info.ContentHash != next.hash {
		t.Fatalf("after refresh the registry serves %+v, %v; want the new package", info, err)
	}

	// refresh of an application the store has quarantined drops it (and says ok:
	// the server is in line with the store)
	src.set(func(s *adminSource) { s.quarantined = true })
	if rec := post(AdminPathRefresh); rec.Code != http.StatusOK {
		t.Fatalf("refresh of a quarantined application = %d", rec.Code)
	}
	if listed() {
		t.Fatal("refresh left a quarantined application in the registry")
	}

	// a store that cannot be read: the registry drops the application, and the
	// route says so instead of claiming it is in line
	src.set(func(s *adminSource) { s.quarantined = false })
	if rec := post(AdminPathRefresh); rec.Code != http.StatusOK || !listed() {
		t.Fatalf("refresh back to active = %d, listed %v", rec.Code, listed())
	}
	src.set(func(s *adminSource) { s.failing = true })
	rec := post(AdminPathRefresh)
	env := mustEnvelope(t, rec)
	if rec.Code != http.StatusServiceUnavailable || env.Code != string(contracts.ErrUnavailable) {
		t.Fatalf("refresh with an unreadable store = %d %+v, want 503 unavailable", rec.Code, env)
	}
	if listed() {
		t.Fatal("a registry that cannot confirm the package is active kept serving it")
	}
}
