//go:build !noexternal

package cli

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	ttseleven "github.com/buxee/alix-ai/adapters/external/tts/elevenlabs"
	ttsgoogle "github.com/buxee/alix-ai/adapters/external/tts/googletts"
	ttsopenai "github.com/buxee/alix-ai/adapters/external/tts/openai"
	ttspolly "github.com/buxee/alix-ai/adapters/external/tts/polly"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// External TTS voice-kit source (ADR 0043). Mirrors providers_external_stt.go: the four adapters
// are registered through RegisterExternalTTSFactory* so -tags noexternal excludes them.

func init() {
	RegisterExternalTTSFactoryOpenAI(extTTSBuildOpenAI)
	RegisterExternalTTSFactoryElevenLabs(extTTSBuildElevenLabs)
	RegisterExternalTTSFactoryGoogle(extTTSBuildGoogle)
	RegisterExternalTTSFactoryPolly(extTTSBuildPolly)
	prev := extFinalVoiceSourceHook
	extFinalVoiceSourceHook = func(a *externalAdmin) extFinalVoiceSource {
		var inner extFinalVoiceSource = extFinalNoVoice{}
		if prev != nil {
			inner = prev(a)
		}
		return extFinalNewTTSSource(a, inner)
	}
}

// extTTSBuildInput is what a TTS adapter factory needs to construct a client.
type extTTSBuildInput struct {
	Provider      string
	Model         string
	CredentialRef string
	Project       string
	Endpoint      string
	Voice         string
	Language      string
	Location      string
	Dialer        *egress.Dialer
	Keys          extSTTKeys
}

// extTTSFactory builds a voice.TTS for one provider id, or returns false when that adapter is not wired.
type extTTSFactory func(in extTTSBuildInput) (voice.TTS, bool)

var (
	extTTSFactoryOpenAI     extTTSFactory
	extTTSFactoryElevenLabs extTTSFactory
	extTTSFactoryGoogle     extTTSFactory
	extTTSFactoryPolly      extTTSFactory
)

// RegisterExternalTTSFactoryOpenAI plugs the OpenAI TTS constructor once adapters/external/tts/openai is linked.
func RegisterExternalTTSFactoryOpenAI(f extTTSFactory) {
	extTTSFactoryOpenAI = f
	RegisterExternalTTSAdapter()
}

// RegisterExternalTTSFactoryElevenLabs plugs the ElevenLabs TTS constructor.
func RegisterExternalTTSFactoryElevenLabs(f extTTSFactory) {
	extTTSFactoryElevenLabs = f
	RegisterExternalTTSAdapter()
}

// RegisterExternalTTSFactoryGoogle plugs the Google Cloud TTS constructor (provider id google-tts).
func RegisterExternalTTSFactoryGoogle(f extTTSFactory) {
	extTTSFactoryGoogle = f
	RegisterExternalTTSAdapter()
}

// RegisterExternalTTSFactoryPolly plugs the Amazon Polly TTS constructor.
func RegisterExternalTTSFactoryPolly(f extTTSFactory) {
	extTTSFactoryPolly = f
	RegisterExternalTTSAdapter()
}

func extTTSBuildOpenAI(in extTTSBuildInput) (voice.TTS, bool) {
	cfg := ttsopenai.Config{CredentialRef: in.CredentialRef, Model: in.Model, Voice: in.Voice}
	if in.Endpoint != "" {
		cfg.BaseURL = in.Endpoint
	}
	c, err := ttsopenai.New(cfg, in.Dialer, in.Keys)
	if err != nil || c == nil {
		return nil, false
	}
	return c, true
}

func extTTSBuildElevenLabs(in extTTSBuildInput) (voice.TTS, bool) {
	if strings.TrimSpace(in.Voice) == "" {
		return nil, false
	}
	cfg := ttseleven.Config{CredentialRef: in.CredentialRef, Model: in.Model, VoiceID: in.Voice}
	if in.Endpoint != "" {
		cfg.BaseURL = in.Endpoint
	}
	c, err := ttseleven.New(cfg, in.Dialer, in.Keys)
	if err != nil || c == nil {
		return nil, false
	}
	return c, true
}

