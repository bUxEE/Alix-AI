package resources

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// probeTimeout bounds one call to a runtime's memory report.
const probeTimeout = 3 * time.Second

// Verdict is the answer of admission control.
type Verdict uint8

const (
	// Admit: the work fits and may start.
	Admit Verdict = iota + 1
	// Hold: the work does not fit now but will once memory or interactive work
	// frees up. A queue (the job queue's lease) keeps it; a caller that cannot
	// wait gets a retryable error.
	Hold
	// Reject: the work cannot be served, now or by waiting. A typed error.
	Reject
)

// String is the verdict's stable name.
func (v Verdict) String() string {
	switch v {
	case Admit:
		return "admit"
	case Hold:
		return "hold"
	case Reject:
		return "reject"
	}
	return "unknown"
}

// Reason says why work was held or rejected. The values are stable strings
// for logs, the ops status and error details.
type Reason string

const (
	ReasonOK                Reason = ""
	ReasonOverBudget        Reason = "over_budget"
	ReasonExceedsBudget     Reason = "exceeds_budget"
	ReasonMemoryPressure    Reason = "memory_pressure"
	ReasonMemoryUnknown     Reason = "memory_unknown"
	ReasonInteractiveActive Reason = "interactive_active"
	ReasonYieldTimeout      Reason = "yield_timeout"
	ReasonInvalid           Reason = "invalid_request"
)

// Request is a piece of work asking to start.
type Request struct {
	// Class is the priority class of section 17.1; it decides which share of
	// the budget the work may use and whether it yields to live turns.
	Class jobs.Class
	// Bytes is the work's estimated peak memory above what the process and the
	// runtime already hold. An upper bound, like every estimate in this
	// codebase.
	Bytes uint64
	// Label names the work in logs ("ingest", "turn"); never user content.
	Label string
}

// Decision is what Check reports about a Request.
type Decision struct {
	Verdict Verdict
	Reason  Reason
	// Used is process RSS + runtime memory + outstanding reservations; Limit is
	// the ceiling that applies to the request's class; Need is the request.
	Used, Limit, Need uint64
}

// Err is nil for Admit, otherwise a typed resource_exhausted (or unavailable
// when memory could not be measured) carrying the reason. A held request's
// error is marked retryable by its code; the details say it is only held.
func (d Decision) Err() error {
	if d.Verdict == Admit {
		return nil
	}
	code := contracts.ErrResourceExhausted
	if d.Reason == ReasonMemoryUnknown {
		code = contracts.ErrUnavailable
	}
	e := contracts.Newf(code, "resource governor: %s (%s)", d.Verdict, d.Reason).
		WithDetails("reason", string(d.Reason)).
		WithDetails("verdict", d.Verdict.String())
	if d.Limit > 0 {
		e = e.WithDetails("need_bytes", d.Need).WithDetails("limit_bytes", d.Limit).WithDetails("used_bytes", d.Used)
	}
	return e
}

// Ticket is a memory reservation held by admitted work. Release it when the
// work ends; releasing twice is harmless. A reservation counts on top of the
// measured RSS until released, so it can double count memory the work has
// already touched: the error is on the safe side.
type Ticket struct {
	g     *Governor
	bytes uint64
	once  sync.Once
}

// Bytes is the reserved size.
func (t *Ticket) Bytes() uint64 {
	if t == nil {
		return 0
	}
	return t.bytes
}

// Release returns the reservation. Safe on a nil Ticket.
func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		t.g.mu.Lock()
		if t.g.reserved >= t.bytes {
			t.g.reserved -= t.bytes
		} else {
			t.g.reserved = 0
		}
		t.g.mu.Unlock()
	})
}

