package actions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

// Identifiers of the synthetic application the tests run against. The
// capabilities are the two the in-memory host (internal/adapters/fake) offers.
const (
	appID     = "demo-app"
	tenantA   = "tenant-a"
	tenantB   = "tenant-b"
	ws1       = "ws-1"
	ws2       = "ws-2"
	capRead   = fake.ReadCapability
	capCreate = fake.ActionCapability
)

// level describes how the package declares items.create: the effect class
// fixes the platform risk floor.
type level struct {
	effect  policy.EffectClass
	risk    contracts.RiskLevel
	confirm policy.ConfirmationMode
}

var (
	level1 = level{policy.EffectReversible, contracts.RiskReversible, policy.ConfirmNone}
	level2 = level{policy.EffectExternal, contracts.RiskConsequential, policy.ConfirmExplicit}
	level3 = level{policy.EffectPrivileged, contracts.RiskPrivileged, policy.ConfirmStepUp}
)

const policyJSON = `{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

// newPackage builds the demo package with items.create declared at lv.
func newPackage(t testing.TB, lv level, mods ...func(*policy.PackageSpec)) *policy.Package {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(policyJSON))
	if err != nil {
		t.Fatal(err)
	}
	spec := policy.PackageSpec{
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
	}
	for _, m := range mods {
		m(&spec)
	}
	p, err := policy.NewPackage(spec)
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	return p
}

// scopeOf returns a valid scope for a principal and session in tenant-a/ws-1.
func scopeOf(principal, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep-1",
		ApplicationID:    appID,
		TenantID:         tenantA,
		WorkspaceID:      ws1,
		PrincipalID:      principal,
		Roles:            []string{"member"},
		CapabilityScopes: []string{capRead, capCreate},
		SessionID:        session,
	}
}

// ---- a scriptable adapter ----

// scripted is an adapter that delegates to the fake host and lets a test
// intercept one operation: to bring the host down at the right moment, to bump
// a data version between two calls, to forge a reply or to stop the process. It
// sits INSIDE adapters.Wrap, so the wrapper still re-validates what it returns,
// as it does for a real transport.
type scripted struct {
	adapters.AppAdapter

	mu             sync.Mutex
	prepareFields  []map[string]any
	onPrepare      func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	onValidate     func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	onCommit       func(req adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	onStatus       func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	commitRequests []adapters.CommitRequest
}

func (s *scripted) Prepare(ctx context.Context, sc contracts.Scope, req adapters.PrepareRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	cp := make(map[string]any, len(req.Fields))
	for k, v := range req.Fields {
		cp[k] = v
	}
	s.prepareFields = append(s.prepareFields, cp)
	hook := s.onPrepare
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Prepare(ctx, sc, req) }
	if hook != nil {
		return hook(next)
	}
	return next()
}

func (s *scripted) Validate(ctx context.Context, sc contracts.Scope, req adapters.ValidateRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	hook := s.onValidate
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Validate(ctx, sc, req) }
	if hook != nil {
		return hook(next)
	}
	return next()
}

func (s *scripted) Commit(ctx context.Context, sc contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	s.commitRequests = append(s.commitRequests, req)
	hook := s.onCommit
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Commit(ctx, sc, req) }
	if hook != nil {
		return hook(req, next)
	}
	return next()
}

func (s *scripted) Status(ctx context.Context, sc contracts.Scope, req adapters.StatusRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	hook := s.onStatus
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Status(ctx, sc, req) }
	if hook != nil {
		return hook(next)
	}
	return next()
}

func (s *scripted) set(f func(*scripted)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// ---- the environment ----

// policyRecorder keeps every decision the gateway made.
type policyRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (r *policyRecorder) Emit(_ context.Context, rec policy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

func (r *policyRecorder) phases() []policy.Phase {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]policy.Phase, 0, len(r.recs))
	for _, rec := range r.recs {
		out = append(out, rec.Phase)
	}
	return out
}

func (r *policyRecorder) last() policy.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recs[len(r.recs)-1]
}

// tee sends a decision record to several sinks and fails if any does.
type tee []policy.Sink

func (t tee) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

type env struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	alog  *audit.Log
	app   *fake.App
	gw    *policy.Gateway
	recs  *policyRecorder
	sc    *scripted
	co    *Coordinator
	guard adapters.AppAdapter

	mu  sync.Mutex
	pkg policy.ActivePackage
}

type envOpts struct {
	lv     level
	dbPath string
	app    *fake.App
	cfg    Config
}

type envOpt func(*envOpts)

func withLevel(lv level) envOpt  { return func(o *envOpts) { o.lv = lv } }
func withDBPath(p string) envOpt { return func(o *envOpts) { o.dbPath = p } }
func withApp(a *fake.App) envOpt { return func(o *envOpts) { o.app = a } }
func withConfig(c Config) envOpt { return func(o *envOpts) { o.cfg = c } }

// newEnv wires a coordinator to a throwaway store, the real audit log and
// policy gateway, and the in-memory host behind adapters.Wrap, exactly as the
// composition root wires them.
func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	o := envOpts{lv: level1}
	for _, f := range opts {
		f(&o)
	}
	e := &env{t: t, clk: clock.NewFake(t0), recs: &policyRecorder{}}
	if o.dbPath == "" {
		e.db = storetest.Open(t, nil)
	} else {
		e.db = openDB(t, o.dbPath)
	}
	var err error
	if e.alog, err = audit.New(e.db, audit.Options{Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](e.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	e.gw = policy.NewGateway(e.clk, policy.WithSink(tee{sink, e.recs}))
	e.app = o.app
	if e.app == nil {
		e.app = fake.NewApp(fake.WithClock(e.clk))
	}
	e.sc = &scripted{AppAdapter: fake.NewAdapter(e.app)}
	if e.guard, err = adapters.Wrap(e.sc, adapters.Options{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	e.pkg = newPackage(t, o.lv)
	e.co, err = New(Deps{
		DB:     e.db,
		Policy: e.gw,
		Audit:  e.alog,
		Packages: PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.pkg, nil
		}),
		Adapters: AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return e.guard, nil }),
		Clock:    e.clk,
		Config:   o.cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) setPackage(p policy.ActivePackage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pkg = p
}

// openDB opens a database file and closes it with the test.
func openDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// restart builds a second coordinator, as a new process would, over the same
// database file and the same host. The previous database handle is closed.
func (e *env) restart(path string) *env {
	e.t.Helper()
	if err := e.db.Close(); err != nil {
		e.t.Fatal(err)
	}
	return newEnv(e.t, withDBPath(path), withApp(e.app), withLevel(levelOf(e.pkg)))
}

// levelOf reads back how items.create is declared in a package.
func levelOf(p policy.ActivePackage) level {
	d, _ := p.Capability(capCreate)
	return level{d.EffectClass, d.RiskLevel, d.Confirmation}
}

func dbFile(t *testing.T) string { return filepath.Join(t.TempDir(), "actions.db") }

// ---- calls and rows ----

// hostOps lists the operations the host received, in order.
func (e *env) hostOps() []adapters.Operation {
	var ops []adapters.Operation
	for _, c := range e.app.Calls() {
		ops = append(ops, c.Operation)
	}
	return ops
}

func (e *env) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range e.app.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.db.ExecContext(context.Background(), query, args...); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
}

// states returns the stored state of a preparation, read around the coordinator.
func (e *env) state(id string) act.PreparedActionState {
	e.t.Helper()
	var s string
	if err := e.db.QueryRowContext(context.Background(), `SELECT state FROM action_preparations WHERE id = ?`, id).Scan(&s); err != nil {
		e.t.Fatalf("state of %s: %v", id, err)
	}
	return act.PreparedActionState(s)
}

// events lists the action.* audit events of the tenant chain, in order.
func (e *env) events() []string {
	e.t.Helper()
	rows, err := e.db.QueryContext(context.Background(),
		`SELECT event_type FROM audit_events WHERE application_id = ? AND tenant_id = ? AND event_type LIKE 'action.%' ORDER BY seq`, appID, tenantA)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (e *env) eventCount(eventType string) int {
	e.t.Helper()
	return e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, eventType)
}

// ---- steps ----

func ctxb() context.Context { return context.Background() }

// prepare prepares an items.create action and fails the test on error.
func (e *env) prepare(sc contracts.Scope, name string) Prepared {
	e.t.Helper()
	p, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": name}})
	if err != nil {
		e.t.Fatalf("Prepare: %v", err)
	}
	return p
}

// confirm confirms a preparation with its own token.
func (e *env) confirm(sc contracts.Scope, p Prepared) {
	e.t.Helper()
	if _, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: string(p.Action.PreparationID), Token: p.Confirmation}); err != nil {
		e.t.Fatalf("Confirm: %v", err)
	}
}

func id(p Prepared) string { return string(p.Action.PreparationID) }

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

func noErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---- schema validation of what the coordinator produces ----

var schemaRoot = filepath.Join("..", "..", "schemas")

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
)

func compiler(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions"} {
			files, err := filepath.Glob(filepath.Join(schemaRoot, area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				schemaErr = err
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					schemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
				if err != nil {
					schemaErr = err
					return
				}
				if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
					schemaErr = err
					return
				}
			}
		}
		schemaComp = c
	})
	if schemaErr != nil || schemaComp == nil {
		t.Fatalf("schemas unavailable: %v", schemaErr)
	}
	return schemaComp
}

// validateAgainst checks v, as JSON, against the published schema id.
func validateAgainst(t testing.TB, schemaID string, v any) {
	t.Helper()
	s, err := compiler(t).Compile(schemaID)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaID, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s rejects %s: %v", schemaID, b, err)
	}
}

const (
	schemaPrepared = "urn:alixai:schema:actions:prepared-action:v1"
	schemaReceipt  = "urn:alixai:schema:actions:action-receipt:v1"
)

func idValid(s, prefix string) bool { return ids.Valid(s, prefix) }

func capCreateID() *common.CapabilityID {
	c := common.CapabilityID(capCreate)
	return &c
}

const time10m = 10 * time.Minute

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
