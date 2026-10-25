package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	evt "github.com/buxee/alix-ai/adapters/events"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The tests of the events area run the real stack of newAPIEnv (real session
// store, real coordinator, real trust store and replay cache behind the real
// adapters/events intake, a real stream hub) with the one thing the composition
// root normally supplies: the notifier. It is the same few lines as
// internal/cli/cmd_events.go (a gateway test cannot import the composition
// root); that file's tests run the real one. adapters/events is imported by this
// test file only: the gateway itself imports no adapter.

// eventsRig is what the events services of one test server are built from.
type eventsRig struct {
	keyID string
	key   ed25519.PrivateKey
	feed  *EventsFeed
}

var eventsRigs sync.Map // *apiEnv -> *eventsRig

func eventsRigOf(t testing.TB, e *apiEnv) *eventsRig {
	t.Helper()
	r, ok := eventsRigs.Load(e)
	if !ok {
		t.Fatal("the test server has no events rig")
	}
	return r.(*eventsRig)
}

// eventsTestIntakes serves the one intake of the test application.
type eventsTestIntakes struct{ intake *evt.Intake }

func (i eventsTestIntakes) EventsHandler(_ context.Context, applicationID string) (http.Handler, error) {
	if applicationID != apiApp {
		return nil, contracts.New(contracts.ErrPermissionDenied, "events: no intake for this application")
	}
	return i.intake.Handler(), nil
}

// eventsTestNotifier opens the notice turn in the hub and announces it, like the composition root's.
type eventsTestNotifier struct {
	svc  *Services
	feed *EventsFeed
}

func (n eventsTestNotifier) Notify(_ context.Context, notice sessions.Notice) error {
	hub, ok := Lookup[*stream.Hub](n.svc)
	if !ok {
		return contracts.New(contracts.ErrUnavailable, "no hub")
	}
	first, ok := notice.Events[0].Value.(conversation.ErrorEvent)
	if !ok {
		return contracts.New(contracts.ErrInternal, "a notice starts with its error event")
	}
	turn, err := hub.Begin(notice.Scope, string(first.TurnID))
	if err != nil {
		return err
	}
	for _, ev := range notice.Events {
		if err := turn.Publish(ev); err != nil {
			_ = turn.Fail(err)
			return err
		}
	}
	_, err = n.feed.Announce(notice.Scope, EventsAnnouncement{
		EventID: notice.EventID, EventType: notice.EventType, Kind: string(notice.Kind), TurnID: string(first.TurnID),
	}, hub.Config().Retention)
	return err
}

var eventsRigSeq atomic.Int64

func init() {
	registerTestServices("v1_events", func(t testing.TB, e *apiEnv, s *Services) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		// A test env can build its services more than once (the AG-UI tests do), and a key id is registered once per application.
		rig := &eventsRig{keyID: fmt.Sprintf("key-events-%d", eventsRigSeq.Add(1)), key: priv, feed: NewEventsFeed(EventsFeedConfig{Clock: e.clk, Heartbeat: 50 * time.Millisecond})}
		ts := trust.NewStore(e.db, e.clk)
		if _, err := ts.RegisterKey(context.Background(), apiApp, rig.keyID, pub, "test"); err != nil {
			t.Fatal(err)
		}
		intake, err := evt.New(evt.Config{
			DeploymentID: apiDep, ApplicationID: apiApp, Verifier: trust.NewVerifier(ts, e.clk),
			Replay: identity.NewSQLiteReplayCache(e.db, e.clk, nil), Sessions: e.sess, Invalidator: e.co,
			Notifier: eventsTestNotifier{svc: s, feed: rig.feed},
		})
		if err != nil {
			t.Fatal(err)
		}
		eventsRigs.Store(e, rig)
		t.Cleanup(func() { eventsRigs.Delete(e) })
		if err := Provide[EventsIntakes](s, eventsTestIntakes{intake: intake}); err != nil {
			t.Fatal(err)
		}
		if err := Provide(s, rig.feed); err != nil {
			t.Fatal(err)
		}
	})
}

// eventsEvent is a valid event about the default tenant and workspace that asks the
// sessions to forget everything pending.
func eventsEvent(id string) evt.Event {
	return evt.Event{
		ID: id, Type: "invoice.paid", Kind: "application_event", ApplicationID: apiApp,
		TenantID: apiTenant, WorkspaceID: apiWsp, Everything: true,
	}
}

