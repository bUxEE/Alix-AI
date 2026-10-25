package resources

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// idleEnv is an env with a 10 minute idle timeout and a 5 minute warm period.
func idleEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	return newEnv(t, append([]func(*Config){func(c *Config) {
		c.IdleUnloadAfter, c.MinWarm = 10*time.Minute, 5*time.Minute
	}}, mutate...)...)
}

func (e *env) tick() []string {
	e.t.Helper()
	done, err := e.gov.Tick(bg)
	if err != nil {
		e.t.Fatalf("Tick: %v", err)
	}
	return done
}

func wantUnloaded(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unloaded %v, want %v", got, want)
	}
}

// SEC-078
// a model is not unloaded before its minimum warm period (and not
// before it has been idle long enough).
func TestIdleUnloadAfterWarmPeriod(t *testing.T) {
	t.Run("unloads only after the idle timeout, and a touch restarts it", func(t *testing.T) {
		e := idleEnv(t)
		e.rt.set(500*mib, LoadedModel{ID: "chat", Bytes: 500 * mib}) // the runtime gives no timestamps
		wantUnloaded(t, e.tick())                                    // first sight: loaded now

		e.clk.Advance(8 * time.Minute)
		e.gov.Touch("chat") // a request restarts the idle clock
		e.clk.Advance(9 * time.Minute)
		wantUnloaded(t, e.tick()) // idle 9m of 10m

		e.clk.Advance(time.Minute)
		wantUnloaded(t, e.tick(), "chat") // idle 10m
		if got := e.rt.unloads(); len(got) != 1 || got[0] != "chat" {
			t.Fatalf("the unload must go through the provider: %v", got)
		}
		if st := e.gov.Stats(); st.Unloads != 1 || st.Reloads != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})

	t.Run("a freshly loaded model is not unloaded even when the runtime says it was last used long ago", func(t *testing.T) {
		e := idleEnv(t)
		now := e.clk.Now()
		e.rt.set(500*mib, LoadedModel{ID: "chat", Bytes: 500 * mib, LoadedAt: now.Add(-time.Minute), LastUsed: now.Add(-time.Hour)})
		wantUnloaded(t, e.tick()) // idle for an hour, but warm for one minute of five
		e.clk.Advance(3 * time.Minute)
		wantUnloaded(t, e.tick()) // four minutes
		e.clk.Advance(time.Minute)
		wantUnloaded(t, e.tick(), "chat") // five: the warm period is over
	})

	t.Run("in-use and pinned models are never unloaded", func(t *testing.T) {
		e := idleEnv(t, func(c *Config) { c.PinnedModels = []string{"controller"} })
		e.rt.set(900*mib, LoadedModel{ID: "controller", Bytes: 600 * mib}, LoadedModel{ID: "stt", Bytes: 300 * mib})
		e.tick()
		release := e.gov.UseModel("stt")
		e.clk.Advance(time.Hour)
		e.gov.Refresh(bg) // keep the report fresh; UseModel set stt's last use an hour ago
		wantUnloaded(t, e.tick())
		release()
		release() // harmless
		e.clk.Advance(9 * time.Minute)
		wantUnloaded(t, e.tick()) // the release restarted stt's idle clock
		e.clk.Advance(time.Minute)
		wantUnloaded(t, e.tick(), "stt") // controller stays: pinned
	})

	t.Run("an unload and a sweep are bounded; the longest idle goes first", func(t *testing.T) {
		e := idleEnv(t, func(c *Config) { c.MaxUnloadsPerSweep = 2 })
		now := e.clk.Now()
		var models []LoadedModel
		for i := range 5 {
			models = append(models, LoadedModel{ID: fmt.Sprintf("m%d", i), Bytes: 10 * mib, LoadedAt: now.Add(-time.Hour), LastUsed: now.Add(-time.Duration(30+i) * time.Minute)})
		}
		e.rt.set(50*mib, models...)
		wantUnloaded(t, e.tick(), "m4", "m3")
		wantUnloaded(t, e.tick(), "m2", "m1")
		wantUnloaded(t, e.tick(), "m0")
		wantUnloaded(t, e.tick())
	})

	t.Run("a failed unload is counted and the sweep goes on", func(t *testing.T) {
		e := idleEnv(t)
		now := e.clk.Now()
		old := now.Add(-time.Hour)
		e.rt.set(20*mib, LoadedModel{ID: "a", LoadedAt: old, LastUsed: old}, LoadedModel{ID: "b", LoadedAt: old, LastUsed: old.Add(time.Second)})
		e.rt.unloadErr = map[string]error{"a": errors.New("runtime refused")}
		done, err := e.gov.Tick(bg)
		if err == nil || len(done) != 1 || done[0] != "b" {
			t.Fatalf("done=%v err=%v, want b unloaded and a's error returned", done, err)
		}
		if st := e.gov.Stats(); st.Unloads != 1 || st.UnloadErrors != 1 {
			t.Fatalf("stats = %+v", st)
		}
	})

	t.Run("without a fresh runtime report nothing is unloaded", func(t *testing.T) {
		e := idleEnv(t)
		old := e.clk.Now().Add(-time.Hour)
		e.rt.set(10*mib, LoadedModel{ID: "chat", LoadedAt: old, LastUsed: old})
		e.gov.Refresh(bg)
		e.clk.Advance(e.gov.Config().StaleAfter + time.Second)
		if done, err := e.gov.Sweep(bg); err != nil || len(done) != 0 {
			t.Fatalf("Sweep on a stale report: %v, %v", done, err)
		}
		e.rt.mu.Lock()
		e.rt.reportErr = errors.New("runtime down")
		e.rt.mu.Unlock()
		wantUnloaded(t, e.tick())
	})

	t.Run("the policy can be turned off", func(t *testing.T) {
		e := idleEnv(t, func(c *Config) { c.IdleUnloadDisabled = true })
		old := e.clk.Now().Add(-time.Hour)
		e.rt.set(10*mib, LoadedModel{ID: "chat", LoadedAt: old, LastUsed: old})
		wantUnloaded(t, e.tick())
	})

	t.Run("a model seen again after an unload is a counted reload", func(t *testing.T) {
		e := idleEnv(t)
		old := e.clk.Now().Add(-time.Hour)
		e.rt.set(10*mib, LoadedModel{ID: "chat", LoadedAt: old, LastUsed: old})
		wantUnloaded(t, e.tick(), "chat")
		e.rt.set(10*mib, LoadedModel{ID: "chat", LoadedAt: e.clk.Now()}) // a request loaded it again
		e.tick()
		if got := e.gov.Stats().Reloads; got != 1 {
			t.Fatalf("Reloads = %d, want 1", got)
		}
		wantUnloaded(t, e.tick()) // and it is warm again
	})

	t.Run("Run sweeps until its context ends", func(t *testing.T) {
		e := idleEnv(t)
		old := e.clk.Now().Add(-time.Hour)
		e.rt.set(10*mib, LoadedModel{ID: "chat", LoadedAt: old, LastUsed: old})
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		e.onSleep = func(n int) {
			if n == 3 {
				cancel()
			}
		}
		e.gov.Run(ctx) // returns only because the context ends
		if got := e.rt.unloads(); len(got) != 1 {
			t.Fatalf("Run unloaded %v, want chat once", got)
		}
	})
}

