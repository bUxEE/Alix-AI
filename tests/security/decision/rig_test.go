package decisionsec

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

var bg = context.Background()

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

const tsLayout = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) common.Timestamp { return common.Timestamp(t.UTC().Format(tsLayout)) }

// ---- the scorer: a scripted fake that remembers everything it was sent ----

// captureScorer wraps the scripted fake scorer and keeps EVERY request and scope it receives (the fake keeps
// only the last request). It is the "prompt/request capture" of the security suite.
type captureScorer struct {
	*fakescorer.Scorer
	mu     sync.Mutex
	reqs   []gen.DecisionRequest
	scopes []contracts.Scope
	crash  bool // the next call panics, as a crashed in-process scorer would
	// crashWith, when set, is the panic value of the next call (instead of the default text).
	crashWith string
}

func newCapture(clk *clock.Fake, steps ...fakescorer.Step) *captureScorer {
	return &captureScorer{Scorer: fakescorer.New(fakescorer.DefaultManifest(), clk).Script(steps...)}
}

func (c *captureScorer) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.scopes = append(c.scopes, scope)
	crash, with := c.crash || c.crashWith != "", c.crashWith
	c.crash, c.crashWith = false, ""
	c.mu.Unlock()
	if crash {
		if with == "" {
			with = "decisionsec: the scorer crashed"
		}
		panic(with)
	}
	return c.Scorer.ScoreDecision(ctx, scope, req)
}

// requests returns the JSON of every request the scorer received, as a wire adapter would send it.
func (c *captureScorer) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.reqs))
	for i, r := range c.reqs {
		b, err := json.Marshal(r)
		if err != nil {
			panic(err)
		}
		out[i] = string(b)
	}
	return out
}

func (c *captureScorer) received() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// ---- audit: the real hash-chained log behind a tee that keeps what was appended ----

type recordedEvent struct {
	Type   string
	Tenant string
	JSON   string
}

type auditTee struct {
	log *audit.Log

	mu     sync.Mutex
	events []recordedEvent
	fail   map[audit.EventType]bool // appends of these types fail; "*" fails every append
}

func (a *auditTee) failing(t audit.EventType) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fail["*"] || a.fail[t]
}

func (a *auditTee) failOn(types ...audit.EventType) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = map[audit.EventType]bool{}
	for _, t := range types {
		a.fail[t] = true
	}
}

func (a *auditTee) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	if a.failing(ev.Type) {
		return audit.Event{}, contracts.New(contracts.ErrUnavailable, "decisionsec: audit append failed on purpose")
	}
	e, err := a.log.AppendAt(ctx, loc, ev)
	if err != nil {
		return e, err
	}
	raw, merr := json.Marshal(ev.Data)
	if merr != nil {
		return e, merr
	}
	a.mu.Lock()
	a.events = append(a.events, recordedEvent{Type: string(ev.Type), Tenant: loc.Chain().TenantID, JSON: string(raw)})
	a.mu.Unlock()
	return e, nil
}

func (a *auditTee) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	return a.log.AppendTx(ctx, tx, loc, ev)
}

func (a *auditTee) all() []recordedEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recordedEvent(nil), a.events...)
}

func (a *auditTee) types() []string {
	var out []string
	for _, e := range a.all() {
		out = append(out, e.Type)
	}
	return out
}