// eventsSign is the delivery of ev, signed by the registered key for five minutes.
func eventsSign(t testing.TB, e *apiEnv, ev evt.Event) []byte {
	t.Helper()
	r := eventsRigOf(t, e)
	body, err := evt.Sign(ev, r.keyID, r.key, e.clk.Now(), e.clk.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func eventsPost(e *apiEnv, body []byte, path string, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodPost, path, body, mods...)
}

type eventsReceipt struct {
	EventID      string `json:"event_id"`
	Status       string `json:"status"`
	Sessions     int    `json:"sessions"`
	Notified     int    `json:"notified"`
	NotifyFailed int    `json:"notify_failed"`
}

// eventsPending is the kind of the pending state the default session reports under mods ("" when none).
func eventsPending(t testing.TB, e *apiEnv, mods ...claims) string {
	t.Helper()
	rec := e.call(http.MethodGet, sessionsPath(apiSess)+"/pending", nil, mods...)
	wantStatus(t, rec, http.StatusOK)
	if p := decodeBody[PendingBody](t, rec); p.Pending != nil {
		return p.Pending.Kind
	}
	return ""
}

func eventsPreparationState(t testing.TB, e *apiEnv, prep string, mods ...claims) act.PreparedActionState {
	t.Helper()
	pa, err := e.co.Get(context.Background(), e.scopeOf(mods...), prep)
	if err != nil {
		t.Fatal(err)
	}
	return pa.State
}

// A signed event is applied: it voids the preparation a session waited on, tells
// nobody to commit anything, and nothing reaches the host. An event with no
// signature, a signature of a key nobody registered and a request with no token
// are refused and change nothing.
func TestEventsRouteDeliversASignedEventAndRefusesAnUnsigned(t *testing.T) {
	e := newAPIEnv(t)
	prep, _ := e.prepareCreate()
	id := prep.Confirmation.PreparationID
	if eventsPending(t, e) != "confirmation" {
		t.Fatal("the session does not wait for a confirmation")
	}

	signed := eventsSign(t, e, eventsEvent("evt_unsigned_1"))
	var env map[string]json.RawMessage
	if err := json.Unmarshal(signed, &env); err != nil {
		t.Fatal(err)
	}
	unsigned, _ := json.Marshal(map[string]any{"event": env["event"], "signatures": []any{}})
	wantError(t, eventsPost(e, unsigned, "/v1/events"), http.StatusUnauthorized, contracts.ErrUnauthenticated)

	// A genuine-looking signature by a key the application never registered.
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	forged, err := evt.Sign(eventsEvent("evt_forged_1"), eventsRigOf(t, e).keyID, stranger, e.clk.Now(), e.clk.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, eventsPost(e, forged, "/v1/events"), http.StatusUnauthorized, contracts.ErrUnauthenticated)

	// Not JSON, and not the right content type.
	wantError(t, eventsPost(e, []byte("not json"), "/v1/events"), http.StatusBadRequest, contracts.ErrSchemaInvalid)
	rec := e.callRaw(e.token(), http.MethodPost, "/v1/events", "text/plain", signed)
	wantStatus(t, rec, http.StatusUnsupportedMediaType)

	// No token: the identity middleware first.
	wantUnauthorized(t, e.callAs("", http.MethodPost, "/v1/events", signed))

	if eventsPending(t, e) != "confirmation" || eventsPreparationState(t, e, id) != act.PreparedActionStatePrepared {
		t.Fatal("a refused delivery changed the session")
	}

	// The signed event is applied.
	rec = eventsPost(e, signed, "/v1/events")
	wantStatus(t, rec, http.StatusOK)
	got := decodeBody[eventsReceipt](t, rec)
	if got.Status != "applied" || got.EventID != "evt_unsigned_1" || got.Sessions != 1 || got.Notified != 1 || got.NotifyFailed != 0 {
		t.Fatalf("receipt = %+v", got)
	}
	if st := eventsPreparationState(t, e, id); st != act.PreparedActionStateInvalidated {
		t.Fatalf("the preparation is %s after the event, want invalidated", st)
	}
	if k := eventsPending(t, e); k == "confirmation" {
		t.Fatal("the session still waits for a confirmation the event voided")
	}

	// The same event again is acknowledged and does nothing.
	rec = eventsPost(e, signed, "/v1/events")
	wantStatus(t, rec, http.StatusOK)
	if dup := decodeBody[eventsReceipt](t, rec); dup.Status != "duplicate" || dup.Sessions != 0 {
		t.Fatalf("a replayed event = %+v", dup)
	}

	// An application event never commits: the secret the user held no longer confirms
	// anything, and the host was never asked to commit, now or before.
	rec = e.call(http.MethodPost, "/v1/actions/"+id+"/confirm", ConfirmRequest{Token: prep.Confirmation.Token})
	if rec.Code == http.StatusOK {
		t.Fatalf("a voided preparation was confirmed: %s", rec.Body.String())
	}
	if e.hostCalls(adapters.OpCommit) != 0 || e.host.Effects() != 0 {
		t.Fatalf("an event reached the host: commits = %d, effects = %d", e.hostCalls(adapters.OpCommit), e.host.Effects())
	}
}

// The tenant and workspace an event is about come only from its signed payload:
// the token's own tenant, a tenant in the query or a header and the path name
// nothing. An event for another application than the token's is refused even
// when the signature is genuine, and a wildcard scope is no scope.
func TestEventsRouteTakesTheScopeOnlyFromThePayload(t *testing.T) {
	e := newAPIEnv(t)
	inA, _ := e.prepareCreate() // tenant ten_acme
	inB, _ := e.prepareCreate(withTenant(apiOther))
	if inA.Confirmation.PreparationID == inB.Confirmation.PreparationID {
		t.Fatal("two tenants share a preparation")
	}

	// The token is the other tenant's, and so are the query and the headers; the
	// signed event is about ten_acme.
	rec := e.callAs(e.token(withTenant(apiOther), withSession("ses_service_1")), http.MethodPost,
		"/v1/events?tenant_id="+apiOther+"&workspace_id="+apiWsp, eventsSign(t, e, eventsEvent("evt_scope_1")),
		"X-Tenant-ID", apiOther, "X-Tenant", apiOther)
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[eventsReceipt](t, rec); got.Sessions != 1 {
		t.Fatalf("receipt = %+v, want the one session of ten_acme", got)
	}
	if st := eventsPreparationState(t, e, inA.Confirmation.PreparationID); st != act.PreparedActionStateInvalidated {
		t.Errorf("the tenant the event names was not invalidated: %s", st)
	}
	if st := eventsPreparationState(t, e, inB.Confirmation.PreparationID, withTenant(apiOther)); st != act.PreparedActionStatePrepared {
		t.Errorf("the tenant of the token was invalidated by an event about another: %s", st)
	}
	if eventsPending(t, e, withTenant(apiOther)) != "confirmation" {
		t.Error("the other tenant's session lost its confirmation")
	}

	// An event about the other tenant reaches that tenant and nobody else.
	ev := eventsEvent("evt_scope_2")
	ev.TenantID = apiOther
	wantStatus(t, eventsPost(e, eventsSign(t, e, ev), "/v1/events"), http.StatusOK)
	if st := eventsPreparationState(t, e, inB.Confirmation.PreparationID, withTenant(apiOther)); st != act.PreparedActionStateInvalidated {
		t.Errorf("the tenant named by the second event was not invalidated: %s", st)
	}

	// An event of another application, genuinely signed, is a scope violation;
	// a wildcard tenant is refused, and neither touches a session.
	foreign := eventsEvent("evt_scope_3")
	foreign.ApplicationID = "app_other"
	wantError(t, eventsPost(e, eventsSign(t, e, foreign), "/v1/events"), http.StatusForbidden, contracts.ErrScopeViolation)
	wild := eventsEvent("evt_scope_4")
	wild.TenantID = "*"
	if rec := eventsPost(e, eventsSign(t, e, wild), "/v1/events"); rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
		t.Errorf("a wildcard tenant = %d: %s", rec.Code, rec.Body.String())
	}
	if e.hostCalls(adapters.OpCommit) != 0 {
		t.Fatal("an event reached the host")
	}
}

