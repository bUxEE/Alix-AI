package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/buxee/alix-ai/adapters/events"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The events area (internal/gateway/routes_v1_events.go): a host application's
// signed events reach the sessions they concern (master plan 9.3 item 4, 11.3).
// This file is the composition root's half of it, because the pieces may not
// know each other: adapters/events (the signed intake) imports neither the
// gateway nor the stream hub, internal/sessions does not know the stream, and
// the gateway imports no adapter.
//
//   - eventsIntakes gives the gateway the intake of an application: an
//     events.Intake bound to that application (the intake itself refuses the
//     platform application, whose keys sign AlixAI's own artifacts, and so does
//     this file before building one), over the trust store the application's
//     keys are registered in, the durable replay cache, the session store and the
//     coordinator narrowed to the one thing an event may do to it, Invalidate.
//     The intake of an application is built on its first event and kept.
//   - eventsNotifier is the sessions.Notifier: it opens the notice turn of a
//     session in the stream hub under the session's own scope, publishes the
//     notice events at seq 0 and 1 and then announces the turn on the session's
//     feed (gateway.EventsFeed), which is how a connected client learns the turn
//     id it could never know. A notice that cannot be delivered is counted by the
//     intake and never undoes the invalidation.
//   - transport_mcp_events.go forwards the MCP notification of an application's
//     server to the same intakes.
//
// Providers run in name order and "events" sorts before "turnevents", so the
// hub, the session store and the coordinator are looked up at the first event,
// not here; the area is provided whatever the deployment's identity
// configuration, and an event is refused (unavailable) while
// identity.deployment_id is not configured, like every other /v1 request.

func init() { registerServeProvider("events", eventsProvide) }

