package events

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

// An event that is not signed by the application's key is refused and touches
// nothing: not the session layer, not the coordinator, not the notifier, and not
// the replay cache, because unauthenticated input must not be able to burn an id.
//
// SEC-045
func TestUnsignedEventRejected(t *testing.T) {
	ctx := context.Background()
	other := newEventKey(t, "key-2")

	// mangle breaks the signature of a delivery by flipping a bit of the signed bytes.
	flipSignature := func(raw []byte) []byte {
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		sig, err := base64.StdEncoding.DecodeString(env.Signatures[0].Signature)
		if err != nil {
			t.Fatal(err)
		}
		sig[0] ^= 0x01
		env.Signatures[0].Signature = base64.StdEncoding.EncodeToString(sig)
		out, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// with rebuilds a delivery around a different event, keeping the signatures.
	withEvent := func(raw []byte, mutate func(map[string]any)) []byte {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		var ev map[string]any
		if err := json.Unmarshal(env["event"], &ev); err != nil {
			t.Fatal(err)
		}
		mutate(ev)
		env["event"], _ = json.Marshal(ev)
		out, _ := json.Marshal(env)
		return out
	}

	cases := []struct {
		name string
		body func(e *eventEnv) []byte
		want contracts.ErrCode
	}{
		{"no signatures", func(e *eventEnv) []byte {
			body, _ := json.Marshal(Envelope{Event: mustJSON(t, e.event("evt-1")), Signatures: []trust.Signature{}})
			return body
		}, contracts.ErrUnauthenticated},
		{"signatures left out", func(e *eventEnv) []byte {
			return []byte(`{"event":` + string(mustJSON(t, e.event("evt-1"))) + `}`)
		}, contracts.ErrUnauthenticated},
		{"a signature that does not verify", func(e *eventEnv) []byte { return flipSignature(e.sign(e.event("evt-1"))) }, contracts.ErrUnauthenticated},
		{"an event changed after it was signed", func(e *eventEnv) []byte {
			return withEvent(e.sign(e.event("evt-1")), func(ev map[string]any) { ev["everything"] = true })
		}, contracts.ErrUnauthenticated},
		{"a tenant changed after it was signed", func(e *eventEnv) []byte {
			return withEvent(e.sign(e.event("evt-1")), func(ev map[string]any) { ev["tenant_id"] = "tenant-b" })
		}, contracts.ErrUnauthenticated},
		{"a key nobody registered", func(e *eventEnv) []byte {
			return e.signWith(e.event("evt-1"), other, e.clk.Now(), e.clk.Now().Add(time.Minute))
		}, contracts.ErrUnauthenticated},
		{"a key of another application", func(e *eventEnv) []byte {
			if _, err := e.trust.RegisterKey(ctx, eventOtherApp, "key-b", other.pub, "other"); err != nil {
				t.Fatal(err)
			}
			return e.signWith(e.event("evt-1"), eventKey{id: "key-b", pub: other.pub, priv: other.priv}, e.clk.Now(), e.clk.Now().Add(time.Minute))
		}, contracts.ErrUnauthenticated},
		{"a revoked key", func(e *eventEnv) []byte {
			body := e.sign(e.event("evt-1"))
			if err := e.trust.RevokeKey(ctx, eventApp, eventKeyID, "compromised"); err != nil {
				t.Fatal(err)
			}
			return body
		}, contracts.ErrUnauthenticated},
		{"an expired signature", func(e *eventEnv) []byte {
			body := e.sign(e.event("evt-1"))
			e.clk.Advance(6 * time.Minute)
			return body
		}, contracts.ErrExpired},
		{"a signature from the future", func(e *eventEnv) []byte {
			return e.signWith(e.event("evt-1"), e.key, e.clk.Now().Add(time.Hour), e.clk.Now().Add(2*time.Hour))
		}, contracts.ErrUnauthenticated},
		{"a signature valid for too long", func(e *eventEnv) []byte {
			return e.signWith(e.event("evt-1"), e.key, e.clk.Now(), e.clk.Now().Add(DefaultMaxValidity+time.Minute))
		}, contracts.ErrUnauthenticated},
		{"a signature made for something else", func(e *eventEnv) []byte {
			// The key signs packages too; a payload that is not an event cannot be one.
			sig, err := trust.SignPayload(map[string]any{"id": "evt-1", "tenant_id": "tenant-a"}, e.key.id, e.key.priv, e.clk.Now(), e.clk.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(Envelope{
				Event:      json.RawMessage(`{"id":"evt-1","tenant_id":"tenant-a"}`),
				Signatures: []trust.Signature{sig},
			})
			return body
		}, contracts.ErrSchemaInvalid},
		{"not JSON", func(*eventEnv) []byte { return []byte("not json") }, contracts.ErrSchemaInvalid},
		{"no event", func(*eventEnv) []byte { return []byte(`{"signatures":[]}`) }, contracts.ErrSchemaInvalid},
		{"an unknown member of the delivery", func(e *eventEnv) []byte {
			return withTopLevel(t, e.sign(e.event("evt-1")), "tenant_id", "tenant-b")
		}, contracts.ErrSchemaInvalid},
		{"an unknown member of the signed event", func(e *eventEnv) []byte {
			var m map[string]any
			_ = json.Unmarshal(mustJSON(t, e.event("evt-1")), &m)
			m["format"] = Format
			m["action"] = "commit_payment"
			signed, _ := json.Marshal(m)
			// The signature is genuine; the member is one this version does not know.
			sig, err := trust.SignPayload(json.RawMessage(signed), e.key.id, e.key.priv, e.clk.Now(), e.clk.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(Envelope{Event: signed, Signatures: []trust.Signature{sig}})
			return body
		}, contracts.ErrSchemaInvalid},
		{"too large", func(e *eventEnv) []byte { return bytes.Repeat([]byte(" "), DefaultMaxBodyBytes+1) }, contracts.ErrSchemaInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEventEnv(t)
			sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
			e.seed(sc, "prep_1")
			version := e.version(sc)
			body := c.body(e)
			base := e.replay.count()

			rc, err := e.intake.Deliver(ctx, body)
			wantCode(t, err, c.want)
			if rc != (Receipt{}) {
				t.Fatalf("a refused delivery returned a receipt: %+v", rc)
			}
			if e.applier.count() != 0 || e.replay.count() != base || len(e.coord.calls) != 0 || e.notes.count() != 0 {
				t.Fatalf("a refused event reached something: applier %d, replay %d (was %d), coordinator %d, notices %d",
					e.applier.count(), e.replay.count(), base, len(e.coord.calls), e.notes.count())
			}
			if e.prepared(sc) != "prep_1" || e.version(sc) != version {
				t.Fatal("a refused event changed a session")
			}

			// Over HTTP the same delivery is refused with a status and a typed error.
			rec := post(e, body)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("HTTP status = %d, want a 4xx: %s", rec.Code, rec.Body)
			}
			var env contracts.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != c.want {
				t.Fatalf("HTTP body = %s (%v), want code %s", rec.Body, err, c.want)
			}
		})
	}

	t.Run("401 for a missing signature", func(t *testing.T) {
		e := newEventEnv(t)
		body, _ := json.Marshal(Envelope{Event: mustJSON(t, e.event("evt-1"))})
		if rec := post(e, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
	})
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// withTopLevel adds a member to the delivery object.
func withTopLevel(t *testing.T, raw []byte, key, value string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = mustJSON(t, value)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func post(e *eventEnv, body []byte, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	e.intake.Handler().ServeHTTP(rec, req)
	return rec
}

// The scope of an event is its signed payload's, and nothing else. An event for
// another application is refused even when its signature is genuine; one that
// names no tenant or workspace, or a wildcard, is refused; one signed for another
// application cannot be moved to this one; nothing in the request names or widens
// a scope; and the session layer receives exactly the application, tenant and
// workspace the signature covers.
//
// SEC-045
func TestEventScopeEnforced(t *testing.T) {
	ctx := context.Background()

	t.Run("another application, genuinely signed", func(t *testing.T) {
		e := newEventEnv(t)
		ev := e.event("evt-1")
		ev.ApplicationID = eventOtherApp
		_, err := e.intake.Deliver(ctx, e.sign(ev))
		wantCode(t, err, contracts.ErrScopeViolation)
		if e.applier.count() != 0 || e.replay.count() != 0 {
			t.Fatalf("applier %d, replay %d", e.applier.count(), e.replay.count())
		}
	})

	t.Run("another application, signed by that application", func(t *testing.T) {
		e := newEventEnv(t)
		bKey := newEventKey(t, "key-b")
		if _, err := e.trust.RegisterKey(ctx, eventOtherApp, bKey.id, bKey.pub, "other"); err != nil {
			t.Fatal(err)
		}
		ev := e.event("evt-1")
		ev.ApplicationID = eventOtherApp
		_, err := e.intake.Deliver(ctx, e.signWith(ev, bKey, e.clk.Now(), e.clk.Now().Add(time.Minute)))
		wantCode(t, err, contracts.ErrUnauthenticated)
		if e.applier.count() != 0 {
			t.Fatal("an event signed by another application reached the session layer")
		}
	})

	t.Run("a scope that is not a scope", func(t *testing.T) {
		for name, mutate := range map[string]func(*Event){
			"no tenant":          func(ev *Event) { ev.TenantID = "" },
			"no workspace":       func(ev *Event) { ev.WorkspaceID = "" },
			"blank tenant":       func(ev *Event) { ev.TenantID = "  " },
			"wildcard tenant":    func(ev *Event) { ev.TenantID = "*" },
			"wildcard workspace": func(ev *Event) { ev.WorkspaceID = "*" },
			"padded tenant":      func(ev *Event) { ev.TenantID = " tenant-a" },
			"long workspace":     func(ev *Event) { ev.WorkspaceID = strings.Repeat("w", contracts.MaxScopeValueLen+1) },
		} {
			t.Run(name, func(t *testing.T) {
				e := newEventEnv(t)
				ev := e.event("evt-1")
				mutate(&ev)
				_, err := e.intake.Deliver(ctx, e.sign(ev))
				if code := contracts.CodeOf(err); code != contracts.ErrScopeMissing && code != contracts.ErrScopeViolation {
					t.Fatalf("error = %v, want scope_missing or scope_violation", err)
				}
				if e.applier.count() != 0 || e.replay.count() != 0 {
					t.Fatalf("applier %d, replay %d", e.applier.count(), e.replay.count())
				}
			})
		}
	})

	t.Run("the operator narrows the scope", func(t *testing.T) {
		e := newEventEnv(t, func(e *eventEnv) {
			e.cfg.AllowScope = func(_ context.Context, tenant, _ string) error {
				if tenant != "tenant-a" {
					return contracts.New(contracts.ErrScopeViolation, "no such tenant")
				}
				return nil
			}
		})
		ev := e.event("evt-1")
		ev.TenantID = "tenant-z"
		_, err := e.intake.Deliver(ctx, e.sign(ev))
		wantCode(t, err, contracts.ErrScopeViolation)
		if e.applier.count() != 0 {
			t.Fatal("a scope the operator refused reached the session layer")
		}
		if _, err := e.intake.Deliver(ctx, e.sign(e.event("evt-2"))); err != nil {
			t.Fatalf("the accepted scope was refused: %v", err)
		}
	})

	t.Run("only the signed scope is used", func(t *testing.T) {
		e := newEventEnv(t)
		inScope := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
		sameTenantOtherWorkspace := e.scope("tenant-a", "ws-2", "user-1", "ses-1")
		otherTenant := e.scope("tenant-b", "ws-1", "user-1", "ses-1")
		e.seed(inScope, "prep_1")
		e.seed(sameTenantOtherWorkspace, "prep_2")
		e.seed(otherTenant, "prep_3")
		before := []int64{e.version(sameTenantOtherWorkspace), e.version(otherTenant)}

		// The request tries to name another tenant, workspace and application in
		// every place a request can.
		rec := post(e, e.sign(e.event("evt-1")), func(r *http.Request) {
			r.URL.RawQuery = "tenant_id=tenant-b&workspace_id=ws-2&application_id=other-app"
			r.Header.Set("X-Tenant-Id", "tenant-b")
			r.Header.Set("X-Workspace-Id", "ws-2")
			r.Header.Set("X-Application-Id", eventOtherApp)
			r.Header.Set("Authorization", "Bearer something")
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}

		if len(e.applier.calls) != 1 {
			t.Fatalf("applier calls = %d", len(e.applier.calls))
		}
		got := e.applier.calls[0].scope
		if got.ApplicationID != eventApp || got.TenantID != "tenant-a" || got.WorkspaceID != "ws-1" ||
			got.DeploymentID != eventDeployment || got.PrincipalID != ServicePrincipal || got.SessionID != ServiceSession ||
			!slices.Equal(got.CapabilityScopes, []string{sessions.EventCapability}) || !slices.Equal(got.Roles, []string{ServiceRole}) {
			t.Fatalf("service scope = %+v", got)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("service scope does not validate: %v", err)
		}
		if e.prepared(inScope) != "" {
			t.Fatal("the session the event is about kept its preparation")
		}
		if e.prepared(sameTenantOtherWorkspace) != "prep_2" || e.prepared(otherTenant) != "prep_3" ||
			e.version(sameTenantOtherWorkspace) != before[0] || e.version(otherTenant) != before[1] {
			t.Fatal("an event for tenant-a ws-1 reached another workspace or tenant")
		}
	})

	t.Run("an intake is bound to an application that is not the platform", func(t *testing.T) {
		e := newEventEnv(t)
		cfg := e.cfg
		cfg.ApplicationID = trust.PlatformApplicationID
		_, err := New(cfg)
		wantCode(t, err, contracts.ErrValidationFailed)
	})
}

// An event id is applied once. A second delivery, with the same or a fresh
// signature, is acknowledged and does nothing: it voids no preparation the user
// made since, tells nobody, and does not reach the session layer.
//
// SEC-045
func TestReplayedEventIgnored(t *testing.T) {
	ctx := context.Background()
	e := newEventEnv(t)
	sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
	e.seed(sc, "prep_1")
	body := e.sign(e.event("evt-1"))

	rc, err := e.intake.Deliver(ctx, body)
	if err != nil || rc.Status != StatusApplied || rc.Sessions != 1 || rc.Notified != 1 || rc.EventID != "evt-1" {
		t.Fatalf("first delivery = %+v, %v", rc, err)
	}
	if e.applier.count() != 1 || e.notes.count() != 1 || !slices.Equal(e.coord.ids(), []string{"prep_1"}) || e.prepared(sc) != "" {
		t.Fatalf("first delivery: applier %d, notices %d, voids %v", e.applier.count(), e.notes.count(), e.coord.ids())
	}

	// The user asks again and a new preparation awaits confirmation.
	e.seed(sc, "prep_2")
	version := e.version(sc)

	// The same delivery again, a re-signed one, and one over HTTP.
	e.clk.Advance(time.Minute)
	for name, again := range map[string][]byte{
		"the same bytes": body,
		"re-signed":      e.sign(e.event("evt-1")),
	} {
		rc, err := e.intake.Deliver(ctx, again)
		if err != nil || rc.Status != StatusDuplicate || rc.EventID != "evt-1" || rc.Sessions != 0 {
			t.Fatalf("%s = %+v, %v", name, rc, err)
		}
	}
	if rec := post(e, body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate"`) {
		t.Fatalf("HTTP replay = %d %s", rec.Code, rec.Body)
	}
	if e.applier.count() != 1 || e.notes.count() != 1 || len(e.coord.calls) != 1 {
		t.Fatalf("a replay reached the session layer: applier %d, notices %d, voids %v", e.applier.count(), e.notes.count(), e.coord.ids())
	}
	if e.prepared(sc) != "prep_2" || e.version(sc) != version {
		t.Fatal("a replayed event voided a preparation made after the original")
	}

	// A different event id is a different event.
	if rc, err := e.intake.Deliver(ctx, e.sign(e.event("evt-2"))); err != nil || rc.Status != StatusApplied || rc.Sessions != 1 {
		t.Fatalf("a new event = %+v, %v", rc, err)
	}

	// An id is remembered per scope: the same id in another workspace is another event.
	other := e.event("evt-2")
	other.WorkspaceID = "ws-2"
	if rc, err := e.intake.Deliver(ctx, e.sign(other)); err != nil || rc.Status != StatusApplied {
		t.Fatalf("the same id in another workspace = %+v, %v", rc, err)
	}

	// Deliveries that overlap in time all end well: the event is applied (the
	// coordinator's Invalidate is idempotent, so a preparation voided twice is
	// voided), the session is told once, and nothing fails.
	e.seed(sc, "prep_3")
	concurrent := e.sign(e.event("evt-3"))
	const n = 8
	statuses := make([]Status, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := e.intake.Deliver(ctx, concurrent)
			statuses[i], errs[i] = rc.Status, err
		}()
	}
	wg.Wait()
	applied := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("delivery %d: %v", i, errs[i])
		}
		if statuses[i] == StatusApplied {
			applied++
		}
	}
	if applied == 0 || e.prepared(sc) != "" || !slices.Contains(e.coord.ids(), "prep_3") || e.notes.count() != 3 {
		t.Fatalf("overlapping deliveries: %d applied, preparation %q, voids %v, notices %d", applied, e.prepared(sc), e.coord.ids(), e.notes.count())
	}
}

// An id is remembered until the signature that carried it expires, so a delivery
// may carry only one signature: with two, a replay after the first expired would
// verify under the second and be applied again, voiding what the user prepared in
// between. The delivery with two is refused whole, before anything is checked or
// recorded, and an event whose signature has expired cannot be replayed at all.
//
// SEC-045
func TestReplayAfterTheFirstSignatureExpires(t *testing.T) {
	ctx := context.Background()
	e := newEventEnv(t)
	sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
	e.seed(sc, "prep_1")
	t0 := e.clk.Now()

	// Signature A is valid for five minutes; B, made three minutes later, for twelve
	// more. Both are within what the intake accepts.
	body := e.sign(e.event("evt-1"))
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	sigB, err := trust.SignPayload(env.Event, e.key.id, e.key.priv, t0.Add(3*time.Minute), t0.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	both := mustJSON(t, Envelope{Event: env.Event, Signatures: append(slices.Clone(env.Signatures), sigB)})

	// Delivered with both signatures at the start, it is refused unseen.
	if _, err := e.intake.Deliver(ctx, both); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Fatalf("a delivery with two signatures = %v, want schema_invalid", err)
	}
	if e.applier.count() != 0 || e.replay.count() != 0 || e.prepared(sc) != "prep_1" {
		t.Fatalf("a refused delivery reached something: applier %d, replay %d", e.applier.count(), e.replay.count())
	}

	// The event as the host sends it is applied, and recorded until A expires.
	if rc, err := e.intake.Deliver(ctx, body); err != nil || rc.Status != StatusApplied {
		t.Fatalf("delivery = %+v, %v", rc, err)
	}
	e.seed(sc, "prep_2")
	voids, version := len(e.coord.calls), e.version(sc)

	// Six minutes on, A has expired and B has not: the attacker's delivery has both.
	e.clk.Advance(6 * time.Minute)
	if _, err := e.intake.Deliver(ctx, both); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Fatalf("replay with a second signature = %v, want schema_invalid", err)
	}
	// And the captured delivery alone is expired.
	if _, err := e.intake.Deliver(ctx, body); contracts.CodeOf(err) != contracts.ErrExpired {
		t.Fatalf("replay of the captured delivery = %v, want expired", err)
	}
	if e.applier.count() != 1 || len(e.coord.calls) != voids || e.prepared(sc) != "prep_2" || e.version(sc) != version || e.notes.count() != 1 {
		t.Fatalf("a replay after the first signature expired reached the session layer: applier %d, voids %d (was %d), preparation %q",
			e.applier.count(), len(e.coord.calls), voids, e.prepared(sc))
	}
}

// The intake asks the replay cache whether an id is live by recording it with an
// expiry that has already passed, and relies on the cache not treating that as a
// presentation. This pins the reliance against the real cache: the probe neither
// records nor burns the id, a recorded id is reported as replayed, and the id is
// then refused to a probe too.
func TestReplayProbe(t *testing.T) {
	ctx := context.Background()
	e := newEventEnv(t)
	sc := serviceScope(eventDeployment, eventApp, "tenant-a", "ws-1")
	cache := e.replay.inner
	key := replayKey("evt-1")
	past := time.Unix(0, 0)
	future := e.clk.Now().Add(time.Minute)

	for i := range 3 {
		if err := cache.Consume(ctx, sc, key, past); err != nil {
			t.Fatalf("probe %d of an unseen id = %v", i, err)
		}
	}
	if err := cache.Consume(ctx, sc, key, future); err != nil {
		t.Fatalf("an id that was only probed was burned: %v", err)
	}
	if err := cache.Consume(ctx, sc, key, past); contracts.CodeOf(err) != contracts.ErrReplayed {
		t.Fatalf("probe of a recorded id = %v, want replayed", err)
	}
	if err := cache.Consume(ctx, sc, key, future); contracts.CodeOf(err) != contracts.ErrReplayed {
		t.Fatalf("second record = %v, want replayed", err)
	}
	// The key space is the intake's: a context token's nonce of the same text is
	// another record.
	if err := cache.Consume(ctx, sc, "evt-1", future); err != nil {
		t.Fatalf("an event key collided with a token nonce: %v", err)
	}
	// Once the record expires the id is free.
	e.clk.Advance(2 * time.Minute)
	if err := cache.Consume(ctx, sc, key, past); err != nil {
		t.Fatalf("probe of an expired record = %v", err)
	}
}

// A failure leaves the event to be delivered again: a preparation that cannot be
// voided leaves the session as it was and the id unrecorded, so the redelivery
// applies; a delivery that applied but could not be recorded says so, and its
// redelivery is harmless; a cache that cannot decide stops the event before it
// reaches anything.
func TestEventRedeliveryAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("the coordinator is down", func(t *testing.T) {
		e := newEventEnv(t)
		sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
		e.seed(sc, "prep_1")
		body := e.sign(e.event("evt-1"))

		e.coord.err = contracts.New(contracts.ErrUnavailable, "coordinator down")
		_, err := e.intake.Deliver(ctx, body)
		wantCode(t, err, contracts.ErrUnavailable)
		if e.prepared(sc) != "prep_1" || e.notes.count() != 0 {
			t.Fatal("a failed invalidation changed the session or told it")
		}
		if rec := post(e, body); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("HTTP status = %d, want 503 so the sender redelivers: %s", rec.Code, rec.Body)
		}

		e.coord.err = nil
		rc, err := e.intake.Deliver(ctx, body)
		if err != nil || rc.Status != StatusApplied || rc.Sessions != 1 || e.prepared(sc) != "" {
			t.Fatalf("redelivery = %+v, %v: a failed delivery was recorded as seen", rc, err)
		}
	})

	t.Run("applied but not recorded", func(t *testing.T) {
		e := newEventEnv(t)
		sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
		e.seed(sc, "prep_1")
		body := e.sign(e.event("evt-1"))
		e.replay.fail = func(n int) error {
			if n == 2 { // the record that follows the application (the first call is the probe)
				return errors.New("disk full")
			}
			return nil
		}
		_, err := e.intake.Deliver(ctx, body)
		wantCode(t, err, contracts.ErrUnavailable)
		if e.prepared(sc) != "" {
			t.Fatal("the event was not applied")
		}
		rc, err := e.intake.Deliver(ctx, body)
		if err != nil || rc.Status != StatusApplied || rc.Sessions != 0 {
			t.Fatalf("redelivery = %+v, %v, want a harmless second application", rc, err)
		}
		if rc, err := e.intake.Deliver(ctx, body); err != nil || rc.Status != StatusDuplicate {
			t.Fatalf("after it was recorded = %+v, %v", rc, err)
		}
		if len(e.coord.calls) != 1 {
			t.Fatalf("the preparation was voided %d times", len(e.coord.calls))
		}
	})

	t.Run("the replay cache cannot decide", func(t *testing.T) {
		e := newEventEnv(t)
		sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
		e.seed(sc, "prep_1")
		e.replay.fail = func(int) error { return errors.New("database is locked") }
		_, err := e.intake.Deliver(ctx, e.sign(e.event("evt-1")))
		wantCode(t, err, contracts.ErrUnavailable)
		if e.applier.count() != 0 || e.prepared(sc) != "prep_1" {
			t.Fatal("an event was applied although its id could not be checked")
		}
	})
}

