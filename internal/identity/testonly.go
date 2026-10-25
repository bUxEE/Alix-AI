package identity

import (
	"crypto/ed25519"
	"encoding/base64"
)

// publishedTestOnlyKeys are the public halves of the TEST-ONLY key pairs that
// are published in this repository on purpose (raw 32 bytes, standard base64):
// the private keys are public too, so anyone can mint a token with them. They
// are the same two keys internal/trust refuses to register as publisher keys
// (trust.IsTestOnlyKey). This package may not import the trust store (the
// module map), so the list is kept here, and a test in internal/cli, which may
// import both, fails if the two lists ever differ.
var publishedTestOnlyKeys = []string{
	// tools/devkeys: the key the fixtures and the reference apps sign with.
	"JVkFKnRqOKZyjsykTQAYkxewYt199JxKMqGn5mWcCBA=",
	// the throwaway key behind the example in schemas/application-package.
	"jS/ItlLHFpgCvo2a/LFu4R5GdYX3r6vOZ/Z+EMjVuZc=",
}

var publishedTestOnlyFingerprints = func() map[string]bool {
	m := map[string]bool{}
	for _, b64 := range publishedTestOnlyKeys {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic("identity: malformed built-in TEST-ONLY key " + b64)
		}
		m[fingerprintOf(raw)] = true
	}
	return m
}()

// IsPublishedTestOnlyKey reports whether pub is one of the published TEST-ONLY
// public keys: keys anyone can sign with, which an issuer key store refuses
// unless the operator gave the explicit developer flag.
func IsPublishedTestOnlyKey(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && publishedTestOnlyFingerprints[fingerprintOf(pub)]
}

// PublishedTestOnlyKeys returns the published TEST-ONLY public keys as standard
// base64, for tests that keep this list in step with internal/trust.
func PublishedTestOnlyKeys() []string { return append([]string(nil), publishedTestOnlyKeys...) }
