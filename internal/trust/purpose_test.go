package trust

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
)

// H1: a platform key is registered for one purpose, the purpose is set once,
// a rotation keeps it, and a verification that names a purpose accepts no
// other key.
func TestKeyPurposes(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	st, _ := newTrustStore(t, clk)
	rel, cp, none := newKey(t, "release-1"), newKey(t, "audit-checkpoint"), newKey(t, "report-signer")
	for _, k := range []struct {
		kp keyPair
		p  KeyPurpose
	}{{rel, PurposeRelease}, {cp, PurposeAuditCheckpoint}, {none, PurposeNone}} {
		got, err := st.RegisterKey(ctx, PlatformApplicationID, k.kp.id, k.kp.pub, "", WithPurpose(k.p))
		if err != nil || got.Purpose != k.p {
			t.Fatalf("register %s: %+v %v", k.kp.id, got, err)
		}
		back, err := st.LookupKey(ctx, PlatformApplicationID, k.kp.id)
		if err != nil || back.Purpose != k.p {
			t.Fatalf("lookup %s: %+v %v", k.kp.id, back, err)
		}
	}

	t.Run("only platform keys have a purpose", func(t *testing.T) {
		k := newKey(t, "app-key")
		if _, err := st.RegisterKey(ctx, testApp, k.id, k.pub, "", WithPurpose(PurposeRelease)); err == nil {
			t.Error("a host application key was registered with a purpose")
		}
		if _, err := st.RegisterKey(ctx, PlatformApplicationID, k.id, k.pub, "", WithPurpose("anything")); err == nil {
			t.Error("an unknown purpose was accepted")
		}
	})

	t.Run("a purpose is set once", func(t *testing.T) {
		if err := st.SetKeyPurpose(ctx, PlatformApplicationID, cp.id, PurposeRelease); !errors.Is(err, ErrKeyConflict) {
			t.Errorf("the checkpoint key became a release key: %v", err)
		}
		if err := st.SetKeyPurpose(ctx, PlatformApplicationID, cp.id, PurposeAuditCheckpoint); err != nil {
			t.Errorf("the same purpose again: %v", err)
		}
		if err := st.SetKeyPurpose(ctx, PlatformApplicationID, rel.id, PurposeNone); err == nil {
			t.Error("a purpose was removed")
		}
		if err := st.SetKeyPurpose(ctx, PlatformApplicationID, "missing", PurposeRelease); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("an unknown key: %v", err)
		}
		// Even someone who writes SQL cannot change it.
		if _, err := st.db.ExecContext(ctx, `UPDATE trust_publisher_keys SET purpose = 'release' WHERE key_id = ?`, cp.id); err == nil {
			t.Error("the trigger let a purpose change")
		}
	})

	t.Run("a rotation keeps the purpose", func(t *testing.T) {
		next := newKey(t, "release-2")
		got, err := st.RotateKey(ctx, PlatformApplicationID, rel.id, next.id, next.pub, "")
		if err != nil || got.Purpose != PurposeRelease {
			t.Fatalf("rotated: %+v %v", got, err)
		}
	})

	t.Run("a verification that names a purpose accepts no other key", func(t *testing.T) {
		v := NewVerifier(st, clk)
		payload := map[string]any{"kind": "x"}
		for _, k := range []keyPair{cp, none} {
			sig, err := SignPayload(payload, k.id, k.priv, t0, t0.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			_, err = v.VerifyPayload(ctx, payload, []Signature{sig}, PayloadOptions{ApplicationID: PlatformApplicationID, Purpose: PurposeRelease})
			var ve *VerifyError
			if !errors.Is(err, ErrWrongPurpose) || !errors.As(err, &ve) || ve.Failures[0].Reason != ReasonWrongPurpose {
				t.Errorf("%s over a release: %v", k.id, err)
			}
			// No purpose named: any registered platform key, as before.
			if _, err := v.VerifyPayload(ctx, payload, []Signature{sig}, PayloadOptions{ApplicationID: PlatformApplicationID}); err != nil {
				t.Errorf("%s with no purpose named: %v", k.id, err)
			}
		}
	})
}

