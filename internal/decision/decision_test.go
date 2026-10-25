package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ---- test doubles ----------------------------------------------------------

type recorder struct {
	mu     sync.Mutex
	events []audit.NewEvent
	ids    []string
	// failType makes appends of that event type fail; "*" fails every append.
	failType audit.EventType
}

func (r *recorder) AppendAt(_ context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if loc.Level() == 0 {
		return audit.Event{}, contracts.New(contracts.ErrScopeMissing, "no locator")
	}
	if r.failType == "*" || r.failType == ev.Type {
		return audit.Event{}, contracts.New(contracts.ErrUnavailable, "audit down")
	}
	id := fmt.Sprintf("aud_%026d", len(r.events)+1)
	r.events = append(r.events, ev)
	r.ids = append(r.ids, id)
	return audit.Event{ID: id, Type: ev.Type}, nil
}

func (r *recorder) AppendTx(context.Context, *store.Tx, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, fmt.Errorf("not used")
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = string(e.Type)
	}
	return out
}

type fakeResolver struct {
	mu           sync.Mutex
	exposedErr   map[string]error
	resolveErr   map[string]error
	resolveAs    map[string]string
	exposedCalls int
	resolved     []string
}

func (f *fakeResolver) Exposed(_ context.Context, _ contracts.Scope, c gen.CandidateSetCandidate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposedCalls++
	return f.exposedErr[string(c.CandidateID)]
}

func (f *fakeResolver) Resolve(_ context.Context, _ contracts.Scope, id string) (Resolved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, id)
	if err := f.resolveErr[id]; err != nil {
		return Resolved{}, err
	}
	if as, ok := f.resolveAs[id]; ok {
		return Resolved{ID: as, Kind: "item", Version: "v1"}, nil
	}
	return Resolved{ID: id, Kind: "item", Version: "v1"}, nil
}

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func ts(t time.Time) common.Timestamp { return common.Timestamp(t.UTC().Format(timestampLayout)) }

type env struct {
	t   *testing.T
	clk *clock.Fake
	sc  *fakescorer.Scorer
	res *fakeResolver
	rec *recorder
	eng *Engine
	cfg Config
}

func testScope(tenant string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app1", TenantID: tenant, WorkspaceID: "ws1", PrincipalID: "pri1",
		Roles: []string{"staff", "admin"}, CapabilityScopes: []string{"cap.read", "cap.write"}, SessionID: "ses1",
	}
}

func testConfig() Config {
	c := DefaultConfig()
	c.Enabled = true
	c.SupportedTypes = []gen.DecisionType{gen.DecisionTypeItemSelection, gen.DecisionTypeActionSelection, gen.DecisionTypeCapabilityRouting}
	c.Calibration = []Calibration{{ModelID: "fake-model", ModelRevision: "rev1", ContractVersion: "cv1", ECE: 0.02, MaxECE: 0.05}}
	return c
}

func newEnv(t *testing.T, mods ...func(*Config)) *env {
	t.Helper()
	e := &env{t: t, clk: clock.NewFake(t0), res: &fakeResolver{}, rec: &recorder{}, cfg: testConfig()}
	for _, m := range mods {
		m(&e.cfg)
	}
	e.sc = fakescorer.New(fakescorer.DefaultManifest(), e.clk)
	eng, err := New(Deps{Clock: e.clk, Provider: e.sc, Resolver: e.res, Audit: e.rec, Config: e.cfg})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = eng
	return e
}

func (e *env) request(tenant string) gen.DecisionRequest {
	cands := []gen.CandidateSetCandidate{}
	for _, id := range []string{"c1", "c2", "c3"} {
		cands = append(cands, gen.CandidateSetCandidate{CandidateID: common.Identifier(id), Label: "label " + id})
	}
	return gen.DecisionRequest{
		TenantID: common.Identifier(tenant), ApplicationID: "app1", SessionID: "ses1", ContractVersion: "cv1",
		DecisionType: gen.DecisionTypeItemSelection, SchemaVersion: 1,
		BoundedState:       map[string]gen.BoundedScalar{"step": "pick", "n": 3},
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet:       gen.CandidateSet{Candidates: cands, MaxSelections: 1, BuiltAt: ts(e.clk.Now())},
		Locale:             "en", Deadline: ts(e.clk.Now().Add(time.Second)),
	}
}

func (e *env) score(req gen.DecisionRequest) Advice {
	e.t.Helper()
	adv, err := e.eng.Score(context.Background(), testScope("ten1"), req)
	if err != nil {
		e.t.Fatal(err)
	}
	return adv
}

func wantFallback(t *testing.T, adv Advice, reason gen.FallbackReason) {
	t.Helper()
	got, ok := adv.FallbackReason()
	if adv.Accepted() || !ok || got != reason {
		t.Fatalf("want fallback %q, got accepted=%v reason=%q", reason, adv.Accepted(), got)
	}
	if len(adv.Result().Selections) != 0 || len(adv.Suggestions()) != 0 {
		t.Fatalf("a fallback must carry no selection: %+v", adv.Result().Selections)
	}
}

