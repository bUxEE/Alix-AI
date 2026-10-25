package extprovidersec

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// fixedKeys is a key source that always returns the canary.
type fixedKeys struct{}

func (fixedKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(keyCanary), nil
}

// adapterTo builds the real OpenAI adapter for base, whose network allowlist lists only allow (a base URL).
func adapterTo(t *testing.T, base, allow string) *openai.Client {
	t.Helper()
	dest, err := egress.DestinationFromURL(allow)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(openai.ProviderName, al); err != nil {
		t.Fatal(err)
	}
	c, err := openai.New(openai.Config{BaseURL: base, CredentialRef: "cred_" + strings.Repeat("0", 25) + "A", Model: "gpt-5-mini"}, // TEST-ONLY opaque reference id
		reg.Dialer(openai.ProviderName), fixedKeys{},
		openai.WithSleeper(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// SEC-124
func TestSecExtNetworkMetadataLeavesOnlyToAllowlistedDestination(t *testing.T) {
	sc := receptionist()

	t.Run("control: the request carries a fixed small set of headers and no identity of the user, tenant or machine", func(t *testing.T) {
		k := newKeyed(t, adaptiveReply)
		k.say(sc, sayFree)
		k.say(sc, sayFree+" again")
		reqs := k.srv.requests()
		if len(reqs) < 2 {
			t.Fatalf("provider requests = %d: the control must reach the provider", len(reqs))
		}
		allowed := []string{"Accept", "Accept-Encoding", "Authorization", "Content-Length", "Content-Type", "User-Agent"}
		host, _ := os.Hostname()
		for i, r := range reqs {
			var names []string
			for name := range r.Header {
				names = append(names, name)
				if !slices.Contains(allowed, name) {
					t.Fatalf("request %d carries the header %s: only %v may leave", i, name, allowed)
				}
			}
			if r.Header.Get("User-Agent") != "alixai-openai-adapter" {
				t.Fatalf("User-Agent %q: no version, host or user information", r.Header.Get("User-Agent"))
			}
			wire := r.Path + "?" + r.RawQuery + " " + strings.Join(names, ",")
			for _, vs := range r.Header {
				wire += " " + strings.Join(vs, ",")
			}
			wire = strings.ReplaceAll(wire, keyCanary, "") // the key is the one value that is meant to be there
			for _, id := range []string{sc.TenantID, sc.PrincipalID, sc.SessionID, sc.WorkspaceID, sc.DeploymentID, tenantTwo().TenantID, tenantTwo().PrincipalID, tenantTwo().SessionID} {
				if strings.Contains(wire, id) || strings.Contains(string(r.Body), id) {
					t.Fatalf("request %d carries %q", i, id)
				}
			}
			if host != "" && (strings.Contains(wire, host) || strings.Contains(string(r.Body), host)) {
				t.Fatalf("request %d carries the host name of this machine", i)
			}
			if !json.Valid(r.Body) {
				t.Fatalf("request %d body is not the JSON the adapter builds", i)
			}
		}
	})

	t.Run("a provider whose base URL is not on the network allowlist is never contacted", func(t *testing.T) {
		listed := newProvider(t, adaptiveReply)
		unlisted := newProvider(t, adaptiveReply)
		c := adapterTo(t, unlisted.srv.URL, listed.srv.URL)
		_, err := c.Generate(ctxb(), sc, simpleRich("hello"))
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Fatalf("err %v, want permission_denied from the egress dialer", err)
		}
		if len(unlisted.requests()) != 0 || len(listed.requests()) != 0 {
			t.Fatalf("requests: unlisted %d listed %d: nothing may be sent", len(unlisted.requests()), len(listed.requests()))
		}
		// Control: the same adapter pointed at the listed destination works.
		if _, err := adapterTo(t, listed.srv.URL, listed.srv.URL).Generate(ctxb(), sc, simpleRich("hello")); err != nil {
			t.Fatalf("control: %v", err)
		}
	})

	t.Run("a provider with no allowlist reaches nothing, a revoked one stops at once, and a wildcard cannot be listed", func(t *testing.T) {
		p := newProvider(t, adaptiveReply)
		reg := egress.NewProviderRegistry()
		if _, err := reg.Dialer("anthropic").DialContext(ctxb(), "tcp", strings.TrimPrefix(p.srv.URL, "http://")); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Fatalf("a provider with no allowlist dialled: %v", err)
		}
		dest, _ := egress.DestinationFromURL(p.srv.URL)
		al, _ := egress.NewAllowlist(dest)
		_ = reg.Set("openai", al)
		conn, err := reg.Dialer("openai").DialContext(ctxb(), "tcp", strings.TrimPrefix(p.srv.URL, "http://"))
		if err != nil {
			t.Fatalf("control: the listed destination: %v", err)
		}
		conn.Close()
		reg.Revoke("openai")
		if _, err := reg.Dialer("openai").DialContext(ctxb(), "tcp", strings.TrimPrefix(p.srv.URL, "http://")); err == nil {
			t.Fatal("a revoked provider dialled")
		}
		for _, bad := range []egress.Destination{{Host: "*", Ports: []int{443}, Scheme: "https"}, {Host: "*.openai.com", Ports: []int{443}, Scheme: "https"}, {Host: "openai.com", Scheme: "https"}} {
			if _, err := egress.NewAllowlist(bad); err == nil {
				t.Fatalf("destination %+v was accepted", bad)
			}
		}
	})

	t.Run("cloud metadata and link-local addresses cannot be dialled even when an operator lists them", func(t *testing.T) {
		refused := 0
		for _, addr := range []string{"169.254.169.254", "169.254.170.2", "168.63.129.16", "100.100.100.200", "[fd00:ec2::254]"} {
			host := strings.Trim(addr, "[]")
			al, err := egress.NewAllowlist(egress.Destination{Host: host, Ports: []int{80}, Scheme: "https"})
			if err != nil {
				refused++ // refused at configuration time, which is also a refusal
				continue
			}
			reg := egress.NewProviderRegistry()
			_ = reg.Set("openai", al)
			conn, err := reg.Dialer("openai").DialContext(ctxb(), "tcp", net.JoinHostPort(host, "80"))
			if err == nil {
				conn.Close()
				t.Fatalf("%s was dialled", addr)
			}
			if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
				t.Fatalf("%s: %v, want a refusal by the dialer before any packet", addr, err)
			}
			refused++
		}
		if refused != 5 {
			t.Fatalf("refused %d of 5 metadata addresses", refused)
		}
	})

	t.Run("certificates are verified: a server with an untrusted certificate gets no request and no key", func(t *testing.T) {
		var handled atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handled.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		c := adapterTo(t, srv.URL, srv.URL)
		if _, err := c.Generate(ctxb(), sc, simpleRich("hello")); err == nil {
			t.Fatal("a request to a server with an untrusted certificate succeeded")
		}
		if handled.Load() != 0 {
			t.Fatalf("the server handled %d requests: the key and the prompt left over an unverified connection", handled.Load())
		}
	})

	t.Run("the transport ignores the proxy environment, never skips verification and does not speak TLS below 1.2", func(t *testing.T) {
		d := egress.NewProviderRegistry().Dialer("openai")
		proxyNil, skip, minVersion := transportFacts(t, d.HTTPClient(0))
		if !proxyNil {
			t.Fatal("the transport honours the proxy environment: a proxy would receive the dial and bypass the allowlist")
		}
		if skip || minVersion < tls.VersionTLS12 {
			t.Fatalf("skip verification %v, minimum TLS version %#x", skip, minVersion)
		}
		// Even a config that asks to skip verification is overridden.
		weak := egress.NewProviderRegistry(egress.WithTLSConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}))
		_, skip, minVersion = transportFacts(t, weak.Dialer("openai").HTTPClient(0))
		if skip || minVersion < tls.VersionTLS12 {
			t.Fatalf("a weak tls config was honoured: skip %v minimum %#x", skip, minVersion)
		}
	})
}

// transportFacts reads, by reflection, what the egress transport is configured with: the package keeps the transport
// unexported on purpose, and the facts that matter are its proxy, its certificate check and its minimum TLS version.
func transportFacts(t *testing.T, c *http.Client) (proxyNil, skipVerify bool, minVersion uint64) {
	t.Helper()
	guard := reflect.ValueOf(c.Transport)
	if guard.Kind() != reflect.Pointer || guard.IsNil() {
		t.Fatalf("transport %T", c.Transport)
	}
	next := guard.Elem().FieldByName("next")
	if !next.IsValid() || next.Kind() != reflect.Pointer {
		t.Fatal("the egress transport changed shape: update this check")
	}
	tr := next.Elem()
	cfg := tr.FieldByName("TLSClientConfig")
	if !cfg.IsValid() || cfg.IsNil() {
		t.Fatal("the egress transport has no TLS config")
	}
	return tr.FieldByName("Proxy").IsNil(), cfg.Elem().FieldByName("InsecureSkipVerify").Bool(), cfg.Elem().FieldByName("MinVersion").Uint()
}
