package profile

import (
	"context"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The gated providers are what the profile registers for the voice roles. Each
// wraps the factory's provider and refuses a call (unavailable, carrying the
// section 18 effect of the role) unless the profile is open and the active pack
// provides the role. They add no goroutine and copy no audio: a call that
// passes the gate goes straight to the wrapped provider, and so does the
// stream it returns.

// denial returns why role cannot serve right now, or "" when it can.
func (p *Profile) denial(role providers.Role) string {
	if p.closed.Load() {
		return "the voice profile is closed"
	}
	pack, ok := p.packs.Active()
	if !ok {
		return "no language pack is active"
	}
	if !pack.Provides(role) {
		return "the active language pack does not provide " + role.String()
	}
	return ""
}

func (p *Profile) gate(role providers.Role) error {
	reason := p.denial(role)
	if reason == "" {
		return nil
	}
	return contracts.New(contracts.ErrUnavailable, "voice: "+reason).
		WithDetails("role", role.String()).
		WithDetails("effect", role.Effect())
}

// gateHealth reports the gate's state, then the wrapped provider's own health.
func (p *Profile) gateHealth(ctx context.Context, role providers.Role, in any) (providers.HealthReport, error) {
	if reason := p.denial(role); reason != "" {
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reason}, nil
	}
	if rt, ok := in.(providers.AIRuntimeProvider); ok {
		return rt.Health(ctx)
	}
	return providers.HealthReport{Status: providers.HealthOK}, nil
}

func nameOf(in any) string {
	if n, ok := in.(interface{ Name() string }); ok {
		return n.Name()
	}
	return "voice-profile"
}

type sttGate struct {
	p  *Profile
	in voice.STT
}

var (
	_ voice.STT                   = sttGate{}
	_ providers.AIRuntimeProvider = sttGate{}
)

func (g sttGate) Name() string { return nameOf(g.in) }
func (g sttGate) Health(ctx context.Context) (providers.HealthReport, error) {
	return g.p.gateHealth(ctx, providers.RoleSTT, g.in)
}

func (g sttGate) Transcribe(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	if err := g.p.gate(providers.RoleSTT); err != nil {
		return voice.Transcript{}, err
	}
	return g.in.Transcribe(ctx, scope, req)
}

func (g sttGate) TranscribeStream(ctx context.Context, scope contracts.Scope, req voice.STTStreamRequest) (voice.STTStream, error) {
	if err := g.p.gate(providers.RoleSTT); err != nil {
		return nil, err
	}
	return g.in.TranscribeStream(ctx, scope, req)
}

type ttsGate struct {
	p  *Profile
	in voice.TTS
}

var (
	_ voice.TTS                   = ttsGate{}
	_ providers.AIRuntimeProvider = ttsGate{}
)

func (g ttsGate) Name() string { return nameOf(g.in) }
func (g ttsGate) Health(ctx context.Context) (providers.HealthReport, error) {
	return g.p.gateHealth(ctx, providers.RoleTTS, g.in)
}

func (g ttsGate) SynthesizeStream(ctx context.Context, scope contracts.Scope, req voice.SynthesizeRequest) (voice.AudioStream, error) {
	if err := g.p.gate(providers.RoleTTS); err != nil {
		return nil, err
	}
	return g.in.SynthesizeStream(ctx, scope, req)
}

type vadGate struct {
	p  *Profile
	in voice.VAD
}

var (
	_ voice.VAD                   = vadGate{}
	_ providers.AIRuntimeProvider = vadGate{}
)

func (g vadGate) Name() string { return nameOf(g.in) }
func (g vadGate) Health(ctx context.Context) (providers.HealthReport, error) {
	return g.p.gateHealth(ctx, providers.RoleVAD, g.in)
}

func (g vadGate) DetectSpeech(ctx context.Context, scope contracts.Scope, req voice.DetectRequest) (voice.DetectResult, error) {
	if err := g.p.gate(providers.RoleVAD); err != nil {
		return voice.DetectResult{}, err
	}
	return g.in.DetectSpeech(ctx, scope, req)
}
