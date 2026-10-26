//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/voice/dictation"
)

// dictationExternalLayer reports whether routing.stt names an external profile.
func dictationExternalLayer() (routed bool, providerHint string) {
	if !externalSection.Enabled {
		return false, ""
	}
	r := externalSection.Routing["stt"]
	if len(r.Profiles) == 0 {
		return false, ""
	}
	return true, "external"
}

// dictationExternalOnlySTT is the recogniser when there is no local pack: the
// external STT gate with a nil local.
func dictationExternalOnlySTT() voice.STT { return extFinalSTT{} }

// dictationPrepareExternal marks a user-recorded clip for the external STT gate.
func dictationPrepareExternal(ctx context.Context) context.Context {
	class := dictation.CurrentConfig().EffectiveAudioClass()
	return extmodal.WithUserSegmented(extmodal.WithAudioClass(ctx, class))
}
