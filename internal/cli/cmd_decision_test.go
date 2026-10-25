package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/config"
	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/dialogue"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The tests of D-15 for the optional decision layer in `alixai serve`: the service of D-09 with
// counters per application and tenant, the supervisor and its tick driver, the off-by-default state
// and the audit of configuration changes. The scorer is a scripted fake behind the provider hook;
// nothing here contacts a scorer endpoint.

// decWireFake is a scripted scorer with a health probe that can be made to fail.
type decWireFake struct {
	*fakescorer.Scorer
	health  atomic.Int64
	failing atomic.Bool
}

func (f *decWireFake) Health(context.Context) error {
	f.health.Add(1)
	if f.failing.Load() {
		return errors.New("the fake scorer is down")
	}
	return nil
}

// decWireInstallFake makes serve build the fake instead of an adapter, and counts instance closes.
func decWireInstallFake(t *testing.T) (*decWireFake, *atomic.Int64) {
	t.Helper()
	f := &decWireFake{Scorer: fakescorer.New(fakescorer.DefaultManifest(), nil)}
	closed := &atomic.Int64{}
	oldBuild, oldClosed := decWireBuildProvider, decWireClosed
	decWireBuildProvider = func(decWireProviderConfig) (any, error) { return f, nil }
	decWireClosed = closed
	t.Cleanup(func() { decWireBuildProvider, decWireClosed = oldBuild, oldClosed })
	return f, closed
}

// decWireEnabledYAML is a decision block that turns the layer on with the fake scorer. The calibration
// entry is a test fixture, not a measurement.
const decWireEnabledYAML = `decision:
  enabled: true
  provider:
    kind: clm
    model: fake-model
    model_revision: rev1
  tick_interval: 20ms
  operators:
    - {application_id: app1, tenant_id: ten1, principal_id: pri1}
  calibration:
    - {model_id: fake-model, model_revision: rev1, contract_version: dialogue-routing-v1, ece: 0.02, max_ece: 0.05}
  lifecycle:
    backoff_initial: 20ms
    backoff_max: 40ms
`

// decWireServe starts serve with extra YAML and returns the decision stack it built and the deps the
// providers saw (a probe provider that runs last).
func decWireServe(t *testing.T, extra string, onBuilt func(*decWireStack)) (*decWireStack, serveDeps, *running) {
	t.Helper()
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	var (
		stack *decWireStack
		saw   serveDeps
	)
	oldHook := decWireOnBuilt
	decWireOnBuilt = func(s *decWireStack) {
		stack = s
		if onBuilt != nil {
			onBuilt(s)
		}
	}
	registerServeProvider("zz_dec_probe", func(_ context.Context, d serveDeps) error { saw = d; return nil })
	t.Cleanup(func() {
		decWireOnBuilt = oldHook
		serveProvidersMu.Lock()
		delete(serveProviders, "zz_dec_probe")
		serveProvidersMu.Unlock()
	})
	cfg, _ := serveConfig(t, f, extra)
	srv := startServe(t, cfg)
	if stack == nil {
		t.Fatal("serve did not build the decision stack")
	}
	return stack, saw, srv
}

func decWireScope(tenant string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app1", TenantID: tenant, WorkspaceID: "ws1", PrincipalID: "pri1",
		Roles: []string{"staff"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses1",
	}
}

func decWireRequest(tenant string) gen.DecisionRequest {
	now := time.Now().UTC()
	ts := func(t time.Time) common.Timestamp { return common.Timestamp(t.Format(decWireTimestamp)) }
	cands := []gen.CandidateSetCandidate{}
	for _, id := range []string{"c1", "c2", "c3"} {
		cands = append(cands, gen.CandidateSetCandidate{CandidateID: common.Identifier(id), Label: "label " + id})
	}
	return gen.DecisionRequest{
		TenantID: common.Identifier(tenant), ApplicationID: "app1", SessionID: "ses1", ContractVersion: ddec.ContractRouting,
		DecisionType: gen.DecisionTypeCapabilityRouting, SchemaVersion: 1,
		BoundedState:       map[string]gen.BoundedScalar{"user_text": "book it"},
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet:       gen.CandidateSet{Candidates: cands, MaxSelections: 1, BuiltAt: ts(now)},
		Locale:             "en", Deadline: ts(now.Add(3 * time.Second)),
	}
}

type decWireResolver struct{}

func (decWireResolver) Exposed(context.Context, contracts.Scope, gen.CandidateSetCandidate) error {
	return nil
}

