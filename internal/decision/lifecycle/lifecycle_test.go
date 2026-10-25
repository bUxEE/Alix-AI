package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// ---- doubles ---------------------------------------------------------------

type fakeInst struct {
	*fakescorer.Scorer
	mu        sync.Mutex
	healthErr error
	closed    int
}

func (f *fakeInst) Health(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthErr
}

func (f *fakeInst) setHealth(err error) {
	f.mu.Lock()
	f.healthErr = err
	f.mu.Unlock()
}

func (f *fakeInst) Close(context.Context) error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeInst) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type fakeLoader struct {
	mu        sync.Mutex
	clk       *clock.Fake
	loadErrs  []error          // consumed in order, one per Load call
	healthErr map[string]error // initial health error per model revision
	calls     map[string]int   // Load calls per model revision
	insts     map[string][]*fakeInst
	total     int
}

func newLoader(clk *clock.Fake) *fakeLoader {
	return &fakeLoader{clk: clk, healthErr: map[string]error{}, calls: map[string]int{}, insts: map[string][]*fakeInst{}}
}

func (l *fakeLoader) Load(_ context.Context, rev Revision) (Instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	key := string(rev.Manifest.ModelRevision)
	l.calls[key]++
	if len(l.loadErrs) > 0 {
		err := l.loadErrs[0]
		l.loadErrs = l.loadErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	in := &fakeInst{Scorer: fakescorer.New(rev.Manifest, l.clk), healthErr: l.healthErr[key]}
	l.insts[key] = append(l.insts[key], in)
	return in, nil
}

func (l *fakeLoader) last(rev string) *fakeInst {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.insts[rev]
	if len(s) == 0 {
		return nil
	}
	return s[len(s)-1]
}

func (l *fakeLoader) loads(rev string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[rev]
}

// fakeGate is a fake probe of the resource governor with a byte budget. It also owns
// the "protected" workers (controller, retrieval, speech) the real governor protects:
// they are resident, and Evict is a trap that nothing in ResourceGate can reach.
type fakeGate struct {
	mu         sync.Mutex
	budget     uint64
	held       uint64
	admits     int
	rejects    int
	labels     []string
	protected  map[string]bool
	evictCalls int
}

func newGate(budget uint64) *fakeGate {
	return &fakeGate{budget: budget, protected: map[string]bool{"controller": true, "retrieval": true, "speech": true}}
}

type fakeRes struct {
	g     *fakeGate
	bytes uint64
	once  sync.Once
}

func (r *fakeRes) Release() {
	r.once.Do(func() {
		r.g.mu.Lock()
		r.g.held -= r.bytes
		r.g.mu.Unlock()
	})
}

func (g *fakeGate) Admit(_ context.Context, bytes uint64, label string) (Reservation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labels = append(g.labels, label)
	if bytes > g.budget-g.held {
		g.rejects++
		return nil, contracts.New(contracts.ErrResourceExhausted, "fake governor: over budget")
	}
	g.held += bytes
	g.admits++
	return &fakeRes{g: g, bytes: bytes}, nil
}

// Evict is never part of ResourceGate; it exists only to be a trap.
func (g *fakeGate) Evict(name string) {
	g.mu.Lock()
	g.evictCalls++
	g.protected[name] = false
	g.mu.Unlock()
}

func (g *fakeGate) heldBytes() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

type memSource map[string][]byte

func (m memSource) Open(_ context.Context, name string) (io.ReadCloser, error) {
	b, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("no such artifact %q", name)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func sum(b []byte) gen.SHA256 {
	h := sha256.Sum256(b)
	return gen.SHA256(hex.EncodeToString(h[:]))
}

func revision(model, rev string, peak uint64, content map[string][]byte) Revision {
	m := fakescorer.DefaultManifest()
	m.ProviderID = common.Identifier("prov-" + model)
	m.ModelID = common.Identifier(model)
	m.ModelRevision = actions.OpaqueVersion(rev)
	m.Files = nil
	for name, b := range content {
		m.Files = append(m.Files, gen.ScorerManifestFileHash{Name: name, SHA256: sum(b), SizeBytes: int64(len(b))})
	}
	return Revision{Manifest: m, PeakBytes: peak}
}

func testConfig() Config {
	return Config{
		MinWarm: 5 * time.Minute, IdleUnload: time.Minute,
		BackoffInitial: time.Second, BackoffMax: 8 * time.Second,
		LoadTimeout: time.Minute, ProbeTimeout: time.Second, Profile: "lite",
	}
}

type rig struct {
	t    *testing.T
	clk  *clock.Fake
	ld   *fakeLoader
	gate *fakeGate
	reg  *MemoryRegistry
	sup  *Supervisor
}

func newRig(t *testing.T, budget uint64, rev Revision, mod ...func(*Config)) *rig {
	t.Helper()
	clk := clock.NewFake(t0)
	cfg := testConfig()
	for _, m := range mod {
		m(&cfg)
	}
	r := &rig{t: t, clk: clk, ld: newLoader(clk), gate: newGate(budget), reg: NewMemoryRegistry()}
	sup, err := New(Deps{Clock: clk, Loader: r.ld, Gate: r.gate, Registry: r.reg, Revision: rev, Config: cfg, Verifier: scorerSecVerifier(t, clk)})
	if err != nil {
		t.Fatal(err)
	}
	r.sup = sup
	return r
}

func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app1", TenantID: "ten1", WorkspaceID: "ws1", PrincipalID: "pri1",
		Roles: []string{"staff"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses1",
	}
}

const tsLayout = "2006-01-02T15:04:05.000Z"

func request(now time.Time) gen.DecisionRequest {
	cands := []gen.CandidateSetCandidate{}
	for _, id := range []string{"c1", "c2", "c3"} {
		cands = append(cands, gen.CandidateSetCandidate{CandidateID: common.Identifier(id), Label: "label " + id})
	}
	return gen.DecisionRequest{
		TenantID: "ten1", ApplicationID: "app1", SessionID: "ses1", ContractVersion: "cv1",
		DecisionType: gen.DecisionTypeItemSelection, SchemaVersion: 1,
		BoundedState:       map[string]gen.BoundedScalar{"step": "pick"},
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet:       gen.CandidateSet{Candidates: cands, MaxSelections: 1, BuiltAt: common.Timestamp(now.UTC().Format(tsLayout))},
		Locale:             "en", Deadline: common.Timestamp(now.Add(time.Second).UTC().Format(tsLayout)),
	}
}

func (r *rig) score() (gen.DecisionScoreResult, error) {
	r.t.Helper()
	return r.sup.ScoreDecision(context.Background(), scope(), request(r.clk.Now()))
}

func (r *rig) mustScore() gen.DecisionScoreResult {
	r.t.Helper()
	res, err := r.score()
	if err != nil {
		r.t.Fatalf("score: %v", err)
	}
	return res
}

func tick(r *rig) []string { return r.sup.Tick(context.Background()) }

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tick events = %v, want %v", got, want)
	}
}

