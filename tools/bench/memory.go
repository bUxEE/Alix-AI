package main

import (
	"context"
	"sync"
	"time"
)

// MemSample is one reading of a container's memory, in MiB. LimitMiB is 0 when
// the source does not report a limit.
type MemSample struct {
	UsedMiB  float64
	LimitMiB float64
}

// MemorySource takes one reading. The docker implementation is dockerStats;
// tests supply scripted sources.
type MemorySource interface {
	Sample(ctx context.Context) (MemSample, error)
}

// sampleTimeout bounds one reading (docker stats --no-stream itself waits for
// a sample, about a second).
const sampleTimeout = 20 * time.Second

// MemoryPlan turns sampling on for a run.
type MemoryPlan struct {
	Source    MemorySource
	Container string
	Interval  time.Duration
}

// memCollector samples a MemorySource on an interval while the benchmark runs.
// The first reading is taken synchronously before warm-up (the baseline), the
// last one in stop, so even a run shorter than one interval has both. It owns
// one goroutine, which stop always ends.
type memCollector struct {
	plan   MemoryPlan
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	used   []float64
	mark   int // used[mark:] was taken after warm-up
	errs   int
	limit  float64
	closed bool
}

// startMemory takes the baseline reading and starts the sampling goroutine. It
// fails when the baseline cannot be taken: a run that was asked to measure
// memory and cannot must not silently measure nothing.
func startMemory(ctx context.Context, plan MemoryPlan) (*memCollector, error) {
	m := &memCollector{plan: plan, done: make(chan struct{})}
	s, err := m.take(ctx)
	if err != nil {
		return nil, err
	}
	m.record(s)
	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx)
	return m, nil
}

func (m *memCollector) take(ctx context.Context) (MemSample, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	return m.plan.Source.Sample(ctx)
}

func (m *memCollector) record(s MemSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used = append(m.used, s.UsedMiB)
	if s.LimitMiB > 0 {
		m.limit = s.LimitMiB
	}
}

func (m *memCollector) loop(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(m.plan.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s, err := m.take(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.mu.Lock()
				m.errs++
				m.mu.Unlock()
				continue
			}
			m.record(s)
		}
	}
}

// observedLimit returns the container memory limit seen so far (MiB), 0 when
// the source reports none.
func (m *memCollector) observedLimit() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limit
}

// markMeasure notes that warm-up is over: the steady-state figure uses only
// readings taken from here on.
func (m *memCollector) markMeasure() {
	m.mu.Lock()
	m.mark = len(m.used)
	m.mu.Unlock()
}

// stop ends sampling, takes a last reading and returns the summary. ctx is the
// run's context only for the last reading; the goroutine is always ended.
func (m *memCollector) stop(ctx context.Context) MemoryInfo {
	m.mu.Lock()
	already := m.closed
	m.closed = true
	m.mu.Unlock()
	if !already {
		m.cancel()
		<-m.done
		if s, err := m.take(context.WithoutCancel(ctx)); err == nil {
			m.record(s)
		} else {
			m.mu.Lock()
			m.errs++
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	base, peak, steady := summarizeMemory(m.used, m.mark)
	interval := m.plan.Interval.Milliseconds()
	container := m.plan.Container
	info := MemoryInfo{
		Sampled: true, Source: MemSourceDocker, Container: &container, IntervalMS: &interval,
		Samples: len(m.used), SampleErrors: m.errs,
		BaselineMB: base, PeakRSSMB: peak, SteadyRSSMB: steady,
	}
	if m.limit > 0 {
		info.LimitMBObserved = round3(m.limit)
	}
	return info
}

// summarizeMemory reduces readings (MiB, oldest first) to the baseline (the
// first), the peak (the largest of all, warm-up included, because loading the
// model is when memory peaks) and the steady figure: the median of the
// readings taken from index mark on, or the last reading when there are none.
func summarizeMemory(used []float64, mark int) (baseline, peak, steady *float64) {
	if len(used) == 0 {
		return nil, nil, nil
	}
	baseline = round3(used[0])
	hi := used[0]
	for _, u := range used {
		if u > hi {
			hi = u
		}
	}
	peak = round3(hi)
	if mark < 0 || mark > len(used) {
		mark = len(used)
	}
	after := used[mark:]
	if len(after) == 0 {
		steady = round3(used[len(used)-1])
		return
	}
	if v, ok := median(after); ok {
		steady = round3(v)
	}
	return
}

// noMemory is the memory block of a run that did not sample.
func noMemory() MemoryInfo {
	return MemoryInfo{Sampled: false, Source: MemSourceNone}
}

// limitMatches reports whether the memory limit Docker reported for the
// container (MiB) is the limit the profile stands for, within 1%: docker stats
// rounds what it prints.
func limitMatches(observedMiB float64, wantMB int) bool {
	want := float64(wantMB)
	d := observedMiB - want
	if d < 0 {
		d = -d
	}
	return d <= want*0.01
}
