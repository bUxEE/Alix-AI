package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// memReplay is a ReplayCache in memory: a token is single use.
type memReplay struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (m *memReplay) Consume(_ context.Context, _ contracts.Scope, jti string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[jti] {
		return contracts.New(contracts.ErrReplayed, "replayed")
	}
	m.seen[jti] = true
	return nil
}

func testIdentity() TokenIdentity {
	return TokenIdentity{
		Issuer: "https://clinic-booking.example.test", Audience: "dep_soak", KeyID: "clinic-soak-key-1",
		Application: "clinic-booking", Tenant: "ten_clinic", Workspace: "wsp_main", Principal: "usr_soak",
		Roles: []string{"receptionist"}, Scopes: []string{"availability.list", "patients.find"},
	}
}

// The tokens the harness mints are accepted by the verifier the stack runs: the
// exact profile of ADR 0013, single use, scope and capability scopes as given.
func TestMintedTokensVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity()
	m, err := NewMinter(priv, id, "ses_soak_ab12cd34")
	if err != nil {
		t.Fatal(err)
	}
	keys := identity.NewKeyRegistry(nil)
	if err := keys.Register(identity.IssuerKey{KeyID: id.KeyID, ApplicationID: id.Application, Issuer: id.Issuer, Public: pub}); err != nil {
		t.Fatal(err)
	}
	v, err := identity.NewVerifier(identity.Options{DeploymentID: id.Audience, Keys: keys, Replay: &memReplay{seen: map[string]bool{}}})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := m.Mint("ops.queue.read")
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("the verifier refused the token: %v", err)
	}
	s := got.Scope
	if s.ApplicationID != "clinic-booking" || s.TenantID != "ten_clinic" || s.WorkspaceID != "wsp_main" || s.PrincipalID != "usr_soak" ||
		s.SessionID != "ses_soak_ab12cd34" || s.DeploymentID != "dep_soak" {
		t.Errorf("scope %+v", s)
	}
	if want := []string{"availability.list", "patients.find", "ops.queue.read"}; !slices.Equal(s.CapabilityScopes, want) {
		t.Errorf("capability scopes %v, want %v", s.CapabilityScopes, want)
	}
	if life := got.ExpiresAt.Sub(got.IssuedAt); life <= 0 || life > identity.MaxLifetime {
		t.Errorf("lifetime %s", life)
	}
	// single use: presenting it again is refused
	if _, err := v.Verify(context.Background(), tok); !contracts.HasCode(err, contracts.ErrReplayed) {
		t.Errorf("a second presentation: %v", err)
	}
	// every call mints another jti; the extra scope is for that token only
	tok2, _ := m.Mint()
	got2, err := v.Verify(context.Background(), tok2)
	if err != nil || got2.TokenID == got.TokenID || slices.Contains(got2.Scope.CapabilityScopes, "ops.queue.read") {
		t.Errorf("second token: %+v, %v", got2, err)
	}

	// the header and the claim set are closed
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	var header, claims map[string]any
	for i, dst := range []*map[string]any{&header, &claims} {
		raw, err := tokenEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, dst) != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
	}
	if len(header) != 3 || header["alg"] != "EdDSA" || header["typ"] != "alixai-context+jwt" || header["kid"] != id.KeyID {
		t.Errorf("header %v", header)
	}
	if len(claims) != 13 {
		t.Errorf("%d claims: %v", len(claims), claims)
	}
	if _, has := claims["deployment_id"]; has {
		t.Error("the token carries a deployment level")
	}
}

func TestNewMinterRefuses(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	for name, edit := range map[string]func(*TokenIdentity){
		"no issuer":      func(i *TokenIdentity) { i.Issuer = "" },
		"no audience":    func(i *TokenIdentity) { i.Audience = "" },
		"no key id":      func(i *TokenIdentity) { i.KeyID = "" },
		"bad tenant":     func(i *TokenIdentity) { i.Tenant = "ten clinic" },
		"no principal":   func(i *TokenIdentity) { i.Principal = "" },
		"no application": func(i *TokenIdentity) { i.Application = "" },
	} {
		id := testIdentity()
		edit(&id)
		if _, err := NewMinter(priv, id, "ses_soak_ab12cd34"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewMinter(nil, testIdentity(), "ses_soak_ab12cd34"); err == nil {
		t.Error("no key: accepted")
	}
	if _, err := NewMinter(priv, testIdentity(), ""); err == nil {
		t.Error("no session: accepted")
	}
}

func writePEM(t *testing.T, dir, name string, der []byte, typ string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadIssuerKey(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	got, err := LoadIssuerKey(writePEM(t, dir, "ed.pem", der, "PRIVATE KEY"))
	if err != nil || !priv.Equal(got) {
		t.Errorf("an Ed25519 key: %v", err)
	}
	// text before the block is ignored (the repository's test key carries a warning there)
	if _, err := LoadIssuerKey(filepath.Join("..", "devkeys", "test-only-publisher.key.pem")); err != nil {
		t.Errorf("the devkeys file: %v", err)
	}

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	for name, path := range map[string]string{
		"no path":      "",
		"missing file": filepath.Join(dir, "nope.pem"),
		"rsa key":      writePEM(t, dir, "rsa.pem", rsaDER, "PRIVATE KEY"),
		"wrong block":  writePEM(t, dir, "pub.pem", der, "PUBLIC KEY"),
	} {
		if _, err := LoadIssuerKey(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// there is no default key: with no path the harness does not start
	if _, err := LoadIssuerKey(""); err == nil || !strings.Contains(err.Error(), "no default key") {
		t.Errorf("empty path: %v", err)
	}
}