var rev1 = func() Revision {
	return revision("model-a", "rev1", 100, map[string][]byte{"w.bin": []byte("weights-1")})
}

// ---- lifecycle -------------------------------------------------------------

func TestDemandLoadAndIdleUnloadAfterWarmPeriod(t *testing.T) {
	r := newRig(t, 1000, rev1())

	// Nothing loads until a request needs it, and an unloaded scorer is loadable, not ready.
	if st := r.sup.State(); st.Phase != "unloaded" || st.Ready || !st.Loadable || !st.Live {
		t.Fatalf("fresh state = %+v", st)
	}
	wantEvents(t, tick(r))
	if r.ld.total != 0 || r.gate.admits != 0 {
		t.Fatalf("Tick loaded a scorer nobody asked for: loads=%d admits=%d", r.ld.total, r.gate.admits)
	}

	r.mustScore()
	if r.ld.loads("rev1") != 1 || r.gate.heldBytes() != 100 {
		t.Fatalf("demand load: loads=%d held=%d", r.ld.loads("rev1"), r.gate.heldBytes())
	}
	if st := r.sup.State(); st.Phase != "ready" || !st.Ready || st.ResidentBytes != 100 {
		t.Fatalf("after load state = %+v", st)
	}
	r.mustScore()
	if r.ld.loads("rev1") != 1 {
		t.Fatalf("a resident scorer was loaded again")
	}

	// Idle for longer than IdleUnload (1m) but still inside MinWarm (5m): stays loaded.
	r.clk.Advance(2 * time.Minute)
	wantEvents(t, tick(r))
	if st := r.sup.State(); !st.Ready {
		t.Fatalf("unloaded during the minimum warm period: %+v", st)
	}

	// Warm period over and idle: unloaded, reservation released, eviction counted.
	r.clk.Advance(3 * time.Minute)
	wantEvents(t, tick(r), "idle_unload")
	if st := r.sup.State(); st.Phase != "unloaded" || st.Ready || st.ResidentBytes != 0 {
		t.Fatalf("after unload state = %+v", st)
	}
	if in := r.ld.last("rev1"); in.closes() != 1 {
		t.Fatalf("instance closed %d times", in.closes())
	}
	if r.gate.heldBytes() != 0 {
		t.Fatalf("reservation not released: %d", r.gate.heldBytes())
	}
	if n := r.reg.CounterTotal(MetricEvictions); n != 1 {
		t.Fatalf("evictions = %d", n)
	}
	if g := r.reg.Gauge(MetricResidentBytes, Labels{Provider: "prov-model-a", Model: "model-a", Revision: "rev1", Profile: "lite"}); g != 0 {
		t.Fatalf("resident gauge = %v", g)
	}

	// The next request demand-loads again.
	r.mustScore()
	if r.ld.loads("rev1") != 2 || r.sup.State().Phase != "ready" {
		t.Fatalf("reload failed: loads=%d state=%+v", r.ld.loads("rev1"), r.sup.State())
	}

	// A request resets the idle clock: with use every 30s it never idles out.
	for i := 0; i < 20; i++ {
		r.clk.Advance(30 * time.Second)
		r.mustScore()
		wantEvents(t, tick(r))
	}
}

