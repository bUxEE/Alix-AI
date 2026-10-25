package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestConfigRefusals(t *testing.T) {
	good := func() Config { return fixtureConfig(t, NetworkTCP, "127.0.0.1:7001") }
	tests := map[string]func(*Config){
		"no application":      func(c *Config) { c.ApplicationID = "" },
		"no endpoint":         func(c *Config) { c.Endpoints = nil },
		"a remote host":       func(c *Config) { c.Endpoints[0].Address = "10.0.0.5:7001" },
		"a DNS name":          func(c *Config) { c.Endpoints[0].Address = "example.com:7001" },
		"a port out of range": func(c *Config) { c.Endpoints[0].Address = "127.0.0.1:0" },
		"a relative socket":   func(c *Config) { c.Endpoints[0].Network, c.Endpoints[0].Address = NetworkUnix, "run/b.sock" },
		"a long socket path": func(c *Config) {
			c.Endpoints[0].Network, c.Endpoints[0].Address = NetworkUnix, "/"+strings.Repeat("a", 120)
		},
		"a named pipe":       func(c *Config) { c.Endpoints[0].Network = "pipe" },
		"no key":             func(c *Config) { c.Endpoints[0].Key = Secret{} },
		"a short key":        func(c *Config) { c.Endpoints[0].Key = NewSecret([]byte("short")) },
		"a malformed key id": func(c *Config) { c.Endpoints[0].KeyID = "bad id" },
		"a reserved method": func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, Method: "alixai.describe"}
		},
		"an unknown endpoint": func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: "nope", Method: "m"}
		},
		"a malformed method": func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, Method: "a b"}
		},
		"a repeated provisioning": func(c *Config) { c.Endpoints[0].Provisioning = []string{"seed", "seed"} },
		"two renames to one": func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, Method: "m", Arguments: map[string]string{"a": "x", "b": "x"}}
		},
	}
	for name, mutate := range tests {
		cfg := good()
		mutate(&cfg)
		if _, err := New(cfg); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
	}
	if _, err := New(good()); err != nil {
		t.Fatal(err)
	}
}

func TestParseAdapterConfig(t *testing.T) {
	ok := "server: " + testEP + "\nkey:\n  id: k1\n  ref: env:LEDGER_KEY\npeer_uid: 1000\nmanagement:\n  provisioning: [seed, create_tenant]\n"
	c, err := ParseAdapterConfig([]byte(ok), testEP)
	if err != nil || c.Key.ID != "k1" || c.Key.Ref != "env:LEDGER_KEY" || *c.PeerUID != 1000 || len(c.Management.Provisioning) != 2 || c.Management.Provisioning[0] != "create_tenant" {
		t.Fatalf("%+v, %v", c, err)
	}
	for name, doc := range map[string]string{
		"a literal key":       "key:\n  id: k1\n  ref: " + testSecret + "\n",
		"no key":              "server: " + testEP + "\n",
		"an unknown member":   "key:\n  id: k1\n  ref: env:K\ntoken: x\n",
		"another endpoint":    "server: other\nkey:\n  id: k1\n  ref: env:K\n",
		"a second document":   "key:\n  id: k1\n  ref: env:K\n---\nkey: {}\n",
		"a relative file ref": "key:\n  id: k1\n  ref: file:keys/a.key\n",
		"a repeated name":     "key:\n  id: k1\n  ref: env:K\nmanagement:\n  provisioning: [a, a]\n",
		"a negative uid":      "key:\n  id: k1\n  ref: env:K\npeer_uid: -1\n",
	} {
		if _, err := ParseAdapterConfig([]byte(doc), testEP); err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), testSecret) {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
}

