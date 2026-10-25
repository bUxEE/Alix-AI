package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/supervisor"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// ---- fakes -----------------------------------------------------------------------------------------------------

// supervisorTestLog is the order in which things happened: runtime starts and supervisor events.
type supervisorTestLog struct {
	mu sync.Mutex
	v  []string
}

func (l *supervisorTestLog) add(s string) {
	l.mu.Lock()
	l.v = append(l.v, s)
	l.mu.Unlock()
}

func (l *supervisorTestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.v...)
}

func (l *supervisorTestLog) index(s string) int {
	for i, x := range l.all() {
		if x == s {
			return i
		}
	}
	return -1
}

// supervisorTestInstance is one run of the fake runtime; closing done is its crash.
type supervisorTestInstance struct{ done chan struct{} }

func (supervisorTestInstance) PID() int                   { return 0 }
func (i supervisorTestInstance) Done() <-chan struct{}    { return i.done }
func (supervisorTestInstance) Stop(context.Context) error { return nil }
func (i supervisorTestInstance) crash()                   { close(i.done) }

// supervisorTestRuntime hands out scripted instances and records every start.
type supervisorTestRuntime struct {
	log *supervisorTestLog

	mu        sync.Mutex
	starts    int
	instances []supervisorTestInstance
	failStart bool
}

func (r *supervisorTestRuntime) Start(context.Context) (supervisor.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	if r.log != nil {
		r.log.add("start")
	}
	if r.failStart {
		return nil, contracts.New(contracts.ErrUnavailable, "scripted start failure")
	}
	inst := supervisorTestInstance{done: make(chan struct{})}
	r.instances = append(r.instances, inst)
	return inst, nil
}

func (r *supervisorTestRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func (r *supervisorTestRuntime) instance(n int) supervisorTestInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.instances[n]
}

// supervisorTestProbe is the adapter's Health as the supervisor sees it.
type supervisorTestProbe struct {
	mu     sync.Mutex
	status providers.Health
}

func (*supervisorTestProbe) Name() string { return "localai" }

func (p *supervisorTestProbe) Health(context.Context) (providers.HealthReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return providers.HealthReport{Status: p.status, Version: "v-test"}, nil
}

func (p *supervisorTestProbe) set(s providers.Health) {
	p.mu.Lock()
	p.status = s
	p.mu.Unlock()
}

// supervisorTestModel counts the calls that reached the adapter.
type supervisorTestModel struct {
	mu    sync.Mutex
	calls int
}

func (m *supervisorTestModel) hit() {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
}

func (m *supervisorTestModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *supervisorTestModel) Chat(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatResult, error) {
	m.hit()
	return providers.ChatResult{}, nil
}

func (m *supervisorTestModel) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	m.hit()
	return nil, nil
}

func (m *supervisorTestModel) Embed(context.Context, contracts.Scope, providers.EmbedRequest) (providers.EmbedResult, error) {
	m.hit()
	return providers.EmbedResult{}, nil
}

// supervisorTestAfter keeps the supervisor's short waits to a millisecond, and never fires the start timeout (the
// sentinel is a duration above half an hour) nor anything while hold is set, so a test can freeze a state.
type supervisorTestAfter struct {
	mu   sync.Mutex
	hold bool
}

func (a *supervisorTestAfter) freeze() {
	a.mu.Lock()
	a.hold = true
	a.mu.Unlock()
}

func (a *supervisorTestAfter) after(d time.Duration) <-chan time.Time {
	a.mu.Lock()
	hold := a.hold
	a.mu.Unlock()
	if hold || d >= 30*time.Minute {
		return nil
	}
	return time.After(time.Millisecond)
}

const supervisorTestStartTimeout = 59 * time.Minute

// supervisorTestRig is a wrapped runtime over scripted fakes.
type supervisorTestRig struct {
	rt    *supervisorRuntime
	run   *supervisorTestRuntime
	probe *supervisorTestProbe
	model *supervisorTestModel
	after *supervisorTestAfter
	log   *supervisorTestLog
	stop  func()
}

