package cli

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/buxee/alix-ai/adapters/events"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The MCP notification of an application event (master plan 9.3 item 4): an
// application's MCP server may send the notification events.MCPNotificationMethod
// ("notifications/alixai/event") whose params are the delivery of adapters/events,
// the same signed {event, signatures} object POST /v1/events takes. The adapters
// may not import each other (module-map rule 8), so adapters/mcp does not know the
// events adapter and the forwarding is the composition root's: eventsMCPForward
// hands the params to the intake bound to the application of the MCP connection
// (Intake.DeliverNotification).
//
// What that is and is not:
//
//   - The connection says which application the notification came from, not that
//     it is genuine: the intake verifies the signature like on the HTTP route, so
//     an MCP server that is compromised, or a notification that was forged on the
//     way, still needs a key registered for the application. The event's own
//     application must be the connection's, and its scope is the signed payload's.
//   - Only events.MCPNotificationMethod is forwarded; any other method is refused
//     before an intake is looked up. An application that does not serve traffic (a
//     quarantined one) has its notifications refused.
//   - The notification is an event like any other: it can only invalidate. Nothing
//     here can prepare, confirm or commit an action or call a tool.
//
// What is missing (the next task): the client of adapters/mcp (the go-sdk
// session) has a fixed table of the methods it receives and rejects any other as
// not handled, so no server notification reaches this function yet. The adapter
// needs a receive hook for this one method (and transport_mcp.go has to pass
// eventsMCPForward to it, with the application id of the Config); neither is part
// of this task's scope.

// eventsMCPSink is the intakes the forwarder delivers to, bound by the events
// provider.
var eventsMCPSink atomic.Pointer[eventsIntakes]

// eventsMCPBind makes in the target of eventsMCPForward.
func eventsMCPBind(in *eventsIntakes) { eventsMCPSink.Store(in) }

// eventsMCPForward delivers an MCP notification that arrived on the connection of
// the application to its intake and returns the receipt. An error means the
// event was not applied (the same errors, and the same redelivery rule, as the
// HTTP route).
func eventsMCPForward(ctx context.Context, applicationID, method string, params json.RawMessage) (events.Receipt, error) {
	if method != events.MCPNotificationMethod {
		return events.Receipt{}, contracts.New(contracts.ErrValidationFailed, "events: not an event notification")
	}
	in := eventsMCPSink.Load()
	if in == nil {
		return events.Receipt{}, contracts.New(contracts.ErrUnavailable, "events: the event intake is not available")
	}
	if applicationID == "" {
		return events.Receipt{}, contracts.New(contracts.ErrScopeMissing, "events: the notification names no application")
	}
	if err := in.activeOf(ctx, applicationID); err != nil {
		return events.Receipt{}, err
	}
	it, err := in.intake(ctx, applicationID)
	if err != nil {
		return events.Receipt{}, err
	}
	return it.DeliverNotification(ctx, method, params)
}
