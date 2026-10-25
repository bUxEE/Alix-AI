package cli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/metrics"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const platformDenyApp = "clinic-booking"

// platformDenyListener opens a loopback listener that accepts and drops connections, so a dial
// that is not refused really connects.
func platformDenyListener(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// platformDenyUse swaps the process-wide deny-list for the duration of a test.
func platformDenyUse(t testing.TB, addrs ...string) {
	t.Helper()
	old := platformDenyList
	p := egress.NewPlatformDeny()
	p.AddSource(func() []string { return addrs })
	platformDenyList = p
	t.Cleanup(func() { platformDenyList = old })
}

// platformDenyDial dials through a transport's dialer and reports the refusal reason ("" = connected).
func platformDenyDial(t testing.TB, d *egress.Dialer, port int) egress.Reason {
	t.Helper()
	if d == nil {
		t.Fatal("the transport built no dialer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		_ = c.Close()
		return ""
	}
	if r := egress.ReasonOf(err); r != "" {
		return r
	}
	t.Fatalf("dial 127.0.0.1:%d failed for another reason: %v", port, err)
	return ""
}

// platformDenyCase is one dial: a platform port that is refused, and the application's own port
// that still connects, so the deny-list is what refuses and not a broken listener.
type platformDenyCase struct {
	name string
	port int
	want egress.Reason
}

// PT-001. A signed package can name any loopback port as its endpoint and the transport lists it;
// the platform's own ports are refused by the dialer all the same, for every transport, while the
// package's own application port is still reachable. Mutations this pins: a transport that forgets
// platformDenyOption; a deny-list consulted only for unlisted entries; a match on the wrong port.
func TestPlatformPortsAreDeniedEvenWhenListed(t *testing.T) {
	platformPort := platformDenyListener(t)
	appPort := platformDenyListener(t)
	platformDenyUse(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(platformPort)))
	cases := []platformDenyCase{
		{"platform port", platformPort, egress.ReasonPlatformPort},
		{"the application's own port", appPort, ""},
	}

	t.Run("mcp", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				pkg, dir := mcpTransportLoadClinic(t)
				mcpTransportRepoint(t, pkg, "clinic-mcp", fmt.Sprintf("http://127.0.0.1:%d/mcp", c.port))
				plan, err := transportsPlanFor(pkg.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := mcpTransportConfig(context.Background(),
					TransportRequest{ApplicationID: platformDenyApp, Kind: TransportKindMCP, Package: pkg, Dir: dir, Endpoints: plan.Endpoints}, mcpTransportLogger())
				if err != nil {
					t.Fatal(err)
				}
				if got := platformDenyDial(t, cfg.Dialer, c.port); got != c.want {
					t.Fatalf("dial reason %q, want %q", got, c.want)
				}
			})
		}
	})

	t.Run("openapi", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				pkg, dir := openapiTransportInstall(t, fmt.Sprintf("http://127.0.0.1:%d", c.port), "", "")
				plan, err := transportsPlanFor(pkg.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := openapiTransportConfig(context.Background(),
					TransportRequest{ApplicationID: platformDenyApp, Kind: TransportKindOpenAPI, Package: pkg, Dir: dir, Endpoints: plan.Endpoints}, openapiTransportLogger())
				if err != nil {
					t.Fatal(err)
				}
				if got := platformDenyDial(t, cfg.Dialer, c.port); got != c.want {
					t.Fatalf("dial reason %q, want %q", got, c.want)
				}
			})
		}
	})

	t.Run("native", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
				pkg, dir := nativeTransportInstall(t, ap.ManifestNativeIPCKindLoopbackTCP, fmt.Sprintf("127.0.0.1:%d", c.port), "")
				plan, err := transportsPlanFor(pkg.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := nativeTransportConfig(context.Background(),
					TransportRequest{ApplicationID: platformDenyApp, Kind: TransportKindNative, Package: pkg, Dir: dir, Endpoints: plan.Endpoints}, nativeTransportLogger())
				if err != nil {
					t.Fatal(err)
				}
				if got := platformDenyDial(t, cfg.Dialer, c.port); got != c.want {
					t.Fatalf("dial reason %q, want %q", got, c.want)
				}
			})
		}
	})
}

// platformDenyResetAdminCache forgets the cached admin endpoint.
func platformDenyResetAdminCache() {
	platformDenyAdminCache.mu.Lock()
	platformDenyAdminCache.at = time.Time{}
	platformDenyAdminCache.mu.Unlock()
}