func supervisorTestNewRig(t *testing.T, maxRestarts int, extra ...supervisor.Option) *supervisorTestRig {
	t.Helper()
	g := &supervisorTestRig{
		log: &supervisorTestLog{}, probe: &supervisorTestProbe{status: providers.HealthOK},
		model: &supervisorTestModel{}, after: &supervisorTestAfter{},
	}
	g.run = &supervisorTestRuntime{log: g.log}
	opts := append([]supervisor.Option{
		supervisor.WithAfter(g.after.after),
		supervisor.WithEvents(func(e supervisor.Event) { g.log.add(string(e.Type)) }),
	}, extra...)
	rt, err := supervisorNewWith(supervisor.Config{MaxRestarts: maxRestarts, StartTimeout: supervisorTestStartTimeout},
		g.run, g.probe, g.model, g.model, discardLog(), clock.Real{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	g.rt = rt
	return g
}

// start runs the supervisor loop and stops it, and waits for it, when the test ends.
func (g *supervisorTestRig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.rt.sup.Run(ctx)
	}()
	var once sync.Once
	g.stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(g.stop)
}

func (g *supervisorTestRig) waitState(t *testing.T, want supervisor.State) {
	t.Helper()
	pollUntil(t, 10*time.Second, "the supervisor to be "+string(want), func() bool { return g.rt.sup.Status().State == want })
}

func supervisorTestScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws", PrincipalID: "usr",
		Roles: []string{"member"}, CapabilityScopes: []string{"demo.create"}, SessionID: "ses",
	}
}

// supervisorTestSection replaces the supervisor section for one test.
func supervisorTestSection(t *testing.T, c supervisorConfig) {
	t.Helper()
	old := supervisorSection
	supervisorSection = c
	t.Cleanup(func() { supervisorSection = old })
}

// ---- TestServeWrapsTheRuntimeWithTheSupervisor ------------------------------------------------------------------

