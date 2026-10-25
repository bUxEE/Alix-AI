package fake

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SynthesizeStream implements voice.TTS. The audio is a pure function of the
// text and the format: one chunk per WithSynthChunks runes of text, the last
// one marked Done. Cancelling the context or closing the stream stops it at
// once.
func (p *Provider) SynthesizeStream(ctx context.Context, scope contracts.Scope, req voice.SynthesizeRequest) (voice.AudioStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(p.limits); err != nil {
		return nil, err
	}
	format := req.Format
	if format.IsZero() {
		format = p.synthFormat
	}
	seq, fail, stall := p.begin(Call{
		Op: OpSynthesize, Scope: scope, Format: format, Language: req.Language, Voice: req.Voice, Text: req.Text,
	})

	runes := utf8.RuneCountInString(req.Text)
	n := (runes + p.runesPerChunk - 1) / p.runesPerChunk
	if n < 1 {
		n = 1
	}
	failAfter, failErr := -1, error(nil)
	switch fail {
	case "":
	case FailPartialStream:
		failAfter, failErr = n/2, injected(fail) // always short of n: the stream cannot complete
	default:
		return nil, injected(fail)
	}

	size := format.BytesFor(p.chunkPlay)
	if size < format.FrameBytes() {
		size = format.FrameBytes()
	}
	chunks := make([]voice.AudioChunk, n)
	for i := range chunks {
		chunks[i] = voice.AudioChunk{
			Seq:    i + 1,
			Data:   synthPCM(req.Text, i+1, format, size),
			Format: format,
		}
	}
	last := &chunks[n-1]
	last.Done = true
	last.Timings = providers.Timings{Total: time.Duration(n) * p.chunkPlay}
	last.TraceID = p.traceID(seq)
	last.Model = p.model

	s := &ttsStream{pull: newPull[voice.AudioChunk](ctx, p, failAfter, failErr, stall)}
	s.push(true, chunks...)
	return s, nil
}

// ttsStream is the fake's voice.AudioStream.
type ttsStream struct {
	*pull[voice.AudioChunk]
}

// Recv implements voice.AudioStream.
func (s *ttsStream) Recv() (voice.AudioChunk, error) { return s.recv() }