func (e *env) wantRejected(req gen.DecisionRequest, code RejectCode) {
	e.t.Helper()
	adv := e.score(req)
	wantFallback(e.t, adv, gen.FallbackReasonCandidateSetRejected)
	if adv.RejectCode() != code {
		e.t.Fatalf("reject code = %q, want %q", adv.RejectCode(), code)
	}
	if e.sc.Calls() != 0 {
		e.t.Fatalf("the provider was called %d times for a rejected set", e.sc.Calls())
	}
}

// ---- candidate guard -------------------------------------------------------

func TestRejectsEmptyCandidateSet(t *testing.T) {
	e := newEnv(t)
	req := e.request("ten1")
	req.CandidateSet.Candidates = nil
	e.wantRejected(req, RejectEmpty)
}

func TestRejectsOversizedCandidateSet(t *testing.T) {
	e := newEnv(t)
	req := e.request("ten1")
	for i := 0; len(req.CandidateSet.Candidates) <= e.cfg.MaxCandidates; i++ {
		req.CandidateSet.Candidates = append(req.CandidateSet.Candidates, gen.CandidateSetCandidate{CandidateID: common.Identifier(fmt.Sprintf("x%d", i)), Label: "x"})
	}
	e.wantRejected(req, RejectOversized)

	long := e.request("ten1")
	long.CandidateSet.Candidates[0].Label = strings.Repeat("a", e.cfg.MaxLabelLen+1)
	e.wantRejected(long, RejectOversized)

	badMax := e.request("ten1")
	badMax.CandidateSet.MaxSelections = 4
	e.wantRejected(badMax, RejectInvalid)
}

func TestRejectsDuplicateCandidates(t *testing.T) {
	e := newEnv(t)
	req := e.request("ten1")
	req.CandidateSet.Candidates[2].CandidateID = "c1"
	e.wantRejected(req, RejectDuplicate)
}

func TestRejectsStaleCandidates(t *testing.T) {
	e := newEnv(t)
	old := e.request("ten1")
	old.CandidateSet.BuiltAt = ts(t0.Add(-time.Minute))
	e.wantRejected(old, RejectStale)

	future := e.request("ten1")
	future.CandidateSet.BuiltAt = ts(t0.Add(time.Minute))
	e.wantRejected(future, RejectStale)

	garbled := e.request("ten1")
	garbled.CandidateSet.BuiltAt = "yesterday"
	e.wantRejected(garbled, RejectStale)

	// The adapter says a candidate's data version moved on.
	e.res.exposedErr = map[string]error{"c2": contracts.New(contracts.ErrStale, "data version changed")}
	e.wantRejected(e.request("ten1"), RejectStale)

	// Time moving on makes a previously fine set stale.
	e.res.exposedErr = nil
	req := e.request("ten1")
	e.clk.Advance(e.cfg.MaxCandidateAge + time.Second)
	req.Deadline = ts(e.clk.Now().Add(time.Second))
	e.wantRejected(req, RejectStale)
}

func TestRejectsCrossTenantCandidates(t *testing.T) {
	e := newEnv(t)
	e.res.exposedErr = map[string]error{"c2": contracts.New(contracts.ErrScopeViolation, "candidate belongs to another tenant")}
	e.wantRejected(e.request("ten1"), RejectCrossTenant)
}

func TestRejectsUnauthorizedCandidates(t *testing.T) {
	e := newEnv(t)
	e.res.exposedErr = map[string]error{"c3": contracts.New(contracts.ErrPermissionDenied, "principal may not see c3")}
	e.wantRejected(e.request("ten1"), RejectUnauthorized)

	// An error the guard does not recognise is still a refusal.
	e.res.exposedErr = map[string]error{"c1": fmt.Errorf("adapter exploded")}
	e.wantRejected(e.request("ten1"), RejectUnauthorized)

	// Choosing among actions: a candidate outside the session's permitted next actions.
	e.res.exposedErr = nil
	cap := common.CapabilityID("book")
	req := e.request("ten1")
	req.DecisionType = gen.DecisionTypeActionSelection
	req.AllowedNextActions = []actions.PermittedNextAction{
		{Action: actions.PermittedNextActionActionInvokeCapability, CapabilityID: &cap},
		{Action: actions.PermittedNextActionActionConfirm},
	}
	req.CandidateSet.Candidates = []gen.CandidateSetCandidate{
		{CandidateID: "book", Label: "Book"}, {CandidateID: "confirm", Label: "Confirm"}, {CandidateID: "delete_everything", Label: "Delete"},
	}
	e.wantRejected(req, RejectUnauthorized)
	req.CandidateSet.Candidates = req.CandidateSet.Candidates[:2]
	if adv := e.score(req); !adv.Accepted() {
		t.Fatalf("permitted action candidates must reach the scorer: %v", adv.Result().FallbackReason)
	}
}

// ---- fail closed -----------------------------------------------------------