func (a *auditTee) count(typ string) int {
	n := 0
	for _, e := range a.all() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func (a *auditTee) ofType(typ string) []recordedEvent {
	var out []recordedEvent
	for _, e := range a.all() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func newAuditTee(t testing.TB, clk clock.Clock) *auditTee {
	t.Helper()
	log, err := audit.New(storetest.Open(t, nil), audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	return &auditTee{log: log}
}

// ---- the deterministic side: a catalogue that knows who owns what ----

// catalog is the CandidateResolver of the engine rig. A candidate belongs to the tenants that own it; an id
// nobody owns is unauthorised. It can revoke an id, mark one stale, or resolve one to a different id.
type catalog struct {
	mu        sync.Mutex
	owners    map[string]map[string]bool // candidate id -> tenants that may see it
	revoked   map[string]bool
	stale     map[string]bool
	resolveAs map[string]string
	exposed   []string
	resolved  []string
}

func newCatalog() *catalog {
	return &catalog{owners: map[string]map[string]bool{}, revoked: map[string]bool{}, stale: map[string]bool{}, resolveAs: map[string]string{}}
}

func (c *catalog) own(tenant string, ids ...string) *catalog {
	for _, id := range ids {
		if c.owners[id] == nil {
			c.owners[id] = map[string]bool{}
		}
		c.owners[id][tenant] = true
	}
	return c
}

func (c *catalog) check(scope contracts.Scope, id string) error {
	switch {
	case len(c.owners[id]) == 0:
		return contracts.New(contracts.ErrPermissionDenied, "decisionsec: nobody owns this candidate")
	case !c.owners[id][scope.TenantID]:
		return contracts.New(contracts.ErrScopeViolation, "decisionsec: the candidate belongs to another tenant")
	case c.revoked[id]:
		return contracts.New(contracts.ErrPermissionDenied, "decisionsec: the candidate was revoked")
	}
	return nil
}

func (c *catalog) Exposed(_ context.Context, scope contracts.Scope, cand gen.CandidateSetCandidate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := string(cand.CandidateID)
	c.exposed = append(c.exposed, id)
	if err := c.check(scope, id); err != nil {
		return err
	}
	if c.stale[id] {
		return contracts.New(contracts.ErrStale, "decisionsec: the candidate data changed")
	}
	return nil
}

func (c *catalog) Resolve(_ context.Context, scope contracts.Scope, id string) (decision.Resolved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolved = append(c.resolved, id)
	if err := c.check(scope, id); err != nil {
		return decision.Resolved{}, err
	}
	if as, ok := c.resolveAs[id]; ok {
		return decision.Resolved{ID: as, Kind: "item", Version: "v1"}, nil
	}
	return decision.Resolved{ID: id, Kind: "item", Version: "v1"}, nil
}

func (c *catalog) resolveCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.resolved...)
}

func (c *catalog) revoke(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked[id] = true
}

// ---- the rig ----

type rig struct {
	t   *testing.T
	clk *clock.Fake
	sc  *captureScorer
	cat *catalog
	aud *auditTee
	eng *decision.Engine
	cfg decision.Config
}

// secConfig is the engine policy under test: on, calibrated for the fake scorer, cache on.
func secConfig() decision.Config {
	c := decision.DefaultConfig()
	c.Enabled = true
	c.SupportedTypes = []gen.DecisionType{gen.DecisionTypeItemSelection, gen.DecisionTypeActionSelection, gen.DecisionTypeCapabilityRouting}
	c.Calibration = []decision.Calibration{{ModelID: "fake-model", ModelRevision: "rev1", ContractVersion: "cv1", ECE: 0.02, MaxECE: 0.05}}
	return c
}

// newRig builds an engine over the real audit log. Tenant ten1 owns c1..c3 and tenant ten2 owns d1..d3.
func newRig(t *testing.T, mods ...func(*decision.Config)) *rig {
	t.Helper()
	r := &rig{t: t, clk: clock.NewFake(t0), cat: newCatalog(), cfg: secConfig()}
	r.cat.own("ten1", "c1", "c2", "c3").own("ten2", "d1", "d2", "d3")
	for _, m := range mods {
		m(&r.cfg)
	}
	r.sc = newCapture(r.clk)
	r.aud = newAuditTee(t, r.clk)
	eng, err := decision.New(decision.Deps{Clock: r.clk, Provider: r.sc, Resolver: r.cat, Audit: r.aud, Config: r.cfg})
	if err != nil {
		t.Fatal(err)
	}
	r.eng = eng
	return r
}

func scopeOf(tenant string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app-sec", TenantID: tenant, WorkspaceID: "ws-1", PrincipalID: "pri1",
		Roles: []string{"staff"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses1",
	}
}

// request is a valid bounded request of one tenant over its candidates.
func (r *rig) request(tenant string, ids ...string) gen.DecisionRequest {
	if len(ids) == 0 {
		prefix := map[string]string{"ten1": "c", "ten2": "d"}[tenant]
		ids = []string{prefix + "1", prefix + "2", prefix + "3"}
	}
	cands := make([]gen.CandidateSetCandidate, len(ids))
	for i, id := range ids {
		cands[i] = gen.CandidateSetCandidate{CandidateID: common.Identifier(id), Label: "label " + id}
	}
	return gen.DecisionRequest{
		TenantID: common.Identifier(tenant), ApplicationID: "app-sec", SessionID: "ses1", ContractVersion: "cv1",
		DecisionType: gen.DecisionTypeItemSelection, SchemaVersion: 1,
		BoundedState:       map[string]gen.BoundedScalar{"step": "pick", "n": 3},
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet:       gen.CandidateSet{Candidates: cands, MaxSelections: 1, BuiltAt: ts(r.clk.Now())},
		Locale:             "en", Deadline: ts(r.clk.Now().Add(time.Second)),
	}
}

func (r *rig) score(scope contracts.Scope, req gen.DecisionRequest) decision.Advice {
	r.t.Helper()
	adv, err := r.eng.Score(bg, scope, req)
	if err != nil {
		r.t.Fatal(err)
	}
	return adv
}

func (r *rig) resolve(scope contracts.Scope, adv decision.Advice) decision.Resolution {
	r.t.Helper()
	res, err := r.eng.Resolve(bg, scope, adv)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func wantFallback(t testing.TB, adv decision.Advice, reason gen.FallbackReason) {
	t.Helper()
	got, ok := adv.FallbackReason()
	if adv.Accepted() || !ok || got != reason {
		t.Fatalf("want fallback %q, got accepted=%v reason=%q", reason, adv.Accepted(), got)
	}
	if len(adv.Suggestions()) != 0 || len(adv.Result().Selections) != 0 {
		t.Fatalf("a fallback must carry no selection: %+v", adv.Result().Selections)
	}
}

func wantAccepted(t testing.TB, adv decision.Advice) {
	t.Helper()
	if !adv.Accepted() {
		reason, _ := adv.FallbackReason()
		t.Fatalf("want an accepted advice, got fallback %q (reject code %q)", reason, adv.RejectCode())
	}
}

func pickStep(id string, conf float64) fakescorer.Step {
	return fakescorer.Step{Picks: []fakescorer.Pick{{ID: id, Score: 0.9, Confidence: conf}}}
}

func mustNotContain(t testing.TB, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s contains %q", what, needle)
	}
}

func mustContain(t testing.TB, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s does not contain %q (the control that proves the check could see it)", what, needle)
	}
}
