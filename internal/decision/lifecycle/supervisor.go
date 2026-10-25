package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Revision is one loadable version of a scorer: the manifest that identifies it (with
// the hash of every file it loads) and the measured peak memory the model registry
// records for it (D-11), which is what the resource gate is asked to reserve.
type Revision struct {
	Manifest  gen.ScorerManifest
	PeakBytes uint64
	// SignatureWindow is the validity window of Manifest.Signature, bound into the signed
	// bytes (see ManifestSigningPayload). Update refuses a revision without a valid one.
	SignatureWindow ManifestSignatureWindow
}

func (r Revision) valid() error {
	m := r.Manifest
	if m.ProviderID == "" || m.ModelID == "" || m.ModelRevision == "" {
		return contracts.New(contracts.ErrValidationFailed, "lifecycle: a revision needs a provider, model and model revision")
	}
	return nil
}

// Instance is a loaded scorer: the unchanged DecisionModelProvider contract plus the
// two operations a supervisor needs. Health is a cheap liveness-and-readiness probe of
// the runtime; Close frees its memory. Both honour ctx.
type Instance interface {
	decision.DecisionModelProvider
	Health(ctx context.Context) error
	Close(ctx context.Context) error
}

// Loader starts an Instance for a revision (a sidecar client, a LocalAI backend, a
// fake). It is the only place a runtime is touched, so replacing the scorer is
// replacing the Loader or the Revision, never the contract.
type Loader interface {
	Load(ctx context.Context, rev Revision) (Instance, error)
}

// Reservation is memory the resource gate granted. Release gives it back.
type Reservation interface {
	Release()
}

// ResourceGate is the lifecycle's view of the resource governor (internal/resources,
// M3-17). It is deliberately an admit-or-reject interface and nothing more: there is
// no way through it to evict, pause or shrink the conversational controller, retrieval
// or speech workers, so a scorer that does not fit is rejected, never made to fit. An
// adapter over resources.Governor.Admit (its *Ticket satisfies Reservation) belongs in
// the composition root.
type ResourceGate interface {
	// Admit reserves bytes for label or fails. Any error is a rejection.
	Admit(ctx context.Context, bytes uint64, label string) (Reservation, error)
}

// Config tunes the supervisor. Every duration is measured on the injected clock.
type Config struct {
	// MinWarm is the minimum time a loaded scorer stays loaded before an idle unload
	// is allowed (master plan 17.2), so a burst of sparse requests does not thrash.
	MinWarm time.Duration
	// IdleUnload is how long a scorer may sit unused before Tick unloads it. Zero
	// keeps it loaded until Stop.
	IdleUnload time.Duration
	// BackoffInitial and BackoffMax bound the restart delay, which doubles per
	// consecutive failure.
	BackoffInitial, BackoffMax time.Duration
	// LoadTimeout bounds one load; ProbeTimeout bounds one health probe.
	LoadTimeout, ProbeTimeout time.Duration
	// Probation keeps the previous revision resident after a staged update switches,
	// so a failed probe or a reported gate breach can return to it. Zero promotes at
	// once and keeps nothing.
	Probation time.Duration
	// Profile labels the metrics (for example lite or balanced).
	Profile string
}

// DefaultConfig returns starting values (hypotheses, to be replaced by measured ones).
func DefaultConfig() Config {
	return Config{
		MinWarm: 5 * time.Minute, IdleUnload: 10 * time.Minute,
		BackoffInitial: time.Second, BackoffMax: time.Minute,
		LoadTimeout: 2 * time.Minute, ProbeTimeout: 5 * time.Second,
	}
}

func (c Config) validate() error {
	switch {
	case c.MinWarm < 0 || c.IdleUnload < 0 || c.Probation < 0:
		return fmt.Errorf("lifecycle: durations must not be negative")
	case c.BackoffInitial <= 0 || c.BackoffMax < c.BackoffInitial:
		return fmt.Errorf("lifecycle: need 0 < BackoffInitial <= BackoffMax")
	case c.LoadTimeout <= 0 || c.ProbeTimeout <= 0:
		return fmt.Errorf("lifecycle: LoadTimeout and ProbeTimeout must be positive")
	}
	return nil
}

// Deps are the supervisor's collaborators; Loader, Gate and a valid Revision are
// required (without a gate there is nothing to stop the scorer starving other work).
type Deps struct {
	Clock    clock.Clock
	Loader   Loader
	Gate     ResourceGate
	Registry Registry
	Revision Revision
	Config   Config
	// Verifier checks the manifest signature of every revision Update is offered. It is
	// optional here so a supervisor that never updates needs none; without one, Update
	// fails closed and trusts no revision.
	Verifier *ManifestVerifier
}

type state int

const (
	stateUnloaded state = iota
	stateBusy           // a load is in flight
	stateReady
	stateFailed // backing off after a failure
)

func (s state) String() string {
	return [...]string{"unloaded", "loading", "ready", "backoff"}[s]
}

const (
	gateLabel       = "decision-scorer"
	stagedGateLabel = "decision-scorer-staged"
)

// slot is one loaded instance and its reservation.
type slot struct {
	rev      Revision
	inst     Instance
	res      Reservation
	loadedAt time.Time
	inflight int
	force    bool // close even with calls in flight (the instance is dead)
}

type probation struct {
	prevRev Revision
	prev    *slot // nil when nothing was resident at the switch
	until   time.Time
}

// Supervisor owns the life of one scorer. It implements decision.DecisionModelProvider
// itself, so the Engine (or a providers.Registry router role) holds a Supervisor and
// never learns which runtime, model or revision is behind it. It is optional, bounded,
// non-generative and non-authoritative like the layer it serves: it loads, probes and
// unloads a scorer and never looks at what the scorer says.
//
// It starts no goroutines. The owner calls Tick periodically (health probe, idle
// unload, restart after backoff, end of probation).
type Supervisor struct {
	clk    clock.Clock
	loader Loader
	gate   ResourceGate
	cfg    Config
	met    *Metrics
	verify *ManifestVerifier

	mu        sync.Mutex
	rev       Revision // the revision that serves (loaded or to be demand-loaded)
	state     state
	cur       *slot
	busy      chan struct{}
	lastUsed  time.Time
	probeOK   bool
	failures  int
	nextRetry time.Time
	lastErr   string
	retired   []*slot
	prob      *probation
	updating  bool
	stopped   bool
}

var _ decision.DecisionModelProvider = (*Supervisor)(nil)

// New validates d and returns a supervisor with nothing loaded.
func New(d Deps) (*Supervisor, error) {
	if d.Loader == nil || d.Gate == nil {
		return nil, fmt.Errorf("lifecycle: a loader and a resource gate are required")
	}
	if err := d.Revision.valid(); err != nil {
		return nil, err
	}
	if err := d.Config.validate(); err != nil {
		return nil, err
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return &Supervisor{
		clk: clk, loader: d.Loader, gate: d.Gate, cfg: d.Config, rev: d.Revision, verify: d.Verifier,
		met: NewMetrics(d.Registry, Labels{Profile: d.Config.Profile}),
	}, nil
}

// Metrics returns the recorder, for the dialogue integration to call ObserveAdvice.
func (s *Supervisor) Metrics() *Metrics { return s.met }

// Manifest implements decision.DecisionModelProvider: the manifest of the revision that
// serves requests now.
func (s *Supervisor) Manifest() gen.ScorerManifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev.Manifest
}

// ScoreDecision implements decision.DecisionModelProvider. It demand-loads the scorer
// when it is not resident. When the resource gate rejects the load it returns a
// resource_exhausted error (the Engine records resource_gate); when the runtime is
// down or backing off it returns unavailable. Neither touches another worker.
func (s *Supervisor) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	sl, err := s.acquire(ctx)
	if err != nil {
		return gen.DecisionScoreResult{}, err
	}
	// A panicking instance still gives its in-flight slot back (and counts as a dead runtime)
	// before the panic travels on to the Engine, which records it as a fallback.
	returned := false
	defer func() {
		if !returned {
			s.finish(ctx, sl, contracts.New(contracts.ErrUnavailable, "lifecycle: the scorer panicked"))
		}
	}()
	res, err := sl.inst.ScoreDecision(ctx, scope, req)
	returned = true
	s.finish(ctx, sl, err)
	return res, err
}

