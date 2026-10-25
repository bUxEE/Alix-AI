package fake_test

import (
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
)

const (
	testApp = "app_demo"
	// limit is the reply size the fake adapter accepts in these tests; the
	// host's oversize reply is four times larger.
	limit = 64 << 10
)

// newSubject builds the fake adapter the way production wires an adapter: the
// transport inside adapters.Wrap.
func newSubject(t *testing.T, appOpts ...fake.AppOption) adaptersuite.Subject {
	t.Helper()
	app := fake.NewApp(append([]fake.AppOption{fake.WithOversizeBytes(4 * limit)}, appOpts...)...)
	guarded, err := adapters.Wrap(fake.NewAdapter(app, fake.WithMaxResponseBytes(limit)), adapters.Options{ApplicationID: testApp})
	if err != nil {
		t.Fatal(err)
	}
	return adaptersuite.Subject{Adapter: guarded, Fixture: app, ApplicationID: testApp, MaxResponseBytes: limit}
}

// TestConformanceFakeAdapter runs the shared adapter conformance suite
// (tests/conformance/adapters) against the in-memory adapter. The suite records
// the identity the host received on every read, prepare and commit, denies a
// commit and checks that nothing is retried around the denial (SEC-044), and
// feeds hung, oversize, malformed and forged replies to the adapter (SEC-051).
// M1-10, M1-11 and M3-14 run the same suite against their transports.
//
// SEC-044
// SEC-051
func TestConformanceFakeAdapter(t *testing.T) {
	adaptersuite.RunAdapterSuite(t, func(t *testing.T) adaptersuite.Subject {
		return newSubject(t)
	})
}