func TestRuntimeFromRegistry(t *testing.T) {
	if RuntimeFromRegistry(nil) != nil {
		t.Fatal("nil registry must give no runtime")
	}
	reg := providers.NewRegistry(newEnv(t).clk)
	plain := fake.New()
	if err := reg.Register(providers.RoleController, plain); err != nil {
		t.Fatal(err)
	}
	if RuntimeFromRegistry(reg) != nil {
		t.Fatal("a provider that does not implement ModelRuntime must give no runtime")
	}

	reg = providers.NewRegistry(newEnv(t).clk)
	rt := &fakeRuntime{}
	rt.set(10*mib, LoadedModel{ID: "chat", Bytes: 10 * mib})
	p := struct {
		*fake.Provider
		*fakeRuntime
	}{fake.New(), rt}
	for _, role := range []providers.Role{providers.RoleController, providers.RoleRouter} {
		if err := reg.Register(role, p); err != nil {
			t.Fatal(err)
		}
	}
	got := RuntimeFromRegistry(reg)
	if got == nil {
		t.Fatal("expected the registered provider's runtime")
	}
	if _, isMulti := got.(*multiRuntime); isMulti {
		t.Fatal("one provider serving two roles must be used once, not fanned out")
	}
	mem, err := got.RuntimeMemory(bg)
	if err != nil || mem.Total() != 10*mib || len(mem.Models) != 1 {
		t.Fatalf("RuntimeMemory = %+v, %v", mem, err)
	}
	if err := got.Unload(bg, "chat"); err != nil || len(rt.unloads()) != 1 {
		t.Fatalf("Unload: %v %v", err, rt.unloads())
	}
}

func TestMultiRuntimeRoutesUnloadToTheOwner(t *testing.T) {
	a, b := &fakeRuntime{}, &fakeRuntime{}
	a.set(10*mib, LoadedModel{ID: "a1", Bytes: 10 * mib})
	b.set(20*mib, LoadedModel{ID: "b1", Bytes: 20 * mib})
	m := &multiRuntime{rts: []ModelRuntime{a, b}}
	if err := m.Unload(bg, "a1"); err == nil {
		t.Fatal("Unload before any report must fail, not guess")
	}
	mem, err := m.RuntimeMemory(bg)
	if err != nil || mem.Total() != 30*mib {
		t.Fatalf("RuntimeMemory = %+v, %v", mem, err)
	}
	if err := m.Unload(bg, "b1"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if len(a.unloads()) != 0 || len(b.unloads()) != 1 {
		t.Fatalf("a=%v b=%v", a.unloads(), b.unloads())
	}
}