// A signed event reaches the sessions it concerns through the webhook: the
// preparation is voided through the coordinator, for the application_event
// reason, under the identity it was prepared under, and the session is told.
func TestSignedEventInvalidatesSession(t *testing.T) {
	e := newEventEnv(t)
	sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
	e.seed(sc, "prep_1")

	rec := post(e, e.sign(e.event("evt-1")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var rc Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &rc); err != nil || rc != (Receipt{EventID: "evt-1", Status: StatusApplied, Sessions: 1, Notified: 1}) {
		t.Fatalf("receipt = %+v (%v): %s", rc, err, rec.Body)
	}
	if len(e.coord.calls) != 1 {
		t.Fatalf("coordinator calls = %+v", e.coord.calls)
	}
	c := e.coord.calls[0]
	if c.id != "prep_1" || c.reason != act.PreparedActionInvalidationConditionsItemApplicationEvent ||
		c.scope.PrincipalID != "user-1" || c.scope.SessionID != "ses-1" {
		t.Fatalf("void = %+v", c)
	}
	if e.prepared(sc) != "" {
		t.Fatal("the session kept its preparation")
	}
	if e.notes.count() != 1 || e.notes.notices[0].Scope.SessionID != "ses-1" || e.notes.notices[0].EventType != "title.merged" {
		t.Fatalf("notices = %+v", e.notes.notices)
	}
}

// An event cannot ask for a commit, a tool call or any other action: the kinds are
// a closed set of invalidations, a signed member this version does not know is a
// refusal, and the coordinator it is given is asked only to void.
//
// SEC-045
func TestEventCannotAskForACommit(t *testing.T) {
	ctx := context.Background()
	e := newEventEnv(t)
	sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
	e.seed(sc, "prep_1")

	for _, kind := range []string{"commit", "confirm", "prepare", "tool_call", "timeout", "", "APPLICATION_EVENT"} {
		ev := e.event("evt-" + kind)
		ev.Kind = kind
		ev.Type = "payment.commit"
		if _, err := e.intake.Deliver(ctx, e.sign(ev)); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("kind %q = %v, want validation_failed", kind, err)
		}
	}
	if e.applier.count() != 0 || e.prepared(sc) != "prep_1" {
		t.Fatal("an event that asked for something else reached the session layer")
	}

	// Every kind that is accepted only invalidates: the coordinator is asked once
	// per event to void the preparation, and is never asked to do anything else
	// (its other methods fail the test).
	for i, ev := range []Event{
		{Kind: "application_event", Everything: true},
		{Kind: "record_revision", Entities: []Entity{{EntityType: "title", EntityID: "t1", Version: "v9"}}},
		{Kind: "permission_change", Everything: true},
		{Kind: "principal_change", PrincipalID: "user-1"},
	} {
		ev.ID, ev.Type, ev.ApplicationID, ev.TenantID, ev.WorkspaceID = "evt-ok-"+strconv.Itoa(i), "any.thing", eventApp, "tenant-a", "ws-1"
		prep := "prep_k" + strconv.Itoa(i)
		e.seed(sc, prep)
		rc, err := e.intake.Deliver(ctx, e.sign(ev))
		if err != nil || rc.Status != StatusApplied || rc.Sessions != 1 {
			t.Fatalf("kind %s = %+v, %v", ev.Kind, rc, err)
		}
		if len(e.coord.calls) != i+1 || e.coord.calls[i].id != prep {
			t.Fatalf("kind %s: coordinator calls = %+v", ev.Kind, e.coord.calls)
		}
	}
}

