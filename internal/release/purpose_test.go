package release_test

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// purposeStore is a trust store holding three platform keys, one per purpose,
// each with its private half.
type purposeStore struct {
	db    *store.DB
	store *trust.Store
	priv  map[trust.KeyPurpose]ed25519.PrivateKey
	ids   map[trust.KeyPurpose]string
}

func newPurposeStore(t *testing.T, f *fx) *purposeStore {
	t.Helper()
	p := &purposeStore{db: storetest.Open(t, nil), priv: map[trust.KeyPurpose]ed25519.PrivateKey{}, ids: map[trust.KeyPurpose]string{}}
	p.store = trust.NewStore(p.db, f.clk)
	for purpose, id := range map[trust.KeyPurpose]string{
		trust.PurposeRelease:         "release-key",
		trust.PurposeAuditCheckpoint: "audit-checkpoint",
		trust.PurposeNone:            "integration-audit-signer",
	} {
		pub, priv, err := trust.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.store.RegisterKey(ctx, trust.PlatformApplicationID, id, pub, "test", trust.WithPurpose(purpose)); err != nil {
			t.Fatal(err)
		}
		p.priv[purpose], p.ids[purpose] = priv, id
	}
	return p
}

// H1: the audit-checkpoint key that serve keeps online under
// <data_dir>/keys is a registered platform key. Stolen, it must not sign a
// release that `upgrade apply` accepts; nor may any other platform key that is
// not a release key.
func TestOnlyReleaseKeysSignReleases(t *testing.T) {
	f := newFx(t)
	p := newPurposeStore(t, f)

	m, acc, err := release.Verify(ctx, f.sign(f.clone(), p.ids[trust.PurposeRelease], p.priv[trust.PurposeRelease]), p.store, f.clk)
	if err != nil || acc.KeyID != "release-key" || m.Signed.Version != "1.2.3" {
		t.Fatalf("a release-key signature (the control): %v %+v", err, acc)
	}

	for name, purpose := range map[string]trust.KeyPurpose{"the checkpoint key": trust.PurposeAuditCheckpoint, "a platform key with no purpose": trust.PurposeNone} {
		trojan := f.clone()
		trojan["version"] = "99.0.0"
		raw := f.sign(trojan, p.ids[purpose], p.priv[purpose])
		if _, _, err := release.Verify(ctx, raw, p.store, f.clk); !errors.Is(err, trust.ErrWrongPurpose) {
			t.Errorf("a release signed by %s: %v, want a wrong-purpose refusal", name, err)
		}
		// Through the upgrade's verifier too: one decision.
		if _, err := (upgradeVerifier(p.store, f)).VerifyRelease(ctx, raw); !errors.Is(err, trust.ErrWrongPurpose) {
			t.Errorf("upgrade: a release signed by %s: %v", name, err)
		}
	}

	// A key registered before purposes existed becomes a release key only when
	// the operator says so, and then nothing else.
	if err := p.store.SetKeyPurpose(ctx, trust.PlatformApplicationID, p.ids[trust.PurposeNone], trust.PurposeRelease); err != nil {
		t.Fatal(err)
	}
	if _, _, err := release.Verify(ctx, f.sign(f.clone(), p.ids[trust.PurposeNone], p.priv[trust.PurposeNone]), p.store, f.clk); err != nil {
		t.Errorf("a key classified as a release key: %v", err)
	}
	if err := p.store.SetKeyPurpose(ctx, trust.PlatformApplicationID, p.ids[trust.PurposeAuditCheckpoint], trust.PurposeRelease); !errors.Is(err, trust.ErrKeyConflict) {
		t.Errorf("the checkpoint key was re-purposed as a release key: %v", err)
	}
}