func TestFailsClosedOnTimeout(t *testing.T) {
	// Simulated latency past the budget, on the injected clock.
	e := newEnv(t)
	e.sc.Script(fakescorer.Step{Latency: time.Second})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonTimeout)

	// A provider error that says timeout.
	e = newEnv(t)
	e.sc.Script(fakescorer.Step{Fail: fakescorer.FailTimeout})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonTimeout)

	// A provider that never answers is cut by the context deadline.
	e = newEnv(t, func(c *Config) { c.Timeout = 20 * time.Millisecond })
	e.sc.Script(fakescorer.Step{Hang: true})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonTimeout)

	// A deadline that has already passed: the provider is not called at all.
	e = newEnv(t)
	req := e.request("ten1")
	req.Deadline = ts(t0.Add(-time.Millisecond))
	wantFallback(t, e.score(req), gen.FallbackReasonTimeout)
	if e.sc.Calls() != 0 {
		t.Fatal("an expired deadline still called the provider")
	}
}

func TestFailsClosedOnMalformedOutput(t *testing.T) {
	cases := map[string]fakescorer.Step{
		"undecodable":          {Fail: fakescorer.FailMalformed},
		"invented id":          {Picks: []fakescorer.Pick{{ID: "zzz", Score: 0.9, Confidence: 0.95}}},
		"duplicate selection":  {Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.95}, {ID: "c1", Score: 0.9, Confidence: 0.95}}},
		"too many selections":  {Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.95}, {ID: "c2", Score: 0.9, Confidence: 0.95}}},
		"nan score":            {Picks: []fakescorer.Pick{{ID: "c1", Score: math.NaN(), Confidence: 0.95}}},
		"confidence above one": {Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 1.5}}},
		"accepted, nothing":    {Picks: []fakescorer.Pick{}},
		"unknown disposition":  {Mutate: func(r *gen.DecisionScoreResult) { r.Disposition = "maybe" }},
		"fallback without reason": {Mutate: func(r *gen.DecisionScoreResult) {
			r.Disposition = gen.DecisionScoreResultDispositionFallback
		}},
		"accepted with reason": {Mutate: func(r *gen.DecisionScoreResult) {
			x := gen.FallbackReasonUnavailable
			r.FallbackReason = &x
		}},
		"wrong schema version": {Mutate: func(r *gen.DecisionScoreResult) { r.SchemaVersion = 2 }},
		"wrong decision type":  {Mutate: func(r *gen.DecisionScoreResult) { r.DecisionType = gen.DecisionTypeDialogueAct }},
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.sc.Script(step)
			wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonMalformedOutput)
		})
	}
}

func TestFailsClosedOnLowConfidence(t *testing.T) {
	e := newEnv(t)
	e.sc.Script(fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c1", Score: 0.99, Confidence: 0.4}}})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonLowConfidence)

	// Every selection must clear the floor, not just the best one.
	e = newEnv(t)
	req := e.request("ten1")
	req.CandidateSet.MaxSelections = 2
	e.sc.Script(fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.95}, {ID: "c2", Score: 0.5, Confidence: 0.5}}})
	wantFallback(t, e.score(req), gen.FallbackReasonLowConfidence)
}

func TestFailsClosedOnCalibrationFailure(t *testing.T) {
	// The measured calibration error is above the operator's bound.
	e := newEnv(t, func(c *Config) { c.Calibration[0].ECE = 0.2 })
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonCalibrationFailed)

	// No calibration was ever measured for this contract version.
	e = newEnv(t)
	req := e.request("ten1")
	req.ContractVersion = "cv2"
	wantFallback(t, e.score(req), gen.FallbackReasonCalibrationFailed)

	// Nor for this model revision.
	e = newEnv(t, func(c *Config) { c.Calibration[0].ModelRevision = "rev0" })
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonCalibrationFailed)
}

func TestFailsClosedOnModelMismatch(t *testing.T) {
	cases := map[string]func(*gen.DecisionScoreResult){
		"model revision": func(r *gen.DecisionScoreResult) { v := actions.OpaqueVersion("rev9"); r.ModelRevision = &v },
		"model id":       func(r *gen.DecisionScoreResult) { v := common.Identifier("other-model"); r.ModelID = &v },
		"provider id":    func(r *gen.DecisionScoreResult) { r.ProviderID = "other-provider" },
		"no model":       func(r *gen.DecisionScoreResult) { r.ModelID = nil },
		"contract":       func(r *gen.DecisionScoreResult) { r.ContractVersion = "cv9" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.sc.Script(fakescorer.Step{Mutate: mut})
			wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonModelMismatch)
		})
	}
	t.Run("pinned revision", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.PinnedRevision = "rev-approved" })
		wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonModelMismatch)
	})
}

func TestFailsClosedOnTenantMismatch(t *testing.T) {
	// A request body that names another tenant than the verified scope.
	e := newEnv(t)
	wantFallback(t, e.score(e.request("ten2")), gen.FallbackReasonTenantMismatch)
	if e.sc.Calls() != 0 || e.res.exposedCalls != 0 {
		t.Fatal("a cross-scope request reached the resolver or the provider")
	}
	// A provider that answers for another tenant.
	e = newEnv(t)
	e.sc.Script(fakescorer.Step{Mutate: func(r *gen.DecisionScoreResult) { r.TenantID = "ten2" }})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonTenantMismatch)
	e = newEnv(t)
	e.sc.Script(fakescorer.Step{Mutate: func(r *gen.DecisionScoreResult) { r.ApplicationID = "app2" }})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonTenantMismatch)

	// An advice minted for one tenant is not resolvable in another.
	e = newEnv(t)
	adv := e.score(e.request("ten1"))
	got, err := e.eng.Resolve(context.Background(), testScope("ten2"), adv)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := got.Fallback(); r != gen.FallbackReasonTenantMismatch || len(got.Selected()) != 0 {
		t.Fatalf("resolve across tenants = %+v", got)
	}
}

