// Package devkeys holds the TEST-ONLY publisher key pair that fixtures,
// reference apps and tests sign application packages with.
//
// # THIS KEY IS NOT A SECRET
//
// The private key is committed to the repository on purpose, so that anyone
// can build a signed fixture package. Anyone can therefore forge a package
// signature with it. It must never be trusted by a real deployment:
//
//   - internal/trust refuses to register it (trust.IsTestOnlyKey) unless the
//     store is opened with trust.AllowTestOnlyKeys, which only tests and
//     developer tools do;
//   - its key ID, alixai-test-only-publisher, and the file names say so;
//   - it is a different key from the throwaway one behind the example in
//     schemas/application-package/examples/signature-manifest/valid.
//
// Real publishers generate their own key with "alixai package keygen".
//
// # Files
//
// The same key pair is available to non-Go code (the TypeScript and Python
// reference apps) as test-only-publisher.key.pem (PKCS#8 PEM, with a warning
// before the block) and test-only-publisher.pub (raw 32-byte public key in
// standard base64, one line). They derive from Seed: the seed of the Ed25519
// key is SHA-256 of that phrase, so there is no opaque secret blob to
// distrust. TestFilesDeriveFromSeed keeps the files, the constant and the
// derivation in step.
package devkeys

import (
	"crypto/ed25519"
	_ "embed"
	"fmt"

	"github.com/buxee/alix-ai/internal/trust"
)

// KeyID is the ID the test key is registered under in test trust stores.
const KeyID = "alixai-test-only-publisher"

// Seed is the phrase the key pair derives from: the Ed25519 seed is its
// SHA-256.
const Seed = "AlixAI TEST-ONLY publisher key v1 - published on purpose, never trust it"

//go:embed test-only-publisher.key.pem
var privatePEM []byte

//go:embed test-only-publisher.pub
var publicB64 string

// PrivateKey returns the TEST-ONLY private key. It panics if the embedded key
// file is corrupt, which a repository check (the test in this package) catches.
func PrivateKey() ed25519.PrivateKey {
	k, err := trust.ParsePrivateKeyPEM(privatePEM)
	if err != nil {
		panic(fmt.Sprintf("devkeys: embedded key file is corrupt: %v", err))
	}
	return k
}

// PublicKey returns the TEST-ONLY public key.
func PublicKey() ed25519.PublicKey {
	k, err := trust.DecodePublicKey(publicB64)
	if err != nil {
		panic(fmt.Sprintf("devkeys: embedded public key file is corrupt: %v", err))
	}
	return k
}

// PublicKeyBase64 returns the public key as the standard base64 of its raw 32
// bytes.
func PublicKeyBase64() string { return trust.EncodePublicKey(PublicKey()) }

// SigningKey returns the key as a trust.SigningKey for trust.SignPackageDir.
func SigningKey() trust.SigningKey {
	return trust.SigningKey{KeyID: KeyID, Key: PrivateKey()}
}