func (s *Supervisor) acquire(ctx context.Context) (*slot, error) {
	for {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return nil, contracts.New(contracts.ErrUnavailable, "lifecycle: the scorer supervisor is stopped")
		}
		now := s.clk.Now()
		switch s.state {
		case stateReady:
			sl := s.cur
			sl.inflight++
			s.lastUsed = now
			s.mu.Unlock()
			return sl, nil
		case stateBusy:
			ch := s.busy
			s.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "lifecycle: waiting for the scorer to load")
			}
		case stateFailed:
			if now.Before(s.nextRetry) {
				retry := s.nextRetry
				s.mu.Unlock()
				return nil, contracts.New(contracts.ErrUnavailable, "lifecycle: the scorer is backing off after a failure").
					WithDetails("retry_at", retry.UTC().Format(time.RFC3339))
			}
		}
		ch, rev, prior := s.beginLoadLocked()
		s.mu.Unlock()
		if err := s.runLoad(ctx, rev, ch, prior); err != nil {
			return nil, err
		}
	}
}

// beginLoadLocked moves to the loading state; the caller then calls runLoad.
func (s *Supervisor) beginLoadLocked() (chan struct{}, Revision, state) {
	prior := s.state
	ch := make(chan struct{})
	s.state = stateBusy
	s.busy = ch
	return ch, s.rev, prior
}