func TestFailsClosedOnPolicyConflict(t *testing.T) {
	// The scorer picked an id the adapter now refuses (policy changed in between).
	e := newEnv(t)
	adv := e.score(e.request("ten1"))
	if !adv.Accepted() {
		t.Fatal("setup: advice should be accepted")
	}
	e.res.resolveErr = map[string]error{"c1": contracts.New(contracts.ErrPermissionDenied, "no longer permitted")}
	got, err := e.eng.Resolve(context.Background(), testScope("ten1"), adv)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := got.Fallback(); !ok || r != gen.FallbackReasonPolicyConflict || len(got.Selected()) != 0 {
		t.Fatalf("resolution = %+v, want policy_conflict and no selection", got)
	}

	// The adapter resolves the id to something else: also a conflict.
	e = newEnv(t)
	e.res.resolveAs = map[string]string{"c1": "c9"}
	got, _ = e.eng.Resolve(context.Background(), testScope("ten1"), e.score(e.request("ten1")))
	if r, _ := got.Fallback(); r != gen.FallbackReasonPolicyConflict {
		t.Fatalf("resolver answered another id; got %q", r)
	}

	// A provider that itself reports policy_conflict is passed through.
	e = newEnv(t)
	e.sc.Script(fakescorer.Step{Picks: []fakescorer.Pick{}, Mutate: func(r *gen.DecisionScoreResult) {
		x := gen.FallbackReasonPolicyConflict
		r.Disposition, r.FallbackReason = gen.DecisionScoreResultDispositionFallback, &x
	}})
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonPolicyConflict)
}

func TestOODMarksUnsupportedShape(t *testing.T) {
	for _, st := range []gen.DecisionScoreResultOodStatus{
		gen.DecisionScoreResultOodStatusUnsupportedShape, gen.DecisionScoreResultOodStatusOutOfDistribution, gen.DecisionScoreResultOodStatusNotEvaluated,
	} {
		e := newEnv(t)
		st := st
		e.sc.Script(fakescorer.Step{Mutate: func(r *gen.DecisionScoreResult) { r.OodStatus = st }})
		adv := e.score(e.request("ten1"))
		wantFallback(t, adv, gen.FallbackReasonOodUnsupportedShape)
		if adv.Result().OodStatus != st {
			t.Fatalf("ood status = %q, want %q", adv.Result().OodStatus, st)
		}
	}

	// Shapes the scorer is not qualified for never reach it.
	e := newEnv(t)
	req := e.request("ten1")
	req.DecisionType = gen.DecisionTypePathVerification
	adv := e.score(req)
	wantFallback(t, adv, gen.FallbackReasonOodUnsupportedShape)
	if adv.Result().OodStatus != gen.DecisionScoreResultOodStatusUnsupportedShape || e.sc.Calls() != 0 {
		t.Fatalf("unsupported decision type: ood=%q calls=%d", adv.Result().OodStatus, e.sc.Calls())
	}
	req = e.request("ten1")
	req.BoundedState["nested"] = map[string]any{"a": 1}
	wantFallback(t, e.score(req), gen.FallbackReasonOodUnsupportedShape)
	req = e.request("ten1")
	req.SchemaVersion = 2
	wantFallback(t, e.score(req), gen.FallbackReasonOodUnsupportedShape)
}

// ---- advisory only ---------------------------------------------------------

func TestResultIsAdvisoryOnly(t *testing.T) {
	banned := regexp.MustCompile(`(?i)authori[sz]|commit|prepare|confirm|permit|allow|execut|approve|grant`)

	// No exported field or method of the guarded outcome types can be read as an
	// authorisation, validation, preparation, confirmation or commit result.
	for _, typ := range []reflect.Type{reflect.TypeOf(Advice{}), reflect.TypeOf(Suggestion{}), reflect.TypeOf(Resolution{}), reflect.TypeOf(ResolvedSelection{})} {
		for i := 0; i < typ.NumMethod(); i++ {
			if banned.MatchString(typ.Method(i).Name) {
				t.Errorf("%s.%s reads like an authority", typ.Name(), typ.Method(i).Name)
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).IsExported() && banned.MatchString(typ.Field(i).Name) {
				t.Errorf("%s.%s reads like an authority", typ.Name(), typ.Field(i).Name)
			}
		}
	}
	if typ := reflect.TypeOf(Advice{}); func() bool {
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).IsExported() {
				return true
			}
		}
		return false
	}() {
		t.Error("Advice must keep its fields unexported so a score cannot be edited into a result")
	}
	// Neither type is a policy decision or an action result, structurally.
	var advice any = Advice{}
	if _, ok := advice.(interface{ Authorized() bool }); ok {
		t.Error("Advice must not answer Authorized()")
	}
	if _, ok := advice.(interface{ Allowed() bool }); ok {
		t.Error("Advice must not answer Allowed()")
	}

	// The wire form carries a disposition and scores, and no authority.
	e := newEnv(t)
	adv := e.score(e.request("ten1"))
	raw, err := json.Marshal(adv.Result())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		if banned.MatchString(k) {
			t.Errorf("DecisionScoreResult carries field %q", k)
		}
	}

	// An accepted advice alone unlocks nothing: its selections are usable only
	// through Resolve, and a zero Advice is a fallback, never an acceptance.
	if (Advice{}).Accepted() {
		t.Error("the zero Advice is accepted")
	}
	got, _ := e.eng.Resolve(context.Background(), testScope("ten1"), Advice{})
	if _, ok := got.Fallback(); !ok || len(got.Selected()) != 0 {
		t.Error("resolving a zero Advice must fall back")
	}
}