// The webhook is a POST of application/json, bounded, and the refusals have
// statuses a sender can act on.
func TestHandlerRefusals(t *testing.T) {
	e := newEventEnv(t)
	body := e.sign(e.event("evt-1"))

	rec := httptest.NewRecorder()
	e.intake.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}

	if rec := post(e, body, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain = %d", rec.Code)
	}
	if rec := post(e, body, func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }); rec.Code != http.StatusOK {
		t.Fatalf("application/json with a charset = %d: %s", rec.Code, rec.Body)
	}

	small := newEventEnv(t, func(e *eventEnv) { e.cfg.MaxBodyBytes = 64 })
	if rec := post(small, body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the limit = %d", rec.Code)
	}

	if rec := post(e, []byte("{")); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d", rec.Code)
	}

	// An event for another application is a 403 and its error names no key and no
	// application of ours; an unsigned one is a 401 with a reason from a closed list.
	other := e.event("evt-9")
	other.ApplicationID = eventOtherApp
	if rec := post(e, e.sign(other)); rec.Code != http.StatusForbidden {
		t.Fatalf("another application = %d: %s", rec.Code, rec.Body)
	}
	unsigned, _ := json.Marshal(Envelope{Event: mustJSON(t, e.event("evt-9"))})
	rec = post(e, unsigned)
	var env contracts.Error
	if rec.Code != http.StatusUnauthorized || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Code != contracts.ErrUnauthenticated {
		t.Fatalf("unsigned = %d: %s", rec.Code, rec.Body)
	}
	e.key.id = "unregistered"
	rec = post(e, e.sign(e.event("evt-10")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "unknown_key") || strings.Contains(rec.Body.String(), "unregistered") {
		t.Fatalf("unknown key = %d: %s (the closed reason, not the attacker's key id)", rec.Code, rec.Body)
	}
}

