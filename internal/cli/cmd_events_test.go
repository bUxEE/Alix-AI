package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/events"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The events area of the composition root: the intakes the gateway asks for, the
// notifier that opens the notice turn in the hub and announces it, and the
// property that matters most, that an application event can only invalidate.
// The services are the real ones (trust store, replay cache, session store,
// stream hub, announcement feed); the coordinator is a strict fake that fails the
// test on anything but Invalidate. No gateway.Server is built: a registry with
// the session store and the hub in it, but not the dialogue engine, would be a
// half-wired /v1 area and refuse to start.

const (
	eventsTestApp        = "app_demo"
	eventsTestDeployment = "dep_cli"
	eventsTestKeyID      = "key-events-1"
)

var eventsTestT0 = time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC)

// eventsStrictCoordinator is the action service of the test: it can do far more than an
// event may ask of it, and any call but Invalidate fails the test.
type eventsStrictCoordinator struct {
	t     *testing.T
	mu    sync.Mutex
	voids []string
}

func (c *eventsStrictCoordinator) Invalidate(_ context.Context, _ contracts.Scope, id string, _ act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.voids = append(c.voids, id)
	return contracts.PreparedAction{}, nil
}

func (c *eventsStrictCoordinator) voided() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.voids...)
}

func (c *eventsStrictCoordinator) Get(context.Context, contracts.Scope, string) (contracts.PreparedAction, error) {
	c.t.Error("an event read a preparation")
	return contracts.PreparedAction{}, nil
}

func (c *eventsStrictCoordinator) Confirm(context.Context, contracts.Scope, actions.ConfirmInput) (contracts.PreparedAction, error) {
	c.t.Error("an event confirmed an action")
	return contracts.PreparedAction{}, nil
}

func (c *eventsStrictCoordinator) Commit(context.Context, contracts.Scope, string) (contracts.ActionReceipt, error) {
	c.t.Error("an event committed an action")
	return contracts.ActionReceipt{}, nil
}

func (c *eventsStrictCoordinator) Reject(context.Context, contracts.Scope, string) (contracts.PreparedAction, error) {
	c.t.Error("an event rejected an action")
	return contracts.PreparedAction{}, nil
}

func (c *eventsStrictCoordinator) Receipt(context.Context, contracts.Scope, string) (contracts.ActionReceipt, error) {
	c.t.Error("an event read a receipt")
	return contracts.ActionReceipt{}, nil
}

// eventsApps is the application service: every application is active but the
// quarantined one.
type eventsApps struct{ quarantined string }

func (a eventsApps) Package(_ context.Context, applicationID string) (capabilities.Package, error) {
	if applicationID == a.quarantined {
		return nil, contracts.New(contracts.ErrQuarantined, "the application is not active")
	}
	return nil, nil
}

func (eventsApps) Records(context.Context, string) ([]apppkg.Record, error) { return nil, nil }
func (eventsApps) Register(context.Context, contracts.Scope, []byte) (*apppkg.Installed, error) {
	return nil, contracts.New(contracts.ErrInternal, "not used")
}

// eventsRig is the provided events area over real stores.
type eventsRig struct {
	t        *testing.T
	clk      *clock.Fake
	db       *store.DB
	svc      *gateway.Services
	key      ed25519.PrivateKey
	sessions *sessions.Store
	coord    *eventsStrictCoordinator
	hub      *stream.Hub
	feed     *gateway.EventsFeed
	intakes  *eventsIntakes
}

