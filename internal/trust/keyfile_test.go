package trust

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/fsperm/fspermtest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestKeyFileRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pemBytes), "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("not a PKCS#8 PEM: %s", pemBytes)
	}
	back, err := ParsePrivateKeyPEM(append([]byte("any warning text before the block\n"), pemBytes...))
	if err != nil || !back.Equal(priv) {
		t.Errorf("round trip: %v", err)
	}
	got, err := DecodePublicKey(" " + EncodePublicKey(pub) + "\n")
	if err != nil || !got.Equal(pub) {
		t.Errorf("public key round trip: %v", err)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	for name, in := range map[string][]byte{
		"empty":            nil,
		"garbage":          []byte("not a key"),
		"wrong block type": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rsaDER}),
		"rsa key":          pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER}),
		"bad der":          pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("xx")}),
	} {
		if _, err := ParsePrivateKeyPEM(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, in := range map[string]string{"empty": "", "short": "AAAA", "url-safe": strings.Repeat("-", 43) + "=", "not base64": "!!!"} {
		if _, err := DecodePublicKey(in); err == nil {
			t.Errorf("public key %s: accepted", name)
		}
	}
	if _, err := MarshalPrivateKeyPEM(ed25519.PrivateKey("short")); err == nil {
		t.Error("a short private key was encoded")
	}
}

// TestSecKeyFilesOwnerOnly: generated private keys are created readable by the
// owner only, in a directory only the owner can enter, and are never
// overwritten.
//
// SEC-081: key material is owner-only (modes are enforced on Unix; Windows
// modes are advisory, see WriteKeyFiles).
func TestSecKeyFilesOwnerOnly(t *testing.T) {
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "keys", "dev")
	privPath, pubPath, err := WriteKeyFiles(dir, "dev-publisher", priv)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(privPath) != dir || filepath.Base(privPath) != "dev-publisher.key" || filepath.Base(pubPath) != "dev-publisher.pub" {
		t.Errorf("paths = %s, %s", privPath, pubPath)
	}
	// Owner only on every platform: the modes on Unix, the ACL on Windows (M2).
	if err := fsperm.Check(privPath, false); err != nil {
		t.Errorf("the private key file is not owner only: %v", err)
	}
	if err := fsperm.Check(dir, true); err != nil {
		t.Errorf("the key directory is not owner only: %v", err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(privPath); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("private key mode = %v (%v), want 0600", st.Mode().Perm(), err)
		}
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("key directory mode = %v (%v), want 0700", st.Mode().Perm(), err)
		}
	}

	loaded, warnings, err := LoadPrivateKeyFile(privPath)
	if err != nil || !loaded.Equal(priv) || len(warnings) != 0 {
		t.Fatalf("LoadPrivateKeyFile = %v, %v, %v", loaded.Equal(priv), warnings, err)
	}
	pubText, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if pub, err := DecodePublicKey(string(pubText)); err != nil || !pub.Equal(priv.Public()) {
		t.Errorf("public key file: %v", err)
	}

	t.Run("never overwritten", func(t *testing.T) {
		_, other, _ := GenerateKey()
		_, _, err := WriteKeyFiles(dir, "dev-publisher", other)
		wantFail(t, err, contracts.ErrConflict)
		again, _, _ := LoadPrivateKeyFile(privPath)
		if !again.Equal(priv) {
			t.Error("an existing key file was overwritten")
		}
	})

	t.Run("a half-written pair is cleaned up", func(t *testing.T) {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "pre.pub"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := WriteKeyFiles(d, "pre", priv)
		if err == nil {
			t.Fatal("expected a conflict on the existing .pub")
		}
		if _, err := os.Stat(filepath.Join(d, "pre.key")); err == nil {
			t.Error("the private key was left behind after the pair failed")
		}
	})

	t.Run("names cannot climb out of the directory", func(t *testing.T) {
		for _, name := range []string{"", "../evil", "a/b", `a\b`, ".hidden", "Upper", "x y", strings.Repeat("a", 65), "con:"} {
			if _, _, err := WriteKeyFiles(t.TempDir(), name, priv); err == nil {
				t.Errorf("name %q accepted", name)
			}
		}
	})

	t.Run("a key file open to others is loaded with a warning", func(t *testing.T) {
		fspermtest.Loosen(t, privPath)
		_, warnings, err := LoadPrivateKeyFile(privPath)
		if err != nil || len(warnings) != 1 {
			t.Errorf("warnings = %v, err = %v", warnings, err)
		}
	})

	t.Run("a key file that is not a regular file or too large is refused", func(t *testing.T) {
		if _, _, err := LoadPrivateKeyFile(dir); err == nil {
			t.Error("a directory was loaded as a key")
		}
		big := filepath.Join(t.TempDir(), "big.key")
		if err := os.WriteFile(big, []byte(strings.Repeat("x", maxKeyFileBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadPrivateKeyFile(big); err == nil {
			t.Error("an oversized key file was loaded")
		}
		if _, _, err := LoadPrivateKeyFile(filepath.Join(dir, "missing.key")); err == nil {
			t.Error("a missing key file was loaded")
		}
	})
}