func (s *Supervisor) endLoad(ch chan struct{}, to state) {
	s.mu.Lock()
	s.state = to
	s.busy = nil
	s.mu.Unlock()
	close(ch)
}

// runLoad admits, loads and probes the revision, then publishes the result. prior is
// the state before the load (unloaded or backing off).
func (s *Supervisor) runLoad(ctx context.Context, rev Revision, ch chan struct{}, prior state) error {
	res, gerr := s.gate.Admit(ctx, rev.PeakBytes, gateLabel)
	if gerr != nil {
		// Not a fault of the scorer: no backoff, no failure count; the next request asks again.
		s.endLoad(ch, prior)
		s.met.event(MetricLoadFailures, "resource_gate", rev)
		return gateRejection(gerr)
	}
	if prior == stateFailed {
		s.met.event(MetricRestarts, "", rev)
	}
	lctx, cancel := context.WithTimeout(ctx, s.cfg.LoadTimeout)
	inst, lerr := s.loader.Load(lctx, rev)
	if lerr == nil {
		lerr = checkIdentity(rev, inst)
	}
	if lerr == nil {
		lerr = s.probe(lctx, inst)
	}
	cancel()
	if lerr != nil {
		s.closeInstance(inst)
		res.Release()
		if ctx.Err() != nil { // the caller gave up; that is not the scorer's fault
			s.endLoad(ch, prior)
			return contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "lifecycle: the caller stopped waiting for the scorer to load")
		}
		s.mu.Lock()
		s.failures++
		s.nextRetry = s.clk.Now().Add(s.backoff(s.failures))
		s.lastErr = string(contracts.CodeOf(lerr))
		s.probeOK = false
		s.mu.Unlock()
		s.met.event(MetricLoadFailures, "load", rev)
		s.endLoad(ch, stateFailed)
		s.publishMemory()
		return contracts.Wrap(contracts.ErrUnavailable, lerr, "lifecycle: the scorer failed to start")
	}
	now := s.clk.Now()
	s.mu.Lock()
	if s.stopped { // Stop ran while the load was in flight
		s.mu.Unlock()
		s.closeInstance(inst)
		res.Release()
		s.endLoad(ch, stateUnloaded)
		return contracts.New(contracts.ErrUnavailable, "lifecycle: the scorer supervisor is stopped")
	}
	s.cur = &slot{rev: rev, inst: inst, res: res, loadedAt: now}
	s.lastUsed = now
	s.probeOK = true
	s.lastErr = ""
	s.mu.Unlock()
	s.endLoad(ch, stateReady)
	s.met.event(MetricLoads, "", rev)
	s.publishMemory()
	return nil
}

