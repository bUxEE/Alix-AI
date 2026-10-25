package realtime

import (
	"context"
	"log/slog"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Compile-time proof that Speech serves both speech roles.
var (
	_ pv.STT = (*Speech)(nil)
	_ pv.TTS = (*Speech)(nil)
)

// FallbackEvent records one fall back from the Realtime pipeline to the
// separate adapter.
type FallbackEvent struct {
	Role   providers.Role    // providers.RoleSTT
	Op     string            // "transcribe" or "transcribe_stream"
	Reason contracts.ErrCode // the code of the Realtime failure (always unavailable)
}

// SpeechOption adjusts a Speech under construction.
type SpeechOption func(*Speech)

// WithFallbackLog sets the logger that receives one warning per fall back
// (role, operation, reason code; never audio or text).
func WithFallbackLog(l *slog.Logger) SpeechOption { return func(s *Speech) { s.log = l } }

// WithFallbackHook registers a function called after each fall back, for a
// composition root that surfaces it in diagnostics. It must not block.
func WithFallbackHook(f func(FallbackEvent)) SpeechOption { return func(s *Speech) { s.hook = f } }

// Speech is the voice pair the composition root registers when the Realtime
// pipeline is configured: speech to text through Realtime when it is available
// and through the separate STT adapter when it is not, and text to speech
// always through the separate TTS adapter.
//
// The fall back is narrow on purpose. It happens only when opening the Realtime
// session failed as unavailable (connection refused, no realtime route, a
// runtime that cannot run a transcription-only session): nothing had been sent,
// so running the request elsewhere hides nothing. A batch Transcribe also falls
// back when the connection is lost part way, because the caller still holds the
// whole buffer. It never falls back on a caller's cancellation or deadline, on
// validation, scope or limit errors, on a stream that already carried audio, or
// on a side-door violation: those are failures to report, and a silent second
// attempt would hide them. Every fall back is logged and handed to the hook.
type Speech struct {
	realtime pv.STT
	stt      pv.STT
	tts      pv.TTS
	log      *slog.Logger
	hook     func(FallbackEvent)

	mu    sync.Mutex
	count int
	last  FallbackEvent
}

// NewSpeech builds the pair. realtime is the Realtime client, stt and tts are
// the separate adapters (adapters/localai/voice, or any pv.STT and pv.TTS).
// A nil realtime means Realtime is not configured: every call goes to stt and
// no fall back is recorded.
func NewSpeech(realtime pv.STT, stt pv.STT, tts pv.TTS, opts ...SpeechOption) *Speech {
	s := &Speech{realtime: realtime, stt: stt, tts: tts, log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Fallbacks returns how many fall backs happened and the most recent one.
func (s *Speech) Fallbacks() (int, FallbackEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.last
}

// SynthesizeStream always uses the separate TTS adapter: LocalAI's Realtime
// pipeline has no verbatim speech path (every response goes through the model),
// so it could change the verified text it is asked to speak.
func (s *Speech) SynthesizeStream(ctx context.Context, scope contracts.Scope, req pv.SynthesizeRequest) (pv.AudioStream, error) {
	if s.tts == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "localai realtime: no text-to-speech adapter is configured")
	}
	return s.tts.SynthesizeStream(ctx, scope, req)
}

// Transcribe implements pv.STT.
func (s *Speech) Transcribe(ctx context.Context, scope contracts.Scope, req pv.TranscribeRequest) (pv.Transcript, error) {
	if s.realtime == nil {
		return s.backup().Transcribe(ctx, scope, req)
	}
	tr, err := s.realtime.Transcribe(ctx, scope, req)
	if err == nil || !s.shouldFallBack(ctx, err) {
		return tr, err
	}
	s.record("transcribe", err)
	return s.backup().Transcribe(ctx, scope, req)
}

// TranscribeStream implements pv.STT. Only the opening can fall back: once the
// Realtime stream exists it carries the audio, and a failure after that is the
// stream's error.
func (s *Speech) TranscribeStream(ctx context.Context, scope contracts.Scope, req pv.STTStreamRequest) (pv.STTStream, error) {
	if s.realtime == nil {
		return s.backup().TranscribeStream(ctx, scope, req)
	}
	st, err := s.realtime.TranscribeStream(ctx, scope, req)
	if err == nil || !s.shouldFallBack(ctx, err) {
		return st, err
	}
	s.record("transcribe_stream", err)
	return s.backup().TranscribeStream(ctx, scope, req)
}

// shouldFallBack is true only for an unavailable Realtime pipeline while the
// caller's context is still live.
func (s *Speech) shouldFallBack(ctx context.Context, err error) bool {
	return ctx.Err() == nil && s.stt != nil && contracts.HasCode(err, contracts.ErrUnavailable)
}

// backup returns the separate STT adapter, or a stand-in that refuses.
func (s *Speech) backup() pv.STT {
	if s.stt == nil {
		return noSTT{}
	}
	return s.stt
}

func (s *Speech) record(op string, err error) {
	ev := FallbackEvent{Role: providers.RoleSTT, Op: op, Reason: contracts.CodeOf(err)}
	s.mu.Lock()
	s.count++
	s.last = ev
	s.mu.Unlock()
	s.log.Warn("localai realtime unavailable; using the separate STT adapter",
		slog.String("role", string(ev.Role)), slog.String("op", ev.Op), slog.String("reason", string(ev.Reason)))
	if s.hook != nil {
		s.hook(ev)
	}
}

// noSTT refuses every call: Realtime is not configured and neither is a
// separate adapter.
type noSTT struct{}

func (noSTT) Transcribe(context.Context, contracts.Scope, pv.TranscribeRequest) (pv.Transcript, error) {
	return pv.Transcript{}, contracts.New(contracts.ErrUnavailable, "localai realtime: no speech-to-text adapter is configured")
}

func (noSTT) TranscribeStream(context.Context, contracts.Scope, pv.STTStreamRequest) (pv.STTStream, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "localai realtime: no speech-to-text adapter is configured")
}