func TestResourceGateRejectsWithoutEvictingController(t *testing.T) {
	r := newRig(t, 50, rev1()) // the scorer needs 100, only 50 may be granted

	_, err := r.score()
	if !contracts.HasCode(err, contracts.ErrResourceExhausted) {
		t.Fatalf("want resource_exhausted, got %v", err)
	}
	if r.ld.total != 0 {
		t.Fatalf("a rejected scorer was loaded")
	}
	if r.gate.evictCalls != 0 || !r.gate.protected["controller"] || !r.gate.protected["retrieval"] || !r.gate.protected["speech"] {
		t.Fatalf("a protected worker was touched: %+v", r.gate.protected)
	}
	if n := r.reg.CounterTotal(MetricEvictions); n != 0 {
		t.Fatalf("evictions = %d", n)
	}
	// Not a fault of the scorer: no backoff, still loadable, nothing held.
	if st := r.sup.State(); st.Phase != "unloaded" || !st.Loadable || st.Failures != 0 || r.gate.heldBytes() != 0 {
		t.Fatalf("state after rejection = %+v", st)
	}

	// The gate is the whole interface: admit or reject, no eviction path.
	if n := reflectMethodCount((*ResourceGate)(nil)); n != 1 {
		t.Fatalf("ResourceGate has %d methods; it must only Admit", n)
	}

	// Through the Engine the same rejection is the resource_gate fallback, never a success.
	e := newEngine(t, r.clk, r.sup, "model-a", "rev1")
	adv, err := e.Score(context.Background(), scope(), request(r.clk.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := adv.FallbackReason(); adv.Accepted() || !ok || reason != gen.FallbackReasonResourceGate {
		t.Fatalf("want resource_gate fallback, got accepted=%v reason=%q", adv.Accepted(), reason)
	}

	// When room appears (the other workers finish) the scorer loads on the next request.
	r.gate.mu.Lock()
	r.gate.budget = 1000
	r.gate.mu.Unlock()
	r.mustScore()
	if r.sup.State().Phase != "ready" || r.gate.evictCalls != 0 {
		t.Fatalf("did not load once room appeared: %+v", r.sup.State())
	}
}

func TestRestartWithBackoff(t *testing.T) {
	r := newRig(t, 1000, rev1(), func(c *Config) { c.IdleUnload = 0 })
	boom := contracts.New(contracts.ErrUnavailable, "runtime down")
	r.ld.loadErrs = []error{boom, boom, nil}

	// Failure 1: the request fails closed, the scorer backs off for 1s.
	if _, err := r.score(); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
	st := r.sup.State()
	if st.Phase != "backoff" || st.Failures != 1 || !st.RetryAt.Equal(t0.Add(time.Second)) || st.Ready || st.Loadable {
		t.Fatalf("after failure 1: %+v", st)
	}
	if r.gate.heldBytes() != 0 {
		t.Fatalf("a failed load kept its reservation")
	}

	// During backoff a request does not touch the loader (no restart storm).
	if _, err := r.score(); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("want unavailable during backoff, got %v", err)
	}
	wantEvents(t, tick(r))
	if r.ld.total != 1 {
		t.Fatalf("loader called %d times during backoff", r.ld.total)
	}

	// Backoff over: Tick restarts, fails again, the delay doubles to 2s.
	r.clk.Advance(time.Second)
	wantEvents(t, tick(r), "restart_failed")
	if st := r.sup.State(); st.Failures != 2 || !st.RetryAt.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("after failure 2: %+v", st)
	}
	r.clk.Advance(time.Second)
	wantEvents(t, tick(r))
	if r.ld.total != 2 {
		t.Fatalf("restarted before the doubled delay: %d loads", r.ld.total)
	}

	// Third attempt succeeds. The failure count only clears after a warm period.
	r.clk.Advance(time.Second)
	wantEvents(t, tick(r), "restarted")
	if st := r.sup.State(); st.Phase != "ready" || !st.Ready || st.Failures != 2 {
		t.Fatalf("after restart: %+v", st)
	}
	if n := r.reg.CounterTotal(MetricRestarts); n != 2 {
		t.Fatalf("restarts counted = %d", n)
	}
	r.mustScore()
	r.clk.Advance(5 * time.Minute)
	wantEvents(t, tick(r), "backoff_reset")
	if st := r.sup.State(); st.Failures != 0 {
		t.Fatalf("failures not reset after a stable warm period: %+v", st)
	}

	// A crash mid-flight (the runtime answers unavailable) schedules a restart too, and
	// the delay is back to the initial 1s.
	r.ld.last("rev1").Script(fakescorer.Step{Fail: fakescorer.FailUnavailable})
	if _, err := r.score(); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("want unavailable from the crashed runtime, got %v", err)
	}
	if st := r.sup.State(); st.Phase != "backoff" || st.Failures != 1 || !st.RetryAt.Equal(r.clk.Now().Add(time.Second)) {
		t.Fatalf("after crash: %+v", st)
	}
	if in := r.ld.last("rev1"); in.closes() != 1 {
		t.Fatalf("the crashed instance was not closed (%d)", in.closes())
	}
	r.clk.Advance(time.Second)
	wantEvents(t, tick(r), "restarted")
	r.mustScore()

	// The delay is capped at BackoffMax (8s).
	r.ld.loadErrs = []error{boom, boom, boom, boom, boom}
	r.ld.last("rev1").setHealth(boom)
	wantEvents(t, tick(r), "probe_failed")
	for i := 0; i < 5; i++ {
		r.clk.Advance(time.Minute)
		wantEvents(t, tick(r), "restart_failed")
	}
	if st := r.sup.State(); st.RetryAt.Sub(r.clk.Now()) != 8*time.Second {
		t.Fatalf("backoff not capped: %+v", st)
	}
}

