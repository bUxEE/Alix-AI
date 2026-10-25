package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Deps is what a run talks to. Everything that touches a stack, a container or
// a process is behind it.
type Deps struct {
	Mem    MemorySource
	Probe  StackProbe // nil: no stack probe (the verdict then judges memory and load only)
	Driver Driver

	// RunID, Host and Git are recorded in the result; RunID is made when empty.
	RunID string
	Host  HostInfo
	Git   GitInfo
}

const (
	preflightTimeout = 2 * time.Minute
	// readTimeout bounds one memory reading and one stack reading (docker stats
	// itself waits about a second for a sample).
	readTimeout  = 30 * time.Second
	closeTimeout = 15 * time.Second
)

// Run soaks the stack for cfg.Duration: it samples memory, swap and the
// stack's own state on an interval while one interactive session and the
// ingestion loop run, then judges the whole run (Evaluate).
//
// It returns an error, and no result, only when the run could not start: a bad
// configuration, a stack that fails its preflight, memory that cannot be read,
// a decision axis the stack is not in. Once the clock is running every
// failure is a recorded observation, and an interrupt (ctx cancelled) still
// returns the result, marked incomplete, which is a failure.
func Run(ctx context.Context, cfg Config, deps Deps) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Mem == nil || deps.Driver == nil {
		return nil, errors.New("soak: a memory source and a driver are required")
	}
	if deps.RunID == "" {
		deps.RunID = ids.New(runIDPrefix)
	}
	cfg.Target.Memory = deps.Mem.Describe()
	cfg.Target.Probe = "none"
	if deps.Probe != nil {
		cfg.Target.Probe = deps.Probe.Kind()
	}
	rules := cfg.Rules(deps.Probe != nil)

	pctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	base, err := preflight(pctx, cfg, deps)
	cancel()
	if err != nil {
		return nil, err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		deps.Driver.Close(cctx)
	}()

	started := time.Now()
	r := &runner{cfg: cfg, deps: deps, started: started}
	r.addSample(base)
	runCtx, stop := context.WithTimeout(ctx, cfg.Duration)
	defer stop()

	var wg sync.WaitGroup
	loops := []func(context.Context){r.sampler}
	if cfg.Mix.Interactive.Enabled {
		loops = append(loops, r.interactive)
	}
	if cfg.Mix.Ingestion.Enabled {
		loops = append(loops, r.ingestion)
	}
	for _, loop := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(runCtx)
		}()
	}
	wg.Wait()

	// One last reading, even after an interrupt: the end of a run is when a leak shows.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*readTimeout)
	r.addSample(r.take(lctx))
	lcancel()

	obs := r.observations(ctx.Err() == nil)
	return newResult(cfg, rules, deps, started, time.Since(started), obs), nil
}

// preflight is everything that must hold before the clock starts: the driver
// can reach the stack, memory can be read, and the stack is in the state the
// decision axis says. It returns the first reading, which opens the run.
func preflight(ctx context.Context, cfg Config, deps Deps) (Sample, error) {
	if err := deps.Driver.Preflight(ctx); err != nil {
		return Sample{}, fmt.Errorf("preflight: %w", err)
	}
	r := &runner{cfg: cfg, deps: deps}
	s, memErr, stackErr := r.read(ctx)
	if memErr != nil {
		return Sample{}, fmt.Errorf("preflight: memory cannot be read, and a run that measures nothing is not started: %w", memErr)
	}
	if deps.Probe != nil {
		if stackErr != nil {
			return Sample{}, fmt.Errorf("preflight: the stack cannot be read, and a run that cannot see eviction is not started: %w", stackErr)
		}
		if s.Stack.Roles != nil {
			if c := s.Stack.Roles["controller"]; !c.Configured || c.Status != statusOK {
				return Sample{}, fmt.Errorf("preflight: the controller role is not ok (configured %v, status %q): the stack is not ready to be soaked", c.Configured, c.Status)
			}
			var mismatch string
			evalDecision(cfg.Rules(true), s.Stack.Roles, func(_, format string, args ...any) { mismatch = fmt.Sprintf(format, args...) })
			if mismatch != "" {
				return Sample{}, fmt.Errorf("preflight: %s", mismatch)
			}
		}
	}
	return s, nil
}