func TestEveryOutcomeIsAcceptedOrHasFallbackReason(t *testing.T) {
	type tc struct {
		name string
		step fakescorer.Step
		mod  func(*env, *gen.DecisionRequest)
	}
	bad := func(f func(*gen.DecisionScoreResult)) fakescorer.Step { return fakescorer.Step{Mutate: f} }
	cases := []tc{
		{name: "accepted"},
		{name: "timeout", step: fakescorer.Step{Fail: fakescorer.FailTimeout}},
		{name: "unavailable", step: fakescorer.Step{Fail: fakescorer.FailUnavailable}},
		{name: "resource", step: fakescorer.Step{Fail: fakescorer.FailResourceExhausted}},
		{name: "malformed", step: fakescorer.Step{Fail: fakescorer.FailMalformed}},
		{name: "low confidence", step: fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.1}}}},
		{name: "ood", step: bad(func(r *gen.DecisionScoreResult) { r.OodStatus = gen.DecisionScoreResultOodStatusOutOfDistribution })},
		{name: "model", step: bad(func(r *gen.DecisionScoreResult) { r.ProviderID = "x" })},
		{name: "tenant", step: bad(func(r *gen.DecisionScoreResult) { r.TenantID = "x" })},
		{name: "empty set", mod: func(_ *env, r *gen.DecisionRequest) { r.CandidateSet.Candidates = nil }},
		{name: "bad deadline", mod: func(_ *env, r *gen.DecisionRequest) { r.Deadline = "soon" }},
		{name: "cross-tenant request", mod: func(_ *env, r *gen.DecisionRequest) { r.TenantID = "other" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.sc.Script(c.step)
			req := e.request("ten1")
			if c.mod != nil {
				c.mod(e, &req)
			}
			adv := e.score(req)
			res := adv.Result()
			reason, has := adv.FallbackReason()
			switch {
			case adv.Accepted():
				if has || res.FallbackReason != nil || len(res.Selections) == 0 {
					t.Fatalf("accepted advice with a reason or no selection: %+v", res)
				}
			default:
				if !has || !reason.Valid() || res.Disposition != gen.DecisionScoreResultDispositionFallback || len(res.Selections) != 0 {
					t.Fatalf("fallback without a valid reason: %+v", res)
				}
			}
			if res.AuditRef == "" || !strings.HasPrefix(string(res.AuditRef), "aud_") || res.TraceID == "" || res.IssuedAt == "" {
				t.Fatalf("outcome lacks audit ref / trace / time: %+v", res)
			}
			if _, err := json.Marshal(res); err != nil {
				t.Fatalf("outcome is not encodable: %v", err)
			}
		})
	}
}

func TestSelectedIDsRequireDeterministicReresolution(t *testing.T) {
	e := newEnv(t)
	req := e.request("ten1")
	req.CandidateSet.MaxSelections = 2
	e.sc.Script(fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c3", Score: 0.8, Confidence: 0.9}, {ID: "c2", Score: 0.9, Confidence: 0.97}}})
	adv := e.score(req)
	if !adv.Accepted() {
		t.Fatalf("setup: %+v", adv.Result())
	}
	if len(e.res.resolved) != 0 {
		t.Fatal("scoring alone must not resolve; only Resolve does")
	}
	sugg := adv.Suggestions()
	if len(sugg) != 2 || sugg[0].CandidateID != "c2" {
		t.Fatalf("suggestions must be best first: %+v", sugg)
	}

	got, err := e.eng.Resolve(context.Background(), testScope("ten1"), adv)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.res.resolved) != 2 || e.res.resolved[0] != "c2" || e.res.resolved[1] != "c3" {
		t.Fatalf("every selected id must be re-resolved by the adapter, got %v", e.res.resolved)
	}
	sel := got.Selected()
	if len(sel) != 2 || sel[0].Resolved().ID != "c2" || sel[0].Resolved().Kind != "item" || sel[0].Suggestion().Confidence != 0.97 {
		t.Fatalf("resolved selections = %+v", sel)
	}

	// One refusal voids the whole proposal.
	e.res.resolveErr = map[string]error{"c3": contracts.New(contracts.ErrPermissionDenied, "no")}
	got, _ = e.eng.Resolve(context.Background(), testScope("ten1"), adv)
	if _, ok := got.Fallback(); !ok || len(got.Selected()) != 0 {
		t.Fatalf("a refused id must void the proposal: %+v", got)
	}
}

