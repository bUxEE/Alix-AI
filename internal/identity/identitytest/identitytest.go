// Package identitytest mints application context tokens for tests of the
// verifier, the gateway, the SDKs and the reference apps, and wires a ready
// Verifier around them. It is test support: import it only from _test.go
// files (internal/identity's own tests enforce that nothing else in the module
// does). Keys are generated per test, never fixtures.
//
// A Draft starts as a valid token and can be bent into any malformed one:
// drop or override a claim or header member, sign with another key, or write
// the JSON by hand.
package identitytest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Defaults a Draft starts from. Tests assert against these constants rather
// than re-typing literals.
const (
	DefaultDeployment    = "dep_test"
	DefaultTenant        = "ten_acme"
	DefaultWorkspace     = "wsp_main"
	DefaultPrincipal     = "usr_alice"
	DefaultSession       = "ses_test_01"
	DefaultLifetime      = 5 * time.Minute
	DefaultApplicationID = "app_demo"
)

// DefaultRoles and DefaultCapabilityScopes are the context a Draft carries.
var (
	DefaultRoles            = []string{"member"}
	DefaultCapabilityScopes = []string{"reservations.read", "reservations.create"}
)

var b64 = base64.RawURLEncoding

// IssuerConfig configures NewIssuer; every field has a default.
type IssuerConfig struct {
	// ApplicationID defaults to DefaultApplicationID.
	ApplicationID string
	// Issuer is the iss value; default "https://<application id>.example.test".
	Issuer string
	// KeyID defaults to "<application id>-key-1".
	KeyID string
	// Audience is the aud value; default DefaultDeployment.
	Audience string
	// Clock stamps iat, nbf and exp; default a fake clock at a fixed instant.
	Clock clock.Clock
	// Key, if set, is the signing key instead of a freshly generated one. Use
	// it only for reproducible fuzz corpora; never commit a real key.
	Key ed25519.PrivateKey
}

// Issuer stands in for a host application: it holds a freshly generated
// Ed25519 key and mints tokens with it.
type Issuer struct {
	ApplicationID string
	Issuer        string
	KeyID         string
	Audience      string
	Clock         clock.Clock
	Public        ed25519.PublicKey
	private       ed25519.PrivateKey
	tb            testing.TB
}

// NewIssuer generates a key and returns an Issuer for the configured
// application.
func NewIssuer(tb testing.TB, c IssuerConfig) *Issuer {
	tb.Helper()
	priv := c.Key
	if priv == nil {
		var err error
		if _, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			tb.Fatalf("identitytest: generating a test key: %v", err)
		}
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	if c.ApplicationID == "" {
		c.ApplicationID = DefaultApplicationID
	}
	if c.Issuer == "" {
		c.Issuer = "https://" + c.ApplicationID + ".example.test"
	}
	if c.KeyID == "" {
		c.KeyID = c.ApplicationID + "-key-1"
	}
	if c.Audience == "" {
		c.Audience = DefaultDeployment
	}
	if c.Clock == nil {
		c.Clock = clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	}
	return &Issuer{
		ApplicationID: c.ApplicationID, Issuer: c.Issuer, KeyID: c.KeyID, Audience: c.Audience,
		Clock: c.Clock, Public: pub, private: priv, tb: tb,
	}
}

// Key returns the registry entry for this issuer's public key.
func (i *Issuer) Key() identity.IssuerKey {
	return identity.IssuerKey{KeyID: i.KeyID, ApplicationID: i.ApplicationID, Issuer: i.Issuer, Public: append(ed25519.PublicKey(nil), i.Public...)}
}

// PrivateKey returns the signing key, for tests that sign by hand.
func (i *Issuer) PrivateKey() ed25519.PrivateKey { return i.private }

// Token mints a valid token with the default context and a fresh jti, then
// applies mods.
func (i *Issuer) Token(mods ...func(*Draft)) string {
	d := i.Draft()
	for _, m := range mods {
		m(d)
	}
	return d.Sign()
}

// Draft returns a valid, unsigned token description to modify.
func (i *Issuer) Draft() *Draft {
	now := i.Clock.Now()
	d := &Draft{
		issuer: i,
		key:    i.private,
		Header: map[string]any{
			identity.HeaderAlgorithm: identity.Algorithm,
			identity.HeaderType:      identity.TokenType,
			identity.HeaderKeyID:     i.KeyID,
		},
		Claims: map[string]any{
			identity.ClaimIssuer:           i.Issuer,
			identity.ClaimAudience:         i.Audience,
			identity.ClaimSubject:          DefaultPrincipal,
			identity.ClaimIssuedAt:         now.Unix(),
			identity.ClaimNotBefore:        now.Unix(),
			identity.ClaimExpiry:           now.Add(DefaultLifetime).Unix(),
			identity.ClaimTokenID:          NewTokenID(),
			identity.ClaimApplication:      i.ApplicationID,
			identity.ClaimTenant:           DefaultTenant,
			identity.ClaimWorkspace:        DefaultWorkspace,
			identity.ClaimRoles:            append([]string(nil), DefaultRoles...),
			identity.ClaimCapabilityScopes: append([]string(nil), DefaultCapabilityScopes...),
			identity.ClaimSession:          DefaultSession,
		},
	}
	return d
}

