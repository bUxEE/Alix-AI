package extmodal

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ---- verified text ----

type attested struct {
	text    string
	expires time.Time
}

// maxAttestedPerScope bounds the verified responses kept speakable per session.
const maxAttestedPerScope = 32

func attestKey(s contracts.Scope) string {
	return s.DeploymentID + "\x00" + s.ApplicationID + "\x00" + s.TenantID + "\x00" + s.WorkspaceID + "\x00" + s.PrincipalID + "\x00" + s.SessionID
}

// Attest records the response of a turn as verified text that external TTS may speak in this scope.
// It accepts only what internal/verify produced: a model text that passed the verifier, or the
// deterministic template. Anything else (an outcome with no text, a model text marked unverified, an
// inconsistent source and reason) is refused, so unverified model text cannot become speakable by
// being attested.
func (s *Service) Attest(scope contracts.Scope, o verify.Outcome) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	text := strings.TrimSpace(o.Text)
	if text == "" {
		return refuse(ModTTS, ReasonTextNotVerified, "there is no verified text to attest")
	}
	ok := false
	switch o.Source {
	case verify.SourceModel:
		ok = o.Reason == verify.ReasonVerified && len(o.Violations) == 0
	case verify.SourceTemplate:
		ok = o.Reason == verify.ReasonDegraded || o.Reason == verify.ReasonVerificationFailed
	}
	if !ok {
		return refuse(ModTTS, ReasonTextNotVerified, "the outcome is not a verified response")
	}
	now := s.clk.Now()
	k := attestKey(scope)
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.attests[k][:0:0]
	for _, a := range s.attests[k] {
		if a.expires.After(now) {
			live = append(live, a)
		}
	}
	live = append(live, attested{text: text, expires: now.Add(s.d.AttestTTL)})
	if len(live) > maxAttestedPerScope {
		live = live[len(live)-maxAttestedPerScope:]
	}
	s.attests[k] = live
	return nil
}

// Verified reports whether text is, or is a contiguous piece of, a response attested in this scope
// and not yet expired. The voice session speaks a long answer in pieces, and a piece of verified text
// carries nothing the whole did not.
func (s *Service) Verified(scope contracts.Scope, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attests[attestKey(scope)] {
		if a.expires.After(now) && strings.Contains(a.text, text) {
			return true
		}
	}
	return false
}

// ---- text to speech ----

type ttsGate struct {
	s     *Service
	t     Target
	inner voice.TTS
}

// NewTTS wraps an external voice.TTS so it can only speak verified response text with the TTS modality
// admitted. The order is fixed: modality gate, then verified text, then the data-egress guard, then the
// provider budget, then the provider. A refusal at any step means the provider is never called.
//
// The reservation is characters_in of the spoken text (ADR 0043) when the profile prices that unit, else
// toks_in when that unit is priced (legacy profiles), plus the request; settled when the stream ends. A
// profile with neither price, or one that prices only audio_seconds_out (length unknown before the call),
// is refused (ReasonNotEstimable) — same rule as STT without audio_seconds_in.
func NewTTS(s *Service, t Target, inner voice.TTS) voice.TTS {
	return &ttsGate{s: s, t: t, inner: inner}
}

func (g *ttsGate) SynthesizeStream(ctx context.Context, scope contracts.Scope, req voice.SynthesizeRequest) (voice.AudioStream, error) {
	if g.inner == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "extmodal: no external synthesiser is installed")
	}
	adm, err := g.s.Admit(ctx, scope, g.t, ModTTS)
	if err != nil {
		return nil, err
	}
	if !g.s.Verified(scope, req.Text) {
		err := refuse(ModTTS, ReasonTextNotVerified, "external speech receives only verified response text")
		g.s.observe(Event{Modality: ModTTS, Provider: adm.Disclosure.Provider, Model: adm.Disclosure.Model, Reason: ReasonTextNotVerified})
		return nil, err
	}
	decision, err := g.s.allowText(ctx, scope, g.t, ModTTS, PurposeSpeech, req.Text)
	if err != nil {
		return nil, err
	}
	if g.t.Profile.Pricing.Prices.AudioSecondsOut != nil {
		return nil, notEstimable(ModTTS, "the audio a speech call returns has no bound before the call")
	}
	units, ok := ttsReserveUnits(g.t.Profile.Pricing, req.Text)
	if !ok {
		return nil, notEstimable(ModTTS, "the speech has no price per character or input tok")
	}
	h, err := g.s.reserve(ctx, scope, g.t, ModTTS, decision, units)
	if err != nil {
		return nil, err
	}
	start := g.s.clk.Now()
	st, err := g.inner.SynthesizeStream(ctx, scope, req)
	if err != nil {
		h.failed(ctx, err)
		return nil, err
	}
	return &meteredAudio{AudioStream: st, s: g.s, hold: h, ctx: context.WithoutCancel(ctx), ev: Event{
		Modality: ModTTS, Provider: adm.Disclosure.Provider, Model: adm.Disclosure.Model, Allowed: true, Route: "external",
		BytesSent: int64(len(req.Text)),
	}, start: start}, nil
}