// ---- cache -----------------------------------------------------------------

func TestCacheKeyIncludesScopeContractVersionAndRevision(t *testing.T) {
	e := newEnv(t)
	m := fakescorer.DefaultManifest()
	scope := testScope("ten1")
	req := e.request("ten1")
	base := cacheKey(scope, m, req)

	vary := map[string]func() CacheKey{
		"application":      func() CacheKey { s := scope; s.ApplicationID = "app2"; return cacheKey(s, m, req) },
		"tenant":           func() CacheKey { s := scope; s.TenantID = "ten2"; return cacheKey(s, m, req) },
		"workspace":        func() CacheKey { s := scope; s.WorkspaceID = "ws2"; return cacheKey(s, m, req) },
		"principal":        func() CacheKey { s := scope; s.PrincipalID = "pri2"; return cacheKey(s, m, req) },
		"roles":            func() CacheKey { s := scope; s.Roles = []string{"staff"}; return cacheKey(s, m, req) },
		"capability scope": func() CacheKey { s := scope; s.CapabilityScopes = []string{"cap.read"}; return cacheKey(s, m, req) },
		"contract version": func() CacheKey { r := req; r.ContractVersion = "cv2"; return cacheKey(scope, m, r) },
		"model revision":   func() CacheKey { x := m; x.ModelRevision = "rev2"; return cacheKey(scope, x, req) },
		"model id":         func() CacheKey { x := m; x.ModelID = "other"; return cacheKey(scope, x, req) },
		"decision type": func() CacheKey {
			r := req
			r.DecisionType = gen.DecisionTypeCapabilityRouting
			return cacheKey(scope, m, r)
		},
		"locale":         func() CacheKey { r := req; r.Locale = "it"; return cacheKey(scope, m, r) },
		"schema version": func() CacheKey { r := req; r.SchemaVersion = 2; return cacheKey(scope, m, r) },
		"max selections": func() CacheKey {
			r := e.request("ten1")
			r.CandidateSet.MaxSelections = 2
			return cacheKey(scope, m, r)
		},
		"candidate label": func() CacheKey {
			r := e.request("ten1")
			r.CandidateSet.Candidates[0].Label = "changed"
			return cacheKey(scope, m, r)
		},
		"bounded state": func() CacheKey {
			r := e.request("ten1")
			r.BoundedState["step"] = "other"
			return cacheKey(scope, m, r)
		},
	}
	for name, f := range vary {
		if f() == base {
			t.Errorf("cache key ignores %s", name)
		}
	}

	// What does not change the scorer's view does not change the key.
	same := e.request("ten1")
	same.CandidateSet.BuiltAt = ts(t0.Add(time.Hour))
	same.Deadline = ts(t0.Add(2 * time.Hour))
	same.SessionID = "ses9"
	reordered := scope
	reordered.Roles = []string{"admin", "staff"}
	if cacheKey(reordered, m, same) != base {
		t.Error("timestamps, session and role order must not change the key")
	}

	// And the engine honours it: a second identical request is a hit, a request under
	// another model revision is not.
	adv1 := e.score(e.request("ten1"))
	adv2 := e.score(e.request("ten1"))
	if adv1.Result().CacheStatus != gen.DecisionScoreResultCacheStatusMiss || adv2.Result().CacheStatus != gen.DecisionScoreResultCacheStatusHit || e.sc.Calls() != 1 {
		t.Fatalf("cache status %q then %q, provider calls %d", adv1.Result().CacheStatus, adv2.Result().CacheStatus, e.sc.Calls())
	}
	e.clk.Advance(e.cfg.CacheTTL + time.Second)
	if adv := e.score(e.request("ten1")); adv.Result().CacheStatus != gen.DecisionScoreResultCacheStatusMiss || e.sc.Calls() != 2 {
		t.Fatal("an expired entry must not be served")
	}
}