func TestStagedUpdateVerifiesHash(t *testing.T) {
	good := map[string][]byte{"w.bin": []byte("weights-2"), "head.bin": []byte("head")}
	src := func(mod func(memSource)) memSource {
		m := memSource{}
		for k, v := range good {
			m[k] = v
		}
		if mod != nil {
			mod(m)
		}
		return m
	}
	encoder := sum([]byte("not-a-listed-file"))
	cases := []struct {
		name     string
		src      memSource
		mutate   func(*Revision)
		wantErr  bool
		wantCode contracts.ErrCode
		mismatch bool
		reason   string
	}{
		{name: "matching hashes promote", src: src(nil)},
		{name: "tampered content", src: src(func(m memSource) { m["w.bin"] = []byte("weights-X") }), wantErr: true,
			wantCode: contracts.ErrValidationFailed, mismatch: true, reason: "hash_mismatch"},
		{name: "same length different bytes", src: src(func(m memSource) { m["head.bin"] = []byte("hxad") }), wantErr: true,
			wantCode: contracts.ErrValidationFailed, mismatch: true, reason: "hash_mismatch"},
		{name: "truncated file", src: src(func(m memSource) { m["w.bin"] = []byte("weights") }), wantErr: true,
			wantCode: contracts.ErrValidationFailed, mismatch: true, reason: "hash_mismatch"},
		{name: "longer file", src: src(func(m memSource) { m["w.bin"] = []byte("weights-2-and-more") }), wantErr: true,
			wantCode: contracts.ErrValidationFailed, mismatch: true, reason: "hash_mismatch"},
		{name: "missing file", src: src(func(m memSource) { delete(m, "head.bin") }), wantErr: true,
			wantCode: contracts.ErrUnavailable, reason: "verification"},
		{name: "path in file name", src: src(nil), wantErr: true, wantCode: contracts.ErrValidationFailed, reason: "verification",
			mutate: func(r *Revision) { r.Manifest.Files[0].Name = "../w.bin" }},
		{name: "upper-case hash", src: src(nil), wantErr: true, wantCode: contracts.ErrValidationFailed, reason: "verification",
			mutate: func(r *Revision) {
				r.Manifest.Files[0].SHA256 = gen.SHA256("ABCDEF" + string(r.Manifest.Files[0].SHA256)[6:])
			}},
		{name: "no files listed", src: src(nil), wantErr: true, wantCode: contracts.ErrValidationFailed, reason: "verification",
			mutate: func(r *Revision) { r.Manifest.Files = nil }},
		{name: "encoder hash matches no verified file", src: src(nil), wantErr: true, wantCode: contracts.ErrValidationFailed,
			mismatch: true, reason: "hash_mismatch", mutate: func(r *Revision) { r.Manifest.Encoder.SHA256 = &encoder }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 1000, rev1())
			r.mustScore() // rev1 is resident and serving
			next := revision("model-a", "rev2", 100, good)
			if tc.mutate != nil {
				tc.mutate(&next)
			}
			rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next), tc.src, nil)
			if !tc.wantErr {
				if err != nil || rep.Status != UpdatePromoted {
					t.Fatalf("update = %+v, %v", rep, err)
				}
				if got := r.sup.Manifest().ModelRevision; got != "rev2" {
					t.Fatalf("serving %q after a verified update", got)
				}
				if res := r.mustScore(); *res.ModelRevision != "rev2" {
					t.Fatalf("served by %q", *res.ModelRevision)
				}
				if r.gate.heldBytes() != 100 {
					t.Fatalf("held %d after promotion (previous revision must be released)", r.gate.heldBytes())
				}
				return
			}
			if !contracts.HasCode(err, tc.wantCode) || errors.Is(err, ErrHashMismatch) != tc.mismatch {
				t.Fatalf("err = %v (code %q, mismatch=%v), want code %q mismatch=%v", err, contracts.CodeOf(err), errors.Is(err, ErrHashMismatch), tc.wantCode, tc.mismatch)
			}
			if rep.Status != UpdateRejected || rep.Reason != tc.reason {
				t.Fatalf("report = %+v, want rejected/%s", rep, tc.reason)
			}
			// Nothing of the unverified revision was admitted, loaded or switched to.
			if r.ld.loads("rev2") != 0 || r.gate.admits != 1 {
				t.Fatalf("unverified revision reached the loader/gate: loads=%d admits=%d", r.ld.loads("rev2"), r.gate.admits)
			}
			if got := r.sup.Manifest().ModelRevision; got != "rev1" || !r.sup.State().Ready {
				t.Fatalf("serving %q ready=%v after a rejected update", got, r.sup.State().Ready)
			}
		})
	}
}