// ttsReserveUnits prefers characters_in when the profile prices it (ADR 0043). Otherwise toks_in when
// that unit is priced (legacy). Neither priced → not estimable (same fail-closed rule as STT).
func ttsReserveUnits(pricing pv.PriceTable, text string) (providerbudget.Units, bool) {
	units := providerbudget.Units{Requests: 1}
	if pricing.Prices.CharactersIn != nil {
		n := pv.Counter(utf8.RuneCountInString(text))
		units.CharactersIn = &n
		return units, true
	}
	if pricing.Prices.ToksIn != nil {
		units.ToksIn = pv.Counter(providers.EstimateTokens(text))
		return units, true
	}
	return units, false
}

type meteredAudio struct {
	voice.AudioStream
	s     *Service
	hold  *hold
	ctx   context.Context // the call's values without its cancellation, for the settlement
	ev    Event
	start time.Time
	once  sync.Once
}

func (m *meteredAudio) Recv() (voice.AudioChunk, error) {
	c, err := m.AudioStream.Recv()
	m.ev.BytesReceived += int64(len(c.Data))
	if err != nil {
		m.finish(err)
	}
	return c, err
}

func (m *meteredAudio) Close() error {
	m.finish(nil)
	return m.AudioStream.Close()
}

// finish observes the call and settles its reservation, once: a stream that ended, or that the caller closed,
// with the reserved units; one that broke, as a failed call.
func (m *meteredAudio) finish(err error) {
	m.once.Do(func() {
		m.ev.Latency = m.s.clk.Now().Sub(m.start)
		m.s.observe(m.ev)
		if err == nil || errors.Is(err, io.EOF) {
			m.hold.estimated(m.ctx)
		} else {
			m.hold.failed(m.ctx, err)
		}
	})
}

// ---- speech to text ----

// Route says where a transcript came from.
type Route string

// The routes, in order of preference.
const (
	// RouteLocalTranscript: the caller already had the finalised local transcript; nothing was sent.
	RouteLocalTranscript Route = "local_transcript"
	// RouteLocalSTT: the local recogniser transcribed the audio; nothing was sent.
	RouteLocalSTT Route = "local_stt"
	// RouteExternalSTT: the raw audio went to the external provider under explicit policy.
	RouteExternalSTT Route = "external_stt"
)

// VoiceInput is one utterance. The finalised local transcript is preferred over any audio.
type VoiceInput struct {
	// Transcript is the finalised local transcript, when there is one.
	Transcript string
	Language   string
	// Audio and Format are the raw utterance, used only when no transcript and no local STT exist.
	Audio  []byte
	Format voice.AudioFormat
	// VADSegmented says the local VAD cut this utterance, so the provider gets speech and not an open
	// microphone. Voice-conversation mode sets this after DetectSpeech.
	VADSegmented bool
	// UserSegmented says the host's record and stop buttons cut this utterance (dictation). Either
	// VADSegmented or UserSegmented must be true before raw audio may leave; an open microphone never does.
	UserSegmented bool
	// Class is the data class the session states for the utterance (public, internal, confidential). Empty is
	// unknown, the most restrictive class: the data-egress guard never lets such audio leave.
	Class string
}

// VoiceResult is the text of an utterance and the way it was obtained.
type VoiceResult struct {
	Text       string
	Route      Route
	Transcript voice.Transcript
	BytesSent  int64
	// Disclosure is set only when audio left.
	Disclosure *Disclosure
}