func (decWireResolver) Resolve(_ context.Context, _ contracts.Scope, id string) (core.Resolved, error) {
	return core.Resolved{ID: id, Kind: "item", Version: "v1"}, nil
}

// decWireEngine is an engine over the stack's own supervisor, appending through the stack's counting
// audit tee: what the layer's engines do, driven directly.
func decWireEngine(t *testing.T, s *decWireStack) *core.Engine {
	t.Helper()
	cc := core.DefaultConfig()
	cc.Enabled = true
	cc.Timeout = 3 * time.Second
	cc.SupportedTypes = []gen.DecisionType{gen.DecisionTypeCapabilityRouting}
	cc.Calibration = []core.Calibration{{ModelID: "fake-model", ModelRevision: "rev1", ContractVersion: ddec.ContractRouting, ECE: 0.02, MaxECE: 0.05}}
	eng, err := core.New(core.Deps{Provider: s.sup, Resolver: decWireResolver{}, Audit: decWireAudit{inner: s.audit, s: s}, Config: cc})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// serve provides gateway.DecisionService; its status counts per application and tenant, and
// rejected_count is the sum of the fallback counts.
func TestServeProvidesTheDecisionServiceWithPerTenantCounters(t *testing.T) {
	fake, _ := decWireInstallFake(t)
	fake.Script(
		fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.97}}},
		fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c1", Score: 0.5, Confidence: 0.5}}}, // below the 0.85 floor
	)
	s, saw, _ := decWireServe(t, decWireEnabledYAML, nil)

	svc, ok := gateway.Lookup[gateway.DecisionService](saw.Services)
	if !ok {
		t.Fatal("serve does not provide gateway.DecisionService")
	}
	if _, ok := gateway.Lookup[ddec.Turner](saw.Services); !ok {
		t.Error("serve does not provide Layer.Wrap(engine) as the turn entry point")
	}
	if !saw.Providers.Usable(providers.RoleRouter) {
		t.Error("the supervisor is not the registered router scorer")
	}
	if p, _ := core.ProviderFromRegistry(saw.Providers); p == nil {
		t.Error("the router role does not hold a decision scorer")
	}
	if got := svc.PlatformFloor(); got != 0.5 {
		t.Errorf("PlatformFloor = %v, want 0.5", got)
	}

	eng := decWireEngine(t, s)
	ctx := context.Background()
	score := func(tenant string) core.Advice {
		adv, err := eng.Score(ctx, decWireScope(tenant), decWireRequest(tenant))
		if err != nil {
			t.Fatal(err)
		}
		return adv
	}
	if adv := score("ten1"); !adv.Accepted() {
		t.Fatalf("tenant ten1, first: %v", adv.Result().FallbackReason)
	}
	if adv := score("ten1"); !adv.Accepted() { // the second answer comes from the cache and is still counted
		t.Fatalf("tenant ten1, second: %v", adv.Result().FallbackReason)
	}
	if adv := score("ten2"); adv.Accepted() {
		t.Fatal("tenant ten2: a result below the confidence floor was accepted")
	}

	status := func(tenant string) gen.DecisionStatus {
		st, err := svc.Status(ctx, decWireScope(tenant))
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, fc := range st.FallbackCounts {
			sum += fc.Count
		}
		if sum != st.RejectedCount {
			t.Errorf("tenant %s: rejected_count %d is not the sum of fallback_counts %d", tenant, st.RejectedCount, sum)
		}
		return st
	}
	a, b, c := status("ten1"), status("ten2"), status("ten3")
	if a.AcceptedCount != 2 || a.RejectedCount != 0 {
		t.Errorf("ten1 = accepted %d rejected %d, want 2 and 0", a.AcceptedCount, a.RejectedCount)
	}
	if b.AcceptedCount != 0 || b.RejectedCount != 1 || len(b.FallbackCounts) != 1 || b.FallbackCounts[0].Reason != gen.FallbackReasonLowConfidence {
		t.Errorf("ten2 = %+v, want one low_confidence", b)
	}
	if c.AcceptedCount != 0 || c.RejectedCount != 0 || len(c.FallbackCounts) != 0 {
		t.Errorf("a tenant that never scored sees counters: %+v", c)
	}
	if a.Mode != gen.DecisionStatusModeShadow || a.ProviderID != "fake-scorer" || a.ModelID == nil || *a.ModelID != "fake-model" {
		t.Errorf("status identity = %+v", a)
	}
	if fake.Calls() != 2 {
		t.Errorf("the scorer was called %d times, want 2 (one answer came from the cache)", fake.Calls())
	}

	// The lifecycle metrics are fed by the layer's Observer (ObserveAdvice), covered by
	// TestMetricsHookObservesEveryAdvice in internal/dialogue/decision; the tee only counts per tenant.
	if st := s.sup.State(); !st.Ready || st.ModelID != "fake-model" {
		t.Errorf("the supervisor did not load the scorer on demand: %+v", st)
	}
}

