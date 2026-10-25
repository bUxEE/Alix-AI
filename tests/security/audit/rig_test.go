package auditsec

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The action rig of SEC-087: the real action coordinator over the real audit log and the real
// policy gateway, with the in-memory host (internal/adapters/fake) behind adapters.Wrap as the
// application, wired the way the composition root wires them. Two things are added around the
// production parts, and neither changes what they do: the audit appender is wrapped in one that
// can be told to fail, and the host's adapter reports every Commit it receives (and what the
// audit log held at that moment) to the test.

const (
	appID     = "demo-app"
	tenantA   = "tenant-a"
	workspace = "ws-1"
	capRead   = fake.ReadCapability
	capCreate = fake.ActionCapability
)

// level says how items.create is declared: the effect class fixes the platform risk floor, which
// decides whether a confirmation is needed.
type level struct {
	effect  policy.EffectClass
	risk    contracts.RiskLevel
	confirm policy.ConfirmationMode
}

var (
	// level1: reversible, no confirmation (prepared -> committing).
	level1 = level{policy.EffectReversible, contracts.RiskReversible, policy.ConfirmNone}
	// level2: external effect, explicit confirmation (prepared -> confirmed -> committing).
	level2 = level{policy.EffectExternal, contracts.RiskConsequential, policy.ConfirmExplicit}
)

