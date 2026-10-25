package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The turn-events area is provided by a serve provider that registers itself,
// builds the process's hub on the process's clock and gives it to the registry
// by the type both areas that use it ask for.
func TestTurneventsProviderGivesTheHubTheProcessClock(t *testing.T) {
	if _, byName := registeredServeProviders(); byName["turnevents"] == nil {
		t.Fatal("the turnevents provider did not register itself")
	}
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	svc := gateway.NewServices()
	deps := serveDeps{Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Clock: clk, Services: svc}
	if err := turneventsProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	hub, ok := gateway.Lookup[*stream.Hub](svc)
	if !ok {
		t.Fatal("the hub is not in the registry under *stream.Hub")
	}
	if err := turneventsProvide(context.Background(), deps); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second hub for one registry = %v, want %s", err, contracts.ErrConflict)
	}

	// The hub runs on the process clock: a finished turn is forgotten once its
	// retention has passed on that clock, and not before.
	scope := contracts.Scope{
		DeploymentID: "dep_cli", ApplicationID: "app_demo", TenantID: "ten_acme", WorkspaceID: "wsp_main",
		PrincipalID: "usr_ada", Roles: []string{"member"}, CapabilityScopes: []string{"items.get"}, SessionID: "ses_one",
	}
	const id = "trn_01J0000000000000000000A001"
	turn, err := hub.Begin(scope, id)
	if err != nil {
		t.Fatal(err)
	}
	done := conversation.DoneEvent{TurnID: act.TurnID(id), Seq: 0, FinishReason: conversation.DoneEventFinishReasonCompleted}
	if err := turn.Publish(conversation.TurnEvent{Value: done}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Lookup(scope, id); err != nil {
		t.Fatalf("a finished turn is not retained: %v", err)
	}
	clk.Advance(hub.Config().Retention + time.Second)
	if _, err := hub.Lookup(scope, id); stream.ReasonOf(err) != stream.ReasonNotVisible {
		t.Fatalf("a turn past its retention on the process clock = %v, want turn_not_visible", err)
	}
}

// serve mounts the turn-events route beside the others, behind the identity
// middleware, with the hub provided by the composition root: a request with no
// token is refused first, and one with a token reaches the handler, which asks
// whether the application is active before it looks at the turn.
func TestServeMountsTheTurnEventsRoute(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, _ := serveConfig(t, f, identityYAML)
	provisionIssuer(t, cfg, iss)
	srv := startServe(t, cfg)

	path := "/v1/sessions/" + identitytest.DefaultSession + "/turns/trn_01J0000000000000000000A001/events"
	if code, body := srv.call(t, nil, http.MethodGet, path, ""); code != http.StatusUnauthorized {
		t.Fatalf("GET %s without a token = %d: %s", path, code, body)
	}
	code, body := srv.call(t, iss, http.MethodGet, path, "")
	var env contracts.Error
	if err := json.Unmarshal(body, &env); err != nil || code != http.StatusForbidden || env.Code != contracts.ErrQuarantined {
		t.Fatalf("GET %s for an application with no package = %d: %s (a 404 would mean the route is not mounted)", path, code, body)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}
