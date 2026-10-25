package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// fakeChild is the path of the stand-in LocalAI binary, built once by TestMain
// from testdata/fakellm with the Go toolchain.
var fakeChild string

func TestMain(m *testing.M) {
	if prebuilt := os.Getenv("SUPERVISOR_FAKELLM"); prebuilt != "" {
		// A test binary cross-built for a host without a Go toolchain.
		fakeChild = prebuilt
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "supervisor-fakellm-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "supervisor tests: temp dir:", err)
		os.Exit(1)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go"+ext)
	}
	exe := filepath.Join(dir, "fakellm"+ext)
	build := exec.Command(goBin, "build", "-o", exe, "./testdata/fakellm")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor tests: building the fake child failed: %v\n%s\n", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	fakeChild = exe
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fastAfter keeps the supervisor's short waits (probe spacing, backoff) to a few
// milliseconds; the delays it asked for are read from its events. A long wait,
// the start timeout, stays real.
func fastAfter(d time.Duration) <-chan time.Time {
	if d >= 10*time.Second {
		return time.After(d)
	}
	return time.After(min(d, 5*time.Millisecond))
}

// recorder collects supervisor events.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) handle(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *recorder) of(t EventType) []Event {
	var out []Event
	for _, e := range r.all() {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) types() []EventType {
	var out []EventType
	for _, e := range r.all() {
		out = append(out, e.Type)
	}
	return out
}

// fileProber reports ok while the ready-<pid> file of the process the supervisor
// currently runs exists. Keyed by pid, a crashed run's file never makes the next
// run look ready.
type fileProber struct {
	dir string
	pid func() int
}

func (p fileProber) Name() string { return "fake" }

func (p fileProber) Health(context.Context) (providers.HealthReport, error) {
	if _, err := os.Stat(readyFile(p.dir, p.pid())); err == nil {
		return providers.HealthReport{Status: providers.HealthOK, Version: "fake"}, nil
	}
	return providers.HealthReport{Status: providers.HealthUnavailable, Reason: "starting"}, nil
}

func readyFile(dir string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("ready-%d", pid))
}

// spyRuntime remembers every instance its inner runtime started, so a test can
// check that none is left running.
type spyRuntime struct {
	inner Runtime
	mu    sync.Mutex
	insts []Instance
}

func (s *spyRuntime) Start(ctx context.Context) (Instance, error) {
	inst, err := s.inner.Start(ctx)
	if err == nil {
		s.mu.Lock()
		s.insts = append(s.insts, inst)
		s.mu.Unlock()
	}
	return inst, err
}

func (s *spyRuntime) instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Instance(nil), s.insts...)
}

// assertAllGone fails if any started instance is still running.
func (s *spyRuntime) assertAllGone(t *testing.T) {
	t.Helper()
	for _, inst := range s.instances() {
		select {
		case <-inst.Done():
		default:
			t.Errorf("instance pid %d is still running", inst.PID())
		}
	}
}

// startsCount is how many times the fake child recorded a start.
func startsCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "starts"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// runningSup is a Supervisor running in a goroutine, stopped at test end.
type runningSup struct {
	sup    *Supervisor
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

func runSup(t *testing.T, sup *Supervisor) *runningSup {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningSup{sup: sup, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- sup.Run(ctx) }()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// stop cancels Run and returns its result (cached after the first call).
func (r *runningSup) stop(t *testing.T) error {
	t.Helper()
	r.once.Do(func() {
		r.cancel()
		select {
		case r.err = <-r.done:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return within 30s of cancellation")
		}
	})
	return r.err
}

// wait returns Run's result once it ends by itself (a give-up).
func (r *runningSup) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.once.Do(func() { r.err = err })
		return err
	case <-time.After(timeout):
		t.Fatalf("Run did not return within %s", timeout)
		return nil
	}
}

// execFixture is a supervisor over the real fake child.
type execFixture struct {
	dir string
	spy *spyRuntime
	rec *recorder
	sup *Supervisor
}

// newExecFixture builds a Supervisor that spawns the fake child with args. The
// backoff is 100ms doubling to a 250ms cap, with the jitter pinned to nothing.
func newExecFixture(t *testing.T, cfg Config, ledger *Ledger, args ...string) *execFixture {
	t.Helper()
	dir := t.TempDir()
	ex, err := NewExec(ExecConfig{Path: fakeChild, Args: append([]string{"-state", dir}, args...)})
	if err != nil {
		t.Fatal(err)
	}
	ex.euid = func() int { return 1000 } // CI containers often run tests as root
	f := &execFixture{dir: dir, spy: &spyRuntime{inner: ex}, rec: &recorder{}}
	cfg.Backoff = Backoff{Initial: 100 * time.Millisecond, Max: 250 * time.Millisecond, Factor: 2, Jitter: 0.2}
	cfg.RestartWindow = time.Hour
	cfg.StartTimeout = 60 * time.Second
	cfg.ProbeInterval = 5 * time.Millisecond
	cfg.HealthInterval = 5 * time.Millisecond
	opts := []Option{WithEvents(f.rec.handle), WithAfter(fastAfter), WithRand(func() float64 { return 0.5 })}
	if ledger != nil {
		opts = append(opts, WithLedger(ledger))
	}
	f.sup, err = New(cfg, f.spy, fileProber{dir: dir, pid: func() int { return f.sup.Status().PID }}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *execFixture) waitRunning(t *testing.T) {
	t.Helper()
	waitFor(t, 60*time.Second, "the runtime to be running", func() bool { return f.sup.Status().State == StateRunning })
}

// mapSource is a ReceiptSource over a fixed map.
type mapSource struct {
	receipts map[string]contracts.ActionReceipt
	fail     map[string]error
}

func (m mapSource) Receipt(_ context.Context, id string) (contracts.ActionReceipt, bool, error) {
	if err := m.fail[id]; err != nil {
		return contracts.ActionReceipt{}, false, err
	}
	r, ok := m.receipts[id]
	return r, ok, nil
}

// fakeClock is the shared fake clock for in-process tests.
func fakeClock() *clock.Fake {
	return clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
}