// Options is everything New needs.
type Options struct {
	// Config sizes the governor; zero fields take the defaults. Use
	// FromConfig for the process configuration.
	Config Config
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Logger receives governor events (classes, reasons and counts, never
	// user content). Nil discards.
	Logger *slog.Logger
	// Memory reads this process's RSS. Nil means OSMemoryProbe.
	Memory MemoryProbe
	// Runtime is the provider runtime (see RuntimeFromRegistry); it fills
	// Reporter and Unloader when those are nil. All three may be nil: the
	// governor then counts no provider-reported memory and unloads nothing.
	Runtime  ModelRuntime
	Reporter MemoryReporter
	Unloader Unloader
	// Sleep waits d or until ctx ends. Nil means a real timer; tests inject
	// one that advances their fake clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

type reading struct {
	v  uint64
	at time.Time
	ok bool
}

// Governor is the resource governor: admission control against a memory
// budget, arbitration between live interactive work and background work, and
// the idle-unload policy of models. It is safe for concurrent use.
//
// Nothing the queue calls inside a transaction (Policy) performs I/O: it reads
// the cached readings that Refresh (and Run) maintain.
type Governor struct {
	cfg      Config
	clk      clock.Clock
	log      *slog.Logger
	mem      MemoryProbe
	reporter MemoryReporter
	unloader Unloader
	sleep    func(ctx context.Context, d time.Duration) error
	pinned   map[string]bool

	mu          sync.Mutex
	rss         reading
	runtime     reading
	reserved    uint64
	interactive int
	models      map[string]*modelState
	unloaded    map[string]time.Time // models this governor unloaded and has not seen loaded again
	stats       Stats
	rssWasOK    bool
	rtWasOK     bool
}

// New builds a Governor. It returns the Config's validation error.
func New(opts Options) (*Governor, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	g := &Governor{
		cfg:      opts.Config.WithDefaults(),
		clk:      opts.Clock,
		log:      opts.Logger,
		mem:      opts.Memory,
		reporter: opts.Reporter,
		unloader: opts.Unloader,
		sleep:    opts.Sleep,
		pinned:   map[string]bool{},
		models:   map[string]*modelState{},
		unloaded: map[string]time.Time{},
		rssWasOK: true,
		rtWasOK:  true,
	}
	if opts.Runtime != nil {
		if g.reporter == nil {
			g.reporter = opts.Runtime
		}
		if g.unloader == nil {
			g.unloader = opts.Runtime
		}
	}
	if g.clk == nil {
		g.clk = clock.Real{}
	}
	if g.log == nil {
		g.log = slog.New(slog.DiscardHandler)
	}
	if g.mem == nil {
		g.mem = OSMemoryProbe{}
	}
	if g.sleep == nil {
		g.sleep = realSleep
	}
	for _, id := range g.cfg.PinnedModels {
		g.pinned[id] = true
	}
	return g, nil
}

// Config returns the effective (defaulted) configuration.
func (g *Governor) Config() Config { return g.cfg }

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// background reports whether a class yields to interactive work.
func (g *Governor) background(c jobs.Class) bool { return int(c) >= g.cfg.BackgroundFrom }

// backgroundLimit is the share of the budget background work may use.
func (g *Governor) backgroundLimit() uint64 {
	return g.cfg.MemoryBudgetBytes / 100 * uint64(g.cfg.BackgroundPercent)
}

func (g *Governor) fresh(r reading, now time.Time) bool {
	return r.ok && !now.Before(r.at) && now.Sub(r.at) <= g.cfg.StaleAfter
}

// Refresh reads the process RSS and asks the runtime for its memory and loaded
// models, then stores the readings. It does I/O (the runtime may be reached
// over HTTP) and must not be called from inside a queue transaction. A failed
// probe keeps its last good value, which goes stale after Config.StaleAfter.
func (g *Governor) Refresh(ctx context.Context) {
	g.refreshRSS()
	if g.reporter == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	rm, err := g.reporter.RuntimeMemory(cctx)
	cancel()
	now := g.clk.Now()

	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.runtime = reading{v: rm.Total(), at: now, ok: true}
		g.observeModelsLocked(rm.Models, now)
	} else if g.rtWasOK {
		g.log.Warn("resource governor: runtime memory report failed", "err", err.Error())
	}
	g.rtWasOK = err == nil
}

// refreshRSS re-reads this process's RSS (a cheap local call).
func (g *Governor) refreshRSS() {
	rss, err := g.mem.ProcessRSS()
	now := g.clk.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.rss = reading{v: rss, at: now, ok: true}
	} else if g.rssWasOK {
		g.log.Warn("resource governor: process memory probe failed", "err", err.Error())
	}
	g.rssWasOK = err == nil
}

// ensureFresh re-reads the process RSS when the cached reading is older than
// the probe interval, so a live request decides on a recent reading even when
// Run is slow. It never calls the runtime: a hung runtime must not delay a
// live request, whose view of runtime memory is the last good report (kept
// fresh by Run).
func (g *Governor) ensureFresh() {
	now := g.clk.Now()
	g.mu.Lock()
	stale := !g.rss.ok || now.Sub(g.rss.at) >= g.cfg.ProbeInterval
	g.mu.Unlock()
	if stale {
		g.refreshRSS()
	}
}

