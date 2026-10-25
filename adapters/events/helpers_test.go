package events

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

var eventT0 = time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC)

const (
	eventApp        = "demo-app"
	eventOtherApp   = "other-app"
	eventDeployment = "dep-1"
	eventKeyID      = "key-1"
)

// eventKey is a generated test key (never a fixture secret on disk).
type eventKey struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newEventKey(t testing.TB, id string) eventKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return eventKey{id: id, pub: pub, priv: priv}
}

// eventApplier counts what reaches the session layer and what scope it carries.
type eventApplier struct {
	inner Sessions
	mu    sync.Mutex
	calls []eventApplied
}

type eventApplied struct {
	scope contracts.Scope
	ev    sessions.ApplicationEvent
}

func (a *eventApplier) ApplyEvent(ctx context.Context, service contracts.Scope, ev sessions.ApplicationEvent, ivr sessions.Invalidator, n sessions.Notifier) (sessions.EventResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, eventApplied{scope: service, ev: ev})
	a.mu.Unlock()
	return a.inner.ApplyEvent(ctx, service, ev, ivr, n)
}

func (a *eventApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// eventReplay counts the calls that reach the replay cache and can be made to fail.
type eventReplay struct {
	inner identity.ReplayCache
	mu    sync.Mutex
	calls int
	// fail is consulted with the number of the call (from 1) and may return an error.
	fail func(n int) error
}

func (r *eventReplay) Consume(ctx context.Context, scope contracts.Scope, jti string, expiresAt time.Time) error {
	r.mu.Lock()
	r.calls++
	n := r.calls
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		if err := fail(n); err != nil {
			return err
		}
	}
	return r.inner.Consume(ctx, scope, jti, expiresAt)
}

func (r *eventReplay) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// eventCoordinator can do far more than the intake is supposed to ask of it: any
// call other than Invalidate fails the test. Commit has the real coordinator's
// signature, so a type assertion to an interface built from it would be caught;
// the import check (TestImportsStayWithinMap) and the Invalidator type are what
// keep the intake away from the rest.
type eventCoordinator struct {
	t     *testing.T
	mu    sync.Mutex
	calls []eventVoid
	err   error
}

type eventVoid struct {
	scope  contracts.Scope
	id     string
	reason act.PreparedActionInvalidationConditionsItem
}

func (c *eventCoordinator) Invalidate(_ context.Context, sc contracts.Scope, id string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return contracts.PreparedAction{}, c.err
	}
	c.calls = append(c.calls, eventVoid{scope: sc, id: id, reason: reason})
	return contracts.PreparedAction{}, nil
}

func (c *eventCoordinator) Commit(context.Context, contracts.Scope, string) (contracts.ActionReceipt, error) {
	c.t.Error("an event committed an action")
	return contracts.ActionReceipt{}, nil
}

func (c *eventCoordinator) Prepare(...any) { c.t.Error("an event prepared an action") }
func (c *eventCoordinator) Confirm(...any) { c.t.Error("an event confirmed an action") }
func (c *eventCoordinator) Call(...any)    { c.t.Error("an event called a tool") }

func (c *eventCoordinator) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, v := range c.calls {
		out = append(out, v.id)
	}
	return out
}

// eventNotifier records the notices.
type eventNotifier struct {
	mu      sync.Mutex
	notices []sessions.Notice
}

func (n *eventNotifier) Notify(_ context.Context, notice sessions.Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notices = append(n.notices, notice)
	return nil
}

func (n *eventNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.notices)
}

// eventEnv is an intake over a real trust store, replay cache and session store.
type eventEnv struct {
	t        *testing.T
	clk      *clock.Fake
	db       *store.DB
	trust    *trust.Store
	key      eventKey
	sessions *sessions.Store
	replay   *eventReplay
	applier  *eventApplier
	coord    *eventCoordinator
	notes    *eventNotifier
	cfg      Config
	intake   *Intake
}