// gateRejection normalises any gate error to resource_exhausted: the gate refusing is
// the resource_gate fallback whatever it chose to call itself.
func gateRejection(err error) error {
	if contracts.HasCode(err, contracts.ErrResourceExhausted) {
		return err
	}
	return contracts.Wrap(contracts.ErrResourceExhausted, err, "lifecycle: the resource gate rejected the scorer")
}

func checkIdentity(rev Revision, inst Instance) error {
	if inst == nil {
		return contracts.New(contracts.ErrUnavailable, "lifecycle: the loader returned no instance")
	}
	got, want := inst.Manifest(), rev.Manifest
	if got.ProviderID != want.ProviderID || got.ModelID != want.ModelID || got.ModelRevision != want.ModelRevision {
		return contracts.New(contracts.ErrValidationFailed, "lifecycle: the loaded scorer is not the requested revision")
	}
	return nil
}

func (s *Supervisor) probe(ctx context.Context, inst Instance) error {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.ProbeTimeout)
	defer cancel()
	return inst.Health(pctx)
}

func (s *Supervisor) closeInstance(inst Instance) {
	if inst == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ProbeTimeout)
	defer cancel()
	_ = inst.Close(ctx)
}

func (s *Supervisor) backoff(failures int) time.Duration {
	d := s.cfg.BackoffInitial
	for i := 1; i < failures && d < s.cfg.BackoffMax; i++ {
		d *= 2
	}
	if d > s.cfg.BackoffMax {
		d = s.cfg.BackoffMax
	}
	return d
}

// finish ends one call. A scorer that answers unavailable is treated as crashed.
func (s *Supervisor) finish(ctx context.Context, sl *slot, callErr error) {
	s.mu.Lock()
	sl.inflight--
	if s.cur == sl {
		s.lastUsed = s.clk.Now()
		if callErr != nil && contracts.HasCode(callErr, contracts.ErrUnavailable) &&
			!errors.Is(callErr, decision.ErrUnsupportedRuntime) && s.state == stateReady {
			s.failLocked(sl, "call_unavailable")
		}
	}
	s.mu.Unlock()
	s.drain(ctx)
}

// failLocked records the loaded instance sl as dead and schedules a restart. During
// probation a dead new revision returns to the previous one instead.
func (s *Supervisor) failLocked(sl *slot, reason string) {
	s.probeOK = false
	s.lastErr = reason
	if s.prob != nil {
		s.rollbackLocked(reason)
		return
	}
	sl.force = true
	s.retired = append(s.retired, sl)
	s.cur = nil
	s.state = stateFailed
	s.failures++
	s.nextRetry = s.clk.Now().Add(s.backoff(s.failures))
	s.met.event(MetricLoadFailures, reason, sl.rev)
}

// drain closes retired instances whose calls have finished (a dead one at once) and
// releases their reservations, then republishes the memory gauge.
func (s *Supervisor) drain(ctx context.Context) {
	s.mu.Lock()
	var closing []*slot
	keep := s.retired[:0]
	for _, sl := range s.retired {
		if sl.force || sl.inflight == 0 {
			closing = append(closing, sl)
		} else {
			keep = append(keep, sl)
		}
	}
	s.retired = keep
	s.mu.Unlock()
	for _, sl := range closing {
		s.closeInstance(sl.inst)
		sl.res.Release()
	}
	s.publishMemory()
}

func (s *Supervisor) publishMemory() {
	s.mu.Lock()
	var total uint64
	add := func(sl *slot) {
		if sl != nil {
			total += sl.rev.PeakBytes
		}
	}
	add(s.cur)
	if s.prob != nil {
		add(s.prob.prev)
	}
	for _, sl := range s.retired {
		add(sl)
	}
	rev := s.rev
	s.mu.Unlock()
	s.met.memory(rev, total)
}