// The migration classifies the key `alixai keys init` registered and leaves
// every other key, of the platform or not, without a purpose.
func TestPurposeMigrationClassifiesTheCheckpointKey(t *testing.T) {
	ctx := context.Background()
	first, err := migrations.ReadFile("migrations/0001_publisher_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	old := store.NewRegistry()
	if err := old.Register("trust", fstest.MapFS{"migrations/0001_publisher_keys.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trust.db")
	db, err := store.Open(ctx, store.Options{Path: path, Migrations: old})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]string{
		{PlatformApplicationID, "audit-checkpoint", "audit checkpoint signing key"},
		{PlatformApplicationID, "report-signer", "audit report signer"},
		{testApp, "pub", "audit checkpoint signing key"},
	} {
		k := newKey(t, row[1])
		if _, err := db.ExecContext(ctx, `INSERT INTO trust_publisher_keys
		   (application_id, key_id, public_key, fingerprint, status, label, registered_at, revoked_reason)
		   VALUES (?, ?, ?, ?, 'active', ?, 0, '')`, row[0], row[1], EncodePublicKey(k.pub), Fingerprint(k.pub), row[2]); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	now := store.NewRegistry()
	if err := now.Register("trust", migrations); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, store.Options{Path: path, Migrations: now})
	if err != nil {
		t.Fatalf("the migration does not apply over an existing store: %v", err)
	}
	defer db.Close()
	st := NewStore(db, nil)
	for _, c := range []struct {
		app, id string
		want    KeyPurpose
	}{
		{PlatformApplicationID, "audit-checkpoint", PurposeAuditCheckpoint},
		{PlatformApplicationID, "report-signer", PurposeNone},
		{testApp, "pub", PurposeNone},
	} {
		k, err := st.LookupKey(ctx, c.app, c.id)
		if err != nil || k.Purpose != c.want {
			t.Errorf("%s/%s: purpose %q (%v), want %q", c.app, c.id, k.Purpose, err, c.want)
		}
	}
}

// A store over a database migration 0002 has not reached (an upgrade verifies
// its release before it migrates; an inspection reads a copy as it is) reads
// the keys, with the purpose the migration would give them, and writes nothing.
func TestUnmigratedStoreReadsPurposes(t *testing.T) {
	ctx := context.Background()
	first, err := migrations.ReadFile("migrations/0001_publisher_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	old := store.NewRegistry()
	if err := old.Register("trust", fstest.MapFS{"migrations/0001_publisher_keys.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "old.db"), Migrations: old})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cp, other := newKey(t, "audit-checkpoint"), newKey(t, "report-signer")
	for _, row := range []struct {
		k     keyPair
		label string
	}{{cp, "audit checkpoint signing key"}, {other, "audit report signer"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO trust_publisher_keys
		   (application_id, key_id, public_key, fingerprint, status, label, registered_at, revoked_reason)
		   VALUES (?, ?, ?, ?, 'active', ?, 0, '')`, PlatformApplicationID, row.k.id, EncodePublicKey(row.k.pub), Fingerprint(row.k.pub), row.label); err != nil {
			t.Fatal(err)
		}
	}
	st := NewStore(db, clock.NewFake(t0))
	k, err := st.LookupKey(ctx, PlatformApplicationID, cp.id)
	if err != nil || k.Purpose != PurposeAuditCheckpoint {
		t.Fatalf("the checkpoint key of an unmigrated store: %+v, %v", k, err)
	}
	keys, err := st.ListKeys(ctx, PlatformApplicationID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListKeys: %v, %v", keys, err)
	}
	for _, k := range keys {
		if k.KeyID == other.id && k.Purpose != PurposeNone {
			t.Errorf("an unlabelled key got purpose %q", k.Purpose)
		}
	}
	// A checkpoint verifies, a release-purpose verification does not.
	payload := map[string]any{"kind": "x"}
	sig, err := SignPayload(payload, cp.id, cp.priv, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(st, clock.NewFake(t0))
	if _, err := v.VerifyPayload(ctx, payload, []Signature{sig}, PayloadOptions{ApplicationID: PlatformApplicationID, Purpose: PurposeAuditCheckpoint}); err != nil {
		t.Errorf("a checkpoint signature over an unmigrated store: %v", err)
	}
	if _, err := v.VerifyPayload(ctx, payload, []Signature{sig}, PayloadOptions{ApplicationID: PlatformApplicationID, Purpose: PurposeRelease}); !errors.Is(err, ErrWrongPurpose) {
		t.Errorf("the checkpoint key over a release, unmigrated store: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('trust_publisher_keys') WHERE name = 'purpose'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("reading migrated the table: %d, %v", n, err)
	}
}