// eventsProvide builds the process's intakes and announcement feed and gives them
// to the gateway's service registry.
func eventsProvide(_ context.Context, d serveDeps) error {
	if d.DB == nil || d.Clock == nil || d.Services == nil {
		return errors.New("events: the provider needs the database, the clock and the service registry")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	feed := gateway.NewEventsFeed(gateway.EventsFeedConfig{Clock: d.Clock})
	in := &eventsIntakes{
		log: log, feed: feed,
		verifier:   trust.NewVerifier(trust.NewStore(d.DB, d.Clock), d.Clock),
		replay:     identity.NewSQLiteReplayCache(d.DB, d.Clock, log),
		deployment: func() string { return identity.CurrentConfig().DeploymentID },
		sessionsOf: func() (*sessions.Store, bool) { return gateway.Lookup[*sessions.Store](d.Services) },
		hubOf:      func() (*stream.Hub, bool) { return gateway.Lookup[*stream.Hub](d.Services) },
		invalidatorOf: func() (sessions.Invalidator, bool) {
			as, ok := gateway.Lookup[gateway.ActionService](d.Services)
			if !ok {
				return nil, false
			}
			return eventsInvalidator(as.Invalidate), true
		},
		activeOf: func(ctx context.Context, applicationID string) error {
			apps, ok := gateway.Lookup[gateway.ApplicationService](d.Services)
			if !ok {
				return contracts.New(contracts.ErrUnavailable, "events: the application service is not available")
			}
			_, err := apps.Package(ctx, applicationID)
			return err
		},
	}
	if err := gateway.Provide[gateway.EventsIntakes](d.Services, in); err != nil {
		return err
	}
	if err := gateway.Provide(d.Services, feed); err != nil {
		return err
	}
	eventsMCPBind(in)
	return nil
}

// eventsInvalidator narrows the coordinator to Invalidate: the one effectful
// thing an event can reach. Nothing that holds it can prepare, confirm or commit.
type eventsInvalidator func(ctx context.Context, scope contracts.Scope, preparationID string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error)

// Invalidate implements sessions.Invalidator.
func (f eventsInvalidator) Invalidate(ctx context.Context, scope contracts.Scope, preparationID string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	return f(ctx, scope, preparationID, reason)
}

// eventsIntakes holds the intake of each application that has delivered an
// event. It implements gateway.EventsIntakes.
type eventsIntakes struct {
	log      *slog.Logger
	feed     *gateway.EventsFeed
	verifier *trust.Verifier
	replay   identity.ReplayCache
	// deployment is the deployment id of the configuration, read when an intake is
	// built; empty refuses (unavailable).
	deployment func() string
	// The services other providers give, looked up when an intake is built (or,
	// for the hub, when a session is told).
	sessionsOf    func() (*sessions.Store, bool)
	hubOf         func() (*stream.Hub, bool)
	invalidatorOf func() (sessions.Invalidator, bool)
	// activeOf says whether the application serves traffic (a quarantined one does
	// not); the route asks the application service itself, the MCP forwarder asks
	// here.
	activeOf func(ctx context.Context, applicationID string) error

	mu    sync.Mutex
	byApp map[string]*events.Intake
}

var _ gateway.EventsIntakes = (*eventsIntakes)(nil)

// EventsHandler implements gateway.EventsIntakes: the handler of the
// application's intake.
func (in *eventsIntakes) EventsHandler(ctx context.Context, applicationID string) (http.Handler, error) {
	it, err := in.intake(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	return it.Handler(), nil
}

// intake returns the intake bound to the application, building it on first use.
// It refuses the platform application and an installation without a deployment
// id, and builds nothing while a service it needs is not there.
func (in *eventsIntakes) intake(_ context.Context, applicationID string) (*events.Intake, error) {
	if applicationID == "" {
		return nil, contracts.New(contracts.ErrScopeMissing, "events: no application")
	}
	if applicationID == trust.PlatformApplicationID {
		return nil, contracts.New(contracts.ErrPermissionDenied, "events: the platform application does not deliver events")
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if it := in.byApp[applicationID]; it != nil {
		return it, nil
	}
	dep := in.deployment()
	if dep == "" {
		return nil, contracts.New(contracts.ErrUnavailable, "events: identity.deployment_id is not configured")
	}
	store, ok := in.sessionsOf()
	if !ok {
		return nil, contracts.New(contracts.ErrUnavailable, "events: the session store is not available")
	}
	ivr, ok := in.invalidatorOf()
	if !ok {
		return nil, contracts.New(contracts.ErrUnavailable, "events: the action service is not available")
	}
	it, err := events.New(events.Config{
		DeploymentID: dep, ApplicationID: applicationID, Verifier: in.verifier, Replay: in.replay,
		Sessions: store, Invalidator: ivr, Notifier: &eventsNotifier{hubOf: in.hubOf, feed: in.feed}, Logger: in.log,
	})
	if err != nil {
		return nil, err
	}
	if in.byApp == nil {
		in.byApp = map[string]*events.Intake{}
	}
	in.byApp[applicationID] = it
	return it, nil
}

// eventsNotifier tells a session an event changed it: the notice turn goes into
// the hub under the session's own scope, then the session is announced.
type eventsNotifier struct {
	hubOf func() (*stream.Hub, bool)
	feed  *gateway.EventsFeed
}

var _ sessions.Notifier = (*eventsNotifier)(nil)

// Notify opens the notice turn in the hub, publishes its events (seq 0 and 1, as
// the session layer built them) and announces the turn. The turn is announced
// only after it is complete in the hub, so a client that follows the announcement
// finds the whole notice.
func (n *eventsNotifier) Notify(_ context.Context, notice sessions.Notice) error {
	hub, ok := n.hubOf()
	if !ok {
		return contracts.New(contracts.ErrUnavailable, "events: the stream hub is not available")
	}
	turnID, err := eventsNoticeTurnID(notice.Events)
	if err != nil {
		return err
	}
	turn, err := hub.Begin(notice.Scope, turnID)
	if err != nil {
		return err
	}
	for _, ev := range notice.Events {
		if err := turn.Publish(ev); err != nil {
			_ = turn.Fail(err)
			return err
		}
	}
	_, err = n.feed.Announce(notice.Scope, gateway.EventsAnnouncement{
		EventID: notice.EventID, EventType: notice.EventType, Kind: string(notice.Kind), TurnID: turnID,
	}, hub.Config().Retention)
	return err
}

// eventsNoticeTurnID is the turn id the notice's events carry: its first event is
// the stale error. The hub refuses an event of any other turn.
func eventsNoticeTurnID(evs []conversation.TurnEvent) (string, error) {
	if len(evs) == 0 {
		return "", contracts.New(contracts.ErrInternal, "events: a notice carries no events")
	}
	ev, ok := evs[0].Value.(conversation.ErrorEvent)
	if !ok || ev.TurnID == "" {
		return "", contracts.New(contracts.ErrInternal, "events: a notice starts with its error event")
	}
	return string(ev.TurnID), nil
}