// Tick does the periodic work and returns what it did (stable strings for logs):
// "restarted" / "restart_failed", "probe_failed", "backoff_reset", "idle_unload",
// "promoted", "rolled_back". It never loads a scorer nobody asked for: an unloaded
// scorer stays unloaded until a request needs it.
func (s *Supervisor) Tick(ctx context.Context) []string {
	var ev []string
	s.drain(ctx)

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	switch s.state {
	case stateFailed:
		if s.clk.Now().Before(s.nextRetry) {
			s.mu.Unlock()
			return nil
		}
		ch, rev, prior := s.beginLoadLocked()
		s.mu.Unlock()
		if err := s.runLoad(ctx, rev, ch, prior); err != nil {
			return append(ev, "restart_failed")
		}
		return append(ev, "restarted")

	case stateReady:
		sl := s.cur
		s.mu.Unlock()
		perr := s.probe(ctx, sl.inst)
		s.mu.Lock()
		if s.cur != sl || s.state != stateReady || (perr != nil && ctx.Err() != nil) {
			s.mu.Unlock()
			return ev
		}
		now := s.clk.Now()
		switch {
		case perr != nil && s.prob != nil:
			s.rollbackLocked("health_probe")
			ev = append(ev, "rolled_back")
		case perr != nil:
			s.failLocked(sl, "health_probe")
			ev = append(ev, "probe_failed")
		default:
			s.probeOK = true
			if s.failures > 0 && now.Sub(sl.loadedAt) >= s.cfg.MinWarm {
				s.failures = 0
				s.nextRetry = time.Time{}
				ev = append(ev, "backoff_reset")
			}
			if s.prob != nil {
				if !now.Before(s.prob.until) {
					s.promoteLocked()
					ev = append(ev, "promoted")
				}
			} else if s.cfg.IdleUnload > 0 && sl.inflight == 0 &&
				!now.Before(sl.loadedAt.Add(s.cfg.MinWarm)) && !now.Before(s.lastUsed.Add(s.cfg.IdleUnload)) {
				s.cur = nil
				s.state = stateUnloaded
				s.probeOK = false
				s.retired = append(s.retired, sl)
				s.met.event(MetricEvictions, "idle", sl.rev)
				ev = append(ev, "idle_unload")
			}
		}
		s.mu.Unlock()
		s.drain(ctx)
		return ev
	}
	s.mu.Unlock()
	return ev
}

// State is the supervisor's report for health and readiness endpoints. It is a
// snapshot: reading it probes nothing.
type State struct {
	// Phase is unloaded, loading, ready or backoff.
	Phase string
	// Live is true while the supervisor runs (false after Stop).
	Live bool
	// Ready is true when a loaded scorer passed its last probe and can serve now.
	Ready bool
	// Loadable is true when a request would try to load the scorer now; false while
	// backing off or stopped. An unloaded scorer is loadable, not ready.
	Loadable      bool
	ModelID       string
	Revision      string
	ResidentBytes uint64
	Failures      int
	RetryAt       time.Time // zero unless backing off
	Probation     bool
	// LastError is the typed code or reason of the last failure, never raw text.
	LastError string
}

// State returns the current snapshot.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{
		Phase: s.state.String(), Live: !s.stopped, ModelID: string(s.rev.Manifest.ModelID),
		Revision: string(s.rev.Manifest.ModelRevision), Failures: s.failures, Probation: s.prob != nil, LastError: s.lastErr,
	}
	st.Ready = !s.stopped && s.state == stateReady && s.probeOK
	st.Loadable = !s.stopped && (s.state == stateUnloaded || s.state == stateBusy ||
		(s.state == stateFailed && !s.clk.Now().Before(s.nextRetry)))
	if s.state == stateFailed {
		st.RetryAt = s.nextRetry
	}
	for _, sl := range []*slot{s.cur, s.prevSlot()} {
		if sl != nil {
			st.ResidentBytes += sl.rev.PeakBytes
		}
	}
	for _, sl := range s.retired {
		st.ResidentBytes += sl.rev.PeakBytes
	}
	return st
}

func (s *Supervisor) prevSlot() *slot {
	if s.prob == nil {
		return nil
	}
	return s.prob.prev
}

// Stop unloads everything and refuses further requests (unavailable). It is
// idempotent.
func (s *Supervisor) Stop(ctx context.Context) {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		if s.cur != nil {
			s.cur.force = true
			s.retired = append(s.retired, s.cur)
			s.cur = nil
		}
		if p := s.prob; p != nil && p.prev != nil {
			p.prev.force = true
			s.retired = append(s.retired, p.prev)
		}
		s.prob = nil
		s.state = stateUnloaded
		s.probeOK = false
	}
	s.mu.Unlock()
	s.drain(ctx)
}
