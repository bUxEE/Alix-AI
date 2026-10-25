package dialogue

import (
	"context"

	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ResponseRealiser is the one narrow seam of the response step (external provider mode, Hybrid
// production mode). A composition root that realises replies with something other than the engine's own
// local call implements it on the value it gives as Deps.Providers; the engine asks it once per turn,
// after it has decided that the act is one a model has something to add to and before it builds its own
// response prompt.
//
// The default is no realiser: LocalSelector does not implement it, so every existing path is unchanged,
// and this package imports no external-provider package. The realiser is trusted code of the
// composition root but its result is not taken on trust: a model text is verified again here against
// the directive and the receipt, and a template outcome is replaced by the deterministic template of
// the same directive, so a buggy realiser cannot put unverified model text, or other facts, on screen.
type ResponseRealiser interface {
	// RealiseResponse writes the response of a turn. handled=false means "not mine": the engine goes on
	// with its own local path as if there were no realiser. An error means the realisation could not be
	// produced; the engine shows the deterministic template (the realiser's own fallbacks are inside it).
	RealiseResponse(ctx context.Context, in ResponseInput) (out verify.Outcome, handled bool, err error)
}

// ResponseInput is what a ResponseRealiser is given: the objects the response must be faithful to, never
// a prompt, the user's text or the session.
type ResponseInput struct {
	Scope contracts.Scope
	// Directive and Receipt are the objects the core built (the receipt is nil unless the directive
	// names one). They are shared with the engine: a realiser must not modify them.
	Directive *conversation.DialogueDirective
	Receipt   *actions.ActionReceipt
	// Options carry the response language and the application's field labels.
	Options verify.Options
}

// responseRealiser is the realiser of the engine, nil when its provider selector has none.
func (e *Engine) responseRealiser() ResponseRealiser {
	r, _ := e.providers.(ResponseRealiser)
	return r
}

// ReplyHook is the one nil-by-default hook after the engine's own response step (M9-02). The engine calls it once
// for a turn that is acknowledged to the caller, with the scope of the turn and the verify.Outcome that is
// Result.Response: the text verify.Realise accepted (a local, an external or a Hybrid model), or the deterministic
// template that replaced it. It exists so a composition root can record which texts are verified replies (external
// speech speaks only those, internal/extmodal) for EVERY turn, not only for the turns a ResponseRealiser took.
//
// The hook receives the outcome only as data: it cannot change the reply, fail the turn or delay it past its own
// return, and a panic in it is recovered and logged by code, never as text. It is not called for a turn that returns
// an error or whose response could not be produced, so a reply that is not delivered is not announced as verified.
// With the hook nil (the default) the engine behaves exactly as before and this package imports nothing for it.
type ReplyHook func(ctx context.Context, scope contracts.Scope, out verify.Outcome)

// announceReply calls Deps.OnReply for the reply of an acknowledged turn.
func (t *turn) announceReply(ctx context.Context, out verify.Outcome) {
	h := t.e.onReply
	if h == nil || out.Text == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			t.e.log.Error("reply hook panicked", "turn_id", t.id)
		}
	}()
	h(ctx, t.scope.Clone(), out)
}
