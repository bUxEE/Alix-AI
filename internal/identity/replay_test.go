package identity_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The replay table follows the store's scope rule: every table carries
// application_id, tenant_id and workspace_id.
func TestReplayTableIsScoped(t *testing.T) {
	env := identitytest.NewEnv(t)
	storetest.AssertScopedTables(t, env.DB)
}

// SEC-034: an entry lives exactly as long as the token could still be
// accepted, so a nonce is refused while it matters and the cache does not
// grow without bound.
func TestReplayEntryExpiresWithToken(t *testing.T) {
	env := identitytest.NewEnv(t)
	const jti = "expiring-nonce-0123456"

	first := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti) })
	if _, err := env.Verifier.Verify(bg, first); err != nil {
		t.Fatal(err)
	}

	// Still inside exp + skew: the nonce is held.
	env.Clock.Advance(identitytest.DefaultLifetime + identity.ClockSkew - time.Second)
	again := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti) })
	_, err := env.Verifier.Verify(bg, again)
	wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)

	// Past it, the old token could not be accepted anyway; the nonce is free.
	env.Clock.Advance(2 * time.Second)
	later := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti) })
	if _, err := env.Verifier.Verify(bg, later); err != nil {
		t.Fatalf("a nonce past its window was still refused: %v", err)
	}
}

func TestReplayPruneRemovesOnlyExpiredRows(t *testing.T) {
	env := identitytest.NewEnv(t)
	for range 5 {
		if _, err := env.Verifier.Verify(bg, env.Issuer.Token()); err != nil {
			t.Fatal(err)
		}
	}
	env.Clock.Advance(identitytest.DefaultLifetime / 2)
	for range 3 {
		// Minted later, so they outlive the first five.
		if _, err := env.Verifier.Verify(bg, env.Issuer.Token()); err != nil {
			t.Fatal(err)
		}
	}
	if got := replayRows(t, env); got != 8 {
		t.Fatalf("rows = %d, want 8", got)
	}

	// Nothing has expired yet.
	if n, err := env.Replay.Prune(bg); err != nil || n != 0 {
		t.Fatalf("early Prune = %d, %v; want 0, nil", n, err)
	}

	// The first five expire; the last three do not.
	env.Clock.Advance(identitytest.DefaultLifetime/2 + identity.ClockSkew)
	n, err := env.Replay.Prune(bg)
	if err != nil || n != 5 {
		t.Fatalf("Prune = %d, %v; want 5, nil", n, err)
	}
	if got := replayRows(t, env); got != 3 {
		t.Fatalf("rows after prune = %d, want 3", got)
	}
}

// Consume sweeps by itself about once a minute, so the table is bounded by
// the token rate times the maximum lifetime without anyone calling Prune.
func TestReplayCacheSweepsItself(t *testing.T) {
	env := identitytest.NewEnv(t)
	for range 10 {
		if _, err := env.Verifier.Verify(bg, env.Issuer.Token()); err != nil {
			t.Fatal(err)
		}
	}
	env.Clock.Advance(identitytest.DefaultLifetime + identity.ClockSkew + 2*time.Minute)
	if _, err := env.Verifier.Verify(bg, env.Issuer.Token()); err != nil {
		t.Fatal(err)
	}
	if got := replayRows(t, env); got != 1 {
		t.Fatalf("rows = %d, want 1: the expired ten should have been swept", got)
	}
}

// SEC-034: the cache survives a restart. A captured token stays useless
// after the process is restarted.
func TestReplayCacheSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alixai.db")
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{Clock: clk})
	keys := identity.NewKeyRegistry(nil)
	if err := keys.Register(iss.Key()); err != nil {
		t.Fatal(err)
	}

	boot := func() (*store.DB, *identity.Verifier) {
		db, err := store.Open(context.Background(), store.Options{Path: path})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		v, err := identity.NewVerifier(identity.Options{
			DeploymentID: identitytest.DefaultDeployment,
			Keys:         keys,
			Replay:       identity.NewSQLiteReplayCache(db, clk, nil),
			Clock:        clk,
		})
		if err != nil {
			t.Fatal(err)
		}
		return db, v
	}

	token := iss.Token()
	db1, v1 := boot()
	if _, err := v1.Verify(bg, token); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, v2 := boot()
	defer db2.Close()
	_, err := v2.Verify(bg, token)
	wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
}

// A replay cache that errors never turns into acceptance.
func TestReplayCacheErrorFailsClosed(t *testing.T) {
	env := identitytest.NewEnv(t)
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment,
		Keys:         env.Keys,
		Replay:       failingReplay{},
		Clock:        env.Clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(bg, env.Issuer.Token())
	wantReject(t, err, contracts.ErrUnavailable, identity.ReasonReplayUnavailable)
}

type failingReplay struct{}

func (failingReplay) Consume(context.Context, contracts.Scope, string, time.Time) error {
	return errBroken
}

// The same nonce under two different tenants is two different entries: the
// key is the whole scope, never a lookup that reaches across it.
func TestReplayKeyIncludesScope(t *testing.T) {
	env := identitytest.NewEnv(t)
	const jti = "shared-nonce-0123456789"
	a := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti) })
	b := env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTokenID, jti).Set(identity.ClaimTenant, "ten_other") })
	if _, err := env.Verifier.Verify(bg, a); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Verifier.Verify(bg, b); err != nil {
		t.Fatalf("same nonce in another tenant was treated as a replay: %v", err)
	}
	if got := replayRows(t, env); got != 2 {
		t.Fatalf("rows = %d, want 2", got)
	}
}
