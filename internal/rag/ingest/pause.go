package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
)

// Verdict is the pause's current answer.
type Verdict struct {
	// Paused is true while ingestion must not write.
	Paused bool
	// Reason is "disk_pressure" or "disk_pressure_unknown" (the hook failed, so
	// the answer is unknown and treated as pressure).
	Reason string
}

// Pause is the disk-pressure gate of ingestion (master plan 18: "pause
// ingestion before interactive or transactional storage is endangered"). It
// caches the hook's verdict for a short interval, because the question is asked
// from inside the job queue's lease transaction (Policy) and at the start of
// every stage.
//
// A Pause stands alone, built from the hook and the clock, because the queue
// takes its DequeuePolicy when it is created and the Service needs the queue:
// the composition root creates the Pause first, installs Policy on the queue
// (jobs.Options.Policy) and hands the same value to the Service (Deps.Pause).
// A Pause is safe for concurrent use.
type Pause struct {
	hook DiskPressure
	clk  clock.Clock
	ttl  time.Duration
	log  *slog.Logger

	mu     sync.Mutex
	at     time.Time
	paused bool
	known  bool
	why    string
}

// NewPause returns a Pause over the hook. A nil hook never pauses. A verdict is
// reused for recheck (zero means DefaultPressureRecheck); a nil clock means
// clock.Real and a nil logger discards.
func NewPause(hook DiskPressure, clk clock.Clock, recheck time.Duration, log *slog.Logger) *Pause {
	if clk == nil {
		clk = clock.Real{}
	}
	if recheck <= 0 {
		recheck = DefaultPressureRecheck
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pause{hook: hook, clk: clk, ttl: recheck, log: log}
}

// Verdict is the current answer, asking the hook when the cached one is older
// than the recheck interval.
func (p *Pause) Verdict(ctx context.Context) Verdict {
	if p == nil || p.hook == nil {
		return Verdict{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clk.Now()
	if p.known && now.Sub(p.at) < p.ttl && !now.Before(p.at) {
		return Verdict{Paused: p.paused, Reason: p.why}
	}
	under, err := p.hook.UnderPressure(ctx)
	paused, why := under, ""
	switch {
	case err != nil:
		paused, why = true, "disk_pressure_unknown"
	case under:
		why = "disk_pressure"
	}
	switch {
	case p.known && p.paused != paused:
		p.log.Warn("ingestion pause changed", "paused", paused, "reason", why)
	case !p.known && paused:
		p.log.Warn("ingestion paused", "reason", why)
	}
	p.at, p.paused, p.known, p.why = now, paused, true, why
	return Verdict{Paused: paused, Reason: why}
}

// Invalidate drops the cached verdict, so the next question asks the hook.
func (p *Pause) Invalidate() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.known = false
	p.mu.Unlock()
}

// Policy returns a jobs.DequeuePolicy that stops the queue from leasing
// ingestion work while the hook reports pressure, and otherwise defers to
// inner (nil means the queue's FIFO). Pausing at the lease means no attempt of
// any job is spent on the pause and nothing is written: the jobs simply wait
// in the queue until the pressure is gone. Install it as jobs.Options.Policy;
// interactive classes are untouched, because only the job types of this
// package (and the embedding job they queue) are held back.
//
// The queue offers a policy the oldest ready job of each tenant in the class,
// so a class that holds both ingestion work and other work of one tenant lets
// the older of the two decide. Classes 5 and 6 are ingestion's own, which is
// why that is acceptable; the stage handlers check the pause again when they
// start, for the window between the lease and the start.
//
// The policy runs inside the lease transaction, so it must be quick: the hook
// is consulted at most once per recheck interval.
func (p *Pause) Policy(inner jobs.DequeuePolicy) jobs.DequeuePolicy {
	types := map[string]bool{}
	for _, name := range PausedJobTypes() {
		types[name] = true
	}
	return jobs.DequeuePolicyFunc(func(ready []jobs.Candidate) int {
		if !p.Verdict(context.Background()).Paused {
			if inner == nil {
				if len(ready) == 0 {
					return -1
				}
				return 0
			}
			return inner.Pick(ready)
		}
		keep := make([]jobs.Candidate, 0, len(ready))
		at := make([]int, 0, len(ready))
		for i, c := range ready {
			if !types[c.Type] {
				keep = append(keep, c)
				at = append(at, i)
			}
		}
		if len(keep) == 0 {
			return -1
		}
		if inner == nil {
			return at[0]
		}
		if j := inner.Pick(keep); j >= 0 && j < len(at) {
			return at[j]
		}
		return -1
	})
}

// Paused reports whether ingestion is paused by disk pressure now.
func (s *Service) Paused(ctx context.Context) Verdict { return s.pause.Verdict(ctx) }

// Pause returns the gate the service consults.
func (s *Service) Pause() *Pause { return s.pause }