// With no decision block the layer is off: the engine is built with the no-op stage and the rule
// detectors, nothing is registered in the router role, no supervisor or tick exists, the service says
// "off" and refuses a change.
func TestDecisionLayerIsOffByDefaultAndChangesNothing(t *testing.T) {
	var got dialogue.Deps
	old := serveProfileNewEngine
	serveProfileNewEngine = func(d dialogue.Deps) (*dialogue.Engine, error) { got = d; return old(d) }
	t.Cleanup(func() { serveProfileNewEngine = old })
	// A provider hook that would fail the test if it were ever reached.
	oldBuild := decWireBuildProvider
	decWireBuildProvider = func(decWireProviderConfig) (any, error) {
		t.Error("a scorer provider was built while the layer is off")
		return nil, nil
	}
	t.Cleanup(func() { decWireBuildProvider = oldBuild })

	s, saw, srv := decWireServe(t, "", nil)

	if _, ok := got.Decision.(dialogue.NoopDecisionStage); !ok {
		t.Errorf("the engine's decision stage is %T, want the no-op stage", got.Decision)
	}
	if !reflect.DeepEqual(got.Detectors, dialogue.Detectors{}) {
		t.Error("the engine has detectors other than the rules")
	}
	if res := got.Decision.Decide(context.Background(), dialogue.DecisionRequest{}); res.Selected != nil || res.Fallback != dialogue.FallbackDisabled {
		t.Errorf("the off stage answered %+v", res)
	}
	if saw.Providers.Has(providers.RoleRouter) {
		t.Error("the router role is registered while the layer is off")
	}
	if s.enabled || s.sup != nil || s.Turner() != nil {
		t.Errorf("the off stack holds a layer: enabled=%v supervisor=%v", s.enabled, s.sup)
	}
	if _, ok := gateway.Lookup[ddec.Turner](saw.Services); ok {
		t.Error("a Turner is provided while the layer is off")
	}
	if strings.Contains(srv.stderr.String(), "decision layer is on") {
		t.Error("serve says the decision layer is on")
	}

	svc, ok := gateway.Lookup[gateway.DecisionService](saw.Services)
	if !ok {
		t.Fatal("serve does not provide gateway.DecisionService")
	}
	st, err := svc.Status(context.Background(), decWireScope("ten1"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != gen.DecisionStatusModeOff || st.ProviderID != "none" || st.AcceptedCount != 0 || st.RejectedCount != 0 || len(st.FallbackCounts) != 0 {
		t.Errorf("the off status = %+v", st)
	}
	if err := svc.AuthorizeOperator(context.Background(), decWireScope("ten1")); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("with no operator configured AuthorizeOperator = %v, want permission_denied", err)
	}
	up := gateway.DecisionConfigUpdate{Profile: "lite", Mode: gen.DecisionStatusModeAdvisory}
	if _, err := svc.SetConfig(context.Background(), decWireScope("ten1"), up); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("SetConfig on an off layer = %v, want unavailable", err)
	}
	// Starting the tick driver of an off stack runs nothing and stopping it is safe.
	s.start(context.Background())()
}

