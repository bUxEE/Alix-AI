package gateway

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-093: a non-loopback bind without TLS configuration is refused, before
// anything is bound.
func TestListenerRefusesNonLoopbackWithoutTLS(t *testing.T) {
	loopback := []string{"127.0.0.1:7470", "127.0.0.1:0", "[::1]:7470", "localhost:7470", "LOCALHOST:1", "127.9.9.9:80", "[::ffff:127.0.0.1]:80"}
	beyond := []string{
		":7470",                  // all interfaces
		"0.0.0.0:7470",           // IPv4 wildcard
		"[::]:7470",              // IPv6 wildcard
		"192.168.1.5:7470",       // LAN
		"10.0.0.1:1",             // private
		"8.8.8.8:53",             // public
		"[fe80::1]:80",           // link local
		"example.com:80",         // a name is never resolved to decide
		"127.0.0.1.example:80",   // looks like loopback, is a name
		"[::ffff:192.168.0.1]:1", // mapped LAN address
	}

	for _, addr := range loopback {
		if err := (ListenPolicy{}).CheckAddr(addr); err != nil {
			t.Errorf("CheckAddr(%q) without TLS = %v, want nil", addr, err)
		}
	}
	for _, addr := range beyond {
		err := (ListenPolicy{}).CheckAddr(addr)
		if err == nil {
			t.Errorf("CheckAddr(%q) without TLS = nil, want a refusal", addr)
			continue
		}
		if !errors.Is(err, ErrInsecureListener) {
			t.Errorf("CheckAddr(%q) = %v, want ErrInsecureListener", addr, err)
		}
		if code := contracts.CodeOf(err); code != contracts.ErrValidationFailed {
			t.Errorf("CheckAddr(%q) code = %s, want validation_failed", addr, code)
		}
		// With TLS configured the same address is acceptable (decided here
		// without binding it).
		if err := (ListenPolicy{TLS: true}).CheckAddr(addr); err != nil {
			t.Errorf("CheckAddr(%q) with TLS = %v, want nil", addr, err)
		}
	}
	if err := (ListenPolicy{}).CheckAddr("no-port"); err == nil {
		t.Error("an address without a port was accepted")
	}

	// Listen refuses before binding: nothing is returned to close.
	ln, err := Listen("0.0.0.0:0", ListenPolicy{})
	if err == nil || ln != nil {
		if ln != nil {
			_ = ln.Close()
		}
		t.Fatalf("Listen on 0.0.0.0 without TLS = (%v, %v), want a refusal", ln, err)
	}
	if !errors.Is(err, ErrInsecureListener) {
		t.Errorf("Listen error = %v, want ErrInsecureListener", err)
	}

	// A listener handed in from outside gets the same decision, and Serve
	// does not serve on it.
	h := newHarness(t)
	foreign := &fakeListener{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 7470}}
	if err := h.srv.Serve(context.Background(), foreign); !errors.Is(err, ErrInsecureListener) {
		t.Errorf("Serve on a non-loopback listener = %v, want ErrInsecureListener", err)
	}
	if !foreign.closed {
		t.Error("the refused listener was left open")
	}
	wild := &fakeListener{addr: &net.TCPAddr{Port: 7470}} // nil IP: all interfaces
	if err := h.srv.Serve(context.Background(), wild); !errors.Is(err, ErrInsecureListener) {
		t.Errorf("Serve on a wildcard listener = %v, want ErrInsecureListener", err)
	}
}

// SEC-092: the unauthenticated spike endpoint is never reachable beyond
// loopback, even when TLS is configured.
func TestSpikeEndpointRefusedBeyondLoopback(t *testing.T) {
	spike := ListenPolicy{TLS: true, SpikeEndpoint: true}
	if err := spike.CheckAddr("127.0.0.1:7470"); err != nil {
		t.Errorf("spike on loopback = %v, want nil", err)
	}
	for _, addr := range []string{"0.0.0.0:7470", "192.168.1.5:7470", ":7470"} {
		err := spike.CheckAddr(addr)
		if !errors.Is(err, ErrSpikeNeedsLoopback) {
			t.Errorf("spike on %q with TLS = %v, want ErrSpikeNeedsLoopback", addr, err)
		}
	}

	h := newHarness(t, withSpike())
	foreign := &fakeListener{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 7470}}
	if err := h.srv.Serve(context.Background(), foreign); !errors.Is(err, ErrSpikeNeedsLoopback) {
		t.Errorf("Serve with the spike on a non-loopback listener = %v, want ErrSpikeNeedsLoopback", err)
	}
	if got := h.srv.Policy(); !got.SpikeEndpoint {
		t.Errorf("Policy() = %+v, want SpikeEndpoint", got)
	}
}

// fakeListener is a listener that reports an address without binding one.
type fakeListener struct {
	addr   net.Addr
	closed bool
}

func (f *fakeListener) Accept() (net.Conn, error) {
	return nil, errors.New("fakeListener: no connections")
}
func (f *fakeListener) Close() error   { f.closed = true; return nil }
func (f *fakeListener) Addr() net.Addr { return f.addr }