// Transcribe turns an utterance into text, keeping audio local whenever it can:
//
//  1. a finalised local transcript is used as it is;
//  2. else a local STT, when installed, transcribes the audio (a failure there is returned, never
//     answered by sending the audio away);
//  3. else, only when external STT is admitted AND the provider and tenant both permit raw audio AND
//     the utterance was segmented (local VAD, or the host's record/stop for dictation) AND it fits the
//     provider's limit AND the data-egress guard allows it (Deps.Audio: the audio of its class, under
//     the audio setting of the egress policy) AND the provider budget reserves the call, the audio
//     goes to ext; the call is then settled.
//
// The reservation is the length of the audio, rounded up to whole seconds, priced at the profile's
// price per input audio second (audio_seconds_in) and the request. A profile with no such price, or
// audio whose length is not known from its format, has no bound to reserve and is refused
// (ReasonNotEstimable).
func (s *Service) Transcribe(ctx context.Context, scope contracts.Scope, t Target, in VoiceInput, local, ext voice.STT) (VoiceResult, error) {
	if err := scope.Validate(); err != nil {
		return VoiceResult{}, err
	}
	if tr := strings.TrimSpace(in.Transcript); tr != "" {
		s.observe(Event{Modality: ModSTT, Provider: string(t.Profile.ProviderID), Allowed: true, Route: string(RouteLocalTranscript)})
		return VoiceResult{Text: tr, Route: RouteLocalTranscript}, nil
	}
	if len(in.Audio) == 0 {
		return VoiceResult{}, invalid(ModSTT, "there is neither a transcript nor audio")
	}
	req := voice.TranscribeRequest{Audio: in.Audio, Format: in.Format, Language: in.Language}
	if local != nil {
		tr, err := local.Transcribe(ctx, scope, req)
		if err != nil {
			return VoiceResult{}, err
		}
		s.observe(Event{Modality: ModSTT, Provider: string(t.Profile.ProviderID), Allowed: true, Route: string(RouteLocalSTT)})
		return VoiceResult{Text: strings.TrimSpace(tr.Text), Route: RouteLocalSTT, Transcript: tr}, nil
	}
	if ext == nil {
		return VoiceResult{}, contracts.New(contracts.ErrUnavailable, "extmodal: no speech recogniser is installed")
	}
	adm, err := s.Admit(ctx, scope, t, ModSTT)
	if err != nil {
		return VoiceResult{}, err
	}
	deny := func(r Reason, msg string) (VoiceResult, error) {
		s.observe(Event{Modality: ModSTT, Provider: adm.Disclosure.Provider, Model: adm.Disclosure.Model, Reason: r})
		return VoiceResult{}, refuse(ModSTT, r, msg)
	}
	if !adm.Policy.RawAudioPermitted() {
		return deny(ReasonRawAudioNotPermitted, "raw audio leaves only under explicit provider and tenant policy")
	}
	if !in.VADSegmented && !in.UserSegmented {
		return deny(ReasonAudioNotSegmented, "raw audio must be cut by the local voice activity detector or by the user's record and stop first")
	}
	if h := t.mediaHandling(ModSTT); h != nil && h.MaxBytes > 0 && int64(len(in.Audio)) > h.MaxBytes {
		return deny(ReasonTooLarge, "the utterance is larger than the provider accepts")
	}
	// The audio is a governed part: the data-egress guard decides on it and records the decision, and only then is
	// the call reserved on the provider budget, against that decision.
	decision, err := s.allowAudio(ctx, scope, t, Audio{Class: in.Class, Bytes: int64(len(in.Audio))})
	if err != nil {
		s.observe(Event{Modality: ModSTT, Provider: adm.Disclosure.Provider, Model: adm.Disclosure.Model, Reason: ReasonAudioDenied})
		return VoiceResult{}, err
	}
	secs, known := audioSeconds(in.Format, len(in.Audio))
	if t.Profile.Pricing.Prices.AudioSecondsIn == nil || !known {
		return VoiceResult{}, notEstimable(ModSTT, "the audio has no price per input second or no known length")
	}
	h, err := s.reserve(ctx, scope, t, ModSTT, decision, providerbudget.Units{AudioSecondsIn: pv.Counter(secs), Requests: 1})
	if err != nil {
		return VoiceResult{}, err
	}
	start := s.clk.Now()
	tr, err := ext.Transcribe(ctx, scope, req)
	if err != nil {
		h.failed(ctx, err)
		return VoiceResult{}, err
	}
	h.estimated(ctx)
	d := adm.Disclosure
	s.observe(Event{
		Modality: ModSTT, Provider: d.Provider, Model: d.Model, Allowed: true, Route: string(RouteExternalSTT),
		BytesSent: int64(len(in.Audio)), BytesReceived: int64(len(tr.Text)), Latency: s.clk.Now().Sub(start),
	})
	return VoiceResult{Text: strings.TrimSpace(tr.Text), Route: RouteExternalSTT, Transcript: tr, BytesSent: int64(len(in.Audio)), Disclosure: &d}, nil
}