// The tick driver runs while serve runs (probe failure, restart after backoff) and serve's stop ends it
// and the supervisor with it: the scorer is closed and nothing ticks any more.
func TestServeTicksAndShutsDownTheScorerSupervisor(t *testing.T) {
	fake, closed := decWireInstallFake(t)
	s, _, srv := decWireServe(t, decWireEnabledYAML+"  peak_bytes: 1048576\n", nil)

	// Nothing is loaded until a request needs it.
	if st := s.sup.State(); st.Phase != "unloaded" || fake.health.Load() != 0 {
		t.Fatalf("the scorer was loaded before any request: %+v health=%d", st, fake.health.Load())
	}
	if _, err := s.sup.ScoreDecision(context.Background(), decWireScope("ten1"), decWireRequest("ten1")); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "health probes from the tick driver", func() bool { return fake.health.Load() >= 3 })
	if st := s.sup.State(); !st.Ready || st.ResidentBytes != 1048576 {
		t.Errorf("a loaded scorer = %+v, want ready with its reservation", st)
	}

	// A failed probe is the supervisor's probe_failed; once the scorer answers again it restarts.
	fake.failing.Store(true)
	pollUntil(t, 10*time.Second, "probe_failed", func() bool { return s.tickEvents()["probe_failed"] >= 1 })
	fake.failing.Store(false)
	pollUntil(t, 10*time.Second, "restarted", func() bool { return s.tickEvents()["restarted"] >= 1 })
	if !s.sup.State().Ready {
		t.Error("the scorer did not come back after the restart")
	}

	if err := srv.stop(); err != nil {
		t.Fatalf("serve stopped with %v", err)
	}
	st := s.sup.State()
	if st.Live || st.ResidentBytes != 0 {
		t.Errorf("after serve stopped the supervisor is %+v, want stopped with nothing resident", st)
	}
	if closed.Load() < 1 {
		t.Error("the scorer instance was never closed")
	}
	seen := fake.health.Load()
	time.Sleep(150 * time.Millisecond)
	if fake.health.Load() != seen {
		t.Error("the tick driver kept probing after serve stopped")
	}
	if _, err := s.sup.ScoreDecision(context.Background(), decWireScope("ten1"), decWireRequest("ten1")); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("a stopped supervisor answered with %v, want unavailable", err)
	}
}

// decWireRecorder records the events appended through it, forwarding to the real log, and can refuse.
type decWireRecorder struct {
	inner  audit.Appender
	mu     sync.Mutex
	events []audit.NewEvent
	chains []audit.ChainKey
	fail   bool
}

func (r *decWireRecorder) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return audit.Event{}, contracts.New(contracts.ErrUnavailable, "the audit log is down")
	}
	out, err := r.inner.AppendAt(ctx, loc, ev)
	if err == nil {
		r.events = append(r.events, ev)
		r.chains = append(r.chains, loc.Chain())
	}
	return out, err
}

func (r *decWireRecorder) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	return r.inner.AppendTx(ctx, tx, loc, ev)
}

