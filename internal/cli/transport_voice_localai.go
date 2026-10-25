package cli

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/buxee/alix-ai/adapters/localai"
	lvoice "github.com/buxee/alix-ai/adapters/localai/voice"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The LocalAI voice adapter of `alixai serve` (master plan 13; M4-09).
//
// voiceProviderFactory (cmd_voice.go) refuses by default, so serve stays text-only. This file
// assigns it from init: it builds one adapters/localai/voice client (STT batch and streaming,
// TTS streaming, VAD) from
//
//   - the LocalAI runtime configuration: the registered "localai" section, which is the one the
//     text client is built from, so voice and text reach the same runtime under the same
//     endpoint policy. A runtime is configured when localai.endpoint is set or the inference
//     path is (inference.models); otherwise there is nothing to connect and voice stays
//     text-only (effect text_input_only), exactly as before this file existed;
//   - the active language pack: voice.active_pack, else the pack `alixai voice activate`
//     recorded, the same choice voiceServeProvide makes. With none active nothing is built.
//
// The API key is the key of localai.api_key_ref, resolved by voice.New with the rules the text
// client's New uses (env: or file:, owner-only file, never printed). It is not read here, so
// this file never holds it; WithAPIKey is for a composition root that has the key in hand.
//
// LocalAI model names. The pack names no LocalAI model, so the convention is: the model of a
// role is called by the file the engine loads, the first file of that role's model in the pack
// manifest (ggml-tiny-q5_1.bin, en_US-ljspeech-medium.onnx, ggml-silero-v5.1.2.bin). The
// operator's LocalAI model configuration uses those names. A pack that lacks a role gets no
// provider for it (an STT-only pack serves speech to text and VAD, never TTS).
//
// TTSNoStream. LocalAI v4.10.0 hangs on a streamed TTS request to the piper backend until its
// backend timeout (M4-06), and kokoro cannot stream either, so Config.TTSNoStream is set from
// the engine of the active pack's TTS model: true for piper and kokoro, false for any other
// engine (the streamed default). It is per client: a pack with another TTS engine gets the
// streaming client. The audio still reaches the session in chunks.
//
// The client is built for the pack that is active when serve starts. A pack activated live
// later (voiceSessionPacks) is gated by the profile as before, but its models are those of the
// pack the client was built for: switching to a pack with other models needs a restart.

// voiceLocalAINoStreamEngines are the TTS engines that cannot stream on LocalAI v4.10.0.
var voiceLocalAINoStreamEngines = map[string]bool{"piper": true, "kokoro": true}

// voiceLocalAIDataDir is the data directory that holds the pack store. A variable so a test
// can point at a temporary one.
var voiceLocalAIDataDir = func() string { return config.Core().DataDir.String() }

func init() {
	voiceProviderFactory = voiceLocalAIFactory
}

// voiceLocalAIExternal, when set, wraps the providers built for the active pack. External provider mode sets it from
// its own file (providers_external.go, not compiled into a noexternal binary), so this file imports nothing from
// milestone X and the order of the two init functions does not matter. Nil leaves the providers as they are.
var voiceLocalAIExternal func(profile.Providers) profile.Providers

// voiceLocalAITTSNoStream reports whether a TTS engine needs a whole-file answer.
func voiceLocalAITTSNoStream(engine string) bool {
	return voiceLocalAINoStreamEngines[strings.ToLower(strings.TrimSpace(engine))]
}

// voiceLocalAIModelName is the LocalAI model name of one role's model: its first file.
func voiceLocalAIModelName(m *packs.Model) string {
	if m == nil || len(m.Files) == 0 {
		return ""
	}
	return m.Files[0].Name
}

// voiceLocalAIConfig is the voice client configuration of pack m.
func voiceLocalAIConfig(m packs.Manifest) lvoice.Config {
	cfg := lvoice.Config{Models: lvoice.ModelSet{
		STT: voiceLocalAIModelName(m.Models.STT),
		TTS: voiceLocalAIModelName(m.Models.TTS),
		VAD: voiceLocalAIModelName(m.Models.VAD),
	}}
	if m.Models.TTS != nil {
		cfg.TTSNoStream = voiceLocalAITTSNoStream(m.Models.TTS.Engine)
	}
	return cfg
}

// voiceLocalAIRuntimeConfigured says whether this deployment has a LocalAI runtime to talk to.
func voiceLocalAIRuntimeConfigured(rt localai.Config) bool {
	return strings.TrimSpace(rt.Endpoint) != "" || len(inferenceSection.Models) > 0
}

// voiceLocalAIActivePack is the pack serve activates: voice.active_pack, else the recorded one.
func voiceLocalAIActivePack() (string, error) {
	if id := profile.CurrentConfig().ActivePack; id != "" {
		return id, nil
	}
	st, err := voiceOpenStore(filepath.Join(voiceLocalAIDataDir(), voicePackDirName))
	if err != nil {
		return "", err
	}
	id, _, err := st.ActiveID()
	return id, err
}

func voiceLocalAIUnavailable(why string) error {
	return contracts.New(contracts.ErrUnavailable, "voice: no voice runtime adapter is connected: "+why)
}

// voiceLocalAIBuild builds the providers of pack m over the runtime rt. Only the roles the pack
// provides are returned.
func voiceLocalAIBuild(rt localai.Config, m packs.Manifest) (profile.Providers, error) {
	client, err := lvoice.New(rt, voiceLocalAIConfig(m))
	if err != nil {
		return profile.Providers{}, err
	}
	var out profile.Providers
	if m.Models.STT != nil {
		out.STT = voice.STT(client)
	}
	if m.Models.TTS != nil {
		out.TTS = voice.TTS(client)
	}
	if m.Models.VAD != nil {
		out.VAD = voice.VAD(client)
	}
	return out, nil
}

// voiceLocalAIFactory is the profile.ProviderFactory of this binary.
func voiceLocalAIFactory(ctx context.Context) (profile.Providers, error) {
	if err := ctx.Err(); err != nil {
		return profile.Providers{}, err
	}
	rt := localaiSection
	if !voiceLocalAIRuntimeConfigured(rt) {
		return profile.Providers{}, voiceLocalAIUnavailable("no LocalAI runtime is configured (localai.endpoint or inference.models)")
	}
	manifests, err := voiceShippedManifests()
	if err != nil {
		return profile.Providers{}, err
	}
	id, err := voiceLocalAIActivePack()
	if err != nil {
		return profile.Providers{}, err
	}
	if id == "" {
		return profile.Providers{}, voiceLocalAIUnavailable("no language pack is active")
	}
	m, ok := manifests.Lookup(id)
	if !ok {
		return profile.Providers{}, voiceLocalAIUnavailable("the active language pack is not an approved pack")
	}
	built, err := voiceLocalAIBuild(rt, m)
	if err != nil {
		return profile.Providers{}, err
	}
	if wrap := voiceLocalAIExternal; wrap != nil {
		built = wrap(built)
	}
	return built, nil
}
