//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Spoken text is attested for every reply (M9-02).
//
// External text to speech speaks only text that was attested as a verified response (extmodal.Service.Attest). Until
// this file, only the turns the Hybrid realiser took were attested (extFinalAttestTurn), because the dialogue core had
// no hook after its own verify.Realise: a reply written by the engine's local path, a refusal or a failure template
// was never speakable externally. The core now has the hook (dialogue.Deps.OnReply, nil by default). The engine
// builder of `alixai serve` is wrapped here so that it is built with attestOnReply, which attests the outcome the
// caller of the turn receives: the model text that passed verify.Realise, or the deterministic template that replaced
// it. Nothing else reaches Attest, and Attest itself refuses an outcome that is not a verified response.
//
// With external mode off the voice stack is not composed (extFinalVoiceHolder is nil) and the hook returns at once;
// the engine's reply is unchanged either way, because the hook only reads. This file is not compiled into a
// noexternal binary, which has no hook and no external speech at all.

func init() {
	serveProfileNewEngine = attestEngineWithHook(serveProfileNewEngine)
}

// attestEngineWithHook wraps an engine builder so that the engine is built with attestOnReply. A Deps that already has
// a hook keeps it: the composition that set it decided.
func attestEngineWithHook(next func(dialogue.Deps) (*dialogue.Engine, error)) func(dialogue.Deps) (*dialogue.Engine, error) {
	return func(d dialogue.Deps) (*dialogue.Engine, error) {
		if d.OnReply == nil {
			d.OnReply = attestOnReply
		}
		return next(d)
	}
}

// attestOnReply is the dialogue.ReplyHook of the process. The voice stack is looked up per call: it is composed after
// the engine is built. A refusal by Attest (an outcome that is not a verified response) leaves the text unspeakable;
// it is logged by code only.
func attestOnReply(_ context.Context, scope contracts.Scope, out verify.Outcome) {
	v := extFinalVoiceHolder.Load()
	if v == nil {
		return
	}
	if err := v.svc.Attest(scope, out); err != nil {
		v.log.Debug("providers: a reply was not attested for external speech", "reason", string(contracts.CodeOf(err)))
	}
}