func TestStagedUpdateResourceGateRejectionKeepsServing(t *testing.T) {
	r := newRig(t, 150, rev1()) // room for one 100-byte scorer, not two
	r.mustScore()
	files := map[string][]byte{"w.bin": []byte("w2")}
	rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, revision("model-a", "rev2", 100, files)), memSource(files), nil)
	if !contracts.HasCode(err, contracts.ErrResourceExhausted) || rep.Status != UpdateRejected || rep.Reason != "resource_gate" {
		t.Fatalf("update = %+v, %v", rep, err)
	}
	if r.ld.loads("rev2") != 0 || r.gate.evictCalls != 0 || r.sup.Manifest().ModelRevision != "rev1" || !r.sup.State().Ready {
		t.Fatalf("rejected update disturbed the serving revision")
	}
}

func TestFailedHealthProbeRollsBackToPreviousRevision(t *testing.T) {
	files := map[string][]byte{"w.bin": []byte("w2")}
	next := func() Revision { return revision("model-a", "rev2", 100, files) }
	probeErr := contracts.New(contracts.ErrUnavailable, "probe failed")

	servedBy := func(r *rig) string { return string(*r.mustScore().ModelRevision) }

	t.Run("staged revision fails its probe", func(t *testing.T) {
		r := newRig(t, 1000, rev1())
		r.mustScore()
		r.ld.healthErr["rev2"] = probeErr
		rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), nil)
		if !contracts.HasCode(err, contracts.ErrUnavailable) || rep.Status != UpdateRolledBack || rep.Reason != "health_probe" {
			t.Fatalf("update = %+v, %v", rep, err)
		}
		if got := r.sup.Manifest().ModelRevision; got != "rev1" || servedBy(r) != "rev1" {
			t.Fatalf("not back on rev1: %q", got)
		}
		if in := r.ld.last("rev2"); in.closes() != 1 {
			t.Fatalf("the failed staged instance was not closed")
		}
		if r.gate.heldBytes() != 100 {
			t.Fatalf("staged reservation leaked: %d", r.gate.heldBytes())
		}
		if r.reg.Counter(MetricRollbacks, Labels{Provider: "prov-model-a", Model: "model-a", Revision: "rev2", Profile: "lite", Reason: "health_probe"}) != 1 {
			t.Fatalf("rollback not counted")
		}
	})

	t.Run("staged revision fails the promotion gate", func(t *testing.T) {
		r := newRig(t, 1000, rev1())
		r.mustScore()
		gate := PromotionGateFunc(func(context.Context, Instance) error { return errors.New("calibration regressed") })
		rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), gate)
		if !contracts.HasCode(err, contracts.ErrValidationFailed) || rep.Status != UpdateRolledBack || rep.Reason != "gate" {
			t.Fatalf("update = %+v, %v", rep, err)
		}
		if servedBy(r) != "rev1" || r.ld.last("rev2").closes() != 1 || r.gate.heldBytes() != 100 {
			t.Fatalf("gate failure did not leave rev1 serving cleanly")
		}
	})

	t.Run("staged revision fails to load", func(t *testing.T) {
		r := newRig(t, 1000, rev1())
		r.mustScore()
		r.ld.loadErrs = []error{contracts.New(contracts.ErrUnavailable, "cannot start")}
		rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), nil)
		if err == nil || rep.Status != UpdateRolledBack || rep.Reason != "load" || servedBy(r) != "rev1" || r.gate.heldBytes() != 100 {
			t.Fatalf("update = %+v, %v", rep, err)
		}
	})

	t.Run("probe fails after the switch, during probation", func(t *testing.T) {
		r := newRig(t, 1000, rev1(), func(c *Config) { c.Probation = 10 * time.Minute; c.IdleUnload = 0 })
		r.mustScore()
		old := r.ld.last("rev1")
		rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), nil)
		if err != nil || rep.Status != UpdateProbation {
			t.Fatalf("update = %+v, %v", rep, err)
		}
		if servedBy(r) != "rev2" || r.gate.heldBytes() != 200 || !r.sup.State().Probation {
			t.Fatalf("probation state wrong: held=%d state=%+v", r.gate.heldBytes(), r.sup.State())
		}
		r.clk.Advance(time.Minute)
		wantEvents(t, tick(r)) // healthy: probation continues

		r.ld.last("rev2").setHealth(probeErr)
		wantEvents(t, tick(r), "rolled_back")
		if r.sup.Manifest().ModelRevision != "rev1" || servedBy(r) != "rev1" {
			t.Fatalf("not back on rev1")
		}
		if r.ld.loads("rev1") != 1 || old.closes() != 0 {
			t.Fatalf("the previous instance should have been kept resident, not reloaded (loads=%d closes=%d)", r.ld.loads("rev1"), old.closes())
		}
		if r.ld.last("rev2").closes() != 1 || r.gate.heldBytes() != 100 || r.sup.State().Probation {
			t.Fatalf("bad revision not released: held=%d", r.gate.heldBytes())
		}
	})

	t.Run("new revision crashes during probation", func(t *testing.T) {
		r := newRig(t, 1000, rev1(), func(c *Config) { c.Probation = 10 * time.Minute })
		r.mustScore()
		if _, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), nil); err != nil {
			t.Fatal(err)
		}
		r.ld.last("rev2").Script(fakescorer.Step{Fail: fakescorer.FailUnavailable})
		if _, err := r.score(); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("want the crash surfaced, got %v", err)
		}
		if servedBy(r) != "rev1" || r.sup.State().Failures != 0 {
			t.Fatalf("crash during probation did not return to rev1: %+v", r.sup.State())
		}
	})

	t.Run("reported gate breach and probation end", func(t *testing.T) {
		r := newRig(t, 1000, rev1(), func(c *Config) { c.Probation = 10 * time.Minute; c.IdleUnload = 0 })
		r.mustScore()
		if _, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), nil); err != nil {
			t.Fatal(err)
		}
		if err := r.sup.Rollback(context.Background()); err != nil || servedBy(r) != "rev1" || r.gate.heldBytes() != 100 {
			t.Fatalf("rollback: %v held=%d", err, r.gate.heldBytes())
		}
		if err := r.sup.Rollback(context.Background()); !contracts.HasCode(err, contracts.ErrConflict) {
			t.Fatalf("rollback with nothing in probation = %v", err)
		}

		if _, err := r.sup.Update(context.Background(), scorerSecSign(r.t, next()), memSource(files), nil); err != nil {
			t.Fatal(err)
		}
		r.clk.Advance(10 * time.Minute)
		wantEvents(t, tick(r), "promoted")
		if servedBy(r) != "rev2" || r.gate.heldBytes() != 100 || r.ld.insts["rev1"][0].closes() != 1 {
			t.Fatalf("promotion did not release rev1: held=%d", r.gate.heldBytes())
		}
	})
}