// NewTokenID returns a fresh random jti (22 base64url characters).
func NewTokenID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("identitytest: no entropy: " + err.Error())
	}
	return b64.EncodeToString(b[:])
}

// Draft is a token under construction. Header and Claims are plain maps so a
// test can set, replace or delete any member.
type Draft struct {
	Header map[string]any
	Claims map[string]any
	issuer *Issuer
	key    ed25519.PrivateKey
}

// Set sets a claim.
func (d *Draft) Set(claim string, value any) *Draft { d.Claims[claim] = value; return d }

// Drop removes a claim.
func (d *Draft) Drop(claim string) *Draft { delete(d.Claims, claim); return d }

// SetHeader sets a header member.
func (d *Draft) SetHeader(member string, value any) *Draft { d.Header[member] = value; return d }

// DropHeader removes a header member.
func (d *Draft) DropHeader(member string) *Draft { delete(d.Header, member); return d }

// Times sets iat, nbf and exp from offsets relative to the issuer's clock.
func (d *Draft) Times(iat, nbf, exp time.Duration) *Draft {
	now := d.issuer.Clock.Now()
	d.Claims[identity.ClaimIssuedAt] = now.Add(iat).Unix()
	d.Claims[identity.ClaimNotBefore] = now.Add(nbf).Unix()
	d.Claims[identity.ClaimExpiry] = now.Add(exp).Unix()
	return d
}

// Lifetime keeps iat and nbf at now and sets exp to now plus d.
func (d *Draft) Lifetime(life time.Duration) *Draft { return d.Times(0, 0, life) }

// SignWith signs with key instead of the issuer's.
func (d *Draft) SignWith(key ed25519.PrivateKey) *Draft { d.key = key; return d }

// Sign marshals the header and claims and signs them. A signature made with
// the header's own bytes is what a host's JOSE library would produce.
func (d *Draft) Sign() string {
	tb := d.issuer.tb
	tb.Helper()
	h, err := json.Marshal(d.Header)
	if err != nil {
		tb.Fatalf("identitytest: marshal header: %v", err)
	}
	p, err := json.Marshal(d.Claims)
	if err != nil {
		tb.Fatalf("identitytest: marshal claims: %v", err)
	}
	return Raw(string(h), string(p), d.key)
}

// Raw writes header and payload JSON exactly as given (duplicate members,
// odd whitespace and all) and signs them with key. A nil key leaves the
// signature segment empty, which is what an unsigned token looks like.
func Raw(headerJSON, payloadJSON string, key ed25519.PrivateKey) string {
	signingInput := b64.EncodeToString([]byte(headerJSON)) + "." + b64.EncodeToString([]byte(payloadJSON))
	if key == nil {
		return signingInput + "."
	}
	return signingInput + "." + b64.EncodeToString(ed25519.Sign(key, []byte(signingInput)))
}

// Env is a complete test deployment: a fake clock, a SQLite store with the
// replay table, a key registry holding Issuer's key and a Verifier for
// DefaultDeployment wired to all of them.
type Env struct {
	Clock    *clock.Fake
	DB       *store.DB
	Keys     *identity.KeyRegistry
	Replay   *identity.SQLiteReplayCache
	Verifier *identity.Verifier
	Issuer   *Issuer
}

// NewEnv builds an Env. mutate may adjust the verifier Options (for example the
// maximum lifetime) before the Verifier is built.
func NewEnv(tb testing.TB, mutate ...func(*identity.Options)) *Env {
	tb.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(tb.TempDir(), "identity.db")})
	if err != nil {
		tb.Fatalf("identitytest: opening the store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	keys := identity.NewKeyRegistry(nil)
	replay := identity.NewSQLiteReplayCache(db, clk, nil)
	iss := NewIssuer(tb, IssuerConfig{Clock: clk})
	if err := keys.Register(iss.Key()); err != nil {
		tb.Fatalf("identitytest: registering the test key: %v", err)
	}
	opts := identity.Options{
		DeploymentID: DefaultDeployment,
		Keys:         keys,
		Replay:       replay,
		Clock:        clk,
	}
	for _, m := range mutate {
		m(&opts)
	}
	v, err := identity.NewVerifier(opts)
	if err != nil {
		tb.Fatalf("identitytest: building the verifier: %v", err)
	}
	return &Env{Clock: clk, DB: db, Keys: keys, Replay: replay, Verifier: v, Issuer: iss}
}

// ExpectedScope is the Scope a default token must verify to.
func ExpectedScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     DefaultDeployment,
		ApplicationID:    DefaultApplicationID,
		TenantID:         DefaultTenant,
		WorkspaceID:      DefaultWorkspace,
		PrincipalID:      DefaultPrincipal,
		Roles:            append([]string(nil), DefaultRoles...),
		CapabilityScopes: append([]string(nil), DefaultCapabilityScopes...),
		SessionID:        DefaultSession,
	}
}