func TestResolveKey(t *testing.T) {
	t.Setenv("NATIVE_TEST_KEY", testSecret)
	if s, err := ResolveKey("env:NATIVE_TEST_KEY"); err != nil || string(s.value()) != testSecret {
		t.Fatalf("env: %v", err)
	}
	t.Setenv("NATIVE_TEST_SHORT", "short")
	for _, ref := range []string{"env:NATIVE_TEST_SHORT", "env:NATIVE_TEST_UNSET", "env:1bad", testSecret, "file:relative", ""} {
		if _, err := ResolveKey(ref); err == nil {
			t.Errorf("ResolveKey(%q) succeeded", ref)
		} else if strings.Contains(err.Error(), testSecret) {
			t.Errorf("the error for %q repeats the key", ref)
		}
	}
	dir := shortDir(t)
	p := filepath.Join(dir, "k.key")
	if err := os.WriteFile(p, []byte(testSecret+"\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ResolveKey("file:" + p); err != nil || string(s.value()) != testSecret {
		t.Fatalf("file: %v", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(p, 0o640)
		if _, err := ResolveKey("file:" + p); err == nil {
			t.Error("a key file that group can read was accepted")
		}
	}
	if err := keyFileModeOK(0o640, "linux"); err == nil {
		t.Error("policy: a group-readable key file")
	}
	if err := keyFileModeOK(0o600, "linux"); err != nil {
		t.Error("policy: an owner-only key file")
	}
	if err := keyFileModeOK(0o666, "windows"); err != nil {
		t.Error("policy: Windows modes carry no bits")
	}
}

// SEC-081 neighbour: no way of printing the key shows it.
func TestSecretIsRedacted(t *testing.T) {
	cfg := fixtureConfig(t, NetworkTCP, "127.0.0.1:7001")
	for _, s := range []string{
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprint(cfg.Endpoints[0].Key),
	} {
		if strings.Contains(s, testSecret) {
			t.Fatalf("the key is printed: %.80s", s)
		}
	}
	b, _ := cfg.Endpoints[0].Key.MarshalJSON()
	if strings.Contains(string(b), testSecret) {
		t.Error("the key is marshalled")
	}
}

func TestPeerAuthenticator(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC))
	pa, err := NewPeerAuthenticator("dep_t", clk, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	peer := PeerSession{ApplicationID: testApp, KeyID: testKeyID, Issuer: "ledger"}
	claims := identity.ContextClaims{
		ApplicationID: testApp, TenantID: "ten_a", WorkspaceID: "wks_a", PrincipalID: "usr_alice",
		Roles: []string{"member", "member"}, CapabilityScopes: []string{fake.ReadCapability}, SessionID: "ses_alice",
	}
	cred, err := pa.Admit(peer, claims)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pa.Authenticate(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := identity.NewScope("dep_t", claims)
	if v.Scope.DeploymentID != "dep_t" || v.Scope.TenantID != want.TenantID || len(v.Scope.Roles) != 1 || v.Issuer != "ledger" || v.KeyID != testKeyID || !v.ExpiresAt.After(v.IssuedAt) {
		t.Errorf("verified = %+v", v)
	}
	if _, err := pa.Authenticate(context.Background(), cred); !contracts.HasCode(err, contracts.ErrUnauthenticated) || identity.ReasonOf(err) != identity.ReasonReplayed {
		t.Errorf("a second redemption: %v, want unauthenticated (replayed)", err)
	}

	cred, _ = pa.Admit(peer, claims)
	clk.Advance(11 * time.Second)
	if _, err := pa.Authenticate(context.Background(), cred); identity.ReasonOf(err) != identity.ReasonExpired {
		t.Errorf("an old ticket: %v, want expired", err)
	}
	if _, err := pa.Authenticate(context.Background(), "nope"); !contracts.HasCode(err, contracts.ErrUnauthenticated) {
		t.Errorf("a made-up ticket: %v", err)
	}

	other := claims
	other.ApplicationID = "app_other"
	if _, err := pa.Admit(peer, other); !contracts.HasCode(err, contracts.ErrScopeViolation) {
		t.Errorf("a peer asserting another application: %v, want scope_violation", err)
	}
	wild := claims
	wild.PrincipalID = "*"
	if _, err := pa.Admit(peer, wild); !contracts.HasCode(err, contracts.ErrScopeViolation) {
		t.Errorf("a wildcard principal: %v, want scope_violation", err)
	}
	missing := claims
	missing.TenantID = ""
	if _, err := pa.Admit(peer, missing); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Errorf("no tenant: %v, want scope_missing", err)
	}
	if _, err := pa.Admit(PeerSession{ApplicationID: testApp}, claims); !contracts.HasCode(err, contracts.ErrUnauthenticated) {
		t.Errorf("an unauthenticated session: %v", err)
	}
	if _, err := NewPeerAuthenticator("", nil, 0); err == nil {
		t.Error("no deployment")
	}
	if _, err := NewPeerAuthenticator("dep_t", nil, time.Hour); err == nil {
		t.Error("a ticket that lives an hour")
	}
}

// Close ends calls in flight and refuses new ones, without a hang, and entity
// resolution goes to the declared resolver only.
func TestCloseAndResolve(t *testing.T) {
	app := fake.NewApp()
	h := startHost(t, NetworkTCP, app)
	cfg := fixtureConfig(t, NetworkTCP, h.addr)
	cfg.Capabilities[0].Resolve = map[string]Binding{"item": {Endpoint: testEP, Method: "items_get"}}
	a := newAdapter(t, cfg)
	w := wrapped(t, a)

	readCap := fake.ReadCapability
	for _, c := range cfg.Capabilities {
		if c.Resolve != nil {
			readCap = c.ID
		}
	}
	before := len(app.Calls())
	if _, err := w.Read(context.Background(), testScope(), adapters.ReadRequest{CapabilityID: readCap, Fields: map[string]any{"entity_type": "patient", "mention": "x"}}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Errorf("an entity type with no resolver: %v, want validation_failed", err)
	}
	if len(app.Calls()) != before {
		t.Error("a resolver that is not declared was called")
	}

	app.Inject(adapters.OpRead, fake.FaultHang)
	done := make(chan error, 1)
	go func() {
		_, err := w.Read(context.Background(), testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "item_1"}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	_ = a.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a call survived Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not end a call in flight")
	}
	err := readItem(w, context.Background())
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable {
		t.Errorf("a call after Close: %v, want unavailable", err)
	}
}
