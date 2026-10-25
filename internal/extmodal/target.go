package extmodal

import (
	"slices"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Target is the exact provider model a call is destined for: its registry profile (what the operator
// declared) and its runtime capabilities (what the adapter reports).
type Target struct {
	Profile pv.ProviderProfile
	Caps    providers.ModelCapabilities
}

// Disclosure is what the host shows before data of a modality leaves: where it goes and how long the
// provider keeps it. It is built from the profile alone and holds no content.
type Disclosure struct {
	Modality  Modality
	Provider  string
	Model     string
	Retention pv.RetentionMode
	// Recorded is true unless the provider keeps nothing at all (retention zero).
	Recorded bool
	Text     string
}

func newDisclosure(t Target, m Modality, r pv.RetentionMode) Disclosure {
	d := Disclosure{Modality: m, Provider: string(t.Profile.ProviderID), Model: string(t.Profile.ModelID), Retention: r, Recorded: r != pv.RetentionModeZero}
	what := map[Modality]string{
		ModVision: "images", ModFileInput: "files", ModSTT: "audio", ModTTS: "text sent for speech and the audio produced",
		ModImageGeneration: "prompts and generated images",
	}[m]
	if d.Recorded {
		d.Text = "This provider keeps " + what + " (" + string(r) + ")."
	} else {
		d.Text = "This provider keeps no " + what + "."
	}
	return d
}

// retentionOf returns the provider's stated retention for m, or "" when the profile does not state it.
func retentionOf(p pv.ProviderProfile, m Modality) pv.RetentionMode {
	var r pv.RetentionMode
	switch m {
	case ModVision:
		if p.Media != nil && p.Media.Images != nil {
			r = p.Media.Images.Retention
		}
	case ModFileInput:
		if p.Media != nil && p.Media.Files != nil {
			r = p.Media.Files.Retention
		}
	case ModSTT, ModTTS:
		r = p.Voice.AudioRetention
	case ModImageGeneration:
		r = p.ImageGeneration.OutputRetention
	}
	switch r {
	case pv.RetentionModeZero, pv.RetentionModeTransient, pv.RetentionModeLimited, pv.RetentionModeIndefinite:
		return r
	}
	return "" // unknown or unset: it cannot be disclosed accurately
}

// declares reports whether the profile declares m with its handling block and the serving role.
func declares(p pv.ProviderProfile, m Modality) bool {
	if !slices.Contains(p.Roles, m.role()) {
		return false
	}
	in, out := p.Modalities.Input, p.Modalities.Output
	switch m {
	case ModVision:
		return slices.Contains(in, pv.ModalityImage) && p.Media != nil && p.Media.Images != nil
	case ModFileInput:
		return slices.Contains(in, pv.ModalityFile) && p.Media != nil && p.Media.Files != nil
	case ModSTT:
		return slices.Contains(in, pv.ModalityAudio) && p.Voice.STT && p.Media != nil && p.Media.Audio != nil
	case ModImageGeneration:
		return slices.Contains(out, pv.ModalityImage) && p.ImageGeneration.Supported
	case ModTTS:
		return slices.Contains(out, pv.ModalityAudio) && p.Voice.TTS
	}
	return false
}

// supports reports whether the runtime capabilities serve m.
func supports(c providers.ModelCapabilities, m Modality) bool {
	has := func(list []providers.Modality, x providers.Modality) bool { return slices.Contains(list, x) }
	switch m {
	case ModVision:
		return has(c.InputModalities, providers.ModImage)
	case ModFileInput:
		return has(c.InputModalities, providers.ModFile)
	case ModSTT:
		return c.SpeechToText && has(c.InputModalities, providers.ModAudio)
	case ModImageGeneration:
		return c.ImageGeneration && has(c.OutputModalities, providers.ModImage)
	case ModTTS:
		return c.TextToSpeech && has(c.OutputModalities, providers.ModAudio)
	}
	return false
}

// mediaHandling returns the profile's handling block for a kind of media, or nil.
func (t Target) mediaHandling(m Modality) *pv.ProviderProfileMediaHandling {
	if t.Profile.Media == nil {
		return nil
	}
	switch m {
	case ModVision:
		return t.Profile.Media.Images
	case ModFileInput:
		return t.Profile.Media.Files
	case ModSTT:
		return t.Profile.Media.Audio
	}
	return nil
}