// serve registers the LocalAI adapter under the supervisor when supervisor.enabled is set, and the bare adapter
// otherwise: the wrapper keeps the adapter's roles, takes Health and Name from the supervisor, and the supervisor
// loop is a service of serve.
func TestServeWrapsTheRuntimeWithTheSupervisor(t *testing.T) {
	t.Run("off keeps the bare adapter", func(t *testing.T) {
		f := newFixture(t)
		supervisorTestSection(t, supervisorConfig{Runtime: supervisorRuntimeUnit})
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, f.localai(), f.inference())
		if off != nil {
			t.Fatalf("inference off: %s: %v", off.reason, off.err)
		}
		p, _ := reg.Provider(providers.RoleController)
		if _, ok := p.(*localai.Client); !ok || supervisorRuntimeOf(reg) != nil {
			t.Fatalf("the controller role holds %T, want the bare *localai.Client", p)
		}
		src := &adapterSourceProbe{}
		if got := supervisorAdapters(reg, src); got != actions.AdapterSource(src) {
			t.Error("with the supervisor off the adapter source must be returned as it is")
		}
	})

	t.Run("exec is refused in serve", func(t *testing.T) {
		f := newFixture(t)
		supervisorTestSection(t, supervisorConfig{Enabled: true, Runtime: supervisorRuntimeExec})
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, f.localai(), f.inference())
		if off == nil || reg.Has(providers.RoleController) {
			t.Fatal("serve accepted supervisor.runtime exec: LocalAI would be spawned from the AlixAI service")
		}
	})

	t.Run("on wraps the adapter and serve runs the loop", func(t *testing.T) {
		f := newFixture(t)
		supervisorTestSection(t, supervisorConfig{Enabled: true, Runtime: supervisorRuntimeUnit})
		reg, off := startInference(context.Background(), discardLog(), clock.Real{}, f.localai(), f.inference())
		if off != nil {
			t.Fatalf("inference off: %s: %v", off.reason, off.err)
		}
		rt := supervisorRuntimeOf(reg)
		if rt == nil {
			p, _ := reg.Provider(providers.RoleController)
			t.Fatalf("the controller role holds %T, want the supervisor's wrapper", p)
		}
		var _ providers.TextGenerator = rt
		var _ providers.Embedder = rt
		if rt.Name() != localai.ProviderName {
			t.Errorf("Name = %q, want the adapter's %q", rt.Name(), localai.ProviderName)
		}
		// The supervisor is not running yet: the role is registered and unavailable, with the supervisor's reason.
		if reg.Usable(providers.RoleController) {
			t.Error("the controller is usable before the supervisor has seen the runtime healthy")
		}
		if rep, _ := rt.Health(context.Background()); rep.Status != providers.HealthUnavailable || rep.Reason == "" {
			t.Errorf("Health = %+v, want the supervisor's unavailable report", rep)
		}

		// serve's provider: the readiness check and the loop, as services of the process.
		var checks []gateway.Check
		var starts []func(context.Context) func()
		d := serveDeps{
			Log: discardLog(), Providers: reg,
			AddCheck:   func(c gateway.Check) { checks = append(checks, c) },
			AddService: func(s func(context.Context) func()) { starts = append(starts, s) },
		}
		if err := supervisorProvide(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		if len(checks) != 1 || checks[0].Name != supervisorCheckName || len(starts) != 1 {
			t.Fatalf("provider added %d checks and %d services, want the supervisor's one each", len(checks), len(starts))
		}
		if checks[0].Fn(context.Background()) == nil {
			t.Error("the readiness check passes while the supervisor has not started")
		}
		stop := starts[0](context.Background())
		stopped := false
		defer func() {
			if !stopped {
				stop()
			}
		}()
		pollUntil(t, 10*time.Second, "the supervisor to reach running", func() bool { return rt.sup.Status().State == supervisor.StateRunning })
		reg.Refresh(context.Background())
		if !reg.Usable(providers.RoleController) {
			t.Fatal("the controller is not usable with the runtime running")
		}
		if rep, _ := rt.Health(context.Background()); rep.Status != providers.HealthOK || rep.Version == "" {
			t.Errorf("Health = %+v, want the adapter's own report while running", rep)
		}
		if err := checks[0].Fn(context.Background()); err != nil {
			t.Errorf("readiness check: %v", err)
		}
		// The adapter's text role still works through the wrapper.
		gen, err := reg.TextGenerator(providers.RoleController)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gen.Chat(context.Background(), supervisorTestScope(), providers.ChatRequest{
			Role: providers.RoleController, Messages: []providers.Message{{Role: providers.MsgUser, Content: "hi"}}, MaxTokens: 8,
		}); err != nil {
			t.Fatalf("Chat through the wrapper: %v", err)
		}
		if got := f.rt.chatModels(); len(got) != 1 || got[0] != serveModelID {
			t.Errorf("the runtime saw chat for %v, want one call for %q", got, serveModelID)
		}
		stop()
		stopped = true
		if err := rt.sup.Admit(); err == nil {
			t.Error("Admit passes after the supervisor stopped")
		}
	})

	t.Run("serve runs it end to end", func(t *testing.T) {
		f := newFixture(t)
		t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
		supervisorTestSection(t, supervisorConfig{Runtime: supervisorRuntimeUnit}) // restored after the config load below
		cfg, _ := serveConfig(t, f, "supervisor:\n  enabled: true\n")
		srv := startServe(t, cfg)
		// /readyz turns ready only when the supervisor loop has run and seen the runtime healthy: the role is
		// unavailable until then.
		var code int
		var body []byte
		pollUntil(t, 20*time.Second, "/readyz to turn ready", func() bool {
			code, _, body = srv.doOnce(t, http.MethodGet, "/readyz", "")
			return code == http.StatusOK
		})
		if !strings.Contains(string(body), supervisorCheckName) {
			t.Errorf("/readyz does not list the %s check: %s", supervisorCheckName, body)
		}
		if err := srv.stop(); err != nil {
			t.Errorf("serve stopped with %v", err)
		}
	})
}

// adapterSourceProbe is an actions.AdapterSource that has no adapters.
type adapterSourceProbe struct{}

func (*adapterSourceProbe) Adapter(context.Context, string) (adapters.AppAdapter, error) {
	return nil, errors.New("none")
}

// ---- TestModelPathFailsClosedWhileTheRuntimeIsDown --------------------------------------------------------------

