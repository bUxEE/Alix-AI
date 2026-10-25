package actionssec

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The synthetic application of the suite: the demo package the in-memory host
// (internal/adapters/fake) implements, with items.create declared at level 2, so
// a commit needs an explicit confirmation first.
const (
	appID   = "demo-app"
	tenantA = "tenant-a"
	ws1     = "ws-1"

	capRead   = fake.ReadCapability
	capCreate = fake.ActionCapability
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

const policyJSON = `{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

// newPackage builds the demo package, items.create at level 2 (an external
// effect: explicit confirmation).
func newPackage(t testing.TB) *policy.Package {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(policyJSON))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member", "visitor", "staff"},
		Capabilities: []policy.CapabilityDecl{
			{ID: capRead, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
			{ID: capCreate, Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential,
				Confirmation: policy.ConfirmExplicit, Fields: []policy.FieldDecl{{Name: "name", Required: true}}},
		},
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	return p
}

// scope is the verified context of the user in tenant-a/ws-1.
func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: appID, TenantID: tenantA, WorkspaceID: ws1,
		PrincipalID: "user-1", Roles: []string{"member"}, CapabilityScopes: []string{capRead, capCreate}, SessionID: "ses-1",
	}
}

func ctx(t testing.TB) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

// world is the installation under test: a database file, a host application
// and a clock, all of which outlive any one process of the service.
type world struct {
	t    *testing.T
	path string
	clk  *clock.Fake
	app  *fake.App
}

// newWorld creates the installation. hostOpts configure the host (for example a
// commit that is not idempotent).
func newWorld(t *testing.T, hostOpts ...fake.AppOption) *world {
	t.Helper()
	clk := clock.NewFake(t0)
	return &world{
		t:    t,
		path: filepath.Join(t.TempDir(), "actions.db"),
		clk:  clk,
		app:  fake.NewApp(append([]fake.AppOption{fake.WithClock(clk)}, hostOpts...)...),
	}
}

// transport sits between the coordinator's adapter wrapper and the host. It
// records every operation the coordinator sends, and a test can make the next
// Prepare or Commit stop the calling goroutine, which is how the process dies.
type transport struct {
	adapters.AppAdapter

	mu         sync.Mutex
	ops        []adapters.Operation
	dispatched []adapters.CommitRequest
	onPrepare  func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	onCommit   func(req adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
}

func (a *transport) note(op adapters.Operation) {
	a.mu.Lock()
	a.ops = append(a.ops, op)
	a.mu.Unlock()
}

func (a *transport) Read(c context.Context, sc contracts.Scope, r adapters.ReadRequest) (act.DeterministicResult, error) {
	a.note(adapters.OpRead)
	return a.AppAdapter.Read(c, sc, r)
}

func (a *transport) Validate(c context.Context, sc contracts.Scope, r adapters.ValidateRequest) (act.DeterministicResult, error) {
	a.note(adapters.OpValidate)
	return a.AppAdapter.Validate(c, sc, r)
}

func (a *transport) Prepare(c context.Context, sc contracts.Scope, r adapters.PrepareRequest) (act.DeterministicResult, error) {
	a.note(adapters.OpPrepare)
	a.mu.Lock()
	hook := a.onPrepare
	a.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return a.AppAdapter.Prepare(c, sc, r) }
	if hook != nil {
		return hook(next)
	}
	return next()
}

func (a *transport) Commit(c context.Context, sc contracts.Scope, r adapters.CommitRequest) (act.DeterministicResult, error) {
	a.note(adapters.OpCommit)
	a.mu.Lock()
	a.dispatched = append(a.dispatched, r)
	hook := a.onCommit
	a.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return a.AppAdapter.Commit(c, sc, r) }
	if hook != nil {
		return hook(r, next)
	}
	return next()
}

func (a *transport) Status(c context.Context, sc contracts.Scope, r adapters.StatusRequest) (act.DeterministicResult, error) {
	a.note(adapters.OpStatus)
	return a.AppAdapter.Status(c, sc, r)
}

// operations returns what this process's coordinator sent to the host so far.
func (a *transport) operations() []adapters.Operation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]adapters.Operation(nil), a.ops...)
}

// commitsDispatched counts, per idempotency key, the commits this process's
// coordinator handed to the adapter: what a recovery dispatched, whether or not
// the host received it.
func (a *transport) commitsDispatched() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]int{}
	for _, r := range a.dispatched {
		out[string(r.IdempotencyKey)]++
	}
	return out
}

func (a *transport) set(f func(*transport)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

// dieInCommit makes the next Commit stop the calling goroutine. With applied,
// the host has applied the commit first: the process dies after the host acted
// and before the coordinator learned of it.
func (a *transport) dieInCommit(applied bool) {
	a.set(func(a *transport) {
		a.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
			if applied {
				_, _ = next()
			}
			runtime.Goexit()
			return act.DeterministicResult{}, nil
		}
	})
}

// diePrepare makes the next Prepare stop the calling goroutine, after the host
// staged the preparation when applied is true.
func (a *transport) diePrepare(applied bool) {
	a.set(func(a *transport) {
		a.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
			if applied {
				_, _ = next()
			}
			runtime.Goexit()
			return act.DeterministicResult{}, nil
		}
	})
}

// process is one run of the service: a store handle, a coordinator over it and
// the transport it reaches the host through.
type process struct {
	w    *world
	db   *store.DB
	co   *actions.Coordinator
	host *transport
	once sync.Once
}

// bootOptions adjust how a process is wired.
type bootOptions struct {
	// adapterErr, when set, is what the adapter source answers: the application
	// is not reachable (quarantined, uninstalled) in this process.
	adapterErr error
}

// boot starts a process over the database file at path.
func (w *world) boot(path string, opts ...func(*bootOptions)) *process {
	w.t.Helper()
	var bo bootOptions
	for _, o := range opts {
		o(&bo)
	}
	db, err := store.Open(ctx(w.t), store.Options{Path: path})
	if err != nil {
		w.t.Fatalf("store.Open: %v", err)
	}
	alog, err := audit.New(db, audit.Options{Clock: w.clk})
	if err != nil {
		w.t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(w.clk, policy.WithSink(sink))
	host := &transport{AppAdapter: fake.NewAdapter(w.app)}
	guard, err := adapters.Wrap(host, adapters.Options{ApplicationID: appID})
	if err != nil {
		w.t.Fatal(err)
	}
	pkg := newPackage(w.t)
	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: alog, Clock: w.clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) {
			if bo.adapterErr != nil {
				return nil, bo.adapterErr
			}
			return guard, nil
		}),
	})
	if err != nil {
		w.t.Fatal(err)
	}
	p := &process{w: w, db: db, co: co, host: host}
	w.t.Cleanup(p.stop)
	return p
}

// stop ends the process the way a clean exit does: the store is closed.
func (p *process) stop() {
	p.once.Do(func() { _ = p.db.Close() })
}

// crashImage returns a copy of the database as it is on disk right now, taken
// while the process still holds the store open and never closes it: the main
// file and the write-ahead log, the state a power cut leaves for the next boot
// (SQLite then recovers from the log). The process is abandoned, not closed.
func (p *process) crashImage() string {
	p.w.t.Helper()
	dir := p.w.t.TempDir()
	dst := filepath.Join(dir, "actions.db")
	for _, suffix := range []string{"", "-wal"} {
		src := p.w.path + suffix
		in, err := os.Open(src)
		if os.IsNotExist(err) && suffix != "" {
			continue // no log: everything was checkpointed into the main file
		}
		if err != nil {
			p.w.t.Fatalf("crash image: %v", err)
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			_ = in.Close()
			p.w.t.Fatalf("crash image: %v", err)
		}
		if _, err := io.Copy(out, in); err != nil {
			p.w.t.Fatalf("crash image: %v", err)
		}
		_ = in.Close()
		if err := out.Close(); err != nil {
			p.w.t.Fatalf("crash image: %v", err)
		}
	}
	return dst
}

// runUntilKilled runs fn on its own goroutine and waits for it to end. fn is
// expected never to return: it calls into the coordinator, and a hook stops the
// goroutine inside the call. If fn returns, the "kill" did not happen and the
// test would prove nothing.
func (p *process) runUntilKilled(fn func()) {
	p.w.t.Helper()
	done, returned := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		fn()
		close(returned)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		p.w.t.Fatal("the call that was meant to be killed did not end")
	}
	select {
	case <-returned:
		p.w.t.Fatal("the call returned although the process was supposed to die inside it")
	default:
	}
}

// ---- steps and readers ----

func (p *process) prepare(name string) actions.Prepared {
	p.w.t.Helper()
	sc := scope()
	pr, err := p.co.Prepare(ctx(p.w.t), sc, actions.PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": name}})
	if err != nil {
		p.w.t.Fatalf("Prepare: %v", err)
	}
	return pr
}

func (p *process) confirm(pr actions.Prepared) error {
	p.w.t.Helper()
	_, err := p.co.Confirm(ctx(p.w.t), scope(), actions.ConfirmInput{PreparationID: prepID(pr), Token: pr.Confirmation})
	return err
}

func (p *process) mustConfirm(pr actions.Prepared) {
	p.w.t.Helper()
	if err := p.confirm(pr); err != nil {
		p.w.t.Fatalf("Confirm: %v", err)
	}
}

func (p *process) commit(pr actions.Prepared) (contracts.ActionReceipt, error) {
	p.w.t.Helper()
	return p.co.Commit(ctx(p.w.t), scope(), prepID(pr))
}

func prepID(pr actions.Prepared) string { return string(pr.Action.PreparationID) }
func keyOf(pr actions.Prepared) string  { return string(pr.Action.IdempotencyKey) }

// state reads the stored state of a preparation, around the coordinator.
func (p *process) state(pr actions.Prepared) act.PreparedActionState {
	p.w.t.Helper()
	var s string
	if err := p.db.QueryRowContext(ctx(p.w.t), `SELECT state FROM action_preparations WHERE id = ?`, prepID(pr)).Scan(&s); err != nil {
		p.w.t.Fatalf("state of %s: %v", prepID(pr), err)
	}
	return act.PreparedActionState(s)
}

func (p *process) count(query string, args ...any) int {
	p.w.t.Helper()
	var n int
	if err := p.db.QueryRowContext(ctx(p.w.t), query, args...).Scan(&n); err != nil {
		p.w.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// events lists the action.* audit events of the tenant chain, in order.
func (p *process) events() []string {
	p.w.t.Helper()
	rows, err := p.db.QueryContext(ctx(p.w.t),
		`SELECT event_type FROM audit_events WHERE application_id = ? AND tenant_id = ? AND event_type LIKE 'action.%' ORDER BY seq`, appID, tenantA)
	if err != nil {
		p.w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			p.w.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (p *process) hasEvent(eventType string) bool {
	for _, e := range p.events() {
		if e == eventType {
			return true
		}
	}
	return false
}

// hostCommits counts, per idempotency key, the commits the HOST received,
// across every process so far: the number of times the side effect could have
// been asked for. It is the detector of a repeated commit.
func (w *world) hostCommits() map[string]int {
	out := map[string]int{}
	for _, c := range w.app.Calls() {
		if c.Operation == adapters.OpCommit {
			out[c.IdempotencyKey]++
		}
	}
	return out
}

// hostOpsSince lists the operations the host received after the first n calls.
func (w *world) hostOpsSince(n int) []adapters.Operation {
	calls := w.app.Calls()
	if n > len(calls) {
		return nil
	}
	var ops []adapters.Operation
	for _, c := range calls[n:] {
		ops = append(ops, c.Operation)
	}
	return ops
}

// maxPerKey is the largest count of any key: 2 or more is a repeated commit.
func maxPerKey(m map[string]int) int {
	top := 0
	for _, n := range m {
		top = max(top, n)
	}
	return top
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func describe(m map[string]int) string {
	s := ""
	for _, k := range sortedKeys(m) {
		s += fmt.Sprintf("%s=%d ", k, m[k])
	}
	return s
}

// expiry parses the expiry the coordinator stamped on a preparation.
func expiry(t testing.TB, pr actions.Prepared) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, string(pr.Action.ExpiresAt))
	if err != nil {
		t.Fatalf("expires_at %q: %v", pr.Action.ExpiresAt, err)
	}
	return at
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

func noErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
