package resources

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	mib = 1 << 20
	gib = 1 << 30
)

var epoch = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

var bg = context.Background()

// fakeRuntime is a provider runtime: a mutable list of loaded models, a call
// log and switchable failures.
type fakeRuntime struct {
	mu        sync.Mutex
	models    []LoadedModel
	total     uint64
	reportErr error
	unloadErr map[string]error
	unloaded  []string
	reports   int
}

func (f *fakeRuntime) RuntimeMemory(context.Context) (RuntimeMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports++
	if f.reportErr != nil {
		return RuntimeMemory{}, f.reportErr
	}
	return RuntimeMemory{Bytes: f.total, Models: append([]LoadedModel(nil), f.models...)}, nil
}

func (f *fakeRuntime) Unload(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.unloadErr[id]; err != nil {
		return err
	}
	f.unloaded = append(f.unloaded, id)
	kept := f.models[:0]
	for _, m := range f.models {
		if m.ID != id {
			kept = append(kept, m)
		}
	}
	f.models = kept
	return nil
}

func (f *fakeRuntime) set(total uint64, models ...LoadedModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total, f.models = total, models
}

func (f *fakeRuntime) unloads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unloaded...)
}

// env is a Governor over a fake clock, a settable RSS reading and a fake
// runtime. Sleep advances the clock instead of waiting.
type env struct {
	t   testing.TB
	clk *clock.Fake
	rt  *fakeRuntime
	gov *Governor

	mu      sync.Mutex
	rss     uint64
	rssErr  error
	sleeps  int
	onSleep func(n int)
}

// newEnv builds an env with a 1 GiB budget and 200 MiB of RSS, refreshed once.
func newEnv(t testing.TB, mutate ...func(*Config)) *env {
	t.Helper()
	e := &env{t: t, clk: clock.NewFake(epoch), rt: &fakeRuntime{}, rss: 200 * mib}
	cfg := Config{MemoryBudgetBytes: gib}
	for _, m := range mutate {
		m(&cfg)
	}
	gov, err := New(Options{
		Config:  cfg,
		Clock:   e.clk,
		Memory:  MemoryProbeFunc(e.readRSS),
		Runtime: e.rt,
		Sleep:   e.sleep,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.gov = gov
	gov.Refresh(bg)
	return e
}

func (e *env) readRSS() (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rss, e.rssErr
}

func (e *env) setRSS(v uint64, err error) {
	e.mu.Lock()
	e.rss, e.rssErr = v, err
	e.mu.Unlock()
}

func (e *env) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	e.sleeps++
	n, hook := e.sleeps, e.onSleep
	e.mu.Unlock()
	e.clk.Advance(d)
	if hook != nil {
		hook(n)
	}
	return nil
}

func (e *env) sleepCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sleeps
}

// wantCode fails unless err is a *contracts.Error with the code and reason.
func wantCode(t *testing.T, err error, code contracts.ErrCode, reason Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s (%s)", code, reason)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *contracts.Error", err)
	}
	if ce.Code != code {
		t.Fatalf("code = %s, want %s (%v)", ce.Code, code, err)
	}
	if got, _ := ce.Details["reason"].(string); got != string(reason) {
		t.Fatalf("reason = %q, want %q (%v)", got, reason, err)
	}
}