// decideLocked is the admission rule: the request is judged against limit.
func (g *Governor) decideLocked(req Request, limit uint64) Decision {
	now := g.clk.Now()
	used := g.rss.v + g.runtime.v + g.reserved
	d := Decision{Verdict: Admit, Used: used, Limit: limit, Need: req.Bytes}
	bg := g.background(req.Class)
	rssKnown := g.fresh(g.rss, now)
	rtKnown := g.reporter == nil || g.fresh(g.runtime, now)
	hold := func(r Reason) Decision { d.Verdict, d.Reason = Hold, r; return d }
	reject := func(r Reason) Decision { d.Verdict, d.Reason = Reject, r; return d }
	switch {
	case !req.Class.Valid():
		return reject(ReasonInvalid)
	case !bg && req.Bytes == 0:
		// Live work that reserves nothing (a commit, an ordinary read) is only
		// arbitrated, never refused for memory: a large resident model must not
		// be able to block commits.
		return d
	case !rssKnown:
		// Our own footprint is unknown: nothing can be judged. Background work
		// waits for the next good reading; a live request fails typed.
		if bg {
			return hold(ReasonMemoryUnknown)
		}
		return reject(ReasonMemoryUnknown)
	case bg && !rtKnown:
		return hold(ReasonMemoryUnknown)
	case bg && g.interactive > 0:
		return hold(ReasonInteractiveActive)
	case req.Bytes > limit:
		return reject(ReasonExceedsBudget)
	case used+req.Bytes > limit:
		if bg {
			return hold(ReasonMemoryPressure)
		}
		return reject(ReasonOverBudget)
	}
	return d
}

func (g *Governor) limitFor(c jobs.Class) uint64 {
	if g.background(c) {
		return g.backgroundLimit()
	}
	return g.cfg.MemoryBudgetBytes
}

// Check evaluates a request without reserving anything. It uses the cached
// readings and never does I/O.
func (g *Governor) Check(req Request) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.decideLocked(req, g.limitFor(req.Class))
}

// Admit evaluates a request against the budget and, when it fits, reserves its
// bytes atomically so concurrent admits cannot all pass on the same reading.
// It never waits and never unloads a model to make room: over-budget work is
// rejected (live work) or held (background work) with a typed error. Release
// the Ticket when the work ends.
func (g *Governor) Admit(_ context.Context, req Request) (*Ticket, error) {
	g.ensureFresh()
	return g.admit(req, g.limitFor(req.Class))
}

func (g *Governor) admit(req Request, limit uint64) (*Ticket, error) {
	g.mu.Lock()
	d := g.decideLocked(req, limit)
	switch d.Verdict {
	case Admit:
		g.reserved += req.Bytes
		g.stats.Admitted++
	case Hold:
		g.stats.Held++
	default:
		g.stats.Rejected++
	}
	g.mu.Unlock()
	if d.Verdict != Admit {
		g.log.Warn("resource governor: work not admitted", "label", req.Label, "class", req.Class.String(),
			"verdict", d.Verdict.String(), "reason", string(d.Reason), "need", d.Need, "used", d.Used, "limit", d.Limit)
		return nil, d.Err()
	}
	return &Ticket{g: g, bytes: req.Bytes}, nil
}

// BeginInteractive registers a live piece of work (an active text or voice
// turn, a commit) and returns its release. While at least one is active, the
// background classes hold: Policy leases them nothing and Admit holds them.
// Classes above Config.InteractiveUpTo register nothing. Call release exactly
// when the work ends; a second call is harmless.
func (g *Governor) BeginInteractive(class jobs.Class) (release func()) {
	if !class.Valid() || int(class) > g.cfg.InteractiveUpTo {
		return func() {}
	}
	g.mu.Lock()
	g.interactive++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.interactive--
			g.mu.Unlock()
		})
	}
}

// InteractiveActive is the number of live interactive pieces of work.
func (g *Governor) InteractiveActive() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.interactive
}

// BackgroundHold reports whether background work should not start now, from
// the cached readings only (no I/O, safe inside a queue transaction): a live
// turn is active, the memory readings are unknown, or background memory is at
// its ceiling.
func (g *Governor) BackgroundHold() (bool, Reason) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Now()
	switch {
	case g.interactive > 0:
		return true, ReasonInteractiveActive
	case !g.fresh(g.rss, now) || (g.reporter != nil && !g.fresh(g.runtime, now)):
		return true, ReasonMemoryUnknown
	case g.rss.v+g.runtime.v+g.reserved >= g.backgroundLimit():
		return true, ReasonMemoryPressure
	}
	return false, ReasonOK
}

