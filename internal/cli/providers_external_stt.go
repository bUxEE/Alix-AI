//go:build !noexternal

package cli

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	sttdeepgram "github.com/buxee/alix-ai/adapters/external/stt/deepgram"
	stteleven "github.com/buxee/alix-ai/adapters/external/stt/elevenlabs"
	sttgoogle "github.com/buxee/alix-ai/adapters/external/stt/googlestt"
	sttmistral "github.com/buxee/alix-ai/adapters/external/stt/mistral"
	sttopenai "github.com/buxee/alix-ai/adapters/external/stt/openai"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// External STT adapters and the speech-to-text voice factory (moved from
// providers_external.go). The OAuth / chat composition root owns the rest of
// that file; this one registers the four STT adapters as the voice kit source.

func init() {
	extFinalVoiceSourceHook = extFinalNewSTTSource
}

var (
	extSTTEgressOnce sync.Once
	extSTTEgress     *egress.ProviderRegistry
)

func extSTTDialerRegistry() *egress.ProviderRegistry {
	extSTTEgressOnce.Do(func() {
		extSTTEgress = egress.NewProviderRegistry(platformDenyOption())
		for id, base := range map[string]string{
			sttopenai.ProviderName:   sttopenai.DefaultBaseURL,
			sttdeepgram.ProviderName: sttdeepgram.DefaultBaseURL,
			stteleven.ProviderName:   stteleven.DefaultBaseURL,
			sttmistral.ProviderName:  sttmistral.DefaultBaseURL,
		} {
			_ = extWireAllowOrigin(extSTTEgress, id, base)
		}
		// Chirp 3 is GA in the us and eu multi-regions only (docs 2026-10-26).
		_ = extSTTAllowOrigins(extSTTEgress, sttgoogle.ProviderName,
			"https://us-speech.googleapis.com",
			"https://eu-speech.googleapis.com",
		)
	})
	return extSTTEgress
}

// extSTTAllowOrigins freezes several origins for one provider when none is set yet.
func extSTTAllowOrigins(reg *egress.ProviderRegistry, provider string, bases ...string) error {
	if reg.Allowlist(provider) != nil {
		return nil
	}
	dests := make([]egress.Destination, 0, len(bases))
	for _, base := range bases {
		d, err := egress.DestinationFromURL(base)
		if err != nil {
			return err
		}
		dests = append(dests, d)
	}
	al, err := egress.NewAllowlist(dests...)
	if err != nil {
		return err
	}
	return reg.Set(provider, al)
}

// extFinalNewSTTSource is the voice kit source with the four STT adapters.
func extFinalNewSTTSource(a *externalAdmin) extFinalVoiceSource {
	if a == nil {
		return extFinalNoVoice{}
	}
	_ = extSTTDialerRegistry()
	return &extSTTSource{admin: a, log: a.log, eg: extSTTDialerRegistry()}
}

// extSTTSource builds voice.STT clients for STT-capable profiles.
type extSTTSource struct {
	admin *externalAdmin
	log   *slog.Logger
	eg    *egress.ProviderRegistry
	mu    sync.Mutex
	cache map[string]voice.STT
}

func (s *extSTTSource) Kit(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile) (extFinalVoiceKit, bool) {
	if s == nil || s.admin == nil {
		return extFinalVoiceKit{}, false
	}
	hasSTT := false
	for _, r := range p.Roles {
		if r == pv.RoleSTT {
			hasSTT = true
			break
		}
	}
	if !hasSTT {
		return extFinalVoiceKit{}, false
	}
	stt, ok := s.clientFor(ctx, scope, p)
	if !ok {
		return extFinalVoiceKit{}, false
	}
	caps := providers.ModelCapabilities{Provider: string(p.ProviderID), Model: string(p.ModelID)}
	return extFinalVoiceKit{STT: stt, Caps: caps}, true
}