// ---- metrics ---------------------------------------------------------------

func TestMetricsHaveNoOutboundPush(t *testing.T) {
	// 1. The package has no way to send: no network, process, plugin or syscall
	//    imports and no goroutines in any non-test source file.
	assertNoOutboundCapability(t)

	// 2. Registry only records; the in-memory registry exposes read methods only.
	for _, typ := range []any{(*Registry)(nil), &MemoryRegistry{}, &Metrics{}} {
		for _, name := range reflectMethodNames(typ) {
			for _, bad := range []string{"Push", "Send", "Export", "Flush", "Post", "Publish", "Emit", "Upload", "Report", "Scrape", "Serve", "Listen", "Dial"} {
				if len(name) >= len(bad) && name[:len(bad)] == bad {
					t.Errorf("%T has an outbound-looking method %s", typ, name)
				}
			}
		}
	}

	// 3. Labels cannot carry a tenant, application, principal or session.
	for _, f := range reflectFieldNames(Labels{}) {
		switch f {
		case "Provider", "Model", "Revision", "Profile", "Reason":
		default:
			t.Errorf("Labels has an unexpected field %s", f)
		}
	}

	// 4. The recorded numbers are readable locally.
	r := newRig(t, 1000, rev1())
	e := newEngine(t, r.clk, r.sup, "model-a", "rev1")
	m := r.sup.Metrics()
	for i := 0; i < 3; i++ {
		adv, err := e.Score(context.Background(), scope(), request(r.clk.Now()))
		if err != nil {
			t.Fatal(err)
		}
		m.ObserveAdvice(adv)
		if adv.Accepted() {
			m.ControllerCallAvoided()
		}
	}
	bad := request(r.clk.Now())
	bad.CandidateSet.Candidates = nil // rejected by the guard: never reached the scorer
	adv, _ := e.Score(context.Background(), scope(), bad)
	m.ObserveAdvice(adv)

	if n := r.reg.CounterTotal(MetricRequests); n != 4 {
		t.Fatalf("requests = %d", n)
	}
	if rate, ok := r.reg.AcceptanceRate(); !ok || rate != 0.75 {
		t.Fatalf("acceptance = %v %v", rate, ok)
	}
	if n := r.reg.CounterTotal(MetricControllerAvoided); n != 3 {
		t.Fatalf("controller calls avoided = %d", n)
	}
	if n := r.reg.CounterByReason(MetricFallback, string(gen.FallbackReasonCandidateSetRejected)); n != 1 {
		t.Fatalf("fallback{candidate_set_rejected} = %d", n)
	}
	// 1 miss then 2 hits.
	if rate, ok := r.reg.CacheHitRate(); !ok || rate < 0.66 || rate > 0.67 {
		t.Fatalf("cache hit rate = %v %v", rate, ok)
	}
	if h := r.reg.Histogram(MetricLatencyMS); h.Count != 3 {
		t.Fatalf("latency observations = %d (a guard rejection must not count)", h.Count)
	}
	if g := r.reg.Gauge(MetricResidentBytes, Labels{Provider: "prov-model-a", Model: "model-a", Revision: "rev1", Profile: "lite"}); g != 100 {
		t.Fatalf("resident gauge = %v", g)
	}
}