func TestCacheNeverCrossesTenants(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reqA, reqB := e.request("ten1"), e.request("ten2")
	for i := 0; i < 2; i++ {
		if adv, _ := e.eng.Score(ctx, testScope("ten1"), reqA); !adv.Accepted() {
			t.Fatal("tenant A scoring failed")
		}
	}
	if e.sc.Calls() != 1 {
		t.Fatalf("tenant A's second call must be a hit, provider calls = %d", e.sc.Calls())
	}
	advB, _ := e.eng.Score(ctx, testScope("ten2"), reqB)
	if e.sc.Calls() != 2 || advB.Result().CacheStatus != gen.DecisionScoreResultCacheStatusMiss {
		t.Fatalf("tenant B must not be served tenant A's entry: calls=%d cache=%q", e.sc.Calls(), advB.Result().CacheStatus)
	}
	if advB.Result().TenantID != "ten2" {
		t.Fatalf("tenant B's advice names %q", advB.Result().TenantID)
	}

	// Even a corrupted entry (a result of tenant B under tenant A's key) is dropped.
	m := fakescorer.DefaultManifest()
	keyA := cacheKey(testScope("ten1"), m, reqA)
	cached, ok := e.eng.cache.get(keyA)
	if !ok {
		t.Fatal("setup: no entry for tenant A")
	}
	cached.TenantID = "ten2"
	e.eng.cache.put(keyA, cached)
	if _, ok := e.eng.cache.get(keyA); ok {
		t.Fatal("an entry naming another tenant was served")
	}

	// Purging one tenant leaves the other's entries.
	kept := advB.Result()
	kept.TenantID = "ten1"
	e.eng.cache.put(cacheKey(testScope("ten1"), m, reqA), kept)
	e.eng.PurgeTenant("app1", "ten2")
	if _, ok := e.eng.cache.get(cacheKey(testScope("ten2"), m, reqB)); ok {
		t.Fatal("PurgeTenant left the tenant's entry")
	}
	if e.eng.cache.len() != 1 {
		t.Fatalf("purge removed another tenant's entries: %d left", e.eng.cache.len())
	}
}

// The cache evicts in insertion order: a read never reorders, a re-put keeps the entry's
// place (and refreshes its expiry), and a purge or an expired read frees slots without
// changing the order of what is left.
func TestCacheEvictsOldestInsertedFirst(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	c := newResultCache(clk, 3, int64(time.Minute))
	key := func(tenant string, n byte) CacheKey {
		return CacheKey{Application: "app1", Tenant: tenant, Input: [32]byte{n}}
	}
	res := func(tenant string) gen.DecisionScoreResult {
		return gen.DecisionScoreResult{ApplicationID: "app1", TenantID: common.Identifier(tenant)}
	}
	has := func(k CacheKey) bool { _, ok := c.get(k); return ok }
	want := func(step string, n int, in, out []CacheKey) {
		t.Helper()
		for _, k := range in {
			if !has(k) {
				t.Fatalf("%s: entry %d is gone", step, k.Input[0])
			}
		}
		for _, k := range out {
			if has(k) {
				t.Fatalf("%s: entry %d is still served", step, k.Input[0])
			}
		}
		if c.len() != n {
			t.Fatalf("%s: %d entries, want %d", step, c.len(), n)
		}
	}

	a1, a2, a3, a4 := key("ten1", 1), key("ten1", 2), key("ten2", 3), key("ten2", 4)
	c.put(a1, res("ten1"))
	c.put(a2, res("ten1"))
	c.put(a3, res("ten2"))
	has(a1)
	c.put(a1, res("ten1"))
	want("a re-put does not evict", 3, []CacheKey{a1, a2, a3}, nil)
	c.put(a4, res("ten2"))
	want("the oldest inserted goes first, read and re-put or not", 3, []CacheKey{a2, a3, a4}, []CacheKey{a1})

	c.purgeTenant("app1", "ten1")
	want("a purge drops only the tenant's entries", 2, []CacheKey{a3, a4}, []CacheKey{a2})
	a5, a6 := key("ten1", 5), key("ten1", 6)
	c.put(a5, res("ten1"))
	c.put(a6, res("ten1"))
	want("after a purge the survivors keep their order", 3, []CacheKey{a4, a5, a6}, []CacheKey{a3})

	clk.Advance(40 * time.Second)
	c.put(a6, res("ten1"))
	clk.Advance(30 * time.Second)
	want("an expired entry is dropped on read; a re-put entry lives on", 1, []CacheKey{a6}, []CacheKey{a4, a5})
	a7, a8, a9 := key("ten2", 7), key("ten2", 8), key("ten2", 9)
	c.put(a7, res("ten2"))
	c.put(a8, res("ten2"))
	c.put(a9, res("ten2"))
	want("the freed slots refill in order", 3, []CacheKey{a7, a8, a9}, []CacheKey{a6})
}

// ---- audit -----------------------------------------------------------------