// eventsNewRig provides the events area into a registry that holds what the other providers would
// have given it, with the application's key registered and the deployment id set.
func eventsNewRig(t *testing.T, withHub bool) *eventsRig {
	t.Helper()
	ctx := context.Background()
	r := &eventsRig{t: t, clk: clock.NewFake(eventsTestT0), svc: gateway.NewServices()}
	r.db = storetest.Open(t, nil)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r.key = priv
	if _, err := trust.NewStore(r.db, r.clk).RegisterKey(ctx, eventsTestApp, eventsTestKeyID, pub, "test"); err != nil {
		t.Fatal(err)
	}
	if r.sessions, err = sessions.New(r.db, r.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	r.coord = &eventsStrictCoordinator{t: t}
	provide := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	provide(gateway.Provide(r.svc, r.sessions))
	provide(gateway.Provide[gateway.ActionService](r.svc, r.coord))
	provide(gateway.Provide[gateway.ApplicationService](r.svc, eventsApps{quarantined: "app_quarantined"}))
	deps := serveDeps{Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Clock: r.clk, DB: r.db, Services: r.svc}
	// The events provider runs before the turnevents one: the hub is provided after it.
	provide(eventsProvide(ctx, deps))
	t.Cleanup(func() { eventsMCPSink.Store(nil) })
	if withHub {
		r.hub = stream.NewHub(stream.Config{Clock: r.clk})
		provide(gateway.Provide(r.svc, r.hub))
	}
	r.feed, _ = gateway.Lookup[*gateway.EventsFeed](r.svc)
	in, _ := gateway.Lookup[gateway.EventsIntakes](r.svc)
	var ok bool
	if r.intakes, ok = in.(*eventsIntakes); !ok || r.feed == nil {
		t.Fatalf("the provider gave %T and %v", in, r.feed)
	}
	r.intakes.deployment = func() string { return eventsTestDeployment }
	return r
}

// scope is the scope a session of the application is bound to.
func (r *eventsRig) scope(session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: eventsTestDeployment, ApplicationID: eventsTestApp, TenantID: "ten_acme", WorkspaceID: "wsp_main",
		PrincipalID: "usr_ada", Roles: []string{"member"}, CapabilityScopes: []string{"items.create"}, SessionID: session,
	}
}

// seed opens the session and leaves it waiting on the preparation prep.
func (r *eventsRig) seed(session, prep string) contracts.Scope {
	r.t.Helper()
	ctx := context.Background()
	sc := r.scope(session)
	sess, err := r.sessions.Open(ctx, sc)
	if err != nil {
		r.t.Fatal(err)
	}
	now := r.clk.Now()
	g := &sessions.Goal{
		CapabilityID: "items.create", Phase: capabilities.PhaseOpen,
		Slots:     map[string]sessions.Slot{"quantity": {Value: "2", Origin: sessions.OriginUserTurn}},
		StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(sessions.DefaultGoalTTL),
	}
	g.SetPrepared(prep, now.Add(10*time.Minute))
	sess.State.Goal = g
	if _, err := r.sessions.Save(ctx, sess, nil); err != nil {
		r.t.Fatal(err)
	}
	return sc
}

// prepared is the preparation the session still references.
func (r *eventsRig) prepared(sc contracts.Scope) string {
	r.t.Helper()
	sess, err := r.sessions.Open(context.Background(), sc)
	if err != nil {
		r.t.Fatal(err)
	}
	if sess.State.Goal == nil {
		return ""
	}
	return sess.State.Goal.PreparedID
}