// An MCP notification carries the same signed delivery, through the same
// checks. A method that is not the event notification is not an event.
func TestMCPNotificationUsesTheSamePath(t *testing.T) {
	ctx := context.Background()
	e := newEventEnv(t)
	sc := e.scope("tenant-a", "ws-1", "user-1", "ses-1")
	e.seed(sc, "prep_1")

	_, err := e.intake.DeliverNotification(ctx, "notifications/tools/list_changed", e.sign(e.event("evt-1")))
	wantCode(t, err, contracts.ErrValidationFailed)
	_, err = e.intake.DeliverNotification(ctx, MCPNotificationMethod, json.RawMessage(`{"event":{"id":"evt-1"}}`))
	wantCode(t, err, contracts.ErrUnauthenticated)
	if e.applier.count() != 0 || e.prepared(sc) != "prep_1" {
		t.Fatal("an unsigned or mislabelled notification reached the session layer")
	}

	body := e.sign(e.event("evt-1"))
	rc, err := e.intake.DeliverNotification(ctx, MCPNotificationMethod, body)
	if err != nil || rc.Status != StatusApplied || e.prepared(sc) != "" {
		t.Fatalf("notification = %+v, %v", rc, err)
	}
	if rc, err := e.intake.DeliverNotification(ctx, MCPNotificationMethod, body); err != nil || rc.Status != StatusDuplicate {
		t.Fatalf("replayed notification = %+v, %v", rc, err)
	}
}

