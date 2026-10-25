package fake

import (
	"context"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// nextScript returns the utterance the next recognition hears.
func (p *Provider) nextScript() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.scripts) == 0 {
		return nil, contracts.New(contracts.ErrInternal, "fake: no scripted transcript for this recognition; call Script(...)")
	}
	i := p.sttServed
	if i >= len(p.scripts) {
		i = len(p.scripts) - 1
	}
	p.sttServed++
	return append([]string(nil), p.scripts[i]...), nil
}

func (p *Provider) addAudio(n int) {
	p.mu.Lock()
	p.audioIn += n
	p.mu.Unlock()
}

// joinSegments is the transcript of a recognition: trimmed non-empty segments
// joined by single spaces.
func joinSegments(segments []string) string {
	parts := make([]string, 0, len(segments))
	for _, s := range segments {
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

func (p *Provider) languageFor(requested string) string {
	if requested != "" {
		return requested
	}
	return p.language
}

// Transcribe implements voice.STT.
func (p *Provider) Transcribe(ctx context.Context, scope contracts.Scope, req voice.TranscribeRequest) (voice.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return voice.Transcript{}, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return voice.Transcript{}, err
	}
	if err := req.Validate(p.limits); err != nil {
		return voice.Transcript{}, err
	}
	seq, fail, stall := p.begin(Call{
		Op: OpTranscribe, Scope: scope, Format: req.Format, Language: req.Language, AudioBytes: len(req.Audio),
	})
	p.addAudio(len(req.Audio))
	if fail != "" {
		return voice.Transcript{}, injected(fail)
	}
	segments, err := p.nextScript()
	if err != nil {
		return voice.Transcript{}, err
	}
	if stall >= 0 {
		return voice.Transcript{}, hang(ctx)
	}
	if err := ctx.Err(); err != nil {
		return voice.Transcript{}, providers.ContextError(err)
	}
	return voice.Transcript{
		Text:     joinSegments(segments),
		Language: p.languageFor(req.Language),
		Duration: req.Format.DurationOf(len(req.Audio)),
		TraceID:  p.traceID(seq),
		Model:    p.model,
	}, nil
}

// TranscribeStream implements voice.STT. The recognition is scripted at open:
// every Send reveals more of the utterance as an interim event, CloseSend
// delivers the final segments and Done.
func (p *Provider) TranscribeStream(ctx context.Context, scope contracts.Scope, req voice.STTStreamRequest) (voice.STTStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	seq, fail, stall := p.begin(Call{Op: OpTranscribeStream, Scope: scope, Format: req.Format, Language: req.Language})
	failAfter, failErr := -1, error(nil)
	switch fail {
	case "":
	case FailPartialStream:
		failAfter, failErr = 1, injected(fail)
	default:
		return nil, injected(fail)
	}
	segments, err := p.nextScript()
	if err != nil {
		return nil, err
	}
	return &sttStream{
		pull:     newPull[voice.TranscriptEvent](ctx, p, failAfter, failErr, stall),
		format:   req.Format,
		limits:   p.limits,
		prov:     p,
		segments: segments,
		words:    strings.Fields(joinSegments(segments)),
		step:     p.interimWords,
		language: p.languageFor(req.Language),
		model:    p.model,
		trace:    p.traceID(seq),
	}, nil
}

// sttStream is the fake's voice.STTStream.
type sttStream struct {
	*pull[voice.TranscriptEvent]
	prov     *Provider
	format   voice.AudioFormat
	limits   voice.Limits
	segments []string
	words    []string
	step     int
	language string
	model    string
	trace    string

	sendMu     sync.Mutex // serialises Send and CloseSend; taken before pull.mu
	sent       int
	sendClosed bool
	seq        int
	revealed   int
}

// Send implements voice.STTStream.
func (s *sttStream) Send(audio []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendClosed {
		return contracts.New(contracts.ErrValidationFailed, "fake: Send after CloseSend")
	}
	s.mu.Lock()
	err := s.liveErrLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.limits.CheckChunk(s.format, audio, s.sent); err != nil {
		return err
	}
	s.sent += len(audio)
	s.prov.addAudio(len(audio))
	if s.revealed < len(s.words) {
		s.revealed += s.step
		if s.revealed > len(s.words) {
			s.revealed = len(s.words)
		}
		s.seq++
		s.push(false, voice.TranscriptEvent{
			Seq:      s.seq,
			Text:     strings.Join(s.words[:s.revealed], " "),
			Language: s.language,
			Offset:   s.format.DurationOf(s.sent),
		})
	}
	return nil
}

// CloseSend implements voice.STTStream. It is idempotent.
func (s *sttStream) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendClosed {
		return nil
	}
	s.mu.Lock()
	err := s.liveErrLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.sendClosed = true
	offset := s.format.DurationOf(s.sent)
	events := make([]voice.TranscriptEvent, 0, len(s.segments)+1)
	for _, seg := range s.segments {
		s.seq++
		events = append(events, voice.TranscriptEvent{
			Seq: s.seq, Text: strings.TrimSpace(seg), Final: true, Language: s.language, Offset: offset,
		})
	}
	s.seq++
	events = append(events, voice.TranscriptEvent{
		Seq: s.seq, Done: true, Language: s.language, Offset: offset, TraceID: s.trace, Model: s.model,
	})
	s.push(true, events...)
	return nil
}

// Recv implements voice.STTStream.
func (s *sttStream) Recv() (voice.TranscriptEvent, error) { return s.recv() }
