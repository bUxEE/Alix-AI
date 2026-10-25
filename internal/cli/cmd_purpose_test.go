package cli

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/trust"
)

// H1: trust add-key -purpose registers a platform key for one purpose, and
// classifies the same key registered before purposes existed instead of
// failing; a purpose is never changed and is only for platform keys.
func TestTrustAddKeyPurpose(t *testing.T) {
	e := newOpsEnv(t, "")
	_, pubFile, pub := e.genKey("release-2027")

	var added keyOut
	decode(t, e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "release-2027", "-public-key-file", pubFile).stdout, &added)
	if added.Purpose != "" {
		t.Fatalf("no -purpose, purpose %q", added.Purpose)
	}
	var classified keyOut
	decode(t, e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "release-2027", "-public-key-file", pubFile, "-purpose", "release").stdout, &classified)
	if classified.Purpose != "release" || classified.Fingerprint != trust.Fingerprint(pub) {
		t.Fatalf("classified = %+v", classified)
	}
	if res := e.run("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "release-2027", "-public-key-file", pubFile, "-purpose", "audit-checkpoint"); res.code != ExitError {
		t.Errorf("a purpose was changed: exit %d", res.code)
	}
	if res := e.run("trust", "add-key", "-app", "demo-library", "-key-id", "x", "-public-key-file", pubFile, "-purpose", "release"); res.code != ExitUsage {
		t.Errorf("a purpose for a host application key: exit %d", res.code)
	}
	if res := e.run("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "x", "-public-key-file", pubFile, "-purpose", "everything"); res.code != ExitUsage {
		t.Errorf("an unknown purpose: exit %d", res.code)
	}
	var listed []keyOut
	decode(t, e.mustRun("trust", "list", "-app", trust.PlatformApplicationID).stdout, &listed)
	if len(listed) != 1 || listed[0].Purpose != "release" {
		t.Errorf("trust list = %+v", listed)
	}
}

// keys init registers the checkpoint key for the audit-checkpoint purpose, and
// serve classifies a configured checkpoint key that an older install
// registered with no purpose (by trust add-key); a key registered for another
// purpose is never used to sign checkpoints.
func TestCheckpointKeyPurpose(t *testing.T) {
	ctx := context.Background()
	cfg, dataDir := minimalConfig(t, "")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	db := openData(t, dataDir)
	ts := trust.NewStore(db, clock.Real{})
	stored, err := ts.LookupKey(ctx, trust.PlatformApplicationID, defaultCheckpointKeyID)
	if err != nil || stored.Purpose != trust.PurposeAuditCheckpoint {
		t.Fatalf("keys init registered %+v, %v", stored, err)
	}

	// An untagged registration of the configured key file is classified at start.
	_, otherPriv, _ := trust.GenerateKey()
	legacy := &checkpointKey{signing: trust.SigningKey{KeyID: "legacy-checkpoint", Key: otherPriv}}
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, "legacy-checkpoint", legacy.signing.Key.Public().(ed25519.PublicKey), "manual"); err != nil {
		t.Fatal(err)
	}
	if err := checkCheckpointKeyRegistered(ctx, db, clock.Real{}, legacy); err != nil {
		t.Fatalf("an untagged registration of the configured key: %v", err)
	}
	if got, _ := ts.LookupKey(ctx, trust.PlatformApplicationID, "legacy-checkpoint"); got.Purpose != trust.PurposeAuditCheckpoint {
		t.Errorf("not classified: %+v", got)
	}

	// A release key configured as the checkpoint key: refused.
	_, relPriv, _ := trust.GenerateKey()
	rel := &checkpointKey{signing: trust.SigningKey{KeyID: "release-key", Key: relPriv}}
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, "release-key", relPriv.Public().(ed25519.PublicKey), "release", trust.WithPurpose(trust.PurposeRelease)); err != nil {
		t.Fatal(err)
	}
	if err := checkCheckpointKeyRegistered(ctx, db, clock.Real{}, rel); err == nil || !strings.Contains(err.Error(), "not for audit checkpoints") {
		t.Errorf("a release key as the checkpoint key: %v", err)
	}
}

// A release key revoked in the data directory's trust store stays revoked when
// it is also named with -pubkey: the command line adds trust, it does not undo
// a revocation (the built-in release key is revoked the same way).
func TestUpgradePubkeyKeepsStoreRevocations(t *testing.T) {
	cfg, dataDir := minimalConfig(t, "")
	db := openData(t, dataDir)
	ts := trust.NewStore(db, clock.Real{})
	keyFiles := t.TempDir()
	sign := func(id string, revoke bool) (manifest, pubFile string) {
		t.Helper()
		pub, priv, err := trust.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.RegisterKey(context.Background(), trust.PlatformApplicationID, id, pub, "release", trust.WithPurpose(trust.PurposeRelease)); err != nil {
			t.Fatal(err)
		}
		if revoke {
			if err := ts.RevokeKey(context.Background(), trust.PlatformApplicationID, id, "compromised"); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(filepath.Join("..", "upgrade", "testdata", "release-manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := release.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		m.Signatures = []trust.Signature{}
		unsigned, err := release.MarshalIndent(m)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := release.Sign(unsigned, trust.SigningKey{KeyID: id, Key: priv}, time.Now().UTC().Add(-time.Minute), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		manifest = filepath.Join(keyFiles, id+".json")
		pubFile = filepath.Join(keyFiles, id+".pub")
		if err := os.WriteFile(manifest, signed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pubFile, []byte(trust.EncodePublicKey(pub)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return manifest, pubFile
	}
	revokedManifest, revokedPub := sign("revoked-release", true)
	goodManifest, goodPub := sign("good-release", false)
	_ = db.Close()

	code, _, errOut := runCLI(t, "upgrade", "check", "-config", cfg, "-manifest", revokedManifest, "-pubkey", "revoked-release="+revokedPub)
	if code == ExitOK || !strings.Contains(errOut, "revoked_key") {
		t.Fatalf("a key revoked in the store, named with -pubkey: exit %d\n%s", code, errOut)
	}
	// Control: the same path verifies an unrevoked key (the check then reports
	// whether the release is this binary's, which a test binary's is not).
	_, out, errOut := runCLI(t, "upgrade", "check", "-config", cfg, "-manifest", goodManifest, "-pubkey", "good-release="+goodPub)
	if !strings.Contains(out, `"signed_by": "good-release"`) || strings.Contains(errOut, "revoked_key") {
		t.Fatalf("an unrevoked key named with -pubkey did not verify:\n%s\n%s", out, errOut)
	}
}
