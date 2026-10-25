package resources

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-078
// loops are bounded. Nothing in this package can run, wait or retry
// without a step, time or context bound, and no configuration value means
// "unlimited".
func TestNoUnboundedLoops(t *testing.T) {
	t.Run("an agent loop that never finishes stops at the step limit", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.MaxLoopSteps = 5 })
		calls := 0
		err := e.gov.NewLoop("dialogue").Run(bg, func(int) (bool, error) { calls++; return false, nil })
		wantCode(t, err, contracts.ErrResourceExhausted, "loop_steps")
		if calls != 5 {
			t.Fatalf("fn ran %d times, want exactly the 5-step limit", calls)
		}
	})

	t.Run("it also stops at the wall-clock limit", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.MaxLoopSteps, c.MaxLoopWall = 64, time.Minute })
		calls := 0
		err := e.gov.NewLoop("tool").Run(bg, func(int) (bool, error) {
			calls++
			e.clk.Advance(20 * time.Second)
			return false, nil
		})
		wantCode(t, err, contracts.ErrResourceExhausted, "loop_wall")
		if calls != 3 {
			t.Fatalf("fn ran %d times, want 3 (60 s of 60 s)", calls)
		}
	})

	t.Run("a loop that finishes, fails or is cancelled stops at once", func(t *testing.T) {
		e := newEnv(t)
		l := e.gov.NewLoop("dialogue")
		if err := l.Run(bg, func(step int) (bool, error) { return step == 3, nil }); err != nil || l.Steps() != 3 {
			t.Fatalf("done at step 3: err=%v steps=%d", err, l.Steps())
		}
		boom := context.DeadlineExceeded
		if err := e.gov.NewLoop("x").Run(bg, func(int) (bool, error) { return false, boom }); err != boom {
			t.Fatalf("fn error: %v", err)
		}
		ctx, cancel := context.WithCancel(bg)
		cancel()
		ran := false
		if err := e.gov.NewLoop("x").Run(ctx, func(int) (bool, error) { ran = true; return true, nil }); err == nil || ran {
			t.Fatalf("a cancelled context must stop the loop before fn: err=%v ran=%v", err, ran)
		}
	})

	t.Run("Step is the same bound for a loop written by hand", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.MaxLoopSteps = 2 })
		l := e.gov.NewLoop("manual")
		n := 0
		for l.Step() == nil {
			n++
			if n > 100 {
				t.Fatal("Step never refused")
			}
		}
		if n != 2 {
			t.Fatalf("admitted %d steps, want 2", n)
		}
	})

	t.Run("no configuration value means unlimited", func(t *testing.T) {
		d := Config{}.WithDefaults()
		v := reflect.ValueOf(d)
		for i := range v.NumField() {
			f := v.Type().Field(i)
			switch fv := v.Field(i); fv.Kind() {
			case reflect.Int, reflect.Int64:
				if fv.Int() == 0 {
					t.Errorf("%s defaults to zero: a limit must be finite", f.Name)
				}
			case reflect.Uint64:
				if fv.Uint() == 0 {
					t.Errorf("%s defaults to zero: a limit must be finite", f.Name)
				}
			}
		}
		for name, c := range map[string]Config{
			"negative steps":    {MaxLoopSteps: -1},
			"huge steps":        {MaxLoopSteps: 1 << 20},
			"negative wall":     {MaxLoopWall: -time.Second},
			"huge wall":         {MaxLoopWall: 24 * time.Hour},
			"negative yield":    {YieldMaxWait: -time.Second},
			"huge yield":        {YieldMaxWait: 24 * time.Hour},
			"negative unloads":  {MaxUnloadsPerSweep: -1},
			"huge unloads":      {MaxUnloadsPerSweep: 1 << 20},
			"negative idle":     {IdleUnloadAfter: -time.Minute},
			"negative min warm": {MinWarm: -time.Minute},
			"negative budget":   {MemoryBudgetBytes: 1},
		} {
			if err := c.Validate(); err == nil {
				t.Errorf("%s: Validate accepted %+v", name, c)
			}
		}
	})

	t.Run("a wait that the clock never ends still ends", func(t *testing.T) {
		// Yield on a clock that does not move: the poll count is the bound.
		e := newEnv(t, func(c *Config) { c.YieldMaxWait, c.YieldPoll = time.Second, 100*time.Millisecond })
		e.gov.sleep = func(ctx context.Context, _ time.Duration) error {
			e.mu.Lock()
			e.sleeps++
			e.mu.Unlock()
			return ctx.Err()
		}
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		defer release()
		wantCode(t, e.gov.Yield(bg, jobs.ClassIngest), contracts.ErrResourceExhausted, ReasonYieldTimeout)
		if got := e.sleepCount(); got > 11 {
			t.Fatalf("Yield polled %d times on a frozen clock, want at most 11", got)
		}
	})

	t.Run("Run exits when its context ends and a sweep is bounded", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.MaxUnloadsPerSweep = 3; c.IdleUnloadAfter, c.MinWarm = time.Minute, time.Second })
		old := e.clk.Now().Add(-time.Hour)
		var models []LoadedModel
		for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			models = append(models, LoadedModel{ID: id, LoadedAt: old, LastUsed: old})
		}
		e.rt.set(0, models...)
		ctx, cancel := context.WithCancel(bg)
		e.onSleep = func(n int) {
			if n == 1 {
				cancel()
			}
		}
		e.gov.Run(ctx)
		if got := len(e.rt.unloads()); got != 3 {
			t.Fatalf("one tick unloaded %d models, want the sweep bound of 3", got)
		}
	})
}
