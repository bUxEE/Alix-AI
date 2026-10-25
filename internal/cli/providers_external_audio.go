//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// extAudioEgress backs extmodal.AudioEgress with the data-egress guard: the utterance is evaluated as a one-part
// candidate (an audio part of the class its session states, for the speech-to-text role) against the registry
// profile of the target, so the kill switch, the policy chain, the audio setting of the egress policy (off by default)
// and the allowlists decide, and the decision is recorded like any other. Raw audio is never stored, so it is not a
// file: the part names it by a reference minted for this check alone, which nothing resolves, and the bytes go to
// the recogniser directly once the decision allows them. A nil error means the guard allowed the audio.
type extAudioEgress struct{ guard dataegress.Guard }

var _ extmodal.AudioEgress = extAudioEgress{}

// AllowAudio implements extmodal.AudioEgress. It returns the envelope and the decision of the guard, which the
// budget reservation of the call belongs to.
func (e extAudioEgress) AllowAudio(ctx context.Context, scope contracts.Scope, t extmodal.Target, a extmodal.Audio) (extmodal.TextDecision, error) {
	if e.guard == nil {
		return extmodal.TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "the data-egress guard is not wired")
	}
	res, err := e.guard.Evaluate(ctx, dataegress.Candidate{
		Scope: scope, Profile: t.Profile, Role: pv.RoleSTT, Channel: pv.ChannelInteractive, TraceID: "trc_extmodal",
		Instructions: "Speech recognition check.",
		Messages: []dataegress.Message{{
			Role: pv.RequestEnvelopeMessageRoleUser,
			Parts: []dataegress.Part{{
				Kind: dataegress.PartAudio, FileID: ids.New("fil"), SizeBytes: a.Bytes, Category: string(pv.SendCategoryAudioContent), Class: a.Class,
			}},
		}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  1, TimeoutMS: 1000,
	})
	if err != nil {
		return extmodal.TextDecision{}, err
	}
	if res.Envelope == nil || len(res.Parts) != 1 || !res.Parts[0].Kept {
		return extmodal.TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "the data-egress guard did not keep this audio").WithDetails("reason", "audio_dropped")
	}
	return extmodal.TextDecision{EnvelopeID: res.Envelope.EnvelopeID, DecisionID: res.Decision.DecisionID}, nil
}
