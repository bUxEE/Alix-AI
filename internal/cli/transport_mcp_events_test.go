package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/events"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// A notification of the MCP method an application's server sends reaches the intake bound to the
// application of the connection: it is verified like an HTTP delivery, applied
// (the session is invalidated and its client is announced to), and a
// notification that is not the event method, is unsigned, is for another
// application, or arrives for an application that serves nothing is refused and
// touches nothing. The strict coordinator of the rig fails the test on anything
// but Invalidate, so the forwarding cannot commit either.
func TestMCPNotificationIsForwardedToTheIntake(t *testing.T) {
	r := eventsNewRig(t, true)
	sc := r.seed("ses_one", "prep_1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := r.feed.Subscribe(sc, stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The refusals first: nothing is applied by any of them.
	signed := r.delivery(r.event("evt_mcp_1"))
	var env map[string]json.RawMessage
	if err := json.Unmarshal(signed, &env); err != nil {
		t.Fatal(err)
	}
	unsigned, _ := json.Marshal(map[string]any{"event": env["event"], "signatures": []any{}})
	foreign := r.event("evt_mcp_2")
	foreign.ApplicationID = "app_other"
	for _, tc := range []struct {
		name        string
		app, method string
		params      []byte
		want        contracts.ErrCode
	}{
		{"another method", eventsTestApp, "notifications/tools/list_changed", signed, contracts.ErrValidationFailed},
		{"no application on the connection", "", events.MCPNotificationMethod, signed, contracts.ErrScopeMissing},
		{"the platform application", trust.PlatformApplicationID, events.MCPNotificationMethod, signed, contracts.ErrPermissionDenied},
		{"an application that serves nothing", "app_quarantined", events.MCPNotificationMethod, signed, contracts.ErrQuarantined},
		{"no signature", eventsTestApp, events.MCPNotificationMethod, unsigned, contracts.ErrUnauthenticated},
		{"a connection of an application whose key did not sign it", "app_second", events.MCPNotificationMethod, signed, contracts.ErrUnauthenticated},
		{"an event for another application, genuinely signed", eventsTestApp, events.MCPNotificationMethod, r.delivery(foreign), contracts.ErrScopeViolation},
		{"params that are not a delivery", eventsTestApp, events.MCPNotificationMethod, []byte(`"event"`), contracts.ErrSchemaInvalid},
	} {
		if _, err := eventsMCPForward(ctx, tc.app, tc.method, tc.params); contracts.CodeOf(err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.want)
		}
	}
	if got := r.prepared(sc); got != "prep_1" || len(r.coord.voided()) != 0 {
		t.Fatalf("a refused notification changed the session: references %q, voided %v", got, r.coord.voided())
	}

	// The signed notification is applied and its client is told.
	receipt, err := eventsMCPForward(ctx, eventsTestApp, events.MCPNotificationMethod, signed)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != events.StatusApplied || receipt.EventID != "evt_mcp_1" || receipt.Sessions != 1 || receipt.Notified != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
	a, err := client.Next(ctx)
	if err != nil || a.EventID != "evt_mcp_1" || a.TurnID == "" {
		t.Fatalf("announcement = %+v, %v", a, err)
	}
	if got := r.prepared(sc); got != "" {
		t.Errorf("the session still references %q", got)
	}
	if got := r.coord.voided(); len(got) != 1 || got[0] != "prep_1" {
		t.Errorf("voided = %v, want prep_1 and nothing else", got)
	}

	// The same notification again is a duplicate.
	if again, err := eventsMCPForward(ctx, eventsTestApp, events.MCPNotificationMethod, signed); err != nil || again.Status != events.StatusDuplicate {
		t.Errorf("a repeated notification = %+v, %v", again, err)
	}

	// With no intakes bound (a server that never started the area) it is unavailable.
	eventsMCPSink.Store(nil)
	if _, err := eventsMCPForward(ctx, eventsTestApp, events.MCPNotificationMethod, signed); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("with no intake bound = %v, want %s", err, contracts.ErrUnavailable)
	}
}