const hostPolicy = `{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

func buildPackage(t testing.TB, lv level) *policy.Package {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(hostPolicy))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID,
		Version:       "1.0.0",
		Status:        policy.StatusActive,
		Roles:         []string{"member", "visitor", "staff"},
		Capabilities: []policy.CapabilityDecl{
			{ID: capRead, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
			{ID: capCreate, Audience: policy.AudienceUser, EffectClass: lv.effect, RiskLevel: lv.risk,
				Confirmation: lv.confirm, Fields: []policy.FieldDecl{{Name: "name", Required: true}}},
		},
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("policy.NewPackage: %v", err)
	}
	return p
}

// scopeOf is a valid scope of a principal and session in tenant-a / ws-1.
func scopeOf(role, principal, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep-1",
		ApplicationID:    appID,
		TenantID:         tenantA,
		WorkspaceID:      workspace,
		PrincipalID:      principal,
		Roles:            []string{role},
		CapabilityScopes: []string{capRead, capCreate},
		SessionID:        session,
	}
}

var member = scopeOf("member", "user-1", "ses-1")

// gatedAppender is an audit.Appender in front of the real audit log that records every append
// it is asked for and fails the ones of the types it was told to fail, before anything is
// written: the same effect as a full disk or a locked table for that one event.
//
// It can also be sabotaged, for the controls of the tests: it can swallow the events of a type
// (report success and write nothing), or hold them back and write them later, to show that the
// checks of SEC-087 see an audit record that is missing or that comes too late.
type gatedAppender struct {
	inner audit.Appender

	mu       sync.Mutex
	failing  map[audit.EventType]bool
	swallow  map[audit.EventType]bool
	late     map[audit.EventType]bool
	held     []heldEvent
	attempts []audit.EventType
	failed   []audit.EventType
}

type heldEvent struct {
	loc audit.Locator
	ev  audit.NewEvent
}

func newGated(inner audit.Appender) *gatedAppender {
	return &gatedAppender{inner: inner, failing: map[audit.EventType]bool{}, swallow: map[audit.EventType]bool{}, late: map[audit.EventType]bool{}}
}

func (g *gatedAppender) fail(t audit.EventType, on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if on {
		g.failing[t] = true
	} else {
		delete(g.failing, t)
	}
}

// sabotage makes the appender swallow (mode "swallow") or hold back (mode "late") the events of t.
func (g *gatedAppender) sabotage(mode string, t audit.EventType) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch mode {
	case "swallow":
		g.swallow[t] = true
	case "late":
		g.late[t] = true
	default:
		panic("unknown sabotage " + mode)
	}
}

// flush writes the held-back events, now.
func (g *gatedAppender) flush(ctx context.Context) error {
	g.mu.Lock()
	held := g.held
	g.held = nil
	g.mu.Unlock()
	for _, h := range held {
		if _, err := g.inner.AppendAt(ctx, h.loc, h.ev); err != nil {
			return err
		}
	}
	return nil
}

// gate decides what happens to an append: err refuses it, skip pretends it happened.
func (g *gatedAppender) gate(loc audit.Locator, ev audit.NewEvent) (skip bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempts = append(g.attempts, ev.Type)
	switch {
	case g.failing[ev.Type]:
		g.failed = append(g.failed, ev.Type)
		return false, contracts.New(contracts.ErrUnavailable, "the audit store is down")
	case g.swallow[ev.Type]:
		return true, nil
	case g.late[ev.Type]:
		g.held = append(g.held, heldEvent{loc: loc, ev: ev})
		return true, nil
	}
	return false, nil
}

func (g *gatedAppender) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	if skip, err := g.gate(loc, ev); err != nil || skip {
		return audit.Event{}, err
	}
	return g.inner.AppendAt(ctx, loc, ev)
}

func (g *gatedAppender) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	if skip, err := g.gate(loc, ev); err != nil || skip {
		return audit.Event{}, err
	}
	return g.inner.AppendTx(ctx, tx, loc, ev)
}

var _ audit.Appender = (*gatedAppender)(nil)

// hostSeam wraps the host's adapter, inside adapters.Wrap, to report each Commit to the test with
// what the audit log held when it arrived.
type hostSeam struct {
	adapters.AppAdapter
	onCommit func(adapters.CommitRequest)
}

func (h *hostSeam) Commit(ctx context.Context, sc contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	if h.onCommit != nil {
		h.onCommit(req)
	}
	return h.AppAdapter.Commit(ctx, sc, req)
}

// decisionRecorder keeps every decision the policy gateway made, beside the audit log's own
// record of it.
type decisionRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (r *decisionRecorder) Emit(_ context.Context, rec policy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

func (r *decisionRecorder) all() []policy.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]policy.Record(nil), r.recs...)
}

// teeSink sends a decision record to several sinks and fails as soon as one fails, which is
// how the gateway's Sink contract reads: a decision that cannot be recorded is not allowed.
type teeSink []policy.Sink

func (t teeSink) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// commitSeen is what the adapter's Commit saw when it was called.
type commitSeen struct {
	key string
	// state is the preparation's stored state at that moment.
	state act.PreparedActionState
	// events are the types of the tenant chain's events at that moment, in order.
	events []audit.EventType
}

type actionRig struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	log   *audit.Log
	gated *gatedAppender
	app   *fake.App
	recs  *decisionRecorder
	co    *actions.Coordinator

	mu      sync.Mutex
	commits []commitSeen
	pkg     *policy.Package
}

func newActionRig(t *testing.T, lv level) *actionRig {
	t.Helper()
	r := &actionRig{t: t, clk: clock.NewFake(clockStart), recs: &decisionRecorder{}, pkg: buildPackage(t, lv)}
	r.db = storetest.Open(t, nil)
	var err error
	if r.log, err = audit.New(r.db, audit.Options{Clock: r.clk}); err != nil {
		t.Fatal(err)
	}
	r.gated = newGated(r.log)
	// Policy decisions are recorded through the same gated appender, as the composition root does.
	sink := audit.NewRecordSink[policy.Record](r.gated, audit.EventPolicyDecision, func(rec policy.Record) (audit.Locator, error) {
		s := rec.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(r.clk, policy.WithSink(teeSink{sink, r.recs}))
	r.app = fake.NewApp(fake.WithClock(r.clk))
	seam := &hostSeam{AppAdapter: fake.NewAdapter(r.app), onCommit: r.noteCommit}
	guard, err := adapters.Wrap(seam, adapters.Options{ApplicationID: appID})
	if err != nil {
		t.Fatal(err)
	}
	r.co, err = actions.New(actions.Deps{
		DB: r.db, Policy: gw, Audit: r.gated, Clock: r.clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return r.pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// noteCommit runs inside the host's Commit.
func (r *actionRig) noteCommit(req adapters.CommitRequest) {
	seen := commitSeen{key: string(req.IdempotencyKey), events: r.chainTypes()}
	if pa, err := r.co.Get(context.Background(), member, string(req.Prepared.PreparationID)); err == nil {
		seen.state = pa.State
	}
	r.mu.Lock()
	r.commits = append(r.commits, seen)
	r.mu.Unlock()
}

func (r *actionRig) commitCalls() []commitSeen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]commitSeen(nil), r.commits...)
}

// chainTypes lists the event types of the tenant's audit chain, in chain order, as the database
// holds them now.
func (r *actionRig) chainTypes() []audit.EventType {
	var out []audit.EventType
	for _, e := range r.chain() {
		out = append(out, e.Type)
	}
	return out
}

// chain reads the tenant's events from the store, in chain order.
func (r *actionRig) chain() []audit.Event {
	rows, err := r.db.QueryContext(context.Background(),
		`SELECT event_type, data FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`, appID, tenantA)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []audit.Event
	for rows.Next() {
		var typ, data string
		if err := rows.Scan(&typ, &data); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, audit.Event{Type: audit.EventType(typ), Data: json.RawMessage(data)})
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// actionEvents are the action.* events of one preparation, in chain order.
func (r *actionRig) actionEvents(preparationID string) []audit.EventType {
	var out []audit.EventType
	for _, e := range r.chain() {
		var d struct {
			PreparationID string `json:"preparation_id"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.PreparationID == preparationID && strings.HasPrefix(string(e.Type), "action.") {
			out = append(out, e.Type)
		}
	}
	return out
}