// Starting, backing off, gave up and not running all refuse model work before the adapter is reached; only a running
// runtime admits it. A commit is not model work and is never refused (TestInFlightActionIsSettledBeforeARestart).
func TestModelPathFailsClosedWhileTheRuntimeIsDown(t *testing.T) {
	scope := supervisorTestScope()
	chat := providers.ChatRequest{Role: providers.RoleController, Messages: []providers.Message{{Role: providers.MsgUser, Content: "hi"}}}

	refused := func(t *testing.T, g *supervisorTestRig, state string) {
		t.Helper()
		_, e1 := g.rt.Chat(context.Background(), scope, chat)
		_, e2 := g.rt.ChatStream(context.Background(), scope, chat)
		_, e3 := g.rt.Embed(context.Background(), scope, providers.EmbedRequest{Inputs: []string{"x"}})
		for i, err := range []error{e1, e2, e3} {
			if !contracts.HasCode(err, contracts.ErrUnavailable) {
				t.Errorf("%s: call %d = %v, want an unavailable refusal", state, i, err)
			}
		}
		if n := g.model.count(); n != 0 {
			t.Errorf("%s: %d calls reached the adapter, want none", state, n)
		}
		rep, err := g.rt.Health(context.Background())
		if err != nil || rep.Status != providers.HealthUnavailable || rep.Reason == "" {
			t.Errorf("%s: Health = %+v, %v, want unavailable with a reason", state, rep, err)
		}
	}

	t.Run("not running", func(t *testing.T) {
		refused(t, supervisorTestNewRig(t, 0), "stopped")
	})

	t.Run("starting", func(t *testing.T) {
		g := supervisorTestNewRig(t, 0)
		g.probe.set(providers.HealthUnavailable) // never becomes healthy; the start timeout never fires
		g.start(t)
		g.waitState(t, supervisor.StateStarting)
		refused(t, g, "starting")
	})

	t.Run("backing off", func(t *testing.T) {
		g := supervisorTestNewRig(t, 0)
		g.start(t)
		g.waitState(t, supervisor.StateRunning)
		g.after.freeze() // the backoff wait never ends
		g.run.instance(0).crash()
		g.waitState(t, supervisor.StateBackoff)
		refused(t, g, "backoff")
	})

	t.Run("gave up", func(t *testing.T) {
		g := supervisorTestNewRig(t, 1)
		g.run.failStart = true
		g.start(t)
		g.waitState(t, supervisor.StateGaveUp)
		refused(t, g, "gave up")
	})

	t.Run("running admits every call", func(t *testing.T) {
		g := supervisorTestNewRig(t, 0)
		g.start(t)
		g.waitState(t, supervisor.StateRunning)
		if _, err := g.rt.Chat(context.Background(), scope, chat); err != nil {
			t.Errorf("Chat: %v", err)
		}
		if _, err := g.rt.ChatStream(context.Background(), scope, chat); err != nil {
			t.Errorf("ChatStream: %v", err)
		}
		if _, err := g.rt.Embed(context.Background(), scope, providers.EmbedRequest{Inputs: []string{"x"}}); err != nil {
			t.Errorf("Embed: %v", err)
		}
		if n := g.model.count(); n != 3 {
			t.Errorf("%d calls reached the adapter, want 3", n)
		}
	})
}

// ---- TestInFlightActionIsSettledBeforeARestart ------------------------------------------------------------------

// supervisorTestReceipts is the action store: a receipt per preparation, or an error.
type supervisorTestReceipts func(preparationID string) (contracts.ActionReceipt, error)

func (f supervisorTestReceipts) Receipt(_ context.Context, _ contracts.Scope, id string) (contracts.ActionReceipt, error) {
	return f(id)
}

// supervisorTestAdapter is an application adapter whose Commit blocks until released.
type supervisorTestAdapter struct {
	adapters.AppAdapter
	entered chan struct{}
	release chan struct{}
}

func (a supervisorTestAdapter) Commit(context.Context, contracts.Scope, adapters.CommitRequest) (act.DeterministicResult, error) {
	close(a.entered)
	<-a.release
	return act.DeterministicResult{}, nil
}

type supervisorTestSource struct{ ad adapters.AppAdapter }

