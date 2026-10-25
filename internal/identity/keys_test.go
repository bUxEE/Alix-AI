package identity_test

import (
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestKeyRegistryRegister(t *testing.T) {
	iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{})
	good := iss.Key()

	reg := identity.NewKeyRegistry(nil)
	if err := reg.Register(good); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(good); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Fatalf("duplicate key id: err = %v, want conflict", err)
	}

	bad := map[string]func(*identity.IssuerKey){
		"empty kid":            func(k *identity.IssuerKey) { k.KeyID = "" },
		"kid with a space":     func(k *identity.IssuerKey) { k.KeyID = "a b" },
		"kid too long":         func(k *identity.IssuerKey) { k.KeyID = strings.Repeat("k", 65) },
		"empty application":    func(k *identity.IssuerKey) { k.ApplicationID = "" },
		"wildcard application": func(k *identity.IssuerKey) { k.ApplicationID = "*" },
		"padded application":   func(k *identity.IssuerKey) { k.ApplicationID = "app " },
		"empty issuer":         func(k *identity.IssuerKey) { k.Issuer = "" },
		"short public key":     func(k *identity.IssuerKey) { k.Public = make([]byte, 31) },
		"long public key":      func(k *identity.IssuerKey) { k.Public = make([]byte, 33) },
		"no public key":        func(k *identity.IssuerKey) { k.Public = nil },
	}
	for name, mod := range bad {
		t.Run(name, func(t *testing.T) {
			k := good
			k.KeyID = "fresh-" + strings.ReplaceAll(name, " ", "-")
			mod(&k)
			if err := identity.NewKeyRegistry(nil).Register(k); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Fatalf("Register = %v, want validation_failed", err)
			}
		})
	}
}

// The registry keeps its own copy of key bytes, and hands out copies.
func TestKeyRegistryCopiesKeyBytes(t *testing.T) {
	iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{})
	k := iss.Key()
	reg := identity.NewKeyRegistry(nil)
	if err := reg.Register(k); err != nil {
		t.Fatal(err)
	}
	k.Public[0] ^= 0xff // the caller's slice changes after registration

	got, err := reg.Lookup(bg, iss.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Public.Equal(iss.Public) {
		t.Fatal("the registry aliased the caller's key bytes")
	}
	got.Public[0] ^= 0xff
	again, _ := reg.Lookup(bg, iss.KeyID)
	if !again.Public.Equal(iss.Public) {
		t.Fatal("Lookup returned the registry's own slice")
	}
	if _, err := reg.Lookup(bg, "missing"); err != identity.ErrKeyNotFound {
		t.Fatalf("Lookup(missing) = %v, want ErrKeyNotFound", err)
	}
}

// Several keys of one application may be active during a rotation; each
// verifies only its own signatures and only for its own application.
func TestKeyRotationOverlapIsBounded(t *testing.T) {
	env := identitytest.NewEnv(t)
	next := identitytest.NewIssuer(t, identitytest.IssuerConfig{KeyID: "app_demo-key-2", Clock: env.Clock})
	if err := env.Keys.Register(next.Key()); err != nil {
		t.Fatal(err)
	}
	// Both keys verify during the overlap.
	for _, i := range []*identitytest.Issuer{env.Issuer, next} {
		if _, err := env.Verifier.Verify(bg, i.Token()); err != nil {
			t.Fatalf("key %s rejected during overlap: %v", i.KeyID, err)
		}
	}
	// A key does not speak for a key id it was not registered under.
	forged := next.Draft().SetHeader(identity.HeaderKeyID, env.Issuer.KeyID).Sign()
	_, err := env.Verifier.Verify(bg, forged)
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonBadSignature)

	// The old key is retired at a known instant; the new one carries on.
	if err := env.Keys.Retire(env.Issuer.KeyID, env.Clock.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(time.Minute)
	_, err = env.Verifier.Verify(bg, env.Issuer.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonKeyRetired)
	if _, err := env.Verifier.Verify(bg, next.Token()); err != nil {
		t.Fatalf("the new key was rejected after the old one retired: %v", err)
	}
}

// The identity section is registered with the configuration loader and its
// environment overrides work.
func TestConfigSectionRegistered(t *testing.T) {
	t.Cleanup(func() { _ = config.Default.LoadFrom("", nil) })
	env := map[string]string{
		"ALIXAI_IDENTITY_DEPLOYMENT_ID":      "dep_from_env",
		"ALIXAI_IDENTITY_MAX_TOKEN_LIFETIME": "5m",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if err := config.Default.LoadFrom("", lookup); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	got := identity.CurrentConfig()
	if got.DeploymentID != "dep_from_env" || got.MaxTokenLifetime != 5*time.Minute {
		t.Fatalf("CurrentConfig = %+v", got)
	}

	env["ALIXAI_IDENTITY_MAX_TOKEN_LIFETIME"] = "16m"
	if err := config.Default.LoadFrom("", lookup); err == nil {
		t.Fatal("a lifetime above the ceiling loaded")
	}
	env["ALIXAI_IDENTITY_MAX_TOKEN_LIFETIME"] = "5m"
	env["ALIXAI_IDENTITY_DEPLOYMENT_ID"] = "*"
	if err := config.Default.LoadFrom("", lookup); err == nil {
		t.Fatal("a wildcard deployment id loaded")
	}
}
