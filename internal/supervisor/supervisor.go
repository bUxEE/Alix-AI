package supervisor

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// State is where the supervisor is in its cycle.
type State string

const (
	// StateStopped: not running (never run, or Run returned after shutdown).
	StateStopped State = "stopped"
	// StateStarting: an instance is being started and has not been healthy yet.
	StateStarting State = "starting"
	// StateRunning: the instance has been healthy and is being watched.
	StateRunning State = "running"
	// StateBackoff: the last instance failed; the next attempt is scheduled.
	StateBackoff State = "backoff"
	// StateGaveUp: the failure limit was reached. Nothing is running and nothing
	// will be started until Run is called again.
	StateGaveUp State = "gave_up"
)

// EventType names a point in the supervision cycle.
type EventType string

const (
	EventStarted EventType = "started"
	EventReady   EventType = "ready"
	EventExited  EventType = "exited"
	EventSettled EventType = "settled"
	EventBackoff EventType = "backoff"
	EventGaveUp  EventType = "gave_up"
	EventStopped EventType = "stopped"
)

// Failure reasons: short and fixed. Nothing the child prints reaches them.
const (
	ReasonStartFailed  = "start_failed"
	ReasonExited       = "exited"
	ReasonStartTimeout = "start_timeout"
	ReasonUnhealthy    = "unhealthy"
	ReasonStopFailed   = "stop_failed"
	reasonShutdown     = "shutdown"
)

// Fixed health reasons the supervisor reports while it is not running.
const (
	healthStarting = "runtime starting"
	healthBackoff  = "runtime failed; restart scheduled"
	healthGaveUp   = "runtime failed repeatedly; supervisor gave up, operator action required"
	healthStopped  = "runtime supervisor not running"
)

// stopTimeout bounds how long the supervisor waits for an instance to be
// confirmed gone, and settleTimeout bounds the receipt lookups at a crash.
const (
	stopTimeout   = 30 * time.Second
	settleTimeout = 10 * time.Second
)

// Event is one observation of the supervision cycle, passed to the WithEvents
// handler. It carries no text from the child.
type Event struct {
	Type    EventType
	At      time.Time
	Attempt int // how many starts have been attempted in this Run
	PID     int
	Reason  string
	Detail  string        // "exit status 3" and the like, fixed format
	Delay   time.Duration // for EventBackoff: the wait before the next attempt
	// Settlements is set on EventSettled.
	Settlements []Settlement
}

// Status is a snapshot of the supervisor.
type Status struct {
	State State
	PID   int
	// Starts counts start attempts in this Run.
	Starts int
	// RecentFailures counts failures inside the restart window.
	RecentFailures int
	LastFailure    string
	// RetryAt is when the next attempt is due, in StateBackoff.
	RetryAt time.Time
	Since   time.Time
	// Settlements are the last in-flight settlements.
	Settlements []Settlement
}

// Option customises a Supervisor.
type Option func(*Supervisor)

// WithClock sets the clock that stamps events and ages failures out of the
// window.
func WithClock(c clock.Clock) Option { return func(s *Supervisor) { s.clk = c } }

// WithLedger sets the in-flight ledger. The default has no receipt source, so
// every settlement is uncommitted.
func WithLedger(l *Ledger) Option { return func(s *Supervisor) { s.ledger = l } }

// WithLogger sets the logger (use the redacting logger from internal/log).
func WithLogger(l *slog.Logger) Option { return func(s *Supervisor) { s.log = l } }

// WithEvents sets a handler called synchronously for every event. It must not
// block and must not call back into the supervisor's Run.
func WithEvents(h func(Event)) Option { return func(s *Supervisor) { s.onEvent = h } }

// WithAfter replaces the timer behind every wait (probe spacing, start timeout,
// backoff). Tests use it to run without real delays.
func WithAfter(f func(time.Duration) <-chan time.Time) Option {
	return func(s *Supervisor) { s.after = f }
}

// WithRand replaces the source of backoff jitter: a uniform draw from [0, 1).
func WithRand(f func() float64) Option { return func(s *Supervisor) { s.rnd = f } }

// Supervisor runs the supervision loop for one Runtime and answers for its
// health. It implements providers.AIRuntimeProvider.
type Supervisor struct {
	cfg     Config
	rt      Runtime
	probe   providers.AIRuntimeProvider
	clk     clock.Clock
	ledger  *Ledger
	log     *slog.Logger
	onEvent func(Event)
	after   func(time.Duration) <-chan time.Time
	rnd     func() float64

	mu       sync.Mutex
	st       Status
	failures []time.Time
	running  bool
}

var _ providers.AIRuntimeProvider = (*Supervisor)(nil)