// delivery signs ev (for the test application unless it names another) with the registered key.
func (r *eventsRig) delivery(ev events.Event) []byte {
	r.t.Helper()
	body, err := events.Sign(ev, eventsTestKeyID, r.key, r.clk.Now(), r.clk.Now().Add(5*time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	return body
}

// event is a valid event about the tenant of the seeded sessions that asks them to forget everything pending.
func (r *eventsRig) event(id string) events.Event {
	return events.Event{
		ID: id, Type: "invoice.paid", Kind: "application_event", ApplicationID: eventsTestApp,
		TenantID: "ten_acme", WorkspaceID: "wsp_main", Everything: true,
	}
}

// The provider registers itself, gives the registry the intakes and the feed by
// their types once, refuses to give them twice, and an application's intake is
// bound to that application: the platform application and an installation with
// no deployment id get none.
func TestEventsProviderGivesTheRegistryTheIntakesAndTheFeed(t *testing.T) {
	if _, byName := registeredServeProviders(); byName["events"] == nil {
		t.Fatal("the events provider did not register itself")
	}
	r := eventsNewRig(t, true)
	if err := eventsProvide(context.Background(), serveDeps{Clock: r.clk, DB: r.db, Services: r.svc}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second events area for one registry = %v, want %s", err, contracts.ErrConflict)
	}

	ctx := context.Background()
	if _, err := r.intakes.EventsHandler(ctx, eventsTestApp); err != nil {
		t.Errorf("the intake of the application: %v", err)
	}
	if _, err := r.intakes.EventsHandler(ctx, trust.PlatformApplicationID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("the platform application's intake = %v, want %s", err, contracts.ErrPermissionDenied)
	}
	if _, err := r.intakes.EventsHandler(ctx, ""); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Errorf("an intake for no application = %v, want %s", err, contracts.ErrScopeMissing)
	}
	r.intakes.deployment = func() string { return "" }
	if _, err := r.intakes.EventsHandler(ctx, "app_second"); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("an intake with no deployment id = %v, want %s", err, contracts.ErrUnavailable)
	}

	// The provider needs what it builds from.
	if err := eventsProvide(ctx, serveDeps{Clock: r.clk, Services: gateway.NewServices()}); err == nil {
		t.Error("the provider accepted a missing database")
	}
}