func (s supervisorTestSource) Adapter(context.Context, string) (adapters.AppAdapter, error) {
	return s.ad, nil
}

// The master plan's rule for a LocalAI crash: an action in flight when the runtime dies is settled before any
// restart, uncommitted unless the coordinator's own receipt for that preparation says committed, queued or partial.
func TestInFlightActionIsSettledBeforeARestart(t *testing.T) {
	const prep = "prep_inflight1"
	receipt := func(id string, status act.ActionReceiptStatus) (contracts.ActionReceipt, error) {
		return contracts.ActionReceipt{ReceiptID: "rcpt_1", PreparationID: act.PreparationID(id), Status: status}, nil
	}
	cases := []struct {
		name          string
		store         supervisorTestReceipts
		wantCommitted bool
		wantReason    string
		wantReconcile bool
	}{
		{"committed receipt proves it", func(id string) (contracts.ActionReceipt, error) { return receipt(id, act.ActionReceiptStatusCommitted) },
			true, supervisor.ReasonReceiptProvesCommit, false},
		{"queued receipt proves it", func(id string) (contracts.ActionReceipt, error) { return receipt(id, act.ActionReceiptStatusQueued) },
			true, supervisor.ReasonReceiptProvesCommit, false},
		{"partial receipt proves it", func(id string) (contracts.ActionReceipt, error) { return receipt(id, act.ActionReceiptStatusPartial) },
			true, supervisor.ReasonReceiptProvesCommit, false},
		{"no receipt yet stays uncommitted", func(string) (contracts.ActionReceipt, error) {
			return contracts.ActionReceipt{}, contracts.New(contracts.ErrConflict, "the preparation has no receipt yet")
		}, false, supervisor.ReasonNoReceipt, true},
		{"uncertain receipt stays uncommitted", func(id string) (contracts.ActionReceipt, error) { return receipt(id, act.ActionReceiptStatusUncertain) },
			false, supervisor.ReasonReceiptUncertain, true},
		{"rejected receipt proves nothing was applied", func(id string) (contracts.ActionReceipt, error) { return receipt(id, act.ActionReceiptStatusRejected) },
			false, supervisor.ReasonReceiptRejected, false},
		{"another preparation's receipt proves nothing", func(string) (contracts.ActionReceipt, error) {
			return receipt("prep_other", act.ActionReceiptStatusCommitted)
		},
			false, supervisor.ReasonReceiptMismatch, true},
		{"a failed lookup stays uncommitted", func(string) (contracts.ActionReceipt, error) {
			return contracts.ActionReceipt{}, contracts.New(contracts.ErrUnavailable, "store busy")
		}, false, supervisor.ReasonLookupFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := supervisorTestNewRig(t, 5)
			g.rt.tracker.bind(tc.store)
			reg := providers.NewRegistry(clock.Real{})
			if err := reg.Register(providers.RoleController, g.rt); err != nil {
				t.Fatal(err)
			}
			ad := supervisorTestAdapter{entered: make(chan struct{}), release: make(chan struct{})}
			wrapped, err := supervisorAdapters(reg, supervisorTestSource{ad}).Adapter(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			g.start(t)
			g.waitState(t, supervisor.StateRunning)

			// A commit is in flight against the runtime, and it is never refused for the runtime's sake.
			committed := make(chan error, 1)
			go func() {
				_, err := wrapped.Commit(context.Background(), supervisorTestScope(), adapters.CommitRequest{
					Prepared: act.PreparedAction{PreparationID: prep},
				})
				committed <- err
			}()
			<-ad.entered
			if n := g.rt.sup.Ledger().Len(); n != 1 {
				t.Fatalf("the ledger holds %d actions, want the one in flight", n)
			}

			g.run.instance(0).crash() // LocalAI dies with the commit in flight
			pollUntil(t, 10*time.Second, "the supervisor to restart the runtime", func() bool { return g.run.startCount() >= 2 })

			settled, second := g.log.index("settled"), indexOfNth(g.log.all(), "start", 2)
			if settled < 0 || second < 0 || settled > second {
				t.Fatalf("order = %v: the in-flight action must be settled before the second start", g.log.all())
			}
			got := g.rt.sup.Status().Settlements
			if len(got) != 1 {
				t.Fatalf("settlements = %+v, want exactly the one in-flight action", got)
			}
			s := got[0]
			if s.PreparationID != prep || s.Committed != tc.wantCommitted || s.Reason != tc.wantReason || s.NeedsReconcile != tc.wantReconcile {
				t.Errorf("settlement = %+v, want committed=%v reason=%s reconcile=%v for %s",
					s, tc.wantCommitted, tc.wantReason, tc.wantReconcile, prep)
			}
			if s.Committed && s.ReceiptID == "" {
				t.Error("a committed settlement names no receipt")
			}

			// The commit itself goes on to its own end; the ledger and the scope book are left empty.
			close(ad.release)
			if err := <-committed; err != nil {
				t.Errorf("Commit: %v", err)
			}
			if n := g.rt.sup.Ledger().Len(); n != 0 {
				t.Errorf("the ledger holds %d actions after the commit returned", n)
			}
			g.rt.tracker.mu.Lock()
			left := len(g.rt.tracker.scopes)
			g.rt.tracker.mu.Unlock()
			if left != 0 {
				t.Errorf("%d scopes are still kept after the commit returned", left)
			}
		})
	}

	t.Run("a commit that ended normally is not settled at a crash", func(t *testing.T) {
		g := supervisorTestNewRig(t, 5)
		g.rt.tracker.bind(supervisorTestReceipts(func(id string) (contracts.ActionReceipt, error) { return receipt(id, act.ActionReceiptStatusCommitted) }))
		reg := providers.NewRegistry(clock.Real{})
		if err := reg.Register(providers.RoleController, g.rt); err != nil {
			t.Fatal(err)
		}
		ad := supervisorTestAdapter{entered: make(chan struct{}), release: make(chan struct{})}
		close(ad.release)
		wrapped, _ := supervisorAdapters(reg, supervisorTestSource{ad}).Adapter(context.Background(), "app")
		g.start(t)
		g.waitState(t, supervisor.StateRunning)
		if _, err := wrapped.Commit(context.Background(), supervisorTestScope(), adapters.CommitRequest{Prepared: act.PreparedAction{PreparationID: prep}}); err != nil {
			t.Fatal(err)
		}
		g.run.instance(0).crash()
		pollUntil(t, 10*time.Second, "the restart", func() bool { return g.run.startCount() >= 2 })
		if g.log.index("settled") >= 0 {
			t.Errorf("order = %v: nothing was in flight, so nothing is settled", g.log.all())
		}
	})

	t.Run("a commit is never refused while the runtime is down", func(t *testing.T) {
		g := supervisorTestNewRig(t, 5) // never started: the supervisor is stopped
		reg := providers.NewRegistry(clock.Real{})
		if err := reg.Register(providers.RoleController, g.rt); err != nil {
			t.Fatal(err)
		}
		ad := supervisorTestAdapter{entered: make(chan struct{}), release: make(chan struct{})}
		close(ad.release)
		wrapped, _ := supervisorAdapters(reg, supervisorTestSource{ad}).Adapter(context.Background(), "app")
		if _, err := wrapped.Commit(context.Background(), supervisorTestScope(), adapters.CommitRequest{Prepared: act.PreparedAction{PreparationID: prep}}); err != nil {
			t.Errorf("Commit with the runtime down: %v", err)
		}
	})
}