// runner holds the state of one run.
type runner struct {
	cfg     Config
	deps    Deps
	started time.Time

	mu      sync.Mutex
	samples []Sample
	turns   []TurnRecord
	ingests []IngestRecord
}

func (r *runner) since() int64 { return time.Since(r.started).Milliseconds() }

func (r *runner) addSample(s Sample) {
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.mu.Unlock()
}

func (r *runner) observations(completed bool) Observations {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Observations{
		Completed: completed,
		Samples:   append([]Sample{}, r.samples...),
		Turns:     append([]TurnRecord{}, r.turns...),
		Ingests:   append([]IngestRecord{}, r.ingests...),
	}
}

// take reads memory and, when there is a probe, the stack. A failed reading is
// part of the sample, not an error of the run.
func (r *runner) take(ctx context.Context) Sample {
	s, _, _ := r.read(ctx)
	return s
}

// read is take with the errors of the two readings, for the preflight to name.
func (r *runner) read(ctx context.Context) (s Sample, memErr, stackErr error) {
	if !r.started.IsZero() {
		s.AtMS = r.since()
	}
	mctx, cancel := context.WithTimeout(ctx, readTimeout)
	m, err := r.deps.Mem.Sample(mctx)
	cancel()
	if err == nil {
		s.MemOK, s.Mem = true, m
	} else {
		memErr = err
	}
	if r.deps.Probe != nil {
		sctx, cancel := context.WithTimeout(ctx, readTimeout)
		st, err := r.deps.Probe.Stats(sctx)
		cancel()
		if err == nil {
			s.StackOK, s.Stack = true, st
		} else {
			stackErr = err
		}
	}
	return s, memErr, stackErr
}

// sampler takes a reading every interval until the run ends.
func (r *runner) sampler(ctx context.Context) {
	t := time.NewTicker(r.cfg.SampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := r.take(ctx)
			if ctx.Err() != nil && !s.MemOK {
				return // cut off by the end of the run, not a failed reading
			}
			r.addSample(s)
		}
	}
}

// interactive is one session: the steps in order, round again, a pause after
// each. A turn the end of the run cut off is dropped, not counted as a failure.
func (r *runner) interactive(ctx context.Context) {
	mix := r.cfg.Mix.Interactive
	for i := 0; ctx.Err() == nil; i++ {
		step := mix.Steps[i%len(mix.Steps)]
		at := r.since()
		t0 := time.Now()
		err := r.deps.Driver.Turn(ctx, step)
		ms := float64(time.Since(t0)) / float64(time.Millisecond)
		if err != nil && ctx.Err() != nil {
			return
		}
		rec := TurnRecord{AtMS: at, Step: step.ID, Class: step.Class, MS: round3v(ms), Err: errorCode(err)}
		r.mu.Lock()
		r.turns = append(r.turns, rec)
		r.mu.Unlock()
		if !sleep(ctx, mix.Think()) {
			return
		}
	}
}

// ingestion re-ingests the source over and over, with a pause between.
func (r *runner) ingestion(ctx context.Context) {
	mix := r.cfg.Mix.Ingestion
	for ctx.Err() == nil {
		at := r.since()
		t0 := time.Now()
		err := r.deps.Driver.Ingest(ctx)
		ms := float64(time.Since(t0)) / float64(time.Millisecond)
		if err != nil && ctx.Err() != nil {
			return
		}
		rec := IngestRecord{AtMS: at, MS: round3v(ms), Err: errorCode(err)}
		r.mu.Lock()
		r.ingests = append(r.ingests, rec)
		r.mu.Unlock()
		if !sleep(ctx, mix.Pause()) {
			return
		}
	}
}

// sleep waits d or until ctx ends; it reports whether the wait completed. A
// wait of zero still yields, so a loop with no pause cannot starve the sampler.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		time.Sleep(time.Millisecond)
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errorCode is the typed code of a failure, "" for none. A failure that is not
// typed is internal, never a success-looking result (contracts.CodeOf).
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return string(contracts.CodeOf(err))
}