// A valid event reaches a client that is connected to its session: the client is
// subscribed to the session's announcements, an event that invalidates the
// session is delivered, the announcement arrives and is the only way the client
// learns the notice turn's id, and the turn it names is a failed turn of a stale
// error at seq 0 and done at seq 1, in the hub under the session's own scope.
// Along the way the event only invalidated: the coordinator saw one Invalidate.
func TestEventsNoticeReachesAConnectedClient(t *testing.T) {
	r := eventsNewRig(t, true)
	sc := r.seed("ses_one", "prep_1")
	r.seed("ses_two", "prep_2")
	otherTenant := r.scope("ses_one") // the same session id in a tenant the event is not about
	otherTenant.TenantID = "ten_globex"
	r.seed2(otherTenant, "prep_3")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := r.feed.Subscribe(sc, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	bystander, err := r.feed.Subscribe(otherTenant, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer bystander.Close()

	h, err := r.intakes.EventsHandler(ctx, eventsTestApp)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(r.delivery(r.event("evt_notice_1"))))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delivery = %d: %s", rec.Code, rec.Body.String())
	}
	var receipt events.Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != events.StatusApplied || receipt.Sessions != 2 || receipt.Notified != 2 || receipt.NotifyFailed != 0 {
		t.Fatalf("receipt = %+v, want two sessions of the tenant told", receipt)
	}

	// The announcement: identifiers only, and the turn id nobody else could know.
	a, err := client.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != eventsTestT0.Unix() || a.EventID != "evt_notice_1" || a.EventType != "invoice.paid" || a.Kind != "application_event" || a.TurnID == "" {
		t.Fatalf("announcement = %+v", a)
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if got, err := bystander.Next(short); err == nil {
		t.Fatalf("another tenant's session was told: %+v", got)
	}

	// The notice is readable in the hub with the client's own scope and nobody else's.
	turn, err := r.hub.Lookup(sc, a.TurnID)
	if err != nil {
		t.Fatalf("the notice turn is not in the hub: %v", err)
	}
	if _, err := r.hub.Lookup(otherTenant, a.TurnID); stream.ReasonOf(err) != stream.ReasonNotVisible {
		t.Errorf("another tenant reads the notice = %v, want turn_not_visible", err)
	}
	sub, err := turn.Subscribe(stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	f0, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f1, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f0.Seq != 0 || f0.Type != "error" || f1.Seq != 1 || f1.Type != "done" {
		t.Fatalf("the notice = %d %s, %d %s; want error at seq 0 and done at seq 1", f0.Seq, f0.Type, f1.Seq, f1.Type)
	}
	var ev conversation.TurnEvent
	if err := json.Unmarshal(f0.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if e, ok := ev.Value.(conversation.ErrorEvent); !ok || e.Error.Code != common.ErrorCodeStale || e.Error.Retryable {
		t.Fatalf("the notice error = %+v", ev.Value)
	}
	var done conversation.TurnEvent
	if err := json.Unmarshal(f1.Data, &done); err != nil {
		t.Fatal(err)
	}
	if d, ok := done.Value.(conversation.DoneEvent); !ok || d.FinishReason != conversation.DoneEventFinishReasonFailed {
		t.Fatalf("the notice end = %+v", done.Value)
	}

	// An application event never commits anything: it voided the two preparations of
	// the tenant through Invalidate and the strict coordinator saw no other call.
	if got := r.prepared(sc); got != "" {
		t.Errorf("the session still references %q", got)
	}
	if got := r.coord.voided(); len(got) != 2 {
		t.Errorf("voided preparations = %v, want prep_1 and prep_2", got)
	}
	if got := r.prepared(otherTenant); got != "prep_3" {
		t.Errorf("another tenant's session was changed: %q", got)
	}
}

// seed2 leaves the session of an arbitrary scope waiting on a preparation.
func (r *eventsRig) seed2(sc contracts.Scope, prep string) {
	r.t.Helper()
	ctx := context.Background()
	sess, err := r.sessions.Open(ctx, sc)
	if err != nil {
		r.t.Fatal(err)
	}
	now := r.clk.Now()
	g := &sessions.Goal{
		CapabilityID: "items.create", Phase: capabilities.PhaseOpen,
		StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(sessions.DefaultGoalTTL),
	}
	g.SetPrepared(prep, now.Add(10*time.Minute))
	sess.State.Goal = g
	if _, err := r.sessions.Save(ctx, sess, nil); err != nil {
		r.t.Fatal(err)
	}
}

// A notice that cannot be delivered (here the hub is not provided) is counted
// and never undoes the invalidation: the session is voided all the same, and
// nothing was announced.
func TestEventsNoticeFailureNeverUndoesTheInvalidation(t *testing.T) {
	r := eventsNewRig(t, false)
	sc := r.seed("ses_one", "prep_1")
	it, err := r.intakes.intake(context.Background(), eventsTestApp)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := it.Deliver(context.Background(), r.delivery(r.event("evt_nohub_1")))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Sessions != 1 || receipt.Notified != 0 || receipt.NotifyFailed != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := r.prepared(sc); got != "" {
		t.Errorf("the session still references %q", got)
	}
	if got := r.coord.voided(); len(got) != 1 || got[0] != "prep_1" {
		t.Errorf("voided = %v", got)
	}
	sub, err := r.feed.Subscribe(sc, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got, err := sub.Next(ctx); err == nil {
		t.Errorf("an undelivered notice was announced: %+v", got)
	}
}

// serve mounts the events routes beside the others, behind the identity
// middleware, with the intakes and the feed provided by the composition root: a
// request with no token is refused first, and one with a token reaches the
// handler, which asks whether the application is active before anything else.
func TestEventsServeMountsTheEventsRoutes(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, _ := serveConfig(t, f, identityYAML)
	provisionIssuer(t, cfg, iss)
	srv := startServe(t, cfg)

	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/events", `{}`},
		{http.MethodGet, "/v1/sessions/" + identitytest.DefaultSession + "/events", ""},
	} {
		if code, body := srv.call(t, nil, rt.method, rt.path, rt.body); code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d: %s", rt.method, rt.path, code, body)
		}
		code, body := srv.call(t, iss, rt.method, rt.path, rt.body)
		var env contracts.Error
		if err := json.Unmarshal(body, &env); err != nil || code != http.StatusForbidden || env.Code != contracts.ErrQuarantined {
			t.Fatalf("%s %s for an application with no package = %d: %s (a 404 would mean the route is not mounted)", rt.method, rt.path, code, strings.TrimSpace(string(body)))
		}
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}
