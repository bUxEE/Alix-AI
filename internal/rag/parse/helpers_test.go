package parse

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// testConfig is a configuration for tests that are not about a limit: generous
// time (an antivirus may scan a freshly built executable on first start) and
// the production worker.
func testConfig(worker string) Config {
	c := DefaultConfig()
	c.WorkerPath = worker
	c.StartupTimeout = 90 * time.Second
	c.WallTimeout = 60 * time.Second
	c.MemoryBytes = 512 << 20
	return c
}

var warmed sync.Map // worker path -> struct{}

// warmUp starts the worker once and ends it, so that the first start of a freshly
// built executable (which an antivirus scans, taking many seconds on a dev
// box) is not charged to a test that times a limit. It is a measurement aid,
// not a weakening: limits are enforced the same either way.
func warmUp(t testing.TB, r *Runner) {
	t.Helper()
	if _, done := warmed.LoadOrStore(r.cfg.WorkerPath, struct{}{}); done {
		return
	}
	if _, err := r.Probe(ctx(t)); err != nil {
		t.Fatalf("warming the worker up: %v", err)
	}
}

// newTestRunner starts a Runner on the production worker, adjusted by mods.
func newTestRunner(t testing.TB, mods ...func(*Config)) *Runner {
	t.Helper()
	cfg := testConfig(prodWorker)
	for _, m := range mods {
		m(&cfg)
	}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	warmUp(t, r)
	return r
}

// newFaultRunner starts a Runner on the fault-injection worker.
func newFaultRunner(t testing.TB, mods ...func(*Config)) *Runner {
	t.Helper()
	cfg := testConfig(faultWorker)
	for _, m := range mods {
		m(&cfg)
	}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	warmUp(t, r)
	return r
}

func ctx(t testing.TB) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return c
}

func mustParse(t testing.TB, r *Runner, f wire.Format, doc []byte) *Result {
	t.Helper()
	res, err := r.Parse(ctx(t), f, doc)
	if err != nil {
		t.Fatalf("Parse(%s): %v", f, err)
	}
	return res
}