func extTTSBuildGoogle(in extTTSBuildInput) (voice.TTS, bool) {
	cfg := ttsgoogle.Config{CredentialRef: in.CredentialRef, VoiceName: in.Voice, LanguageCode: in.Language}
	if in.Endpoint != "" {
		cfg.BaseURL = in.Endpoint
	}
	c, err := ttsgoogle.New(cfg, in.Dialer, in.Keys)
	if err != nil || c == nil {
		return nil, false
	}
	return c, true
}

func extTTSBuildPolly(in extTTSBuildInput) (voice.TTS, bool) {
	cfg := ttspolly.Config{CredentialRef: in.CredentialRef, VoiceID: in.Voice, Region: extTTSPollyRegion(in)}
	if in.Model != "" {
		cfg.Engine = in.Model
	}
	if in.Endpoint != "" {
		cfg.BaseURL = in.Endpoint
	}
	c, err := ttspolly.New(cfg, in.Dialer, in.Keys)
	if err != nil || c == nil {
		return nil, false
	}
	return c, true
}

// extTTSPollyRegion picks an AWS region from the build input (Location, else default).
func extTTSPollyRegion(in extTTSBuildInput) string {
	if r := strings.TrimSpace(in.Location); r != "" {
		return r
	}
	return ttspolly.DefaultRegion
}

var (
	extTTSEgressOnce sync.Once
	extTTSEgress     *egress.ProviderRegistry
)

func extTTSDialerRegistry() *egress.ProviderRegistry {
	extTTSEgressOnce.Do(func() {
		extTTSEgress = egress.NewProviderRegistry(platformDenyOption())
		_ = extWireAllowOrigin(extTTSEgress, ttsopenai.ProviderName, ttsopenai.DefaultBaseURL)
		_ = extWireAllowOrigin(extTTSEgress, ttseleven.ProviderName, ttseleven.DefaultBaseURL)
		_ = extSTTAllowOrigins(extTTSEgress, ttsgoogle.ProviderName, ttsgoogle.DefaultBaseURL)
		// Polly is regional: allow the common SynhesizeSpeech hosts (operator Endpoint may replace BaseURL).
		_ = extSTTAllowOrigins(extTTSEgress, ttspolly.ProviderName,
			"https://polly.us-east-1.amazonaws.com",
			"https://polly.us-east-2.amazonaws.com",
			"https://polly.us-west-1.amazonaws.com",
			"https://polly.us-west-2.amazonaws.com",
			"https://polly.eu-west-1.amazonaws.com",
			"https://polly.eu-west-2.amazonaws.com",
			"https://polly.eu-central-1.amazonaws.com",
			"https://polly.ap-northeast-1.amazonaws.com",
			"https://polly.ap-southeast-1.amazonaws.com",
			"https://polly.ap-southeast-2.amazonaws.com",
		)
	})
	return extTTSEgress
}

func extFinalNewTTSSource(a *externalAdmin, inner extFinalVoiceSource) extFinalVoiceSource {
	if a == nil {
		return inner
	}
	_ = extTTSDialerRegistry()
	return &extTTSSource{admin: a, log: a.log, eg: extTTSDialerRegistry(), inner: inner}
}

// extTTSSource fills Kit.TTS for TTS-capable profiles and delegates STT to the inner source.
type extTTSSource struct {
	admin *externalAdmin
	log   *slog.Logger
	eg    *egress.ProviderRegistry
	inner extFinalVoiceSource
	mu    sync.Mutex
	cache map[string]voice.TTS
}

func (s *extTTSSource) Kit(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile) (extFinalVoiceKit, bool) {
	var kit extFinalVoiceKit
	if s.inner != nil {
		if k, ok := s.inner.Kit(ctx, scope, p); ok {
			kit = k
		}
	}
	hasTTS := false
	for _, r := range p.Roles {
		if r == pv.RoleTTS {
			hasTTS = true
			break
		}
	}
	if !hasTTS {
		return kit, kit.STT != nil || kit.TTS != nil
	}
	tts, ok := s.clientFor(ctx, scope, p)
	if !ok {
		return kit, kit.STT != nil || kit.TTS != nil
	}
	kit.TTS = tts
	if kit.Caps.Provider == "" {
		kit.Caps = providers.ModelCapabilities{Provider: string(p.ProviderID), Model: string(p.ModelID)}
	}
	return kit, true
}