func TestServeLoopbackAndShutdown(t *testing.T) {
	h := newHarness(t)
	ln, err := Listen("127.0.0.1:0", h.srv.Policy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.srv.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz") // plain-HTTP loopback
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("GET /healthz = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("a CORS header was sent")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

// A graceful shutdown ends the open event streams (a turn's events and the
// session's announcements) at once, and their clients resume from the cursor;
// it does not wait its whole shutdown_timeout for them and then cut them.
func TestServeShutdownEndsOpenEventStreams(t *testing.T) {
	e := newAPIEnv(t)
	_, turnID := turneventsOpenTurn(t, e, 1)
	ln, err := Listen("127.0.0.1:0", e.srv.Policy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(ctx, ln) }()

	var streams []*bufio.Reader
	for _, path := range []string{turneventsPath(turnID), sessionsPath(apiSess) + "/events"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+e.token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		br := bufio.NewReader(resp.Body)
		if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "retry: ") {
			t.Fatalf("GET %s began with %q, %v", path, line, err)
		}
		streams = append(streams, br)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after a clean shutdown", err)
		}
	case <-time.After(e.srv.cfg.ShutdownTimeout / 2):
		<-done
		t.Fatalf("the shutdown waited for the open event streams")
	}
	for i, br := range streams {
		if _, err := io.ReadAll(br); err != nil {
			t.Errorf("stream %d did not end cleanly: %v", i, err)
		}
	}
}

func TestNewServerRejectsBadTLSConfig(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.pem")
	for name, tc := range map[string]TLSConfig{
		"cert without key":     {CertFile: missing},
		"key without cert":     {KeyFile: missing},
		"client CA without id": {ClientCAFile: missing},
		"unreadable cert":      {CertFile: missing, KeyFile: missing},
	} {
		_, err := NewServer(Options{Config: Config{TLS: tc}})
		if err == nil {
			t.Errorf("%s: NewServer accepted the configuration", name)
		}
	}
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	pki := newTestPKI(t, dir)
	if _, err := NewServer(Options{Config: Config{TLS: TLSConfig{CertFile: pki.serverCert, KeyFile: pki.serverKey, ClientCAFile: bad}}}); err == nil {
		t.Error("NewServer accepted a client CA file without a certificate")
	}
}

// TestServeTLS runs the TLS paths over a unix socket (the machine's loopback
// TCP TLS is intercepted by antivirus software), including mutual TLS.
func TestServeTLS(t *testing.T) {
	dir := t.TempDir()
	pki := newTestPKI(t, dir)
	sock := filepath.Join(dir, "g.sock")

	serve := func(t *testing.T, cfg TLSConfig) (dial func(*tls.Config) (*http.Client, error), stop func()) {
		t.Helper()
		h := newHarness(t, withConfig(func(c *Config) { c.TLS = cfg }))
		if !h.srv.TLSEnabled() {
			t.Fatal("TLSEnabled() = false with a certificate configured")
		}
		_ = os.Remove(sock)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Skipf("unix sockets unavailable: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _ = h.srv.Serve(ctx, ln); close(done) }()
		return func(tc *tls.Config) (*http.Client, error) {
				return &http.Client{
					Timeout: 5 * time.Second,
					Transport: &http.Transport{
						TLSClientConfig: tc,
						DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
							return (&net.Dialer{}).DialContext(ctx, "unix", sock)
						},
					},
				}, nil
			}, func() {
				cancel()
				<-done
			}
	}

	roots := x509.NewCertPool()
	roots.AddCert(pki.caCert)

	t.Run("server certificate", func(t *testing.T) {
		dial, stop := serve(t, TLSConfig{CertFile: pki.serverCert, KeyFile: pki.serverKey})
		defer stop()
		client, _ := dial(&tls.Config{RootCAs: roots, ServerName: "gateway.test"})
		resp, err := client.Get("https://gateway.test/healthz")
		if err != nil {
			t.Fatalf("TLS request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.TLS == nil {
			t.Fatalf("status %d, TLS %v", resp.StatusCode, resp.TLS != nil)
		}
		// A client that does not trust the certificate is refused by its own
		// verification, and a TLS 1.1 client by the server.
		client, _ = dial(&tls.Config{RootCAs: x509.NewCertPool(), ServerName: "gateway.test"})
		if _, err := client.Get("https://gateway.test/healthz"); err == nil {
			t.Error("a client that does not trust the CA completed a request")
		}
		client, _ = dial(&tls.Config{RootCAs: roots, ServerName: "gateway.test", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})
		if _, err := client.Get("https://gateway.test/healthz"); err == nil {
			t.Error("a TLS 1.1 handshake succeeded")
		}
	})

	t.Run("mutual TLS", func(t *testing.T) {
		dial, stop := serve(t, TLSConfig{CertFile: pki.serverCert, KeyFile: pki.serverKey, ClientCAFile: pki.caFile})
		defer stop()
		client, _ := dial(&tls.Config{RootCAs: roots, ServerName: "gateway.test"})
		if resp, err := client.Get("https://gateway.test/healthz"); err == nil {
			_ = resp.Body.Close()
			t.Fatal("a client without a certificate was served")
		}
		clientCert, err := tls.LoadX509KeyPair(pki.clientCert, pki.clientKey)
		if err != nil {
			t.Fatal(err)
		}
		client, _ = dial(&tls.Config{RootCAs: roots, ServerName: "gateway.test", Certificates: []tls.Certificate{clientCert}})
		resp, err := client.Get("https://gateway.test/healthz")
		if err != nil {
			t.Fatalf("a client with a valid certificate was refused: %v", err)
		}
		_ = resp.Body.Close()
	})
}

// testPKI is a throwaway CA with a server and a client certificate, generated
// per test; no key material lives in the repository.
type testPKI struct {
	caCert                *x509.Certificate
	caFile                string
	serverCert, serverKey string
	clientCert, clientKey string
}

func newTestPKI(t *testing.T, dir string) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "alixai gateway test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	p := testPKI{caCert: caCert, caFile: filepath.Join(dir, "ca.pem")}
	writePEM(t, p.caFile, "CERTIFICATE", caDER)

	issue := func(name string, serial int64, usage x509.ExtKeyUsage, dns []string) (certFile, keyFile string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			DNSNames:     dns,
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		writePEM(t, certFile, "CERTIFICATE", der)
		writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
		return certFile, keyFile
	}
	p.serverCert, p.serverKey = issue("server", 2, x509.ExtKeyUsageServerAuth, []string{"gateway.test"})
	p.clientCert, p.clientKey = issue("client", 3, x509.ExtKeyUsageClientAuth, nil)
	return p
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
	for name, c := range map[string]Config{
		"negative timeout": {ReadTimeout: -time.Second},
		"huge timeout":     {WriteTimeout: 48 * time.Hour},
		"negative body":    {MaxBodyBytes: -1},
		"huge body":        {MaxBodyBytes: 1 << 40},
		"huge header":      {MaxHeaderBytes: 1 << 30},
		"cert without key": {TLS: TLSConfig{CertFile: "a"}},
		"client ca alone":  {TLS: TLSConfig{ClientCAFile: "a"}},
		"blank path":       {TLS: TLSConfig{CertFile: " ", KeyFile: "k"}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil", name)
		}
	}
	got := (Config{}).withDefaults()
	if got != DefaultConfig() {
		t.Errorf("zero config with defaults = %+v, want %+v", got, DefaultConfig())
	}
}

// The gateway sections register on the config registry, spike off by default,
// and the environment can switch it on.
func TestConfigSections(t *testing.T) {
	reg := config.NewRegistry()
	RegisterSections(reg)
	t.Cleanup(func() { // the sections are package-level: leave the defaults
		*ConfigSection() = DefaultConfig()
		*DevSection() = DefaultDevConfig()
	})
	if err := reg.LoadFrom("", func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	if DevSection().SpikeEndpoint {
		t.Fatal("dev.spike_endpoint defaults to true")
	}
	if *ConfigSection() != DefaultConfig() {
		t.Fatalf("gateway defaults = %+v", *ConfigSection())
	}
	env := map[string]string{"ALIXAI_DEV_SPIKE_ENDPOINT": "true", "ALIXAI_GATEWAY_MAX_BODY_BYTES": "2048"}
	if err := reg.LoadFrom("", func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err != nil {
		t.Fatal(err)
	}
	if !DevSection().SpikeEndpoint || ConfigSection().MaxBodyBytes != 2048 {
		t.Fatalf("environment overrides not applied: dev=%+v gateway=%+v", *DevSection(), *ConfigSection())
	}
	if err := reg.LoadFrom("", func(k string) (string, bool) {
		return "1m", k == "ALIXAI_GATEWAY_TLS_CERT_FILE"
	}); err == nil {
		t.Error("a certificate without a key loaded")
	}
}

func TestAccessLogHasNoSecrets(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/healthz?token=QUERYSECRET", nil,
		"Authorization", "Bearer HEADERSECRET", "Cookie", "sid=COOKIESECRET", HeaderRequestID, "attacker-chosen")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if id := rec.Header().Get(HeaderRequestID); id == "attacker-chosen" || !requestIDRE.MatchString(id) {
		t.Errorf("response %s = %q: a caller-supplied ID was trusted", HeaderRequestID, id)
	}
	h.do(http.MethodGet, "/nope?api_key=QUERYSECRET2", nil, "Authorization", "Bearer HEADERSECRET")
	logs := h.logs.String()
	for _, secret := range []string{"QUERYSECRET", "HEADERSECRET", "COOKIESECRET", "attacker-chosen"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
	for _, want := range []string{`"request_id":"req_`, `"path":"/healthz"`, `"status":200`, `"path":"/nope"`, `"status":404`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %s:\n%s", want, logs)
		}
	}
}

func TestFakeClockDrivesDurations(t *testing.T) {
	h := newHarness(t)
	if _, ok := any(h.clk).(clock.Clock); !ok {
		t.Fatal("the harness clock is not a clock.Clock")
	}
}