// audioSeconds is the length of n bytes of audio in format f, rounded up to whole seconds, or false when the
// format does not say how long the audio lasts.
func audioSeconds(f voice.AudioFormat, n int) (int64, bool) {
	if f.Validate() != nil || n <= 0 {
		return 0, false
	}
	bps := int64(f.BytesPerSecond())
	return (int64(n) + bps - 1) / bps, true
}

type sttGate struct {
	s          *Service
	t          Target
	local, ext voice.STT
}

type audioClassKey struct{}

// WithAudioClass returns a context that tells NewSTT the data class the session states for the audio of the call.
// A call whose context carries none has audio of unknown class, which never leaves.
func WithAudioClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, audioClassKey{}, class)
}

func audioClass(ctx context.Context) string {
	v, _ := ctx.Value(audioClassKey{}).(string)
	return v
}

type vadSegmentedKey struct{}

// WithVADSegmented returns a context that tells NewSTT the audio of the call was cut by the local voice activity
// detector. Only a session that really cuts utterances with the local VAD before it transcribes may set it; the
// marker is per call and is not inherited by anything that did not receive this context.
func WithVADSegmented(ctx context.Context) context.Context {
	return context.WithValue(ctx, vadSegmentedKey{}, true)
}

func vadSegmented(ctx context.Context) bool {
	v, _ := ctx.Value(vadSegmentedKey{}).(bool)
	return v
}

type userSegmentedKey struct{}

// WithUserSegmented returns a context that tells NewSTT the audio of the call is a user-recorded dictation
// clip: the host's record and stop buttons defined the utterance (no VAD). Only the dictation path may set
// it; the marker is per call and is not inherited. Voice-conversation mode must keep using WithVADSegmented.
func WithUserSegmented(ctx context.Context) context.Context {
	return context.WithValue(ctx, userSegmentedKey{}, true)
}

func userSegmented(ctx context.Context) bool {
	v, _ := ctx.Value(userSegmentedKey{}).(bool)
	return v
}

// NewSTT returns a voice.STT with the order of Transcribe: local first, external only under explicit
// raw-audio policy. The decorator does not assume that its caller cut the audio: an utterance is treated as
// segmented only when the call's context carries WithVADSegmented (voice conversation) or WithUserSegmented
// (dictation), so a session that hands over whole, client-submitted audio (or any caller that never heard of
// the marker) gets ReasonAudioNotSegmented before a byte leaves. The class of the audio is the one
// WithAudioClass put on the call's context; without it the class is unknown and the data-egress guard keeps
// the audio local. Local recognition is unaffected. Streaming recognition is served by the local recogniser
// only: raw audio is never streamed out.
func NewSTT(s *Service, t Target, local, ext voice.STT) voice.STT {
	return &sttGate{s: s, t: t, local: local, ext: ext}
}

func (g *sttGate) Transcribe(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	r, err := g.s.Transcribe(ctx, scope, g.t, VoiceInput{
		Audio: req.Audio, Format: req.Format, Language: req.Language,
		VADSegmented: vadSegmented(ctx), UserSegmented: userSegmented(ctx), Class: audioClass(ctx),
	}, g.local, g.ext)
	if err != nil {
		return voice.Transcript{}, err
	}
	out := r.Transcript
	out.Text = r.Text
	return out, nil
}

func (g *sttGate) TranscribeStream(ctx context.Context, scope contracts.Scope, req voice.STTStreamRequest) (voice.STTStream, error) {
	if g.local == nil {
		return nil, unsupported(ModSTT)
	}
	return g.local.TranscribeStream(ctx, scope, req)
}
