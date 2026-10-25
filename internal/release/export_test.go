package release

import "crypto/ed25519"

// PinKeyForTest makes id/pub the built-in release key until the returned
// function is called: a test cannot sign with the real one.
func PinKeyForTest(id string, pub ed25519.PublicKey) (restore func()) {
	oldID, oldKey := pinnedKeyID, pinnedKey
	pinnedKeyID, pinnedKey = id, pub
	return func() { pinnedKeyID, pinnedKey = oldID, oldKey }
}