// indexOfNth is the index of the n-th (1-based) occurrence of s, or -1.
func indexOfNth(v []string, s string, n int) int {
	for i, x := range v {
		if x == s {
			if n--; n == 0 {
				return i
			}
		}
	}
	return -1
}

// ---- the section and the exec command ---------------------------------------------------------------------------

func TestSupervisorConfigValidate(t *testing.T) {
	ok := supervisorConfig{Runtime: supervisorRuntimeUnit}
	if err := ok.Validate(); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
	abs := filepath.Join(t.TempDir(), "local-ai")
	cases := map[string]supervisorConfig{
		"unknown runtime":       {Runtime: "shell"},
		"empty runtime":         {},
		"restarts out of range": {Runtime: supervisorRuntimeUnit, Config: supervisor.Config{MaxRestarts: 1000}},
		"relative exec path":    {Enabled: true, Runtime: supervisorRuntimeExec, Exec: supervisorExecConfig{Path: "local-ai"}},
		"bad env entry":         {Runtime: supervisorRuntimeUnit, Exec: supervisorExecConfig{Path: abs, Env: []string{"NOEQUALS"}}},
		"key var set twice":     {Runtime: supervisorRuntimeUnit, Exec: supervisorExecConfig{Path: abs, APIKeyEnv: "K", Env: []string{"K=1"}}},
		"bad key var name":      {Runtime: supervisorRuntimeUnit, Exec: supervisorExecConfig{Path: abs, APIKeyEnv: "A B"}},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := supervisorConfig{Enabled: true, Runtime: supervisorRuntimeExec, Exec: supervisorExecConfig{Path: abs, APIKeyEnv: "LOCALAI_API_KEY"}}
	if err := good.Validate(); err != nil {
		t.Errorf("a valid exec section is refused: %v", err)
	}
}

func TestSupervisorSectionIsRegistered(t *testing.T) {
	cfg := writeConfig(t, "supervisor:\n  enabled: true\n  max_restarts: 3\n  backoff:\n    initial: 2s\n")
	env := serveEnv(t, os.Stdout, os.Stderr)
	t.Cleanup(func() { supervisorSection = supervisorConfig{Runtime: supervisorRuntimeUnit} })
	if err := env.Config.LoadFrom(cfg, env.LookupEnv); err != nil {
		t.Fatal(err)
	}
	if s := supervisorSection; !s.Enabled || s.Runtime != supervisorRuntimeUnit || s.MaxRestarts != 3 || s.Backoff.Initial != 2*time.Second {
		t.Errorf("section = %+v", s)
	}
}

// The exec runtime's environment is the configured entries and the LocalAI key under the configured name, nothing
// else; the key never appears in an error.
func TestSupervisorExecConfigFrom(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "localai-api-key")
	if err := os.WriteFile(keyFile, []byte("TESTONLY-key-value\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := supervisorExecConfig{Path: filepath.Join(dir, "local-ai"), Args: []string{"run"}, Env: []string{"LOCALAI_ADDRESS=127.0.0.1:8080"}, APIKeyEnv: "LOCALAI_API_KEY", MaxOpenFiles: 4096}
	cfg, err := supervisorExecConfigFrom(e, "file:"+keyFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"LOCALAI_ADDRESS=127.0.0.1:8080", "LOCALAI_API_KEY=TESTONLY-key-value"}
	if strings.Join(cfg.Env, "|") != strings.Join(want, "|") || cfg.Path != e.Path || cfg.Limits.MaxOpenFiles != 4096 || cfg.Stdout != nil || cfg.Stderr != nil {
		t.Errorf("config = %+v", cfg)
	}
	if _, err := supervisorExecConfigFrom(e, "env:LOCALAI_KEY"); err == nil {
		t.Error("an env: reference was accepted as the key source")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisorExecConfigFrom(e, "file:"+empty); err == nil {
		t.Error("an empty key file was accepted")
	}
	// No key variable named: nothing is passed, and no reference is needed.
	e.APIKeyEnv = ""
	if cfg, err := supervisorExecConfigFrom(e, ""); err != nil || len(cfg.Env) != 1 {
		t.Errorf("without api_key_env: %+v, %v", cfg, err)
	}
}

// `supervisor exec` is the LocalAI unit's command: it refuses a configuration that is not an exec one, so it cannot be
// started from the attach configuration of the AlixAI service.
func TestSupervisorExecRefusesAnAttachConfiguration(t *testing.T) {
	for name, c := range map[string]supervisorConfig{
		"disabled":      {Runtime: supervisorRuntimeExec},
		"unit runtime":  {Enabled: true, Runtime: supervisorRuntimeUnit},
		"default value": supervisorSection,
	} {
		err := supervisorExecRun(context.Background(), discardLog(), clock.Real{}, c, localai.Config{})
		if err == nil {
			t.Errorf("%s: supervisor exec ran", name)
		}
	}
}
