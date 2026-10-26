package dictation

import (
	"context"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	voicegen "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// Locality of recognition for capabilities / transcripts.
const (
	LocalityLocal    = "local"
	LocalityExternal = "external"
	LocalityNone     = "none"
)

// Observe is a metric/audit sink: duration, locality, provider and outcome only.
// It must never receive audio or transcript text.
type Observe func(Event)

// Event is what is recorded about one dictation call.
type Event struct {
	DurationMS int64
	Locality   string
	Provider   string
	Outcome    string // ok | validation_failed | unavailable | denied | error
}

// Deps builds a Service.
type Deps struct {
	// STT is the recogniser (local, external-gated, or both). Nil → unavailable.
	STT voice.STT
	// Locality is local, external or none for capabilities.
	Locality string
	// Provider is the external provider slug when Locality is external; empty for local.
	Provider string
	// Languages are BCP 47 hints the recogniser accepts.
	Languages []voicegen.Language
	// MaxSeconds caps clip length (default DefaultMaxSeconds, ceiling HardMaxSeconds).
	MaxSeconds int
	// Observe records outcome metadata (optional).
	Observe Observe
	// Logger receives debug records without audio or transcript (optional).
	Logger *slog.Logger
	// External marks locality as external when Locality is empty.
	External bool
	// PrepareCtx, when set by the composition root for external STT, adds the
	// user-segmented and audio-class markers. Kept out of this package so a
	// -tags noexternal build does not link extmodal.
	PrepareCtx func(context.Context) context.Context
}

// Service implements gateway.TranscriptionService for record-to-text dictation.
type Service struct {
	stt        voice.STT
	locality   string
	provider   string
	languages  []voicegen.Language
	maxSeconds int
	observe    Observe
	log        *slog.Logger
	external   bool
	prepare    func(context.Context) context.Context
}

// New builds a Service. A nil STT yields capabilities with available=false.
func New(d Deps) *Service {
	max := d.MaxSeconds
	if max <= 0 {
		max = DefaultMaxSeconds
	}
	if max > HardMaxSeconds {
		max = HardMaxSeconds
	}
	loc := d.Locality
	if loc == "" {
		if d.STT == nil {
			loc = LocalityNone
		} else if d.External {
			loc = LocalityExternal
		} else {
			loc = LocalityLocal
		}
	}
	langs := append([]voicegen.Language(nil), d.Languages...)
	return &Service{
		stt: d.STT, locality: loc, provider: d.Provider, languages: langs,
		maxSeconds: max, observe: d.Observe, log: d.Logger, external: d.External,
		prepare: d.PrepareCtx,
	}
}

// MaxSeconds returns the operator clip cap for the scope (hard-ceiling applied).
func (s *Service) MaxSeconds(_ context.Context, _ contracts.Scope) (int64, error) {
	return int64(s.maxSeconds), nil
}

// MaxBodyBytes is WAV header headroom plus max_seconds of PCM.
func (s *Service) MaxBodyBytes(ctx context.Context, scope contracts.Scope) (int64, error) {
	sec, err := s.MaxSeconds(ctx, scope)
	if err != nil {
		return 0, err
	}
	return 256 + sec*int64(SampleRateHz)*2, nil
}

// Capabilities reports whether dictation is available for the scope.
func (s *Service) Capabilities(_ context.Context, scope contracts.Scope) (voicegen.TranscriptionCapabilities, error) {
	if err := scope.Validate(); err != nil {
		return voicegen.TranscriptionCapabilities{}, err
	}
	langs := s.languages
	if langs == nil {
		langs = []voicegen.Language{}
	}
	avail := s.stt != nil && s.locality != LocalityNone
	loc := voicegen.TranscriptionCapabilitiesLocality(s.locality)
	if !avail {
		loc = voicegen.TranscriptionCapabilitiesLocalityNone
		langs = []voicegen.Language{}
	}
	return voicegen.TranscriptionCapabilities{
		Available:    avail,
		Locality:     loc,
		MaxSeconds:   int64(s.maxSeconds),
		SampleRateHz: SampleRateHz,
		Format:       "audio/wav",
		Languages:    langs,
	}, nil
}

// Transcribe recognises one user-recorded WAV clip. Raw audio is zeroed before return.
func (s *Service) Transcribe(ctx context.Context, scope contracts.Scope, audio []byte, language string) (voicegen.DictationTranscript, error) {
	if err := scope.Validate(); err != nil {
		return voicegen.DictationTranscript{}, err
	}
	defer zero(audio)
	if s.stt == nil {
		s.record(0, "", "unavailable")
		return voicegen.DictationTranscript{}, contracts.New(contracts.ErrUnavailable, "dictation is not available on this server")
	}
	clip, err := ParseWAV(audio)
	if err != nil {
		s.record(0, "", "validation_failed")
		return voicegen.DictationTranscript{}, err
	}
	defer zero(clip.PCM)
	ms := clip.Duration.Milliseconds()
	if clip.Duration < time.Duration(MinDurationMS)*time.Millisecond {
		s.record(ms, "", "validation_failed")
		return voicegen.DictationTranscript{}, invalid("the clip is shorter than 300 ms").WithDetails("reason", "too_short")
	}
	if clip.Duration > time.Duration(s.maxSeconds)*time.Second {
		s.record(ms, "", "validation_failed")
		return voicegen.DictationTranscript{}, invalid("the clip is longer than the configured maximum").
			WithDetails("reason", "too_long").WithDetails("max_seconds", s.maxSeconds)
	}
	if IsSilent(clip.PCM) {
		s.record(ms, "", "validation_failed")
		return voicegen.DictationTranscript{}, invalid("the clip is silent").WithDetails("reason", "silent")
	}
	req := voice.TranscribeRequest{Audio: clip.PCM, Format: clip.Format, Language: language}
	callCtx := ctx
	if s.prepare != nil {
		callCtx = s.prepare(ctx)
	}
	tr, err := s.stt.Transcribe(callCtx, scope, req)
	if err != nil {
		s.record(ms, s.provider, outcomeOf(err))
		return voicegen.DictationTranscript{}, err
	}
	lang := tr.Language
	if lang == "" {
		lang = language
	}
	out := voicegen.DictationTranscript{
		Text:        tr.Text,
		Language:    voicegen.Language(lang),
		DurationMS:  ms,
		Locality:    voicegen.DictationTranscriptLocality(s.localityForResult()),
		SegmentKind: "user_recorded",
	}
	if s.localityForResult() == LocalityExternal && s.provider != "" {
		p := s.provider
		out.Provider = &p
	}
	s.record(ms, s.provider, "ok")
	return out, nil
}

func (s *Service) localityForResult() string {
	if s.locality == LocalityExternal {
		return LocalityExternal
	}
	return LocalityLocal
}

func (s *Service) record(ms int64, provider, outcome string) {
	if s.observe == nil {
		return
	}
	s.observe(Event{DurationMS: ms, Locality: s.locality, Provider: provider, Outcome: outcome})
}

func outcomeOf(err error) string {
	switch contracts.CodeOf(err) {
	case contracts.ErrValidationFailed:
		return "validation_failed"
	case contracts.ErrUnavailable:
		return "unavailable"
	case contracts.ErrPermissionDenied:
		return "denied"
	case contracts.ErrRateLimited, contracts.ErrResourceExhausted:
		return "denied"
	default:
		return "error"
	}
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