// Policy returns a jobs.DequeuePolicy that stops the queue from leasing
// background classes while BackgroundHold says so, and otherwise defers to
// inner (nil means the queue's FIFO). Holding at the lease means no attempt is
// spent and nothing is written: the jobs wait in the durable queue, which is
// the "queue before memory is exhausted" half of master plan 17.2. Interactive
// classes are never held here.
//
// The queue has one Policy slot, so compose: install
// resources.Policy(ingestPause.Policy(nil)) as jobs.Options.Policy.
//
// The policy runs inside the lease transaction and performs no I/O.
func (g *Governor) Policy(inner jobs.DequeuePolicy) jobs.DequeuePolicy {
	return jobs.DequeuePolicyFunc(func(ready []jobs.Candidate) int {
		hold := false
		for _, c := range ready {
			if g.background(c.Class) {
				hold, _ = g.BackgroundHold()
				break
			}
		}
		if !hold {
			if inner != nil {
				return inner.Pick(ready)
			}
			if len(ready) == 0 {
				return -1
			}
			return 0
		}
		// Held: only the interactive-class candidates remain. Look before
		// allocating, so the common case (a class of background jobs only)
		// costs nothing.
		var keep []jobs.Candidate
		var at []int
		for i, c := range ready {
			if !g.background(c.Class) {
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

// Yield is the cooperative half of arbitration, for a running background
// handler: call it between units of work. It returns at once for an
// interactive class or when no live work is active, otherwise it waits for the
// live work to end, at most Config.YieldMaxWait (and never more than
// YieldMaxWait/YieldPoll+1 polls, whatever the clock does). When the wait
// runs out it returns a retryable resource_exhausted: the handler should
// return it and let the queue retry later rather than run over a live turn.
func (g *Governor) Yield(ctx context.Context, class jobs.Class) error {
	if !g.background(class) {
		return nil
	}
	polls := int(g.cfg.YieldMaxWait/g.cfg.YieldPoll) + 1
	deadline := g.clk.Now().Add(g.cfg.YieldMaxWait)
	for i := 0; i < polls; i++ {
		if g.InteractiveActive() == 0 {
			return nil
		}
		if !g.clk.Now().Before(deadline) {
			break
		}
		if err := g.sleep(ctx, g.cfg.YieldPoll); err != nil {
			return err
		}
	}
	if g.InteractiveActive() == 0 {
		return nil
	}
	g.mu.Lock()
	g.stats.YieldTimeouts++
	g.mu.Unlock()
	return Decision{Verdict: Hold, Reason: ReasonYieldTimeout}.Err()
}

// Stats is the observable state of the governor, for the ops status and the
// soak harness (evictions and reloads).
type Stats struct {
	// Counters since start.
	Admitted, Held, Rejected uint64
	YieldTimeouts            uint64
	Unloads, UnloadErrors    uint64
	// Reloads counts models seen loaded again after this governor unloaded
	// them: each is an eviction that cost a cold load.
	Reloads uint64

	// Gauges.
	InteractiveActive int
	RSSBytes          uint64
	RuntimeBytes      uint64
	ReservedBytes     uint64
	BudgetBytes       uint64
	// BackgroundHeld is true, with its reason, while Policy holds background.
	BackgroundHeld bool
	HoldReason     Reason
	At             time.Time
}

// Stats returns the counters and gauges now.
func (g *Governor) Stats() Stats {
	held, why := g.BackgroundHold()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stats
	st.InteractiveActive = g.interactive
	st.RSSBytes, st.RuntimeBytes, st.ReservedBytes = g.rss.v, g.runtime.v, g.reserved
	st.BudgetBytes = g.cfg.MemoryBudgetBytes
	st.BackgroundHeld, st.HoldReason = held, why
	st.At = g.clk.Now()
	return st
}

// Run refreshes the readings and sweeps idle models every
// Config.ProbeInterval until ctx ends. It does one tick before the first wait,
// so the queue's background hold (which treats unknown memory as pressure)
// lifts at start. Each tick unloads at most Config.MaxUnloadsPerSweep models.
func (g *Governor) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := g.Tick(ctx); err != nil && ctx.Err() == nil {
			g.log.Warn("resource governor: sweep failed", "err", err.Error())
		}
		if err := g.sleep(ctx, g.cfg.ProbeInterval); err != nil {
			return
		}
	}
}

// Tick is one Run iteration: Refresh, then Sweep. It returns the models
// unloaded.
func (g *Governor) Tick(ctx context.Context) ([]string, error) {
	g.Refresh(ctx)
	return g.Sweep(ctx)
}