// New builds a Supervisor over a Runtime. probe is the runtime provider whose
// Health defines healthy (the LocalAI adapter, injected by the composition
// root; this package never reaches LocalAI itself).
func New(cfg Config, rt Runtime, probe providers.AIRuntimeProvider, opts ...Option) (*Supervisor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "supervisor: invalid configuration")
	}
	if rt == nil || probe == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "supervisor: a runtime and a health probe are required")
	}
	s := &Supervisor{
		cfg:   cfg.WithDefaults(),
		rt:    rt,
		probe: probe,
		clk:   clock.Real{},
		log:   slog.New(slog.DiscardHandler),
		after: time.After,
		rnd:   rand.Float64,
		st:    Status{State: StateStopped},
	}
	for _, o := range opts {
		o(s)
	}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	if s.ledger == nil {
		s.ledger = NewLedger(s.clk, nil)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.after == nil {
		s.after = time.After
	}
	if s.rnd == nil {
		s.rnd = rand.Float64
	}
	s.st.Since = s.clk.Now()
	return s, nil
}

// Ledger is the in-flight ledger the supervisor settles at every crash.
func (s *Supervisor) Ledger() *Ledger { return s.ledger }

// Status returns a snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Settlements = append([]Settlement(nil), st.Settlements...)
	return st
}

// Admit reports whether model-driven work may start now. It fails closed: only
// a running, healthy-once instance admits work.
func (s *Supervisor) Admit() error {
	if st := s.Status().State; st != StateRunning {
		return contracts.Newf(contracts.ErrUnavailable, "supervisor: the model runtime is %s", string(st))
	}
	return nil
}

// Name implements providers.AIRuntimeProvider with the supervised runtime's name.
func (s *Supervisor) Name() string { return s.probe.Name() }

// Health implements providers.AIRuntimeProvider. While the instance is running
// it is the runtime's own report (so degraded and version still flow through);
// in every other state it is unavailable with a fixed reason.
func (s *Supervisor) Health(ctx context.Context) (providers.HealthReport, error) {
	switch s.Status().State {
	case StateRunning:
		return s.probe.Health(ctx)
	case StateStarting:
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: healthStarting}, nil
	case StateBackoff:
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: healthBackoff}, nil
	case StateGaveUp:
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: healthGaveUp}, nil
	default:
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: healthStopped}, nil
	}
}

// Run supervises until ctx ends, then stops the instance and returns nil. It
// returns an unavailable error, with the state left at StateGaveUp, when the
// failure limit is reached or an instance cannot be confirmed stopped. Only one
// Run may be active; calling Run again after it returned starts a fresh budget.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return contracts.New(contracts.ErrConflict, "supervisor: already running")
	}
	s.running = true
	s.failures = nil
	s.st = Status{State: StateStopped, Since: s.clk.Now()}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	var starts int
	for {
		if ctx.Err() != nil {
			return s.stopped()
		}
		s.set(func(st *Status) { st.State, st.PID, st.Starts = StateStarting, 0, starts+1 })
		starts++
		inst, err := s.rt.Start(ctx)

		var reason, detail string
		if err != nil {
			if ctx.Err() != nil {
				return s.stopped()
			}
			reason = ReasonStartFailed
			s.log.Warn("supervisor: runtime start failed", "attempt", starts, "err_code", string(contracts.CodeOf(err)))
		} else {
			s.set(func(st *Status) { st.PID = inst.PID() })
			s.emit(Event{Type: EventStarted, Attempt: starts, PID: inst.PID()})
			reason, detail = s.watch(ctx, inst, starts)
			if stopErr := s.stop(inst); stopErr != nil {
				// A process that may still be alive is never replaced: that would
				// be two LocalAI instances sharing the models and the port.
				s.settle(ReasonStopFailed)
				return s.giveUp(starts, ReasonStopFailed, "supervisor: the runtime could not be confirmed stopped")
			}
			if reason != "" {
				s.emit(Event{Type: EventExited, Attempt: starts, PID: inst.PID(), Reason: reason, Detail: detail})
			}
		}

		// Before any restart: in-flight actions are settled, never resumed.
		if reason == "" {
			s.settle(reasonShutdown)
			return s.stopped()
		}
		s.settle(reason)

		n := s.recordFailure()
		s.set(func(st *Status) { st.LastFailure, st.PID = reason, 0 })
		if n > s.cfg.MaxRestarts {
			return s.giveUp(starts, reason, "supervisor: the runtime failed too often; giving up")
		}
		delay := s.cfg.Backoff.Delay(n, s.rnd())
		retry := s.clk.Now().Add(delay)
		s.set(func(st *Status) { st.State, st.RetryAt = StateBackoff, retry })
		s.emit(Event{Type: EventBackoff, Attempt: starts, Reason: reason, Delay: delay})
		select {
		case <-ctx.Done():
			return s.stopped()
		case <-s.after(delay):
		}
	}
}

