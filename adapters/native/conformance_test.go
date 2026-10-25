package native

import (
	"testing"

	adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
)

// TestConformanceNativeAdapter runs the shared adapter conformance suite
// (tests/conformance/adapters) against the native adapter, over a Unix domain
// socket and over the loopback TCP fallback, with fake.App behind a wire-level
// host. The suite records the identity the host received on every read, prepare
// and commit, denies a commit and checks that nothing is retried around the
// denial, drops connections, hangs, takes the host down and feeds oversize,
// malformed and forged replies to the adapter.
//
// SEC-044
// SEC-050
// SEC-051
func TestConformanceNativeAdapter(t *testing.T) {
	for _, network := range []string{NetworkUnix, NetworkTCP} {
		t.Run(network, func(t *testing.T) {
			adaptersuite.RunAdapterSuite(t, func(t *testing.T) adaptersuite.Subject { return subject(t, network) })
		})
	}
}
