package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/trust"
)

// L3: restoring an older backup must not make a publisher key or an issuer key
// that was revoked after the backup valid again. By default the restore keeps
// the revocations of the database it replaces; only an explicit opt-out
// restores the backup's key states as they were.
func TestRestoreKeepsNewerRevocations(t *testing.T) {
	type world struct {
		e                       *env
		backupDir               string
		trustPub, issuerPub     ed25519.PublicKey
		latePub                 ed25519.PublicKey
		trustKeyID, issuerKeyID string
	}
	setup := func(t *testing.T) world {
		e := newEnv(t)
		w := world{e: e, trustKeyID: "publisher-2026", issuerKeyID: "issuer-2026"}
		w.trustPub, _, _ = ed25519.GenerateKey(rand.Reader)
		w.issuerPub, _, _ = ed25519.GenerateKey(rand.Reader)
		w.latePub, _, _ = ed25519.GenerateKey(rand.Reader)
		ts := trust.NewStore(e.db, e.clk)
		if _, err := ts.RegisterKey(ctx, "demo-library", w.trustKeyID, w.trustPub, "publisher"); err != nil {
			t.Fatal(err)
		}
		is := identity.NewIssuerKeyStore(e.db, e.clk, identity.IssuerKeyStoreOptions{})
		if _, err := is.Add(ctx, identity.AddIssuerKey{
			Key:      identity.IssuerKey{KeyID: w.issuerKeyID, ApplicationID: "demo-library", Issuer: "https://issuer.example", Public: w.issuerPub},
			Declared: identity.ContextExchange{KeyIDs: []string{w.issuerKeyID}, Issuer: "https://issuer.example"},
		}); err != nil {
			t.Fatal(err)
		}
		w.backupDir = e.backup(t.TempDir()).Dir

		// After the backup: both keys are revoked as compromised, and a key
		// registered later is revoked too.
		e.clk.Advance(1e9)
		if err := ts.RevokeKey(ctx, "demo-library", w.trustKeyID, "compromised"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := is.Revoke(ctx, "demo-library", w.issuerKeyID, "compromised"); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.RegisterKey(ctx, "demo-library", "late-key", w.latePub, "late"); err != nil {
			t.Fatal(err)
		}
		if err := ts.RevokeKey(ctx, "demo-library", "late-key", "compromised"); err != nil {
			t.Fatal(err)
		}
		e.close()
		return w
	}

	t.Run("by default the newer revocations are kept", func(t *testing.T) {
		w := setup(t)
		rr, err := Restore(ctx, RestoreOptions{BackupDir: w.backupDir, DataDir: w.e.dataDir, Keys: w.e.keys, Clock: w.e.clk})
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if rr.CarriedRevocations != 3 {
			t.Errorf("carried %d revocations, want 3 (two keys revoked, one added as revoked)", rr.CarriedRevocations)
		}
		w.e.open()
		ts := trust.NewStore(w.e.db, w.e.clk)
		if k, err := ts.LookupKey(ctx, "demo-library", w.trustKeyID); err != nil || k.Status != trust.KeyRevoked || k.RevokedReason != "compromised" {
			t.Errorf("the publisher key after the restore: %+v, %v", k, err)
		}
		if _, err := identity.NewIssuerKeyStore(w.e.db, w.e.clk, identity.IssuerKeyStoreOptions{}).Lookup(ctx, w.issuerKeyID); !errors.Is(err, identity.ErrKeyNotFound) {
			t.Errorf("the revoked issuer key verifies again after the restore: %v", err)
		}
		// The key registered and revoked after the backup stays on record, so its
		// material cannot be registered again under a new name.
		if _, err := ts.RegisterKey(ctx, "demo-library", "renamed", w.latePub, "again"); !errors.Is(err, trust.ErrKeyConflict) {
			t.Errorf("revoked material was registered again after the restore: %v", err)
		}
		// Everything else is the backup's: the ledger tables it held are there.
		if n := count(t, w.e.db, `SELECT count(*) FROM ledger_a`); n != 0 {
			t.Errorf("ledger_a rows = %d", n)
		}
	})

	t.Run("the explicit opt-out restores the backup's key states", func(t *testing.T) {
		w := setup(t)
		rr, err := Restore(ctx, RestoreOptions{BackupDir: w.backupDir, DataDir: w.e.dataDir, Keys: w.e.keys, Clock: w.e.clk, SkipNewerRevocations: true})
		if err != nil || rr.CarriedRevocations != 0 {
			t.Fatalf("Restore: %+v, %v", rr, err)
		}
		w.e.open()
		if k, err := trust.NewStore(w.e.db, w.e.clk).LookupKey(ctx, "demo-library", w.trustKeyID); err != nil || k.Status != trust.KeyActive {
			t.Errorf("with the opt-out the backup's active key is active: %+v, %v", k, err)
		}
	})

	t.Run("nothing in place: nothing to carry", func(t *testing.T) {
		w := setup(t)
		rr, err := Restore(ctx, RestoreOptions{BackupDir: w.backupDir, DataDir: t.TempDir(), Keys: w.e.keys, Clock: w.e.clk})
		if err != nil || rr.CarriedRevocations != 0 {
			t.Fatalf("Restore into an empty data directory: %+v, %v", rr, err)
		}
	})
}
