package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/stream"
)

// The stream hub of the turn-events area (internal/gateway/routes_v1_turnevents.go
// and the POST turn handler in routes_v1_sessions.go). The hub holds the events
// of each turn in a bounded replay buffer so a client that lost its connection
// resumes from a cursor without the turn being run again; its limits (events and
// bytes per turn, turns and bytes per hub and per tenant, retention) are the
// package defaults, which keep it inside a few tens of megabytes however many
// turns are served. It is built here because the process owns the clock and the
// logger, and it is provided as *stream.Hub: both areas that use it ask for that
// type, so a server that has the hub for one and not the other does not start.

func init() { registerServeProvider("turnevents", turneventsProvide) }

// turneventsProvide builds the process's stream hub and gives it to the
// gateway's service registry. The hub logs counts and ids only, never event
// text.
func turneventsProvide(_ context.Context, d serveDeps) error {
	hub := stream.NewHub(stream.Config{Clock: d.Clock, Logger: d.Log})
	return gateway.Provide(d.Services, hub)
}