// ---- the session-level announcement ----

// eventsConn is an open announcement stream: the frames it received, in order.
type eventsConn struct {
	t      testing.TB
	frames chan turneventsFrame
	cancel context.CancelFunc
	resp   *http.Response
}

func eventsConnect(t *testing.T, e *apiEnv, hs *httptest.Server, query string, headers ...string) *eventsConn {
	t.Helper()
	return eventsConnectAs(t, hs, e.token(), query, headers...)
}

func eventsConnectAs(t *testing.T, hs *httptest.Server, token, query string, headers ...string) *eventsConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	u := hs.URL + sessionsPath(apiSess) + "/events"
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &eventsConn{t: t, frames: make(chan turneventsFrame, 32), cancel: cancel, resp: resp}
	t.Cleanup(c.close)
	if resp.StatusCode != http.StatusOK {
		return c
	}
	go func() {
		defer close(c.frames)
		sc := bufio.NewScanner(resp.Body)
		var f turneventsFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f != (turneventsFrame{}) {
					c.frames <- f
				}
				f = turneventsFrame{}
			case strings.HasPrefix(line, "id: "):
				f.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ": keep-alive"):
				c.frames <- turneventsFrame{Event: "keep-alive"}
			}
		}
	}()
	return c
}

func (c *eventsConn) close() {
	c.cancel()
	c.resp.Body.Close()
}