// decisionEvents are the policy.decision events of the chain, decoded to phase and outcome.
func (r *actionRig) decisionEvents() []decision {
	var out []decision
	for _, e := range r.chain() {
		if e.Type != audit.EventPolicyDecision {
			continue
		}
		var d decision
		if err := json.Unmarshal(e.Data, &d); err != nil {
			r.t.Fatalf("a policy decision event does not decode: %v", err)
		}
		out = append(out, d)
	}
	return out
}

type decision struct {
	Phase        policy.Phase   `json:"phase"`
	Outcome      policy.Outcome `json:"outcome"`
	CapabilityID string         `json:"capability_id"`
	DecisionID   string         `json:"decision_id"`
}

func (r *actionRig) prepare(sc contracts.Scope, name string) (actions.Prepared, error) {
	return r.co.Prepare(context.Background(), sc, actions.PrepareInput{
		Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": name},
	})
}

func (r *actionRig) mustPrepare(sc contracts.Scope, name string) actions.Prepared {
	r.t.Helper()
	p, err := r.prepare(sc, name)
	if err != nil {
		r.t.Fatalf("Prepare: %v", err)
	}
	return p
}

func (r *actionRig) confirm(sc contracts.Scope, p actions.Prepared) error {
	_, err := r.co.Confirm(context.Background(), sc, actions.ConfirmInput{PreparationID: string(p.Action.PreparationID), Token: p.Confirmation})
	return err
}

func (r *actionRig) mustConfirm(sc contracts.Scope, p actions.Prepared) {
	r.t.Helper()
	if err := r.confirm(sc, p); err != nil {
		r.t.Fatalf("Confirm: %v", err)
	}
}

func (r *actionRig) commit(sc contracts.Scope, p actions.Prepared) (contracts.ActionReceipt, error) {
	return r.co.Commit(context.Background(), sc, string(p.Action.PreparationID))
}

// state is the preparation's stored state, read through the coordinator.
func (r *actionRig) state(sc contracts.Scope, p actions.Prepared) act.PreparedActionState {
	r.t.Helper()
	pa, err := r.co.Get(context.Background(), sc, string(p.Action.PreparationID))
	if err != nil {
		r.t.Fatalf("Get: %v", err)
	}
	return pa.State
}

func (r *actionRig) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range r.app.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}