func newEventEnv(t *testing.T, mutate ...func(*eventEnv)) *eventEnv {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(eventT0)
	db := storetest.Open(t, nil)
	e := &eventEnv{t: t, clk: clk, db: db, key: newEventKey(t, eventKeyID)}
	e.trust = trust.NewStore(db, clk)
	if _, err := e.trust.RegisterKey(ctx, eventApp, e.key.id, e.key.pub, "test"); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.sessions, err = sessions.New(db, clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	e.replay = &eventReplay{inner: identity.NewSQLiteReplayCache(db, clk, nil)}
	e.applier = &eventApplier{inner: e.sessions}
	e.coord = &eventCoordinator{t: t}
	e.notes = &eventNotifier{}
	e.cfg = Config{
		DeploymentID: eventDeployment, ApplicationID: eventApp,
		Verifier: trust.NewVerifier(e.trust, clk), Replay: e.replay,
		Sessions: e.applier, Invalidator: e.coord, Notifier: e.notes,
	}
	for _, m := range mutate {
		m(e)
	}
	if e.intake, err = New(e.cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

// event is a valid, unsigned application event about title t1 in tenant-a ws-1.
func (e *eventEnv) event(id string) Event {
	return Event{
		ID: id, Type: "title.merged", Kind: "application_event", ApplicationID: eventApp,
		TenantID: "tenant-a", WorkspaceID: "ws-1",
		Entities: []Entity{{EntityType: "title", EntityID: "t1"}},
	}
}

// sign returns the delivery of ev signed by the registered key, valid for five
// minutes from now.
func (e *eventEnv) sign(ev Event) []byte {
	e.t.Helper()
	return e.signWith(ev, e.key, e.clk.Now(), e.clk.Now().Add(5*time.Minute))
}

func (e *eventEnv) signWith(ev Event, k eventKey, signedAt, expiresAt time.Time) []byte {
	e.t.Helper()
	body, err := Sign(ev, k.id, k.priv, signedAt, expiresAt)
	if err != nil {
		e.t.Fatal(err)
	}
	return body
}

// scope is the scope a session of the application is bound to.
func (e *eventEnv) scope(tenant, workspace, principal, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: eventDeployment, ApplicationID: eventApp, TenantID: tenant, WorkspaceID: workspace,
		PrincipalID: principal, Roles: []string{"member"}, CapabilityScopes: []string{"items.create"}, SessionID: session,
	}
}

// seed opens the session of sc with a goal that has a preparation awaiting
// confirmation and a title the host resolved (t1).
func (e *eventEnv) seed(sc contracts.Scope, prep string) {
	e.t.Helper()
	ctx := context.Background()
	sess, err := e.sessions.Open(ctx, sc)
	if err != nil {
		e.t.Fatal(err)
	}
	now := e.clk.Now()
	g := &sessions.Goal{
		CapabilityID: "items.create", Phase: capabilities.PhaseOpen,
		Slots: map[string]sessions.Slot{
			"title":    {Value: "t1", Origin: sessions.OriginEntityResolution},
			"quantity": {Value: "2", Origin: sessions.OriginUserTurn},
		},
		Entities:  []sessions.ResolvedEntity{{Field: "title", EntityType: "title", EntityID: "t1", Version: "v1"}},
		StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(sessions.DefaultGoalTTL),
	}
	g.SetPrepared(prep, now.Add(10*time.Minute))
	sess.State.Goal = g
	if _, err := e.sessions.Save(ctx, sess, nil); err != nil {
		e.t.Fatal(err)
	}
}

// prepared reports the preparation the session of sc still references.
func (e *eventEnv) prepared(sc contracts.Scope) string {
	e.t.Helper()
	sess, err := e.sessions.Open(context.Background(), sc)
	if err != nil {
		e.t.Fatal(err)
	}
	if sess.State.Goal == nil {
		return ""
	}
	return sess.State.Goal.PreparedID
}

func (e *eventEnv) version(sc contracts.Scope) int64 {
	e.t.Helper()
	sess, err := e.sessions.Open(context.Background(), sc)
	if err != nil {
		e.t.Fatal(err)
	}
	return sess.Version
}