// An intake is not built from a configuration that leaves out anything it depends
// on: there is no default that accepts events.
func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	e := newEventEnv(t)
	for name, mutate := range map[string]func(*Config){
		"deployment":  func(c *Config) { c.DeploymentID = "" },
		"application": func(c *Config) { c.ApplicationID = "" },
		"verifier":    func(c *Config) { c.Verifier = nil },
		"replay":      func(c *Config) { c.Replay = nil },
		"sessions":    func(c *Config) { c.Sessions = nil },
		"invalidator": func(c *Config) { c.Invalidator = nil },
		"scope":       func(c *Config) { c.DeploymentID = "dep *" },
	} {
		cfg := e.cfg
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: an intake was built without it", name)
		}
	}
}

// adapters/events is one of the concrete adapters of docs/architecture/module-map.md
// section 3: it may import the base packages, internal/trust, internal/identity and
// internal/sessions, and nothing that commits, calls a tool, streams, or reaches
// another adapter. It starts no process and opens no listener.
func TestImportsStayWithinMap(t *testing.T) {
	const mod = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts": true, "internal/clock": true, "internal/ids": true, "internal/config": true, "internal/log": true,
		"internal/trust": true, "internal/identity": true, "internal/sessions": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 5 {
		t.Fatalf("found %d files (%v)", len(files), err)
	}
	for _, f := range files {
		isTest := strings.HasSuffix(f, "_test.go")
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || p == "plugin" || p == "unsafe" || p == "net" {
				t.Errorf("%s imports %s", f, p)
			}
			if !strings.HasPrefix(p, mod) {
				continue
			}
			rel := strings.TrimPrefix(p, mod)
			switch {
			case strings.HasPrefix(rel, "pkg/contracts"):
			case allowed[rel]:
			case isTest && (rel == "internal/store" || rel == "internal/store/storetest" || rel == "internal/capabilities"):
				// test support: a real database and a goal in the prepared phase
			default:
				t.Errorf("%s imports %s, which the module map does not allow for adapters/events", f, rel)
			}
		}
	}
}