func (s *extSTTSource) clientFor(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile) (voice.STT, bool) {
	id := string(p.ProviderID)
	model := string(p.ModelID)
	ref, project, ok := s.credentialRef(ctx, scope, p.ProviderID)
	if !ok {
		return nil, false
	}
	cacheKey := id + "\x00" + model + "\x00" + ref + "\x00" + project
	s.mu.Lock()
	if s.cache != nil {
		if c, ok := s.cache[cacheKey]; ok {
			s.mu.Unlock()
			return c, true
		}
	}
	s.mu.Unlock()

	keys := extSTTKeys{svc: s.admin.creds, provider: p.ProviderID}
	var (
		stt voice.STT
		err error
	)
	switch id {
	case sttopenai.ProviderName:
		cfg := sttopenai.Config{CredentialRef: ref, Model: model}
		if p.Endpoint != "" {
			cfg.BaseURL = p.Endpoint
		}
		stt, err = sttopenai.New(cfg, s.eg.Dialer(id), keys)
	case sttmistral.ProviderName:
		cfg := sttmistral.Config{CredentialRef: ref, Model: model}
		if p.Endpoint != "" {
			cfg.BaseURL = p.Endpoint
		}
		stt, err = sttmistral.New(cfg, s.eg.Dialer(id), keys)
	case sttdeepgram.ProviderName:
		cfg := sttdeepgram.Config{CredentialRef: ref, Model: model}
		if p.Endpoint != "" {
			cfg.BaseURL = p.Endpoint
		}
		stt, err = sttdeepgram.New(cfg, s.eg.Dialer(id), keys)
	case stteleven.ProviderName:
		cfg := stteleven.Config{CredentialRef: ref, Model: model}
		if p.Endpoint != "" {
			cfg.BaseURL = p.Endpoint
		}
		stt, err = stteleven.New(cfg, s.eg.Dialer(id), keys)
	case sttgoogle.ProviderName:
		if project == "" {
			if s.log != nil {
				s.log.Debug("external STT adapter not built", slog.String("provider", id), slog.String("reason", "missing_project_id"))
			}
			return nil, false
		}
		cfg := sttgoogle.Config{CredentialRef: ref, Model: model, Project: project, Location: extSTTGoogleLocation(p)}
		if p.Endpoint != "" {
			cfg.BaseURL = p.Endpoint
		}
		stt, err = sttgoogle.New(cfg, s.eg.Dialer(id), keys)
	default:
		return nil, false
	}
	if err != nil || stt == nil {
		if s.log != nil {
			s.log.Debug("external STT adapter not built", slog.String("provider", id), slog.String("code", string(contracts.CodeOf(err))))
		}
		return nil, false
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]voice.STT{}
	}
	s.cache[cacheKey] = stt
	s.mu.Unlock()
	return stt, true
}

// extSTTGoogleLocation picks the Chirp multi-region from the profile's processing regions.
func extSTTGoogleLocation(p pv.ProviderProfile) string {
	for _, r := range p.Region.ProcessingRegions {
		switch strings.ToLower(string(r)) {
		case "eu", "europe":
			return "eu"
		case "us", "america", "americas":
			return "us"
		}
	}
	return sttgoogle.DefaultLocation
}

func (s *extSTTSource) credentialRef(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (ref, project string, ok bool) {
	// Prefer the principal's own credential when they bound STT (ADR 0041).
	if principalLLMHolder != nil {
		if b, found, err := principalLLMHolder.reg.PrincipalBindingFor(ctx, scope, pv.PrincipalLlmRoleSTT); err == nil && found && b.ProviderID == provider {
			if cred, cok, cerr := s.admin.creds.ActivePrincipalRef(ctx, scope, provider); cerr == nil && cok {
				if cred.Account.ProjectID != nil {
					project = strings.TrimSpace(*cred.Account.ProjectID)
				}
				return string(cred.RefID), project, true
			}
		}
	}
	refs, err := s.admin.creds.List(ctx, scope, provider)
	if err != nil {
		return "", "", false
	}
	best, found := externalBestRef(refs)
	if !found || best.Status != pv.CredentialRefStatusActive {
		return "", "", false
	}
	if best.Account.ProjectID != nil {
		project = strings.TrimSpace(*best.Account.ProjectID)
	}
	return string(best.RefID), project, true
}

type extSTTKeys struct {
	svc      *credentials.Service
	provider pv.ProviderID
}

func (k extSTTKeys) Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error) {
	return k.svc.Resolve(ctx, scope, ref, credentials.Use{Provider: k.provider, Purpose: "inference"})
}

// ---- moved from providers_external.go: extFinalSTT ----

// extFinalSTT is the speech-to-text of the voice profile with external mode on.
type extFinalSTT struct{ local voice.STT }

var _ voice.STT = extFinalSTT{}

// Transcribe implements voice.STT.
func (s extFinalSTT) Transcribe(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	v := extFinalVoiceHolder.Load()
	rt, err := v.route(ctx, scope, modes.RoleSTT)
	if err != nil {
		return voice.Transcript{}, err
	}
	if !rt.external || rt.kit.STT == nil {
		return s.localOnly(ctx, scope, req)
	}
	got, err := extmodal.NewSTT(v.svc, rt.target, s.local, rt.kit.STT).Transcribe(ctx, scope, req)
	if err != nil && s.local != nil && extFinalLocalAllowed(rt.policy, err) {
		return s.local.Transcribe(ctx, scope, req)
	}
	return got, err
}

func (s extFinalSTT) localOnly(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	if s.local == nil {
		return voice.Transcript{}, contracts.New(contracts.ErrUnavailable, "voice: no speech recogniser is installed")
	}
	return s.local.Transcribe(ctx, scope, req)
}

// TranscribeStream implements voice.STT. Raw audio is never streamed out: recognition streams are local.
func (s extFinalSTT) TranscribeStream(ctx context.Context, scope contracts.Scope, req voice.STTStreamRequest) (voice.STTStream, error) {
	if s.local == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "voice: no speech recogniser is installed")
	}
	return s.local.TranscribeStream(ctx, scope, req)
}