// watch follows one instance until it fails or ctx ends. It returns the failure
// reason, or "" when ctx ended.
func (s *Supervisor) watch(ctx context.Context, inst Instance, attempt int) (reason, detail string) {
	exited := func() (string, string) {
		d := ""
		if x, ok := inst.(exitDetailer); ok {
			d = x.ExitDetail()
		}
		return ReasonExited, d
	}
	startTimer := s.after(s.cfg.StartTimeout)
	ready := false
	bad := 0
	for {
		select {
		case <-inst.Done():
			return exited()
		default:
		}
		if ctx.Err() != nil {
			return "", ""
		}
		healthy := s.probeOnce(ctx)
		if ctx.Err() != nil {
			return "", ""
		}
		switch {
		case !ready && healthy:
			ready = true
			s.set(func(st *Status) { st.State = StateRunning })
			s.emit(Event{Type: EventReady, Attempt: attempt, PID: inst.PID()})
		case ready && healthy:
			bad = 0
		case ready:
			if bad++; bad >= s.cfg.UnhealthyAfter {
				return ReasonUnhealthy, ""
			}
		}

		wait := s.cfg.ProbeInterval
		timeout := startTimer
		if ready {
			wait, timeout = s.cfg.HealthInterval, nil
		}
		select {
		case <-ctx.Done():
			return "", ""
		case <-inst.Done():
			return exited()
		case <-timeout:
			return ReasonStartTimeout, ""
		case <-s.after(wait):
		}
	}
}

// probeOnce asks the injected provider once, with its own timeout. Only ok and
// degraded count as healthy; an error, a timeout, unavailable or an invalid
// status do not.
func (s *Supervisor) probeOnce(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.ProbeTimeout)
	defer cancel()
	rep, err := s.probe.Health(pctx)
	if err != nil {
		return false
	}
	return rep.Status == providers.HealthOK || rep.Status == providers.HealthDegraded
}

// stop ends the instance and waits until it is confirmed gone. It uses its own
// context: the one passed to Run is already cancelled on shutdown.
func (s *Supervisor) stop(inst Instance) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := inst.Stop(ctx); err != nil {
		s.log.Error("supervisor: the runtime could not be stopped", "pid", inst.PID(), "err_code", string(contracts.CodeOf(err)))
		return err
	}
	return nil
}

// settle ends the in-flight actions of the lost instance and publishes what
// became of them.
func (s *Supervisor) settle(cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	out := s.ledger.Settle(ctx)
	if len(out) == 0 {
		return
	}
	s.set(func(st *Status) { st.Settlements = out })
	for _, o := range out {
		s.log.Info("supervisor: in-flight action settled",
			"preparation", o.PreparationID, "committed", o.Committed, "reason", o.Reason, "cause", cause)
	}
	s.emit(Event{Type: EventSettled, Reason: cause, Settlements: out})
}

// recordFailure notes a failure now and returns how many fall inside the window.
func (s *Supervisor) recordFailure() int {
	now := s.clk.Now()
	cutoff := now.Add(-s.cfg.RestartWindow)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.failures[:0]
	for _, t := range s.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.failures = append(kept, now)
	s.st.RecentFailures = len(s.failures)
	return len(s.failures)
}

func (s *Supervisor) giveUp(starts int, reason, msg string) error {
	s.set(func(st *Status) { st.State, st.PID = StateGaveUp, 0 })
	s.emit(Event{Type: EventGaveUp, Attempt: starts, Reason: reason})
	s.log.Error("supervisor: giving up", "starts", starts, "reason", reason)
	return contracts.New(contracts.ErrUnavailable, msg).WithDetails("reason", reason)
}

func (s *Supervisor) stopped() error {
	s.set(func(st *Status) { st.State, st.PID = StateStopped, 0 })
	s.emit(Event{Type: EventStopped})
	return nil
}

func (s *Supervisor) set(f func(*Status)) {
	s.mu.Lock()
	before := s.st.State
	f(&s.st)
	if s.st.State != before {
		s.st.Since = s.clk.Now()
	}
	s.mu.Unlock()
}

func (s *Supervisor) emit(e Event) {
	e.At = s.clk.Now()
	s.log.Debug("supervisor: event", "type", string(e.Type), "attempt", e.Attempt, "pid", e.PID, "reason", e.Reason)
	if s.onEvent != nil {
		s.onEvent(e)
	}
}