// The built-in release key verifies with no trust store setup, is still
// revocable through the store, and its id never stands for other material.
func TestPinnedReleaseKey(t *testing.T) {
	t.Run("the constant is the published key", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "keys", release.PinnedKeyID+".pub"))
		if err != nil {
			t.Fatal(err)
		}
		pub, err := trust.DecodePublicKey(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !pub.Equal(release.PinnedPublicKey()) {
			t.Fatal("the built-in release key is not keys/alixai-release-2026.pub")
		}
	})

	f := newFx(t)
	pinPub, pinPriv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer release.PinKeyForTest(release.PinnedKeyID, pinPub)()
	raw := f.sign(f.clone(), release.PinnedKeyID, pinPriv)

	t.Run("an empty store", func(t *testing.T) {
		st := trust.NewStore(storetest.Open(t, nil), f.clk)
		if _, acc, err := release.Verify(ctx, raw, st, f.clk); err != nil || acc.KeyID != release.PinnedKeyID {
			t.Fatalf("the built-in key in an empty store: %v", err)
		}
		if _, _, err := release.Verify(ctx, raw, release.StaticKeys{}, f.clk); err != nil {
			t.Fatalf("the built-in key with no -pubkey of its own: %v", err)
		}
	})
	t.Run("registered and revoked", func(t *testing.T) {
		st := trust.NewStore(storetest.Open(t, nil), f.clk)
		if _, err := st.RegisterKey(ctx, trust.PlatformApplicationID, release.PinnedKeyID, pinPub, "pinned", trust.WithPurpose(trust.PurposeRelease)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, raw, st, f.clk); err != nil {
			t.Fatalf("registered, before the revocation: %v", err)
		}
		if err := st.RevokeKey(ctx, trust.PlatformApplicationID, release.PinnedKeyID, "compromised"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, raw, st, f.clk); !errors.Is(err, trust.ErrRevokedKey) {
			t.Fatalf("the revoked built-in key: %v", err)
		}
	})
	t.Run("its material revoked under another id", func(t *testing.T) {
		st := trust.NewStore(storetest.Open(t, nil), f.clk)
		if _, err := st.RegisterKey(ctx, trust.PlatformApplicationID, "old-name", pinPub, "pinned"); err != nil {
			t.Fatal(err)
		}
		if err := st.RevokeKey(ctx, trust.PlatformApplicationID, "old-name", "compromised"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, raw, st, f.clk); !errors.Is(err, trust.ErrRevokedKey) {
			t.Fatalf("the built-in key whose material was revoked: %v", err)
		}
	})
	t.Run("registered before purposes existed, under another id", func(t *testing.T) {
		st := trust.NewStore(storetest.Open(t, nil), f.clk)
		if _, err := st.RegisterKey(ctx, trust.PlatformApplicationID, "release-2026", pinPub, "pinned"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, f.sign(f.clone(), "release-2026", pinPriv), st, f.clk); err != nil {
			t.Fatalf("the built-in material registered without a purpose: %v", err)
		}
	})
	t.Run("its id with other material", func(t *testing.T) {
		otherPub, otherPriv, _ := trust.GenerateKey()
		st := trust.NewStore(storetest.Open(t, nil), f.clk)
		if _, err := st.RegisterKey(ctx, trust.PlatformApplicationID, release.PinnedKeyID, otherPub, "impostor", trust.WithPurpose(trust.PurposeRelease)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := release.Verify(ctx, f.sign(f.clone(), release.PinnedKeyID, otherPriv), st, f.clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Fatalf("other material under the built-in id: %v", err)
		}
		keys := release.StaticKeys{Keys: map[string]ed25519.PublicKey{release.PinnedKeyID: otherPub}}
		if _, _, err := release.Verify(ctx, f.sign(f.clone(), release.PinnedKeyID, otherPriv), keys, f.clk); !errors.Is(err, trust.ErrUnknownKey) {
			t.Fatalf("other material under the built-in id, by -pubkey: %v", err)
		}
	})
}

// The TEST-ONLY key stays refused for releases (unless -allow-dev-key) even when
// a development deployment registered it in the store that is now opened normally.
func TestDevKeyInTheStoreIsNotAReleaseKey(t *testing.T) {
	f := newFx(t)
	db := storetest.Open(t, nil)
	dev := trust.NewStore(db, f.clk, trust.AllowTestOnlyKeys())
	if _, err := dev.RegisterKey(ctx, trust.PlatformApplicationID, devkeys.KeyID, devkeys.PublicKey(), "dev", trust.WithPurpose(trust.PurposeRelease)); err != nil {
		t.Fatal(err)
	}
	raw := f.sign(f.clone(), devkeys.KeyID, devkeys.PrivateKey())
	if _, _, err := release.Verify(ctx, raw, dev, f.clk); err != nil {
		t.Fatalf("the store opened with the explicit allow (the control): %v", err)
	}
	normal := trust.NewStore(db, f.clk)
	_, _, err := release.Verify(ctx, raw, normal, f.clk)
	if !errors.Is(err, trust.ErrUnknownKey) || !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("the TEST-ONLY key through a normally opened store: %v", err)
	}
}

func upgradeVerifier(keys trust.KeyLookup, f *fx) upgrade.ReleaseVerifier {
	return upgrade.ReleaseVerifier{Keys: keys, Clock: f.clk}
}

// The first upgrade of an install made before key purposes existed verifies
// its release against a trust store the purpose migration has not reached
// (upgrade opens it without migrating). The built-in key verifies there and the
// install's checkpoint key does not.
func TestReleaseVerifiesAgainstAnUnmigratedStore(t *testing.T) {
	f := newFx(t)
	pinPub, pinPriv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer release.PinKeyForTest(release.PinnedKeyID, pinPub)()

	first, err := os.ReadFile(filepath.Join("..", "trust", "migrations", "0001_publisher_keys.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "old.db")
	old := store.NewRegistry()
	if err := old.Register("trust", fstest.MapFS{"migrations/0001_publisher_keys.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, store.Options{Path: path, Migrations: old})
	if err != nil {
		t.Fatal(err)
	}
	cpPub, cpPriv, _ := trust.GenerateKey()
	if _, err := db.ExecContext(ctx, `INSERT INTO trust_publisher_keys
	   (application_id, key_id, public_key, fingerprint, status, label, registered_at, revoked_reason)
	   VALUES (?, 'audit-checkpoint', ?, ?, 'active', 'audit checkpoint signing key', 0, '')`,
		trust.PlatformApplicationID, trust.EncodePublicKey(cpPub), trust.Fingerprint(cpPub)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	// Opened as upgrade opens it: no migrations.
	db, err = store.Open(ctx, store.Options{Path: path, Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := trust.NewStore(db, f.clk)
	if _, _, err := release.Verify(ctx, f.sign(f.clone(), release.PinnedKeyID, pinPriv), st, f.clk); err != nil {
		t.Fatalf("the built-in key against an unmigrated store: %v", err)
	}
	if _, _, err := release.Verify(ctx, f.sign(f.clone(), "audit-checkpoint", cpPriv), st, f.clk); !errors.Is(err, trust.ErrWrongPurpose) {
		t.Fatalf("the checkpoint key against an unmigrated store: %v", err)
	}
}
