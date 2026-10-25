package openapi

import (
	"testing"

	adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
)

// TestConformanceOpenAPIAdapter runs the shared adapter conformance suite
// (tests/conformance/adapters) against the OpenAPI adapter, with fake.App behind
// the fixture HTTP API that the fixture's OpenAPI document describes. The suite
// records the identity the host received on every read, prepare and commit,
// denies a commit and checks that nothing is retried around the denial, drops
// connections, hangs, takes the host down and feeds oversize, malformed and
// forged replies to the adapter.
//
// SEC-044
// SEC-050
// SEC-051
func TestConformanceOpenAPIAdapter(t *testing.T) {
	adaptersuite.RunAdapterSuite(t, subject)
}
