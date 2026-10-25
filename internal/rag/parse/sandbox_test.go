package parse

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// pidRecorder collects the process ids the Runner starts.
type pidRecorder struct {
	mu   sync.Mutex
	pids []int
}

func (p *pidRecorder) add(pid int) {
	p.mu.Lock()
	p.pids = append(p.pids, pid)
	p.mu.Unlock()
}

func (p *pidRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pids)
}

func (p *pidRecorder) last(t testing.TB) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pids) == 0 {
		t.Fatal("no worker was started")
	}
	return p.pids[len(p.pids)-1]
}

// waitGone polls until the process no longer exists.
func waitGone(t testing.TB, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("worker process %d is still running after the parse returned", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func code(err error) contracts.ErrCode { return contracts.CodeOf(err) }

func reasonOf(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		s, _ := ce.Details["reason"].(string)
		return s
	}
	return ""
}

// SEC-016: a hanging fixture is killed within the limit, only its own worker
// dies, and the host and the next document are unaffected.
func TestParserTimeoutKilled(t *testing.T) {
	t.Run("a hung parser is killed at the wall limit", func(t *testing.T) {
		pids := &pidRecorder{}
		r := newFaultRunner(t, func(c *Config) { c.WallTimeout = 2 * time.Second })
		r.onStart = pids.add
		start := time.Now()
		_, err := r.parse(ctx(t), "x-hang", nil)
		elapsed := time.Since(start)
		if code(err) != contracts.ErrTimeout || reasonOf(err) != "wall_timeout" {
			t.Fatalf("err = %v (reason %q), want timeout/wall_timeout", err, reasonOf(err))
		}
		// The clock starts when the sandbox is verified, so process start (which an
		// antivirus can slow) is not counted against the hang; the wall limit is.
		if elapsed < 2*time.Second || elapsed > 20*time.Second {
			t.Errorf("returned after %s, want about 2s plus process start", elapsed)
		}
		waitGone(t, pids.last(t))
		// The Runner is unharmed and the next document parses.
		res, err := r.Parse(ctx(t), wire.FormatMD, []byte("# After\n\nstill here\n"))
		if err != nil || !strings.Contains(res.Text, "still here") {
			t.Fatalf("after the timeout: %v", err)
		}
	})

	t.Run("a spinning parser is stopped by the CPU limit", func(t *testing.T) {
		pids := &pidRecorder{}
		r := newFaultRunner(t, func(c *Config) {
			c.WallTimeout = 45 * time.Second
			c.CPUTime = 2 * time.Second
		})
		r.onStart = pids.add
		start := time.Now()
		_, err := r.parse(ctx(t), "x-spin", nil)
		if code(err) != contracts.ErrTimeout {
			t.Fatalf("err = %v, want a timeout", err)
		}
		if reason := reasonOf(err); reason != "cpu_time" && reason != "wall_timeout" {
			t.Errorf("reason = %q", reason)
		}
		if el := time.Since(start); el > 40*time.Second {
			t.Errorf("a 2 s CPU limit took %s to enforce", el)
		}
		waitGone(t, pids.last(t))
		stillWorks(t, r)
	})

	t.Run("cancelling the context kills the worker", func(t *testing.T) {
		pids := &pidRecorder{}
		r := newFaultRunner(t)
		r.onStart = pids.add
		cctx, cancel := context.WithCancel(ctx(t))
		go func() {
			for pids.count() == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			cancel()
		}()
		_, err := r.parse(cctx, "x-hang", nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		waitGone(t, pids.last(t))
	})

	t.Run("a worker that never starts is a startup timeout, not a parse timeout", func(t *testing.T) {
		r, err := NewRunner(func() Config {
			c := testConfig(testExecutable(t))
			c.StartupTimeout = time.Second
			return c
		}())
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.args = []string{"-hostile=silent"}
		_, err = r.Parse(ctx(t), wire.FormatTXT, []byte("x"))
		if code(err) != contracts.ErrTimeout || reasonOf(err) != "startup_timeout" {
			t.Fatalf("err = %v (reason %q), want timeout/startup_timeout", err, reasonOf(err))
		}
	})
}

// SEC-016: a memory-hungry fixture is stopped within the memory limit and the
// service stays within its budget. The in-process guard is turned off here so
// that the test shows the enforcement from outside the worker: the Job Object
// on Windows, RLIMIT_AS and the resident-set watchdog on Linux.
func TestParserMemoryLimitKilled(t *testing.T) {
	const limit = 128 << 20
	run := func(t *testing.T, selfGuard bool) {
		pids := &pidRecorder{}
		r := newFaultRunner(t, func(c *Config) {
			c.MemoryBytes = limit
			c.WallTimeout = 45 * time.Second
		})
		r.selfGuard = selfGuard
		r.onStart = pids.add
		start := time.Now()
		_, err := r.parse(ctx(t), "x-alloc", nil)
		if code(err) != contracts.ErrResourceExhausted || reasonOf(err) != "memory_limit" {
			t.Fatalf("err = %v (reason %q), want resource_exhausted/memory_limit", err, reasonOf(err))
		}
		if el := time.Since(start); el > 40*time.Second {
			t.Errorf("took %s to stop a runaway allocation", el)
		}
		var ce *contracts.Error
		errors.As(err, &ce)
		peak, _ := ce.Details["peak_memory_bytes"].(int64)
		kind, _ := ce.Details["peak_kind"].(string)
		t.Logf("selfGuard=%v: stopped at a peak of %d MiB (%s) against a limit of %d MiB", selfGuard, peak>>20, kind, limit>>20)
		// Windows counts committed memory under a hard limit: the peak cannot pass
		// it. Linux samples the resident set, so allow what one 8 MiB step and a
		// sampling interval can add.
		if ceiling := int64(limit) + 64<<20; peak > ceiling {
			t.Errorf("peak %d MiB passed the limit by more than the sampling allowance (%d MiB)", peak>>20, ceiling>>20)
		}
		// A worker stopped at a few MiB was not stopped by a memory limit: it never
		// got to allocate. That happened on Linux when RLIMIT_AS was set below the
		// address space the Go runtime had already reserved, and the test passed
		// without exercising the memory limit. The peak here is the worker's own
		// (VmHWM, or the Job Object's peak), so it must show real growth. The
		// worker's own guard may end it a sampling interval after the allocation
		// that crossed the line, so only the external enforcement is held to a
		// floor.
		if !selfGuard && peak < limit/2 {
			t.Errorf("peak %d MiB is under half the limit (%d MiB): the worker was stopped before it could allocate", peak>>20, limit>>21)
		}
		waitGone(t, pids.last(t))
		stillWorks(t, r)
	}
	t.Run("stopped by the operating system and the host", func(t *testing.T) { run(t, false) })
	t.Run("stopped by the worker's own guard", func(t *testing.T) { run(t, true) })

	// The other half of the contract: memory under the budget is available. A
	// worker that touches half its budget and returns must succeed on every
	// platform, with and without the in-process guard, and the peak the host
	// records must be the worker's own.
	for _, selfGuard := range []bool{false, true} {
		t.Run(fmt.Sprintf("a document that fits is not stopped (guard %v)", selfGuard), func(t *testing.T) {
			const budget, touch = 256 << 20, 128 << 20
			r := newFaultRunner(t, func(c *Config) { c.MemoryBytes = budget })
			r.selfGuard = selfGuard
			res, err := r.parse(ctx(t), "x-touch", []byte(strconv.Itoa(touch)))
			if err != nil {
				t.Fatalf("touching %d MiB of a %d MiB budget: %v", touch>>20, budget>>20, err)
			}
			if res.Text != fmt.Sprintf("touched %d", touch) {
				t.Errorf("text = %q", res.Text)
			}
			peak := res.Stats.PeakMemoryBytes
			t.Logf("touched %d MiB, host-measured peak %d MiB (%s), working set %d MiB", touch>>20, peak>>20, res.Stats.PeakKind, res.Stats.PeakWorkingSetBytes>>20)
			// At least what it touched. At most that plus the runtime's own footprint
			// (about 50 MiB committed on Windows, a few MiB resident on Linux) and the
			// text frames: a peak near the host's own size would mean the figure is not
			// the worker's.
			if peak < touch || peak > touch+96<<20 {
				t.Errorf("peak %d MiB for a worker that touched %d MiB", peak>>20, touch>>20)
			}
		})
	}
}

// SEC-016: a crashing fixture kills only its own worker and the failure is
// reported as one document's, not the service's.
func TestParserCrashIsolated(t *testing.T) {
	t.Run("a crash fails that document and the runner goes on", func(t *testing.T) {
		pids := &pidRecorder{}
		r := newFaultRunner(t)
		r.onStart = pids.add
		_, err := r.parse(ctx(t), "x-crash", nil)
		if code(err) != contracts.ErrInternal || reasonOf(err) != "worker_crashed" {
			t.Fatalf("err = %v (reason %q), want internal/worker_crashed", err, reasonOf(err))
		}
		var ce *contracts.Error
		errors.As(err, &ce)
		if ce.Retryable {
			t.Error("a crash must not be retryable: the same bytes crash it again")
		}
		waitGone(t, pids.last(t))
		stillWorks(t, r)
	})

	t.Run("an exit with a status is a crash too", func(t *testing.T) {
		r := newFaultRunner(t)
		_, err := r.parse(ctx(t), "x-exit", nil)
		if code(err) != contracts.ErrInternal || reasonOf(err) != "worker_crashed" {
			t.Fatalf("err = %v", err)
		}
		var ce *contracts.Error
		errors.As(err, &ce)
		if got, _ := ce.Details["exit_code"].(int); got != 7 {
			t.Errorf("exit_code = %v, want 7", ce.Details["exit_code"])
		}
	})

	t.Run("a worker killed from outside is one failed document", func(t *testing.T) {
		pids := &pidRecorder{}
		r := newFaultRunner(t, func(c *Config) { c.WallTimeout = 30 * time.Second })
		r.onStart = pids.add
		done := make(chan error, 1)
		go func() {
			_, err := r.parse(ctx(t), "x-hang", nil)
			done <- err
		}()
		for {
			pids.mu.Lock()
			n := len(pids.pids)
			pids.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(time.Second) // let it finish starting
		killProcess(t, pids.last(t))
		select {
		case err := <-done:
			if err == nil || code(err) == contracts.ErrTimeout && reasonOf(err) == "wall_timeout" {
				t.Fatalf("err = %v, want a crash, not a hang", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the Runner did not notice the worker had died")
		}
		stillWorks(t, r)
	})

	t.Run("one document crashing does not disturb a concurrent one", func(t *testing.T) {
		r := newFaultRunner(t, func(c *Config) { c.MaxConcurrent = 2 })
		var wg sync.WaitGroup
		var okCount, crashed atomic.Int32
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					if _, err := r.parse(ctx(t), "x-crash", nil); reasonOf(err) == "worker_crashed" {
						crashed.Add(1)
					}
					return
				}
				if res, err := r.Parse(ctx(t), wire.FormatMD, []byte("# Doc\n\nconcurrent text\n")); err == nil && strings.Contains(res.Text, "concurrent text") {
					okCount.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if okCount.Load() != 3 || crashed.Load() != 3 {
			t.Fatalf("ok=%d crashed=%d, want 3 and 3", okCount.Load(), crashed.Load())
		}
	})
}