func TestHistogramQuantiles(t *testing.T) {
	reg := NewMemoryRegistry()
	for i := 0; i < 100; i++ {
		reg.Observe(MetricLatencyMS, Labels{}, float64(i+1)) // 1..100
	}
	h := reg.Histogram(MetricLatencyMS)
	if h.Count != 100 || h.P50 != 50 || h.P95 != 100 || h.P99 != 100 {
		t.Fatalf("histogram = %+v", h)
	}
	reg.Observe(MetricLatencyMS, Labels{}, -1)
	if reg.Histogram(MetricLatencyMS).Count != 100 {
		t.Fatalf("a negative observation was kept")
	}
}

// ---- provider replacement --------------------------------------------------

func TestProviderReplacementKeepsContractUnchanged(t *testing.T) {
	big := revision("model-big", "revB1", 800, map[string][]byte{"w.bin": []byte("big")})
	smallFiles := map[string][]byte{"w.bin": []byte("small")}
	small := revision("model-small", "revS1", 100, smallFiles)
	small.Manifest.ProviderID = "other-provider"

	r := newRig(t, 1000, big)

	// The supervisor is registered under the router role exactly like any scorer, and
	// the registry hands back the same object after the replacement.
	reg := providers.NewRegistry(r.clk)
	if err := decision.RegisterRouter(reg, r.sup); err != nil {
		t.Fatal(err)
	}
	var contractHolder decision.DecisionModelProvider = r.sup
	e := newEngine(t, r.clk, contractHolder, "model-big", "revB1", "model-small", "revS1")

	req := request(r.clk.Now())
	before, err := e.Score(context.Background(), scope(), req)
	if err != nil || !before.Accepted() {
		t.Fatalf("before: accepted=%v err=%v", before.Accepted(), err)
	}
	if *before.Result().ModelID != "model-big" {
		t.Fatalf("served by %q", *before.Result().ModelID)
	}

	// Replace the scorer: a different provider, model, revision, size and runtime.
	rep, err := r.sup.Update(context.Background(), scorerSecSign(r.t, small), memSource(smallFiles), nil)
	if err != nil || rep.Status != UpdatePromoted {
		t.Fatalf("update = %+v, %v", rep, err)
	}

	// Same engine, same request, same types: the Engine and its caller changed nothing.
	req.Deadline = common.Timestamp(r.clk.Now().Add(time.Second).UTC().Format(tsLayout))
	req.CandidateSet.BuiltAt = common.Timestamp(r.clk.Now().UTC().Format(tsLayout))
	after, err := e.Score(context.Background(), scope(), req)
	if err != nil || !after.Accepted() {
		t.Fatalf("after: accepted=%v err=%v", after.Accepted(), err)
	}
	res := after.Result()
	if *res.ModelID != "model-small" || res.ProviderID != "other-provider" || *res.ModelRevision != "revS1" {
		t.Fatalf("result does not echo the new scorer: %+v", res)
	}
	if res.CacheStatus == gen.DecisionScoreResultCacheStatusHit {
		t.Fatalf("a result cached for the old revision answered the new one")
	}
	if got, want := fmt.Sprint(after.Suggestions()[0].CandidateID), fmt.Sprint(before.Suggestions()[0].CandidateID); got != want {
		t.Fatalf("selection changed shape: %s vs %s", got, want)
	}
	if res.ContractVersion != before.Result().ContractVersion || res.DecisionType != before.Result().DecisionType || res.SchemaVersion != before.Result().SchemaVersion {
		t.Fatalf("contract fields changed across the replacement")
	}

	// The registry still yields the same provider, typed as the unchanged contract.
	p, why := decision.ProviderFromRegistry(reg)
	if why != "" || p == nil {
		t.Fatalf("registry lost the provider: %q", why)
	}
	if p.Manifest().ModelID != "model-small" {
		t.Fatalf("registry provider serves %q", p.Manifest().ModelID)
	}
	if n := reflectMethodCount((*decision.DecisionModelProvider)(nil)); n != 2 {
		t.Fatalf("DecisionModelProvider has %d methods", n)
	}
}