// next returns the next announcement, skipping keep-alives.
func (c *eventsConn) next() EventsAnnouncement {
	c.t.Helper()
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				c.t.Fatal("the announcement stream ended")
			}
			if f.Event == "keep-alive" {
				continue
			}
			if f.Event != EventsAnnouncementType {
				c.t.Fatalf("frame event = %q, want %s", f.Event, EventsAnnouncementType)
			}
			var a EventsAnnouncement
			if err := json.Unmarshal([]byte(f.Data), &a); err != nil {
				c.t.Fatalf("announcement %q: %v", f.Data, err)
			}
			if f.ID == "" || f.ID != strconv.FormatInt(a.Seq, 10) {
				c.t.Fatalf("frame id %q does not carry the seq %d", f.ID, a.Seq)
			}
			return a
		case <-time.After(10 * time.Second):
			c.t.Fatal("no announcement arrived")
		}
	}
}

// A connected client learns that an event changed its session, and with it the id
// of the notice turn it could never know: it reads the failed turn (a stale
// error, then done) through the existing turn-events route. The stream is the
// session's own: another session, another principal and another tenant see
// nothing of it, and a stream for a session that is not the token's is refused.
func TestEventsAnnouncementReachesAConnectedClient(t *testing.T) {
	e := newAPIEnv(t)
	hs := httptest.NewServer(e.srv.Handler()) // plain-HTTP loopback
	t.Cleanup(hs.Close)                       // runs after every stream's own cleanup, which ends it
	e.prepareCreate()

	base := e.clk.Now().Unix() // the session's feed starts when its first stream opens
	client := eventsConnect(t, e, hs, "")
	if client.resp.StatusCode != http.StatusOK || !strings.HasPrefix(client.resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("open = %d %s", client.resp.StatusCode, client.resp.Header.Get("Content-Type"))
	}
	bystander := eventsConnect(t, e, hs, "")                             // the same session, a second stream
	bob := eventsConnectAs(t, hs, e.token(withPrincipal("usr_bob")), "") // another principal of the same session id

	wantStatus(t, eventsPost(e, eventsSign(t, e, eventsEvent("evt_announce_1")), "/v1/events"), http.StatusOK)
	a := client.next()
	if a.Seq != base || a.Type != EventsAnnouncementType || a.EventID != "evt_announce_1" || a.EventType != "invoice.paid" ||
		a.Kind != "application_event" || a.TurnID == "" {
		t.Fatalf("announcement = %+v", a)
	}
	if b := bystander.next(); b.TurnID != a.TurnID {
		t.Errorf("two streams of one session got different turns: %q, %q", a.TurnID, b.TurnID)
	}

	// The announcement carries identifiers only; the notice is the turn it names.
	rec := turneventsGet(e, a.TurnID, "", nil)
	wantStatus(t, rec, http.StatusOK)
	sent := turneventsParse(t, rec.Body.String())
	if len(sent.frames) != 2 || sent.frames[0].Event != "error" || sent.frames[1].Event != "done" || sent.frames[0].ID != "0" || sent.frames[1].ID != "1" {
		t.Fatalf("the notice turn = %+v", sent.frames)
	}
	var notice conversation.TurnEvent
	if err := json.Unmarshal([]byte(sent.frames[0].Data), &notice); err != nil {
		t.Fatal(err)
	}
	if ev, ok := notice.Value.(conversation.ErrorEvent); !ok || ev.Error.Code != common.ErrorCodeStale || ev.Error.Retryable || !strings.Contains(ev.Error.Message, "Nothing was committed") {
		t.Fatalf("the notice = %+v", notice.Value)
	}

	// A client that connects after the event still learns of it, from the cursor.
	late := eventsConnect(t, e, hs, "")
	if l := late.next(); l.Seq != a.Seq || l.TurnID != a.TurnID {
		t.Fatalf("a late client got %+v", l)
	}
	// Resuming after the last announcement waits for the next; the cursor is never ahead.
	resumed := eventsConnect(t, e, hs, "cursor="+strconv.FormatInt(a.Seq, 10))
	select {
	case f := <-resumed.frames:
		if f.Event != "keep-alive" {
			t.Fatalf("a client at the end of the feed was sent %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no keep-alive on an idle stream")
	}
	if c := eventsConnect(t, e, hs, "cursor="+strconv.FormatInt(a.Seq+1, 10)); c.resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a cursor ahead of the feed = %d, want 400", c.resp.StatusCode)
	}
	if c := eventsConnect(t, e, hs, "cursor=x"); c.resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed cursor = %d, want 400", c.resp.StatusCode)
	}

	// Nobody else saw it: another principal's stream (opened before the event)
	// stays silent and another tenant's feed is empty. A cursor from before that
	// feed began (one a client kept across a restart of the process) is not
	// refused: the feed is replayed from its start, and it has nothing.
	select {
	case f := <-bob.frames:
		if f.Event != "keep-alive" {
			t.Fatalf("another principal's stream was sent %+v", f)
		}
	case <-time.After(2 * time.Second):
	}
	tenantB := eventsConnectAs(t, hs, e.token(withTenant(apiOther)), "cursor="+strconv.FormatInt(a.Seq-1, 10))
	if tenantB.resp.StatusCode != http.StatusOK {
		t.Fatalf("a cursor older than the feed = %d, want the stream", tenantB.resp.StatusCode)
	}
	select {
	case f := <-tenantB.frames:
		if f.Event != "keep-alive" {
			t.Fatalf("another tenant's stream was sent %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no keep-alive on another tenant's idle stream")
	}

	// The session in the path must be the token's; no token, no stream.
	wantError(t, e.call(http.MethodGet, sessionsPath("ses_not_mine")+"/events", nil), http.StatusForbidden, contracts.ErrScopeViolation)
	wantUnauthorized(t, e.callAs("", http.MethodGet, sessionsPath(apiSess)+"/events", nil))
}