// The operator list is the configuration's alone; a change is audited first (per-mille integers, the
// tenant chain of the operator) and applied only if the audit succeeded; a profile the deployment does
// not approve, or a threshold under the floor, changes nothing and records nothing.
func TestDecisionConfigChangesAreAudited(t *testing.T) {
	decWireInstallFake(t)
	var rec *decWireRecorder
	s, _, _ := decWireServe(t, decWireEnabledYAML, func(s *decWireStack) {
		rec = &decWireRecorder{inner: s.audit}
		s.audit = rec
	})
	ctx := context.Background()
	op, other := decWireScope("ten1"), decWireScope("ten1")
	other.PrincipalID = "pri2"
	other.Roles = []string{"operator", "admin"} // asserted by the token, worth nothing
	other.CapabilityScopes = []string{"decision.status.read", "decision.config.write"}
	if err := s.AuthorizeOperator(ctx, op); err != nil {
		t.Errorf("the configured operator was refused: %v", err)
	}
	if err := s.AuthorizeOperator(ctx, other); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("a token that asserts operator roles passed: %v", err)
	}
	wrongTenant := decWireScope("ten2")
	if err := s.AuthorizeOperator(ctx, wrongTenant); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("the operator of another tenant passed: %v", err)
	}

	before := s.cur.Load().layer
	min := 0.9
	got, err := s.SetConfig(ctx, op, gateway.DecisionConfigUpdate{Profile: "lite", Mode: gen.DecisionStatusModeAdvisory, MinConfidence: &min})
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "lite" || got.Mode != gen.DecisionStatusModeAdvisory || got.MinConfidence != 0.9 {
		t.Errorf("SetConfig = %+v", got)
	}
	if len(rec.events) != 1 || rec.events[0].Type != eventDecWireConfigChanged.Name() {
		t.Fatalf("recorded %d events, want one decision.config_changed", len(rec.events))
	}
	if ch := rec.chains[0]; ch.ApplicationID != "app1" || ch.TenantID != "ten1" {
		t.Errorf("the event is on chain %+v, want the operator's tenant chain", ch)
	}
	data, ok := rec.events[0].Data.(decWireConfigAudit)
	if !ok || data.Profile != "lite" || !data.Running || data.Mode != "advisory" || data.PreviousMode != "shadow" ||
		data.MinConfidencePermille != 900 || data.PreviousMinConfidencePermille != 850 {
		t.Errorf("the audit payload = %+v", rec.events[0].Data)
	}
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), ".") {
		t.Errorf("the audit payload holds a non-integer number: %s", raw)
	}
	if s.cur.Load().layer == before || s.cur.Load().mode != gen.DecisionStatusModeAdvisory {
		t.Error("a new threshold did not rebuild the layer (the engine fixes MinConfidence when built)")
	}
	if st, _ := s.Status(ctx, op); st.Mode != gen.DecisionStatusModeAdvisory {
		t.Errorf("status mode = %s", st.Mode)
	}
	if cfg, _ := s.Config(ctx, op); len(cfg) != 1 || cfg[0].MinConfidence != 0.9 || cfg[0].Mode != gen.DecisionStatusModeAdvisory {
		t.Errorf("Config = %+v", cfg)
	}

	// Refusals change nothing and record nothing.
	low := 0.4
	for name, up := range map[string]gateway.DecisionConfigUpdate{
		"unapproved profile": {Profile: "performance", Mode: gen.DecisionStatusModeAdvisory},
		"below the floor":    {Profile: "lite", Mode: gen.DecisionStatusModeAdvisory, MinConfidence: &low},
		"invalid mode":       {Profile: "lite", Mode: "enforce"},
	} {
		if _, err := s.SetConfig(ctx, op, up); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s: SetConfig = %v, want validation_failed", name, err)
		}
	}
	if len(rec.events) != 1 {
		t.Errorf("a refused change was audited: %d events", len(rec.events))
	}

	// If the audit cannot be written the change is not applied.
	rec.mu.Lock()
	rec.fail = true
	rec.mu.Unlock()
	if _, err := s.SetConfig(ctx, op, gateway.DecisionConfigUpdate{Profile: "lite", Mode: gen.DecisionStatusModeOff}); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("SetConfig without an audit log = %v, want unavailable", err)
	}
	if s.cur.Load().mode != gen.DecisionStatusModeAdvisory {
		t.Error("a change that could not be audited was applied")
	}
	rec.mu.Lock()
	rec.fail = false
	rec.mu.Unlock()

	// Off disables the stage without a rebuild and without calling the scorer.
	if _, err := s.SetConfig(ctx, op, gateway.DecisionConfigUpdate{Profile: "lite", Mode: gen.DecisionStatusModeOff}); err != nil {
		t.Fatal(err)
	}
	if res := s.stage().Decide(ctx, dialogue.DecisionRequest{}); res.Selected != nil || res.Fallback != dialogue.FallbackDisabled {
		t.Errorf("the stage of a profile that is off answered %+v", res)
	}
	if len(rec.events) != 2 {
		t.Errorf("%d events recorded, want 2", len(rec.events))
	}
}

// The section is OFF by default and refuses what it cannot run.
func TestDecisionSectionDefaultsAndValidation(t *testing.T) {
	d := decWireDefaults()
	if d.Enabled || d.Validate() != nil {
		t.Fatalf("the default section must be off and valid: %+v", d)
	}
	reg := config.NewRegistry()
	decWireRegisterSections(reg)
	if decWireSectionOf(reg).Enabled {
		t.Error("a fresh registry has the layer enabled")
	}
	bad := map[string]func(*decWireConfig){
		"floor under 0.5": func(c *decWireConfig) { c.PlatformFloor = 0.4 },
		"threshold under floor": func(c *decWireConfig) {
			c.Profiles = map[string]decWireProfile{"lite": {Mode: "shadow", MinConfidence: 0.4}}
		},
		"unknown mode": func(c *decWireConfig) {
			c.Profiles = map[string]decWireProfile{"lite": {Mode: "enforce", MinConfidence: 0.9}}
		},
		"operator without id": func(c *decWireConfig) { c.Operators = []decWireOperator{{ApplicationID: "a", TenantID: "t"}} },
		"unknown contract":    func(c *decWireConfig) { c.Calibration = []decWireCalibration{{ContractVersion: "v9"}} },
		"enabled, no kind":    func(c *decWireConfig) { c.Enabled = true },
	}
	for name, mod := range bad {
		c := decWireDefaults()
		mod(&c)
		if c.Validate() == nil {
			t.Errorf("%s: the section was accepted", name)
		}
	}
}