func (s *extTTSSource) clientFor(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile) (voice.TTS, bool) {
	id := string(p.ProviderID)
	model := string(p.ModelID)
	ref, project, voiceID, language, ok := s.credentialRef(ctx, scope, p.ProviderID)
	if !ok {
		return nil, false
	}
	cacheKey := id + "\x00" + model + "\x00" + ref + "\x00" + project + "\x00" + voiceID + "\x00" + language + "\x00" + p.Endpoint
	s.mu.Lock()
	if s.cache != nil {
		if c, hit := s.cache[cacheKey]; hit {
			s.mu.Unlock()
			return c, true
		}
	}
	s.mu.Unlock()

	keys := extSTTKeys{svc: s.admin.creds, provider: p.ProviderID}
	in := extTTSBuildInput{
		Provider: id, Model: model, CredentialRef: ref, Project: project,
		Endpoint: p.Endpoint, Voice: voiceID, Language: language,
		Location: extTTSPollyLocation(p), Dialer: s.eg.Dialer(id), Keys: keys,
	}
	if id == ttspolly.ProviderName && in.Endpoint == "" {
		// Ensure the dialer can reach the regional host chosen for this profile.
		_ = extWireAllowOrigin(s.eg, id, "https://polly."+extTTSPollyRegion(in)+".amazonaws.com")
	}
	var (
		tts   voice.TTS
		built bool
	)
	switch id {
	case ttsopenai.ProviderName:
		if extTTSFactoryOpenAI != nil {
			tts, built = extTTSFactoryOpenAI(in)
		}
	case ttseleven.ProviderName:
		if extTTSFactoryElevenLabs != nil {
			tts, built = extTTSFactoryElevenLabs(in)
		}
	case ttsgoogle.ProviderName:
		if extTTSFactoryGoogle != nil {
			tts, built = extTTSFactoryGoogle(in)
		}
	case ttspolly.ProviderName:
		if extTTSFactoryPolly != nil {
			tts, built = extTTSFactoryPolly(in)
		}
	}
	if !built || tts == nil {
		if s.log != nil {
			s.log.Debug("external TTS adapter not built", slog.String("provider", id))
		}
		return nil, false
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]voice.TTS{}
	}
	s.cache[cacheKey] = tts
	s.mu.Unlock()
	return tts, true
}

// extTTSPollyLocation reads an AWS region from the profile's processing regions when present.
func extTTSPollyLocation(p pv.ProviderProfile) string {
	for _, r := range p.Region.ProcessingRegions {
		s := strings.ToLower(string(r))
		switch s {
		case "eu", "europe":
			return "eu-west-1"
		case "us", "america", "americas":
			return "us-east-1"
		}
		if strings.Count(s, "-") >= 2 {
			return s
		}
	}
	return ""
}

func (s *extTTSSource) credentialRef(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (ref, project, voiceID, language string, ok bool) {
	if principalLLMHolder != nil {
		if b, found, err := principalLLMHolder.reg.PrincipalBindingFor(ctx, scope, pv.PrincipalLlmRoleTTS); err == nil && found && b.ProviderID == provider {
			if b.Voice != nil {
				voiceID = strings.TrimSpace(string(*b.Voice))
			}
			if b.Language != nil {
				language = strings.TrimSpace(string(*b.Language))
			}
			if cred, cok, cerr := s.admin.creds.ActivePrincipalRef(ctx, scope, provider); cerr == nil && cok {
				if cred.Account.ProjectID != nil {
					project = strings.TrimSpace(*cred.Account.ProjectID)
				}
				return string(cred.RefID), project, voiceID, language, true
			}
		}
	}
	refs, err := s.admin.creds.List(ctx, scope, provider)
	if err != nil {
		return "", "", "", "", false
	}
	best, found := externalBestRef(refs)
	if !found || best.Status != pv.CredentialRefStatusActive {
		return "", "", "", "", false
	}
	if best.Account.ProjectID != nil {
		project = strings.TrimSpace(*best.Account.ProjectID)
	}
	return string(best.RefID), project, voiceID, language, true
}
