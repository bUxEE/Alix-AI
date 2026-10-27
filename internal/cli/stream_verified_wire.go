package cli

import (
	"context"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// streamVerifiedHub is the process hub used by OnVerifiedSegment (ADR 0046).
// turneventsProvide stores it; the hook publishes mid-turn when StreamVerified is on.
var streamVerifiedHub atomic.Pointer[stream.Hub]

func init() {
	serveProfileNewEngine = streamVerifiedEngine(serveProfileNewEngine)
}

func streamVerifiedEngine(next func(dialogue.Deps) (*dialogue.Engine, error)) func(dialogue.Deps) (*dialogue.Engine, error) {
	return func(d dialogue.Deps) (*dialogue.Engine, error) {
		d.Config.StreamVerified = dialogue.Loaded().StreamVerified
		if d.OnVerifiedSegment == nil {
			d.OnVerifiedSegment = publishVerifiedSegment
		}
		return next(d)
	}
}

func publishVerifiedSegment(_ context.Context, scope contracts.Scope, step dialogue.VerifiedSegment) {
	hub := streamVerifiedHub.Load()
	if hub == nil || step.TurnID == "" {
		return
	}
	turn, err := hub.Lookup(scope, step.TurnID)
	if err != nil {
		turn, err = hub.Begin(scope, step.TurnID)
		if err != nil {
			return
		}
	}
	if step.Replace {
		_ = turn.PublishTextReplaced(step.Text)
		return
	}
	_ = turn.PublishVerifiedSegment(step.Text)
}
