package ledger_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/events"
	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// voider is the one effectful thing an event can reach: the coordinator's Invalidate.
// It has no way to commit, confirm or prepare, and that is the point.
type voider struct {
	mu    sync.Mutex
	calls []string
}

func (v *voider) Invalidate(_ context.Context, sc contracts.Scope, id string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, fmt.Sprintf("%s/%s/%s:%s", sc.TenantID, sc.WorkspaceID, id, reason))
	return contracts.PreparedAction{}, nil
}

func (v *voider) list() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

type notices struct {
	mu   sync.Mutex
	list []sessions.Notice
}

func (n *notices) Notify(_ context.Context, notice sessions.Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, notice)
	return nil
}

func (n *notices) types() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, x := range n.list {
		out = append(out, x.EventType)
	}
	return out
}

// intakeEnv is AlixAI's event intake over a real trust store, durable replay cache and
// session store, served over loopback HTTP, with the ledger's events key registered.
type intakeEnv struct {
	clk      *clock.Fake
	url      string
	key      ed25519.PrivateKey
	sessions *sessions.Store
	void     *voider
	notes    *notices
	received *int
}

const eventKeyID = "ledger-events-2026" // the key_id of the package's events endpoint

func newIntake(t *testing.T) *intakeEnv {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(epoch)
	db := storetest.Open(t, nil)
	tr := trust.NewStore(db, clk)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RegisterKey(ctx, ledger.ApplicationID, eventKeyID, pub, "ledger events"); err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e := &intakeEnv{clk: clk, key: priv, sessions: sess, void: &voider{}, notes: &notices{}, received: new(int)}
	intake, err := events.New(events.Config{
		DeploymentID: "dep_test", ApplicationID: ledger.ApplicationID, Verifier: trust.NewVerifier(tr, clk),
		Replay: identity.NewSQLiteReplayCache(db, clk, nil), Sessions: sess, Invalidator: e.void, Notifier: e.notes,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := intake.Handler()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*e.received++
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

func (e *intakeEnv) signer() ledger.Signer {
	return ledger.Signer{KeyID: eventKeyID, Key: e.key, Clock: e.clk.Now}
}

// open starts a conversation of a principal whose goal holds a prepared action that
// relies on one invoice at version 1.
func (e *intakeEnv) open(t *testing.T, sc contracts.Scope, invoiceID, prep string) contracts.Scope {
	t.Helper()
	ctx := context.Background()
	sc.DeploymentID = "dep_test"
	s, err := e.sessions.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	g := &sessions.Goal{
		CapabilityID: "payments.record", Phase: capabilities.PhaseOpen,
		Slots:     map[string]sessions.Slot{"invoice": {Value: invoiceID, Origin: sessions.OriginEntityResolution}},
		Entities:  []sessions.ResolvedEntity{{Field: "invoice", EntityType: "invoice", EntityID: invoiceID, Version: "1"}},
		StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(sessions.DefaultGoalTTL),
	}
	g.SetPrepared(prep, now.Add(10*time.Minute))
	s.State.Goal = g
	if _, err := e.sessions.Save(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	return sc
}

func (e *intakeEnv) prepared(t *testing.T, sc contracts.Scope) string {
	t.Helper()
	s, err := e.sessions.Open(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if s.State.Goal == nil {
		return ""
	}
	return s.State.Goal.PreparedID
}

func post(t *testing.T, url string, body []byte) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestLedgerEventsSigned: the ledger's application events are signed with its events
// key, accepted by AlixAI's real intake, and invalidate exactly the sessions that hold
// state about the changed record. They can only invalidate: nothing is committed,
// and anything unsigned, altered, replayed or for another application is refused.
func TestLedgerEventsSigned(t *testing.T) {
	in := newIntake(t)
	sink, err := ledger.NewHTTPSink(in.url, in.signer())
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, func(c *ledger.Config) { c.Events = sink })
	w.clk.Set(in.clk.Now())
	fx := w.provision()
	acct, mgr := w.as("acct-a-north"), w.as("mgr-a-north")
	invN, invS := fx.record["inv-a-north"], fx.record["inv-a-south"]

	// Two conversations hold a prepared action about the invoice that is about to be
	// paid; a third, in another organisation, holds one about nothing of this.
	viewer := in.open(t, w.as("view-a-north"), invN, "prep_viewer_1")
	other := in.open(t, w.as("acct-b"), fx.record["inv-b"], "prep_other_1")
	south := in.open(t, w.as("acct-a-south"), invS, "prep_south_1")

	t.Run("a payment emits invoice.paid, signed and delivered, and voids the preparation", func(t *testing.T) {
		prep := w.prepare(acct, "payments.record", map[string]any{"invoice": invN, "amount_minor": 125050})
		isOK(t, w.committed(acct, "payments.record", w.preparedFrom("payments.record", prep)))
		flush(t, w) // delivery is off the reply path: wait for it before looking
		if left := w.app.Outbox(); len(left) != 0 {
			t.Fatalf("an event was not delivered: %+v", left)
		}
		if got := in.void.list(); len(got) != 1 || got[0] != fx.tenantOf["view-a-north"]+"/"+fx.teamOf["view-a-north"]+"/prep_viewer_1:data_version_changed" {
			t.Fatalf("voided = %v", got)
		}
		if in.prepared(t, viewer) != "" {
			t.Fatal("the session kept a preparation about an invoice that was paid")
		}
		if in.prepared(t, other) != "prep_other_1" || in.prepared(t, south) != "prep_south_1" {
			t.Fatal("an event reached a session it does not concern")
		}
		if got := in.notes.types(); fmt.Sprint(got) != "[invoice.paid]" {
			t.Fatalf("notices = %v", got)
		}
	})

	t.Run("a void emits invoice.voided to the team of the invoice only", func(t *testing.T) {
		prep := w.prepare(mgr, "invoices.void", map[string]any{"invoice": invS, "reason": "wrong_amount"})
		p := w.preparedFrom("invoices.void", prep)
		sc := w.as("mgr-a-north")
		if err := w.app.ApproveVoid(bridge.Identity{ApplicationID: sc.ApplicationID, TenantID: sc.TenantID, WorkspaceID: sc.WorkspaceID, PrincipalID: sc.PrincipalID, Roles: sc.Roles, SessionID: sc.SessionID}, text(t, prep, "void_reference")); err != nil {
			t.Fatal(err)
		}
		isOK(t, w.committed(mgr, "invoices.void", p))
		flush(t, w)
		if in.prepared(t, south) != "" {
			t.Fatal("the south team's preparation about the voided invoice survived")
		}
		if got := in.notes.types(); fmt.Sprint(got) != "[invoice.paid invoice.voided]" {
			t.Fatalf("notices = %v", got)
		}
		if len(w.app.Outbox()) != 0 {
			t.Fatal("an event is still queued")
		}
	})

	t.Run("an event only invalidates: nothing was committed, prepared or called", func(t *testing.T) {
		// The only effectful object the intake holds is Invalidate: every call it made is a
		// void, and a revision of a record voids for the reason data_version_changed.
		for _, c := range in.void.list() {
			if !strings.HasSuffix(c, ":data_version_changed") {
				t.Fatalf("a call that is not a revision void: %s", c)
			}
		}
	})

	t.Run("anything unsigned, altered, replayed or for another application is refused", func(t *testing.T) {
		ev := ledger.Event{
			ID: "lev_forged", Type: ledger.EventInvoicePaid, Kind: "record_revision", ApplicationID: ledger.ApplicationID,
			TenantID: fx.tenantOf["acct-a-north"], WorkspaceID: fx.teamOf["acct-a-north"], Entities: []ledger.EventEntity{{EntityType: "invoice", EntityID: invN, Version: "9"}},
		}
		good, err := in.signer().Sign(ev)
		if err != nil {
			t.Fatal(err)
		}
		// altered after signing: the tenant is another one's
		var env map[string]json.RawMessage
		_ = json.Unmarshal(good, &env)
		var body map[string]any
		_ = json.Unmarshal(env["event"], &body)
		body["tenant_id"] = fx.tenant["org-b"]
		env["event"], _ = json.Marshal(body)
		altered, _ := json.Marshal(env)
		if code, _ := post(t, in.url, altered); code != http.StatusUnauthorized {
			t.Fatalf("an altered event = %d", code)
		}
		// unsigned
		env2 := map[string]any{"event": json.RawMessage(mustJSON(t, ev)), "signatures": []any{}}
		if code, _ := post(t, in.url, mustJSON(t, env2)); code != http.StatusUnauthorized {
			t.Fatalf("an unsigned event = %d", code)
		}
		// signed by a key nobody registered
		_, stranger, _ := ed25519.GenerateKey(rand.Reader)
		forged, _ := ledger.Signer{KeyID: eventKeyID, Key: stranger, Clock: in.clk.Now}.Sign(ev)
		if code, _ := post(t, in.url, forged); code != http.StatusUnauthorized {
			t.Fatalf("an event signed by another key = %d", code)
		}
		// genuine signature, but for another application
		ev2 := ev
		ev2.ID, ev2.ApplicationID = "lev_other_app", "clinic-booking"
		foreign, _ := in.signer().Sign(ev2)
		if code, _ := post(t, in.url, foreign); code == http.StatusOK {
			t.Fatal("an event for another application was accepted")
		}
		// the genuine event is accepted once and a replay is a duplicate that changes nothing
		before := len(in.void.list())
		if code, got := post(t, in.url, good); code != http.StatusOK {
			t.Fatalf("a genuine event = %d %s", code, got)
		}
		code, got := post(t, in.url, good)
		if code != http.StatusOK || !bytes.Contains([]byte(got), []byte("duplicate")) {
			t.Fatalf("a replay = %d %s", code, got)
		}
		if after := len(in.void.list()); after != before {
			t.Fatal("a replayed event voided something again")
		}
		// a signature that would outlive the intake's memory is not even made
		long := in.signer()
		long.Validity = time.Hour
		if _, err := long.Sign(ev); err == nil {
			t.Fatal("a signature valid for an hour was made")
		}
	})

	t.Run("the signature is the one AlixAI's own signer makes", func(t *testing.T) {
		at := in.clk.Now()
		ev := ledger.Event{
			ID: "lev_same", Type: ledger.EventInvoiceVoided, Kind: "record_revision", ApplicationID: ledger.ApplicationID,
			TenantID: "tnt_a<&>", WorkspaceID: "ws_é ", Entities: []ledger.EventEntity{{EntityType: "invoice", EntityID: "inv_1", Version: "2"}},
		}
		mine, err := in.signer().Sign(ev)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := events.Sign(events.Event{
			ID: ev.ID, Type: ev.Type, Kind: ev.Kind, ApplicationID: ev.ApplicationID, TenantID: ev.TenantID, WorkspaceID: ev.WorkspaceID,
			Entities: []events.Entity{{EntityType: "invoice", EntityID: "inv_1", Version: "2"}},
		}, eventKeyID, in.key, at, at.Add(5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		sig := func(b []byte) string {
			var e events.Envelope
			if err := json.Unmarshal(b, &e); err != nil || len(e.Signatures) != 1 {
				t.Fatalf("%v %s", err, b)
			}
			return e.Signatures[0].Signature + e.Signatures[0].SignedAt + e.Signatures[0].ExpiresAt
		}
		if sig(mine) != sig(theirs) {
			t.Fatal("the ledger's canonical JSON differs from the one AlixAI verifies")
		}
		if _, err := base64.StdEncoding.DecodeString(func() string { var e events.Envelope; _ = json.Unmarshal(mine, &e); return e.Signatures[0].Signature }()); err != nil {
			t.Fatal(err)
		}
	})
}

// A commit never fails, and never waits, because an event could not be delivered: the
// event stays in the outbox, in order, and goes out with the next flush. Delivery is off
// the reply path, so a slow intake cannot turn a committed payment into an uncertain one.
func TestLedgerEventsSurviveAnUnreachableIntake(t *testing.T) {
	in := newIntake(t)
	sink, err := ledger.NewHTTPSink(in.url, in.signer())
	if err != nil {
		t.Fatal(err)
	}
	var down atomic.Bool
	down.Store(true)
	sink.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if down.Load() {
			return nil, errors.New("connection refused")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	w := newWorld(t, func(c *ledger.Config) { c.Events = sink })
	w.clk.Set(in.clk.Now())
	fx := w.provision()
	acct := w.as("acct-a-north")
	invN := fx.record["inv-a-north"]
	viewer := in.open(t, w.as("view-a-north"), invN, "prep_viewer_2")

	for _, amount := range []int{100, 200} {
		prep := w.prepare(acct, "payments.record", map[string]any{"invoice": invN, "amount_minor": amount})
		isOK(t, w.committed(acct, "payments.record", w.preparedFrom("payments.record", prep)))
	}
	if err := w.app.Flush(context.Background()); err == nil {
		t.Fatal("a flush to an unreachable intake reported success")
	}
	if got := eventTypes(w); fmt.Sprint(got) != "[invoice.payment_recorded invoice.payment_recorded]" {
		t.Fatalf("outbox = %v", got)
	}
	if in.prepared(t, viewer) != "prep_viewer_2" {
		t.Fatal("an event that was not delivered voided a preparation")
	}
	down.Store(false)
	flush(t, w)
	if len(w.app.Outbox()) != 0 || in.prepared(t, viewer) != "" {
		t.Fatalf("the queued events were not delivered in the next flush: %v", w.app.Outbox())
	}
	// A contract change tells AlixAI the contracts source changed; no session is concerned.
	if err := w.app.PutContract(fx.tenantOf["acct-a-north"], fx.teamOf["acct-a-north"], "master-services", "Master services agreement", "Payment is due in 45 days."); err != nil {
		t.Fatal(err)
	}
	if len(w.app.Outbox()) != 0 {
		t.Fatalf("the contract event was not delivered: %v", w.app.Outbox())
	}
}

// The reply to a commit does not wait for the intake: with an intake that never answers, the
// commit still returns at once, and the event waits in the outbox until the intake does.
func TestLedgerCommitDoesNotWaitForTheIntake(t *testing.T) {
	in := newIntake(t)
	sink, err := ledger.NewHTTPSink(in.url, in.signer())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	sink.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return http.DefaultTransport.RoundTrip(r)
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	w := newWorld(t, func(c *ledger.Config) { c.Events = sink })
	w.clk.Set(in.clk.Now())
	fx := w.provision()
	acct := w.as("acct-a-north")
	prep := w.prepare(acct, "payments.record", map[string]any{"invoice": fx.record["inv-a-north"], "amount_minor": 100})
	start := time.Now()
	isOK(t, w.committed(acct, "payments.record", w.preparedFrom("payments.record", prep)))
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the commit waited %v for the intake", took)
	}
	select {
	case <-entered: // the delivery started, and is parked at the intake
	case <-time.After(5 * time.Second):
		t.Fatal("the event was never sent")
	}
	if len(w.app.Outbox()) != 1 {
		t.Fatalf("outbox = %v", w.app.Outbox())
	}
	close(release)
	flush(t, w)
	if len(w.app.Outbox()) != 0 {
		t.Fatalf("the event was not delivered once the intake answered: %v", w.app.Outbox())
	}
}

// flush waits for every queued event to be delivered: delivery is off the reply path.
func flush(t *testing.T, w *world) {
	t.Helper()
	if err := w.app.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
