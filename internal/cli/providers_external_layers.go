//go:build !noexternal

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/profile"
)

// Layer locality lines for operators (source of truth: external_providers.routing
// plus inference.models / voice packs). Printed by `alixai config check` and
// `alixai providers layers`. A role not listed in routing stays local only when
// a pack (or for llm, a pinned model) actually provides that role.

func init() {
	configCheckAfterLoad = func() error { return externalLightTextModelOK() }
	configCheckExtraLines = externalLayerStatusLines
	Register(Command{
		Name:    "providers layers",
		Summary: "print the effective locality of the llm, stt and tts layers from the configuration",
		Run:     runProvidersLayers,
	})
}

func externalLayerStatusLines() []string {
	return []string{
		"layers:",
		"  llm: " + externalLayerLLM(),
		"  stt: " + externalLayerSpeech("stt"),
		"  tts: " + externalLayerSpeech("tts"),
	}
}

func externalLayerLLM() string {
	r := externalSection.Routing["controller"]
	if externalSection.Enabled && len(r.Profiles) > 0 {
		return "external (" + externalLayerProfileHint(r.Profiles) + ")"
	}
	if len(inferenceSection.Models) > 0 {
		return "local (inference.models " + strings.Join(inferenceSection.Models, ", ") + ")"
	}
	return "none"
}

func externalLayerSpeech(role string) string {
	r := externalSection.Routing[role]
	if externalSection.Enabled && len(r.Profiles) > 0 {
		if role == "tts" && !externalTTSAdapterReady() {
			return "none (external TTS adapter not registered)"
		}
		return "external (" + externalLayerProfileHint(r.Profiles) + ")"
	}
	if id, ok := externalLayerLocalSpeechPack(role); ok {
		return "local (pack " + id + ")"
	}
	return "none"
}

// externalLayerLocalSpeechPack reports the active language pack id when that
// pack provides the speech role. LocalAI alone is not enough: without a pack
// that declares stt or tts the layer is none.
func externalLayerLocalSpeechPack(role string) (string, bool) {
	id := strings.TrimSpace(profile.CurrentConfig().ActivePack)
	if id == "" {
		return "", false
	}
	manifests, err := voiceShippedManifests()
	if err != nil {
		return "", false
	}
	m, ok := manifests.Lookup(id)
	if !ok {
		return "", false
	}
	want := providers.Role(role)
	for _, got := range m.Roles() {
		if got == want {
			return m.ID, true
		}
	}
	return "", false
}

func externalLayerProfileHint(ids []string) string {
	if len(ids) == 0 {
		return "no profile"
	}
	return "profile " + ids[0]
}

func runProvidersLayers(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("providers layers")
	path := env.ConfigFlag(fs)
	if err := env.ParseFlags(fs, "alixai providers layers [-config f]", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := env.Config.LoadFrom(*path, env.LookupEnv); err != nil {
		return err
	}
	if err := externalLightTextModelOK(); err != nil {
		return err
	}
	for _, line := range externalLayerStatusLines() {
		if _, err := fmt.Fprintln(env.Stdout, line); err != nil {
			return err
		}
	}
	return nil
}