func TestAuditEventsForInvocationAcceptanceAndFallback(t *testing.T) {
	// Accepted: invoked, accepted, then the final action once resolved.
	e := newEnv(t)
	adv := e.score(e.request("ten1"))
	if _, err := e.eng.Resolve(context.Background(), testScope("ten1"), adv); err != nil {
		t.Fatal(err)
	}
	want := []string{"decision.invoked", "decision.accepted", "decision.final_action"}
	if got := e.rec.types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if string(adv.Result().AuditRef) != e.rec.ids[1] {
		t.Fatalf("audit_ref %q is not the accepted event %q", adv.Result().AuditRef, e.rec.ids[1])
	}

	// Fallback after invocation: invoked, fallback.
	e = newEnv(t)
	e.sc.Script(fakescorer.Step{Fail: fakescorer.FailUnavailable})
	adv = e.score(e.request("ten1"))
	if got := e.rec.types(); !reflect.DeepEqual(got, []string{"decision.invoked", "decision.fallback"}) {
		t.Fatalf("events = %v", got)
	}
	if string(adv.Result().AuditRef) != e.rec.ids[1] {
		t.Fatal("fallback audit_ref is not the fallback event")
	}

	// Rejected before any call: only the fallback.
	e = newEnv(t)
	req := e.request("ten1")
	req.CandidateSet.Candidates = nil
	e.score(req)
	if got := e.rec.types(); !reflect.DeepEqual(got, []string{"decision.fallback"}) {
		t.Fatalf("events = %v", got)
	}

	// Payloads carry the registered required keys and no content.
	e = newEnv(t)
	adv = e.score(e.request("ten1"))
	e.eng.Resolve(context.Background(), testScope("ten1"), adv)
	for _, ev := range e.rec.events {
		spec, ok := audit.DefaultRegistry.Lookup(ev.Type)
		if !ok {
			t.Fatalf("%s is not registered with internal/audit", ev.Type)
		}
		raw, _ := json.Marshal(ev.Data)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		for _, k := range spec.Required {
			if _, ok := m[k]; !ok {
				t.Errorf("%s lacks required key %q", ev.Type, k)
			}
		}
		for _, secret := range []string{"label c1", "pick", "utterance"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s leaks content %q: %s", ev.Type, secret, raw)
			}
		}
	}

	// An invocation that cannot be recorded is never made.
	e = newEnv(t)
	e.rec.failType = "decision.invoked"
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonUnavailable)
	if e.sc.Calls() != 0 {
		t.Fatal("the provider was called without an audit record")
	}
	// An acceptance that cannot be recorded is not used.
	e = newEnv(t)
	e.rec.failType = "decision.accepted"
	wantFallback(t, e.score(e.request("ten1")), gen.FallbackReasonUnavailable)
}

func TestDisabledProviderIsNeverCalled(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Enabled = false })
	adv := e.score(e.request("ten1"))
	wantFallback(t, adv, gen.FallbackReasonDisabled)
	if e.sc.Calls() != 0 || e.res.exposedCalls != 0 {
		t.Fatalf("a disabled layer touched the provider (%d) or resolver (%d)", e.sc.Calls(), e.res.exposedCalls)
	}

	// The zero Config is off.
	if (Config{}).Enabled {
		t.Fatal("the zero Config is enabled")
	}
	if DefaultConfig().Enabled {
		t.Fatal("DefaultConfig is enabled")
	}

	// No provider at all is unavailable, not a panic.
	eng, err := New(Deps{Clock: e.clk, Resolver: e.res, Audit: e.rec, Config: testConfig()})
	if err != nil {
		t.Fatal(err)
	}
	adv, err = eng.Score(context.Background(), testScope("ten1"), e.request("ten1"))
	if err != nil {
		t.Fatal(err)
	}
	wantFallback(t, adv, gen.FallbackReasonUnavailable)

	// An unrecognised decision type cannot be answered at all: an error, not an Advice.
	bogus := e.request("ten1")
	bogus.DecisionType = "bogus"
	if _, err := e.eng.Score(context.Background(), testScope("ten1"), bogus); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("unknown decision type: %v", err)
	}

	// A missing scope is an error, never a default.
	if _, err := e.eng.Score(context.Background(), contracts.Scope{}, e.request("ten1")); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Fatalf("zero scope: %v", err)
	}
}

// ---- registry --------------------------------------------------------------

func TestScorerOccupiesTheRouterRole(t *testing.T) {
	clk := clock.NewFake(t0)
	reg := providers.NewRegistry(clk)
	if p, reason := ProviderFromRegistry(reg); p != nil || reason != gen.FallbackReasonUnavailable {
		t.Fatalf("empty registry: %v %q", p, reason)
	}
	sc := fakescorer.New(fakescorer.DefaultManifest(), nil)
	if err := RegisterRouter(reg, sc); err != nil {
		t.Fatal(err)
	}
	p, reason := ProviderFromRegistry(reg)
	if p == nil || reason != "" || p.Manifest().ModelID != "fake-model" {
		t.Fatalf("lookup: %v %q", p, reason)
	}
	// The slot is a TextGenerator for the registry's sake and never generates.
	tg, err := reg.TextGenerator(providers.RoleRouter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.Chat(context.Background(), testScope("ten1"), providers.ChatRequest{Role: providers.RoleRouter}); err == nil {
		t.Fatal("the scorer slot generated text")
	}
	// Other roles are untouched.
	if reg.Has(providers.RoleController) || reg.Has(providers.RoleEmbedder) {
		t.Fatal("registering the scorer registered another role")
	}
	if err := reg.SetHealth(providers.RoleRouter, providers.HealthUnavailable, "down"); err != nil {
		t.Fatal(err)
	}
	if p, reason := ProviderFromRegistry(reg); p != nil || reason != gen.FallbackReasonUnavailable {
		t.Fatalf("unavailable role: %v %q", p, reason)
	}

	// A plain text model in the router role is not a scorer.
	reg = providers.NewRegistry(clk)
	if err := reg.Register(providers.RoleRouter, fake.New()); err != nil {
		t.Fatal(err)
	}
	if p, reason := ProviderFromRegistry(reg); p != nil || reason != gen.FallbackReasonUnsupportedRuntime {
		t.Fatalf("text router: %v %q", p, reason)
	}
}
