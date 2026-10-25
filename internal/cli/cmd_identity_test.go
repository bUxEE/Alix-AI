package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// addKeyArgs is the add-issuer-key command line for iss against the example
// package, with extra flags appended.
func addKeyArgs(cfg string, iss *identitytest.Issuer, extra ...string) []string {
	return append([]string{"identity", "add-issuer-key", "-config", cfg, "-package", hostPackageDir,
		"-key-id", iss.KeyID, "-public-key", trust.EncodePublicKey(iss.Public)}, extra...)
}

func storedKeys(t *testing.T, dataDir string) []identity.StoredIssuerKey {
	t.Helper()
	keys, err := issuerKeyStore(openData(t, dataDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := keys.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestIdentityAddIssuerKeyRegistersAgainstTheManifest(t *testing.T) {
	cfg, dataDir := minimalConfig(t, identityYAML)
	iss := newHostIssuer(t)

	code, out, errOut := runCLI(t, addKeyArgs(cfg, iss, "-label", "primary")...)
	if code != ExitOK {
		t.Fatalf("add-issuer-key = %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, hostKeyID) || !strings.Contains(out, hostApp) || !strings.Contains(out, hostIssuer) {
		t.Errorf("output = %q", out)
	}
	if errOut != "" {
		t.Errorf("a clean registration warned: %q", errOut)
	}
	list := storedKeys(t, dataDir)
	if len(list) != 1 || list[0].KeyID != hostKeyID || list[0].ApplicationID != hostApp || list[0].Issuer != hostIssuer ||
		list[0].Label != "primary" || list[0].Status != identity.IssuerKeyActive || !list[0].Public.Equal(iss.Public) || list[0].DevKey {
		t.Fatalf("stored = %+v", list)
	}

	// The same id again: a conflict, not a second key.
	if code, _, errOut := runCLI(t, addKeyArgs(cfg, iss)...); code != ExitError || !strings.Contains(errOut, "already registered") {
		t.Errorf("registering the same key id twice = %d: %s", code, errOut)
	}

	// list: the table, and JSON for scripts. Public information only.
	code, out, _ = runCLI(t, "identity", "list", "-config", cfg)
	if code != ExitOK || !strings.Contains(out, hostKeyID) || !strings.Contains(out, "active") || strings.Contains(out, trust.EncodePublicKey(iss.Public)) {
		t.Errorf("identity list = %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "identity", "list", "-config", cfg, "-json", "-app", hostApp)
	var rows []struct {
		KeyID       string `json:"key_id"`
		Status      string `json:"status"`
		Fingerprint string `json:"fingerprint"`
		DevKey      bool   `json:"dev_key"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil || code != ExitOK || len(rows) != 1 || rows[0].KeyID != hostKeyID || rows[0].Fingerprint == "" {
		t.Errorf("identity list -json = %d %v\n%s", code, err, out)
	}
	if code, out, _ := runCLI(t, "identity", "list", "-config", cfg, "-app", "another-app"); code != ExitOK || !strings.Contains(out, "no issuer keys") {
		t.Errorf("list of an application with no keys = %d: %s", code, out)
	}
}

func TestIdentityAddIssuerKeyIsCheckedAgainstTheManifest(t *testing.T) {
	cfg, dataDir := minimalConfig(t, identityYAML)
	iss := newHostIssuer(t)
	nothingStored := func(t *testing.T) {
		t.Helper()
		if list := storedKeys(t, dataDir); len(list) != 0 {
			t.Fatalf("a refused key was stored: %+v", list)
		}
	}

	t.Run("key id the package does not declare", func(t *testing.T) {
		undeclared := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: hostApp, Issuer: hostIssuer, KeyID: "undeclared-key"})
		code, _, errOut := runCLI(t, addKeyArgs(cfg, undeclared)...)
		if code != ExitError || !strings.Contains(errOut, "context_exchange.key_ids") || !strings.Contains(errOut, hostKeyID) {
			t.Fatalf("an undeclared key id = %d: %s", code, errOut)
		}
		nothingStored(t)
	})
	t.Run("issuer other than the package's", func(t *testing.T) {
		code, _, errOut := runCLI(t, addKeyArgs(cfg, iss, "-issuer", "https://evil.example.test")...)
		if code != ExitError || !strings.Contains(errOut, "context_exchange.issuer") {
			t.Fatalf("a different issuer = %d: %s", code, errOut)
		}
		nothingStored(t)
	})
	t.Run("application other than the package's", func(t *testing.T) {
		code, _, errOut := runCLI(t, addKeyArgs(cfg, iss, "-app", "another-app")...)
		if code != ExitUsage || !strings.Contains(errOut, hostApp) {
			t.Fatalf("a different application = %d: %s", code, errOut)
		}
		nothingStored(t)
	})
	t.Run("package that does not load", func(t *testing.T) {
		code, _, errOut := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", filepath.Join(fixtures, "broken-unsigned"),
			"-key-id", iss.KeyID, "-public-key", trust.EncodePublicKey(iss.Public))
		if code != ExitError || !strings.Contains(errOut, "critical findings") {
			t.Fatalf("a package with critical findings = %d: %s", code, errOut)
		}
		nothingStored(t)
	})
	t.Run("no package", func(t *testing.T) {
		code, _, errOut := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-key-id", iss.KeyID, "-public-key", trust.EncodePublicKey(iss.Public))
		if code != ExitUsage || !strings.Contains(errOut, "-package") {
			t.Fatalf("no -package = %d: %s", code, errOut)
		}
		if code, _, _ := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", filepath.Join(t.TempDir(), "missing"), "-key-id", iss.KeyID,
			"-public-key", trust.EncodePublicKey(iss.Public)); code != ExitError {
			t.Errorf("a package path that does not exist = %d", code)
		}
		nothingStored(t)
	})
	t.Run("audience of the package differs from the deployment's", func(t *testing.T) {
		other, _ := minimalConfig(t, "identity:\n  deployment_id: dep_cli\n  audience: another-service\n")
		code, _, errOut := runCLI(t, addKeyArgs(other, iss)...)
		if code != ExitOK || !strings.Contains(errOut, "another-service") || !strings.Contains(errOut, "refused") {
			t.Fatalf("an audience mismatch = %d: %s (the key is registered, and the operator is told every token will be refused)", code, errOut)
		}
	})
}

// Public keys only: a private key, in any of the forms an operator might paste,
// is refused without being stored, logged or echoed.
func TestIdentityAddIssuerKeyRefusesPrivateKeys(t *testing.T) {
	cfg, dataDir := minimalConfig(t, identityYAML)
	iss := newHostIssuer(t)
	priv := iss.PrivateKey()
	pemBytes, err := trust.MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemFile := filepath.Join(t.TempDir(), "private.key")
	if err := os.WriteFile(pemFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// The 64-byte Ed25519 private key, and its 32-byte seed, as base64.
	b64Private := trust.EncodePublicKey(ed25519.PublicKey(priv))
	b64Seed := trust.EncodePublicKey(ed25519.PublicKey(priv.Seed()))

	for name, args := range map[string][]string{
		"PEM file": {"-public-key-file", pemFile},
		"PEM text": {"-public-key", string(pemBytes)},
		"64 bytes": {"-public-key", b64Private},
		"junk":     {"-public-key", "not base64 !!"},
	} {
		t.Run(name, func(t *testing.T) {
			full := append([]string{"identity", "add-issuer-key", "-config", cfg, "-package", hostPackageDir, "-key-id", hostKeyID}, args...)
			code, out, errOut := runCLI(t, full...)
			if code != ExitUsage {
				t.Fatalf("code = %d\n%s\n%s", code, out, errOut)
			}
			for _, secret := range []string{b64Private, b64Seed, "BEGIN PRIVATE KEY", strings.TrimSpace(strings.Split(string(pemBytes), "\n")[1])} {
				if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
					t.Errorf("the refusal echoes key material: %q", errOut)
				}
			}
		})
	}
	if list := storedKeys(t, dataDir); len(list) != 0 {
		t.Fatalf("a refused key was stored: %+v", list)
	}
	// Both -public-key and -public-key-file, or neither, is a usage error.
	if code, _, _ := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", hostPackageDir, "-key-id", hostKeyID); code != ExitUsage {
		t.Errorf("no key given = %d, want a usage error", code)
	}
	if code, _, _ := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", hostPackageDir, "-key-id", hostKeyID,
		"-public-key", trust.EncodePublicKey(iss.Public), "-public-key-file", pemFile); code != ExitUsage {
		t.Errorf("two keys given = %d, want a usage error", code)
	}
	// A public key file as `package keygen` writes it works.
	pubFile := filepath.Join(t.TempDir(), "host.pub")
	if err := os.WriteFile(pubFile, []byte(trust.EncodePublicKey(iss.Public)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", hostPackageDir, "-key-id", hostKeyID, "-public-key-file", pubFile); code != ExitOK {
		t.Fatalf("a public key file = %d\n%s\n%s", code, out, errOut)
	}
}

// The published TEST-ONLY keys are refused unless the explicit developer flag is
// given, and then everything says so.
func TestIdentityAddIssuerKeyRefusesPublishedTestKeysWithoutTheFlag(t *testing.T) {
	cfg, dataDir := minimalConfig(t, identityYAML)
	pub := trust.EncodePublicKey(devkeys.PublicKey())
	args := []string{"identity", "add-issuer-key", "-config", cfg, "-package", hostPackageDir, "-key-id", hostKeyID, "-public-key", pub}

	code, _, errOut := runCLI(t, args...)
	if code != ExitError || !strings.Contains(errOut, "TEST-ONLY") || !strings.Contains(errOut, "developer flag") {
		t.Fatalf("a published TEST-ONLY key without the flag = %d: %s", code, errOut)
	}
	if list := storedKeys(t, dataDir); len(list) != 0 {
		t.Fatalf("the refused key was stored: %+v", list)
	}

	code, out, errOut := runCLI(t, append(args, "-allow-test-key")...)
	if code != ExitOK || !strings.Contains(errOut, "TEST-ONLY") {
		t.Fatalf("with the developer flag = %d\n%s\n%s", code, out, errOut)
	}
	list := storedKeys(t, dataDir)
	if len(list) != 1 || !list[0].DevKey {
		t.Fatalf("stored = %+v, want the key marked as a developer key", list)
	}
	if code, out, _ := runCLI(t, "identity", "list", "-config", cfg); code != ExitOK || !strings.Contains(out, "TEST-ONLY") {
		t.Errorf("identity list does not mark the developer key:\n%s", out)
	}
}

func TestIdentityRevokeIssuerKey(t *testing.T) {
	cfg, dataDir := minimalConfig(t, identityYAML)
	iss := newHostIssuer(t)
	if code, out, errOut := runCLI(t, addKeyArgs(cfg, iss)...); code != ExitOK {
		t.Fatalf("add = %d\n%s\n%s", code, out, errOut)
	}

	// The wrong application finds nothing and changes nothing.
	if code, _, errOut := runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-app", "another-app", "-key-id", hostKeyID); code != ExitError || !strings.Contains(errOut, "has no issuer key") {
		t.Errorf("revoking another application's key = %d: %s", code, errOut)
	}
	if list := storedKeys(t, dataDir); list[0].Status != identity.IssuerKeyActive {
		t.Fatal("the key was revoked by the wrong application")
	}
	if code, _, _ := runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-key-id", hostKeyID); code != ExitUsage {
		t.Errorf("no -app = %d, want a usage error", code)
	}

	code, out, errOut := runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-app", hostApp, "-key-id", hostKeyID, "-reason", "compromised")
	if code != ExitOK || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke = %d\n%s\n%s", code, out, errOut)
	}
	if list := storedKeys(t, dataDir); list[0].Status != identity.IssuerKeyRevoked || list[0].RevokedReason != "compromised" {
		t.Fatalf("stored = %+v", list)
	}
	// Again: said so, nothing more recorded.
	if code, out, _ := runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-app", hostApp, "-key-id", hostKeyID); code != ExitOK || !strings.Contains(out, "already revoked") {
		t.Errorf("second revoke = %d: %s", code, out)
	}
	// A revoked key id and its material never come back.
	if code, _, errOut := runCLI(t, addKeyArgs(cfg, iss)...); code != ExitError || !strings.Contains(errOut, "already registered") && !strings.Contains(errOut, "revoked") {
		t.Errorf("re-adding a revoked key = %d: %s", code, errOut)
	}
}

// A change that cannot be recorded in the audit log is not made: the record is
// written in the transaction of the change (SEC-037).
func TestIdentityKeyChangesAreAuditedInTheSameTransaction(t *testing.T) {
	cfg, dataDir := minimalConfig(t, identityYAML)
	iss := newHostIssuer(t)
	// Drop the audit table: appends fail, so the key cannot be added.
	db := openData(t, dataDir)
	if _, err := db.ExecContext(context.Background(), `DROP TABLE audit_checkpoints`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, addKeyArgs(cfg, iss)...)
	if code != ExitError {
		t.Fatalf("add without a working audit log = %d: %s", code, errOut)
	}
	if list := storedKeys(t, dataDir); len(list) != 0 {
		t.Fatalf("a key was added although its audit record could not be written: %+v", list)
	}
}