// What a CopilotKit client sees of an event is the state: the client-safe
// projection a run or a connect run begins with no longer shows the voided
// confirmation, so the next run's initial state differs from the one it had
// (STATE_DELTA on /alixai in patch mode, STATE_SNAPSHOT otherwise). The reason is
// the announcement; carrying it as a CUSTOM event is a proposal for internal/agui.
func TestEventsChangeTheStateAGUIProjects(t *testing.T) {
	e := newAPIEnv(t)
	e.prepareCreate()
	ctx := context.Background()
	before, err := e.sess.Open(ctx, e.scopeOf())
	if err != nil {
		t.Fatal(err)
	}
	was := agui.ProjectState(before.State)
	if was.Pending == nil || was.Pending.Kind != "confirmation" {
		t.Fatalf("the state before the event = %+v", was.Pending)
	}

	wantStatus(t, eventsPost(e, eventsSign(t, e, eventsEvent("evt_agui_1")), "/v1/events"), http.StatusOK)
	after, err := e.sess.Open(ctx, e.scopeOf())
	if err != nil {
		t.Fatal(err)
	}
	now := agui.ProjectState(after.State)
	if now.Pending != nil && now.Pending.PreparationID != "" {
		t.Fatalf("the state after the event still names the voided preparation: %+v", now.Pending)
	}
	delta, err := agui.StateChange(was, now)
	if err != nil || delta == nil || delta.Kind() != agui.StateDelta {
		t.Fatalf("StateChange = %v, %v; want a STATE_DELTA", delta, err)
	}
	snap, err := agui.InitialState(agui.StateSnapshotMode, now)
	if err != nil || snap.Kind() != agui.StateSnapshot {
		t.Fatalf("InitialState = %v, %v", snap, err)
	}
	// Both are events the pinned AG-UI schemas accept.
	sch := aguiSchemas(t)
	for _, ev := range []agui.Event{delta, snap} {
		raw, err := agui.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.ValidateEvent(raw); err != nil {
			t.Errorf("%s: %v", ev.Kind(), err)
		}
	}
}