// The list the composition root fills is the running configuration: the gateway listen address,
// the metrics listener (enabled or not), the LocalAI base URL, the admin interface that wrote its
// endpoint file, and every listener serve registered. A source that drops one lets a package reach it.
func TestPlatformDenyListFollowsTheRunningConfiguration(t *testing.T) {
	oldAdmin, oldLocalAI := platformDenyAdminURL, localaiSection
	t.Cleanup(func() {
		platformDenyAdminURL, localaiSection = oldAdmin, oldLocalAI
		platformDenyResetAdminCache()
		platformDenyExtra.mu.Lock()
		platformDenyExtra.addrs = nil
		platformDenyExtra.mu.Unlock()
	})
	platformDenyAdminURL = func(string) string { return "http://127.0.0.1:41999" }
	platformDenyResetAdminCache()
	localaiSection = localai.Config{Endpoint: "http://127.0.0.1:18080"}

	got := strings.Join(platformDenyConfigured(), " ")
	for name, want := range map[string]string{
		"the gateway listen address": config.Core().Server.Listen,
		"the metrics listener":       metrics.FromConfig().Listen,
		"the configured LocalAI URL": "http://127.0.0.1:18080",
		"the admin interface":        "http://127.0.0.1:41999",
	} {
		if want == "" || !strings.Contains(got, want) {
			t.Errorf("%s (%q) is not on the platform deny-list %q", name, want, got)
		}
	}

	// an unset LocalAI endpoint means the adapter's default, which is still LocalAI
	localaiSection = localai.Config{}
	if got := strings.Join(platformDenyConfigured(), " "); !strings.Contains(got, localai.DefaultEndpoint) {
		t.Errorf("an unset LocalAI endpoint did not put the default %s on the list: %s", localai.DefaultEndpoint, got)
	}

	// the live list refuses the admin port and the default LocalAI port for a dialer that lists them
	for _, port := range []int{41999, 8080} {
		a, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{port}, Scheme: egress.SchemeHTTP})
		if err != nil {
			t.Fatal(err)
		}
		if got := platformDenyDial(t, egress.NewDialer(a, platformDenyOption()), port); got != egress.ReasonPlatformPort {
			t.Errorf("port %d: dial reason %q, want platform_port", port, got)
		}
	}

	// a listener serve registers is denied, once however often it is registered
	platformDenyAddListener("127.0.0.1:42777")
	platformDenyAddListener("127.0.0.1:42777")
	platformDenyAddListener("")
	if got := platformDenyExtraAddrs(); len(got) != 1 || got[0] != "127.0.0.1:42777" {
		t.Errorf("registered listeners = %v, want [127.0.0.1:42777]", got)
	}
	a, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{42777}, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	if got := platformDenyDial(t, egress.NewDialer(a, platformDenyOption()), 42777); got != egress.ReasonPlatformPort {
		t.Errorf("registered listener: dial reason %q, want platform_port", got)
	}
}

// lastWireAllowing lists a loopback port on an allowlist, the way a package's own endpoint is listed.
func lastWireAllowing(t testing.TB, ports ...int) *egress.Allowlist {
	t.Helper()
	a, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: ports, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// PT-001. `alixai serve` denies the port of every listener it binds the moment it binds it (an ephemeral
// port is known only then), and the registry the RAG sources dial through is built with the deny-list, so a
// source that names a listed platform port is refused while its own application's port connects. Mutations
// this pins: a listener serve forgets to register (the public one, the operator admin interface); a registry
// built without platformDenyOption.
func TestServeDeniesPlatformPortsOnEveryListenerAndRegistry(t *testing.T) {
	t.Cleanup(func() {
		platformDenyExtra.mu.Lock()
		platformDenyExtra.addrs = nil
		platformDenyExtra.mu.Unlock()
		platformDenyResetAdminCache()
	})
	platformDenyExtra.mu.Lock()
	platformDenyExtra.addrs = nil
	platformDenyExtra.mu.Unlock()

	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, dataDir := serveConfig(t, f, "")
	srv := startServe(t, cfg)

	pub, err := url.Parse(srv.base)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := gateway.ReadAdminEndpoint(dataDir)
	if err != nil {
		t.Fatalf("the admin endpoint: %v", err)
	}
	adm, err := url.Parse(ep.URL)
	if err != nil {
		t.Fatal(err)
	}
	listeners := map[string]string{"the public listener": pub.Host, "the operator admin interface": adm.Host}
	registered := strings.Join(platformDenyExtraAddrs(), " ")
	for name, hostPort := range listeners {
		if !strings.Contains(registered, hostPort) {
			t.Errorf("serve did not register %s (%s) on the deny-list: %q", name, hostPort, registered)
		}
	}

	appPort := platformDenyListener(t)
	ports := map[string]int{}
	for name, hostPort := range listeners {
		_, p, err := net.SplitHostPort(hostPort)
		if err != nil {
			t.Fatal(err)
		}
		if ports[name], err = strconv.Atoi(p); err != nil {
			t.Fatal(err)
		}
	}

	// the registry of the RAG sources: every listener is refused even when the application lists it
	nw := ragServeNewNetwork(nil)
	if err := nw.egress.Set(platformDenyApp, lastWireAllowing(t, ports["the public listener"], ports["the operator admin interface"], appPort)); err != nil {
		t.Fatal(err)
	}
	d := nw.egress.Dialer(platformDenyApp)
	for name, port := range ports {
		if got := platformDenyDial(t, d, port); got != egress.ReasonPlatformPort {
			t.Errorf("RAG registry, %s: dial reason %q, want platform_port", name, got)
		}
	}
	if got := platformDenyDial(t, d, appPort); got != "" {
		t.Errorf("RAG registry, the application's own port: dial reason %q, want a connection", got)
	}
}
