package cli

import (
	"context"
	"log/slog"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/voice/dictation"
	"github.com/buxee/alix-ai/internal/voice/profile"
	voicegen "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// Dictation (record-to-text) composition root: provides gateway.TranscriptionService
// when an STT layer is available (local pack and/or external STT route).

func init() { registerServeProvider("dictation", dictationServeProvide) }

func dictationServeProvide(_ context.Context, d serveDeps) error {
	cfg := dictation.CurrentConfig()
	stt, locality, provider, external := dictationResolveSTT(d)
	langs := []voicegen.Language{}
	if pack, ok := dictationActivePack(); ok {
		langs = append(langs, voicegen.Language(pack.Language))
	}
	if len(langs) == 0 && stt != nil {
		langs = []voicegen.Language{"en"}
	}
	deps := dictation.Deps{
		STT: stt, Locality: locality, Provider: provider, External: external,
		Languages: langs, MaxSeconds: cfg.EffectiveMaxSeconds(),
		Logger: d.Log,
		Observe: func(e dictation.Event) {
			if d.Log == nil {
				return
			}
			d.Log.Info("dictation",
				slog.Int64("duration_ms", e.DurationMS),
				slog.String("locality", e.Locality),
				slog.String("provider", e.Provider),
				slog.String("outcome", e.Outcome),
			)
		},
	}
	if external {
		deps.PrepareCtx = dictationPrepareExternal
	}
	svc := dictation.New(deps)
	return gateway.Provide[gateway.TranscriptionService](d.Services, svc)
}

func dictationActivePack() (profile.Pack, bool) {
	id := profile.CurrentConfig().ActivePack
	if id == "" {
		return profile.Pack{}, false
	}
	manifests, err := voiceShippedManifests()
	if err != nil {
		return profile.Pack{}, false
	}
	m, ok := manifests.Lookup(id)
	if !ok {
		return profile.Pack{}, false
	}
	return m.Pack(), true
}

// dictationResolveSTT picks the recogniser and locality. External routing is
// resolved through dictationExternalLayer (build-tagged).
func dictationResolveSTT(d serveDeps) (stt voice.STT, locality, provider string, external bool) {
	if d.Providers != nil {
		if s, err := voice.LookupSTT(d.Providers); err == nil {
			stt = s
		}
	}
	extRoute, hint := dictationExternalLayer()
	switch {
	case stt != nil && extRoute:
		return stt, dictation.LocalityExternal, hint, true
	case stt != nil:
		return stt, dictation.LocalityLocal, "", false
	case extRoute:
		if s := dictationExternalOnlySTT(); s != nil {
			return s, dictation.LocalityExternal, hint, true
		}
		return nil, dictation.LocalityNone, "", false
	default:
		return nil, dictation.LocalityNone, "", false
	}
}