// ---- the feed ----

func eventsFeedScope(session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: apiDep, ApplicationID: apiApp, TenantID: apiTenant, WorkspaceID: apiWsp,
		PrincipalID: "usr_alice", Roles: []string{"member"}, CapabilityScopes: []string{capGet}, SessionID: session,
	}
}

func eventsAnnouncement(id string) EventsAnnouncement {
	return EventsAnnouncement{EventID: id, EventType: "x.y", Kind: "application_event", TurnID: "trn_" + id}
}

// The feed is bounded in time (an announcement lives as long as the notice it names), per
// session, in sessions and in streams, and visible to exactly the scope it was made for.
func TestEventsFeedIsBoundedAndScoped(t *testing.T) {
	clk := clock.NewFake(apiNow)
	f := NewEventsFeed(EventsFeedConfig{Clock: clk, MaxPerSession: 3, MaxSessions: 2, MaxSubscribers: 1})
	a, b := eventsFeedScope("ses_a"), eventsFeedScope("ses_b")
	for _, id := range []string{"e1", "e2", "e3", "e4"} {
		if _, err := f.Announce(a, eventsAnnouncement(id), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sub, err := f.Subscribe(a, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for range 3 {
		got, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, got.Seq)
	}
	if base := apiNow.Unix(); seqs[0] != base+1 || seqs[2] != base+3 {
		t.Errorf("a session keeps its newest three: got seqs %v", seqs)
	}
	if _, err := f.Subscribe(a, stream.FromStart); !errors.Is(err, stream.ErrSubscribers) {
		t.Errorf("a second stream = %v, want the subscribers refusal", err)
	}
	sub.Close()

	// Another session of the same principal sees none of it.
	subB, err := f.Subscribe(b, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if _, err := subB.Next(short); err == nil {
		t.Error("a session saw another session's announcement")
	}
	stop()
	subB.Close()

	// A third session does not fit while the first two hold announcements.
	if _, err := f.Announce(b, eventsAnnouncement("e5"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Announce(eventsFeedScope("ses_c"), eventsAnnouncement("e6"), time.Minute); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Errorf("a full feed = %v, want resource_exhausted", err)
	}
	// Past their retention the announcements are gone and the room is back.
	clk.Advance(2 * time.Minute)
	sub, err = f.Subscribe(a, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	gone, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if got, err := sub.Next(gone); err == nil {
		t.Errorf("an expired announcement was sent: %+v", got)
	}
	sub.Close()
	if _, err := f.Announce(eventsFeedScope("ses_c"), eventsAnnouncement("e7"), time.Minute); err != nil {
		t.Errorf("an announcement after the others expired = %v", err)
	}
	// Malformed announcements are refused.
	if _, err := f.Announce(a, EventsAnnouncement{EventID: "e"}, time.Minute); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("an announcement with no turn = %v", err)
	}
}

// eventsSeqs reads n announcements of a subscription and returns their seqs.
func eventsSeqs(t *testing.T, sub *EventsSubscription, n int) []int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var seqs []int64
	for range n {
		got, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("after %v: %v", seqs, err)
		}
		seqs = append(seqs, got.Seq)
	}
	return seqs
}

// A session's announcements are numbered from the clock (its Unix time in seconds)
// when its record in the feed is made, so a client that resumes across a restart of
// the process, which makes a new record, with the cursor it kept is neither refused
// nor skipped: its cursor is older than the feed, and the feed is replayed from its
// start.
func TestEventsFeedNumbersFromTheClockAcrossARestart(t *testing.T) {
	clk := clock.NewFake(apiNow)
	a := eventsFeedScope("ses_a")
	before := NewEventsFeed(EventsFeedConfig{Clock: clk})
	first, err := before.Announce(a, eventsAnnouncement("e1"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first != apiNow.Unix() {
		t.Fatalf("the first announcement = %d, want the clock's %d", first, apiNow.Unix())
	}
	// It is a cursor the streams accept: ten digits at most, and not over stream.MaxSeq.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(first, 10))
	if got, err := stream.ParseCursor(req); err != nil || got != first {
		t.Fatalf("the seq %d as a cursor = %d, %v", first, got, err)
	}
	kept, err := before.Announce(a, eventsAnnouncement("e2"), time.Hour)
	if err != nil || kept != first+1 {
		t.Fatalf("the second announcement = %d, %v; want %d", kept, err, first+1)
	}

	// The process restarts a minute later, and the client resumes with the cursor it
	// kept before anything new is announced: it is followed, and told of the next one.
	clk.Advance(time.Minute)
	after := NewEventsFeed(EventsFeedConfig{Clock: clk})
	sub, err := after.Subscribe(a, kept)
	if err != nil {
		t.Fatalf("a cursor from before the restart = %v, want it followed", err)
	}
	next, err := after.Announce(a, eventsAnnouncement("e3"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if next != clk.Now().Unix() || next <= kept {
		t.Fatalf("the first announcement after the restart = %d, want the clock's %d, past the old %d", next, clk.Now().Unix(), kept)
	}
	if got := eventsSeqs(t, sub, 1); got[0] != next {
		t.Fatalf("the resumed client got %v, want [%d]", got, next)
	}
	sub.Close()

	// A client that comes back only after more was announced is sent all of it.
	more, err := after.Announce(a, eventsAnnouncement("e4"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	late, err := after.Subscribe(a, kept)
	if err != nil {
		t.Fatalf("a cursor from before the restart = %v", err)
	}
	defer late.Close()
	if got := eventsSeqs(t, late, 2); got[0] != next || got[1] != more || more != next+1 {
		t.Fatalf("the late client got %v, want [%d %d]", got, next, next+1)
	}

	// A number no cursor could carry is never given: past stream.MaxSeq the feed refuses.
	end := NewEventsFeed(EventsFeedConfig{Clock: clock.NewFake(time.Unix(stream.MaxSeq, 0))})
	if seq, err := end.Announce(a, eventsAnnouncement("e5"), time.Hour); err != nil || seq != stream.MaxSeq {
		t.Fatalf("the announcement at the end of the range = %d, %v", seq, err)
	}
	if seq, err := end.Announce(a, eventsAnnouncement("e6"), time.Hour); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("an announcement past the range = %d, %v; want resource_exhausted", seq, err)
	}
}

// When the feed forgets a session's record to make room and the session is announced
// to again in the same process, its numbering goes on upward, even inside the same
// second; a client holding the old cursor is sent the new announcement. A cursor past
// the newest announcement is still refused.
func TestEventsFeedNumbersNeverGoBackAfterAPrune(t *testing.T) {
	clk := clock.NewFake(apiNow)
	f := NewEventsFeed(EventsFeedConfig{Clock: clk, MaxSessions: 1})
	a, b := eventsFeedScope("ses_a"), eventsFeedScope("ses_b")
	var last int64
	for _, id := range []string{"e1", "e2", "e3"} {
		var err error
		if last, err = f.Announce(a, eventsAnnouncement(id), time.Nanosecond); err != nil {
			t.Fatal(err)
		}
	}
	if last != apiNow.Unix()+2 {
		t.Fatalf("three announcements in one second end at %d, want %d", last, apiNow.Unix()+2)
	}
	// They expire, and another session takes the only room: the first record is forgotten.
	clk.Advance(time.Nanosecond)
	if _, err := f.Announce(b, eventsAnnouncement("e4"), time.Nanosecond); err != nil {
		t.Fatalf("the feed made no room: %v", err)
	}
	clk.Advance(time.Nanosecond)
	again, err := f.Announce(a, eventsAnnouncement("e5"), time.Minute)
	if err != nil {
		t.Fatalf("the feed made no room: %v", err)
	}
	if again != last+1 {
		t.Fatalf("the session's numbering went from %d to %d, want %d", last, again, last+1)
	}
	sub, err := f.Subscribe(a, last)
	if err != nil {
		t.Fatalf("the cursor of the forgotten record = %v, want it followed", err)
	}
	defer sub.Close()
	if got := eventsSeqs(t, sub, 1); got[0] != again {
		t.Fatalf("the client got %v, want [%d]", got, again)
	}
	if _, err := f.Subscribe(a, again+1); !errors.Is(err, stream.ErrCursorAhead) {
		t.Errorf("a cursor past the newest announcement = %v, want the cursor-ahead refusal", err)
	}
}