// ---- engine plumbing -------------------------------------------------------

type fakeResolver struct{}

func (fakeResolver) Exposed(context.Context, contracts.Scope, gen.CandidateSetCandidate) error {
	return nil
}

func (fakeResolver) Resolve(_ context.Context, _ contracts.Scope, id string) (decision.Resolved, error) {
	return decision.Resolved{ID: id, Kind: "item", Version: "v1"}, nil
}

type fakeAudit struct{ n int }

func (a *fakeAudit) AppendAt(_ context.Context, _ audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	a.n++
	return audit.Event{ID: fmt.Sprintf("aud_%026d", a.n), Type: ev.Type}, nil
}

func (a *fakeAudit) AppendTx(context.Context, *store.Tx, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, fmt.Errorf("not used")
}

// newEngine builds a decision.Engine over p, calibrated for the given (model, revision) pairs.
func newEngine(t *testing.T, clk clock.Clock, p decision.DecisionModelProvider, pairs ...string) *decision.Engine {
	t.Helper()
	cfg := decision.DefaultConfig()
	cfg.Enabled = true
	cfg.SupportedTypes = []gen.DecisionType{gen.DecisionTypeItemSelection}
	for i := 0; i+1 < len(pairs); i += 2 {
		cfg.Calibration = append(cfg.Calibration, decision.Calibration{
			ModelID: pairs[i], ModelRevision: pairs[i+1], ContractVersion: "cv1", ECE: 0.02, MaxECE: 0.05,
		})
	}
	e, err := decision.New(decision.Deps{Clock: clk, Provider: p, Resolver: fakeResolver{}, Audit: &fakeAudit{}, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestConcurrentRequestsShareOneDemandLoad(t *testing.T) {
	r := newRig(t, 1000, rev1())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.score(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("score: %v", err)
	}
	if r.ld.loads("rev1") != 1 || r.gate.admits != 1 {
		t.Fatalf("loads=%d admits=%d, want one shared load", r.ld.loads("rev1"), r.gate.admits)
	}
}
