package trust

import (
	"crypto/ed25519"
	"encoding/base64"
)

// Published TEST-ONLY public keys (raw 32 bytes, standard base64). The matching
// private keys are public too: one is in tools/devkeys, the other signs the
// example in schemas/application-package/examples/signature-manifest/valid.
// Anyone can sign with them, so a trust store refuses to register them unless
// it was opened with AllowTestOnlyKeys.
const (
	// DevKeysPublicKeyB64 is the public key of tools/devkeys (the key ID there
	// is alixai-test-only-publisher).
	DevKeysPublicKeyB64 = "JVkFKnRqOKZyjsykTQAYkxewYt199JxKMqGn5mWcCBA="
	// SchemaExamplePublicKeyB64 is the throwaway key behind the M1-01 example
	// signature (docs/integration/application-package.md section 13).
	SchemaExamplePublicKeyB64 = "jS/ItlLHFpgCvo2a/LFu4R5GdYX3r6vOZ/Z+EMjVuZc="
)

var testOnlyFingerprints = func() map[string]bool {
	m := map[string]bool{}
	for _, b64 := range []string{DevKeysPublicKeyB64, SchemaExamplePublicKeyB64} {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic("trust: malformed built-in TEST-ONLY key " + b64)
		}
		m[Fingerprint(raw)] = true
	}
	return m
}()

// IsTestOnlyKey reports whether pub is one of the published TEST-ONLY keys.
func IsTestOnlyKey(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && testOnlyFingerprints[Fingerprint(pub)]
}
