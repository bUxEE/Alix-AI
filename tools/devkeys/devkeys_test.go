package devkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestFilesDeriveFromSeed(t *testing.T) {
	seed := sha256.Sum256([]byte(Seed))
	want := ed25519.NewKeyFromSeed(seed[:])
	if !PrivateKey().Equal(want) {
		t.Error("test-only-publisher.key.pem is not the key derived from Seed")
	}
	if !PublicKey().Equal(want.Public()) {
		t.Error("test-only-publisher.pub is not the public half of the key derived from Seed")
	}
	if PublicKeyBase64() != trust.DevKeysPublicKeyB64 {
		t.Errorf("trust.DevKeysPublicKeyB64 = %s, key files give %s", trust.DevKeysPublicKeyB64, PublicKeyBase64())
	}
}

func TestKeyIsLabelledTestOnly(t *testing.T) {
	if !strings.Contains(strings.ToLower(KeyID), "test-only") {
		t.Errorf("key id %q does not say test-only", KeyID)
	}
	pem, err := os.ReadFile("test-only-publisher.key.pem")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pem), "TEST-ONLY KEY. NOT A SECRET.") {
		t.Error("the key file must open with the TEST-ONLY warning")
	}
	for _, name := range []string{"test-only-publisher.key.pem", "test-only-publisher.pub"} {
		if !strings.Contains(name, "test-only") {
			t.Errorf("file name %q does not say test-only", name)
		}
	}
}

func TestKeyIsNotTheSchemaExampleKey(t *testing.T) {
	if PublicKeyBase64() == trust.SchemaExamplePublicKeyB64 {
		t.Error("devkeys must not reuse the throwaway key behind the M1-01 example signature")
	}
	if !trust.IsTestOnlyKey(PublicKey()) {
		t.Error("trust does not recognise the dev key as TEST-ONLY")
	}
}

// TestSignedFixtureVerifiesOnlyWhereTestKeysAreAllowed: a package signed with
// the test key verifies in a store that opted in to test keys, and a normal
// store refuses to trust the key at all, so a fixture key cannot slip into a
// real deployment.
func TestSignedFixtureVerifiesOnlyWhereTestKeysAreAllowed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alixai.yaml"), []byte("application:\n  id: demo-library\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: "demo-library", PackageVersion: "1.0.0"},
		Keys:            []trust.SigningKey{SigningKey()},
		Now:             now,
		ExpiresAt:       now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	opts := trust.PackageOptions{ApplicationID: "demo-library", PackageVersion: "1.0.0", PublisherKeyIDs: []string{KeyID}}

	// storetest.Open(t, nil) uses store.Default, which importing trust filled.
	strict := trust.NewStore(storetest.Open(t, nil), clk)
	if _, err := strict.RegisterKey(ctx, "demo-library", KeyID, PublicKey(), "test"); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Fatalf("a normal trust store registered the TEST-ONLY key: %v", err)
	}
	if _, err := trust.NewVerifier(strict, clk).VerifyPackageDir(ctx, dir, opts); err == nil {
		t.Error("a package signed with the TEST-ONLY key verified in a store that never registered it")
	}

	optedIn := trust.NewStore(storetest.Open(t, nil), clk, trust.AllowTestOnlyKeys())
	if _, err := optedIn.RegisterKey(ctx, "demo-library", KeyID, PublicKey(), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := trust.NewVerifier(optedIn, clk).VerifyPackageDir(ctx, dir, opts); err != nil {
		t.Errorf("fixture package does not verify in a store that opted in: %v", err)
	}
}
