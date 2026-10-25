package fake

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The detector looks at 10 ms frames and bridges silences shorter than 100 ms,
// as real VADs do, so a pause between words does not split a segment.
const (
	vadFrame    = 10 * time.Millisecond
	vadMergeGap = 100 * time.Millisecond
)

// DetectSpeech implements voice.VAD with a small energy detector: a 10 ms
// frame is speech when its peak amplitude reaches the threshold (see
// WithVADThreshold). Silence and low-level noise give no segments.
func (p *Provider) DetectSpeech(ctx context.Context, scope contracts.Scope, req voice.DetectRequest) (voice.DetectResult, error) {
	if err := ctx.Err(); err != nil {
		return voice.DetectResult{}, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return voice.DetectResult{}, err
	}
	if err := req.Validate(p.limits); err != nil {
		return voice.DetectResult{}, err
	}
	seq, fail, stall := p.begin(Call{Op: OpDetect, Scope: scope, Format: req.Format, AudioBytes: len(req.Audio)})
	p.addAudio(len(req.Audio))
	if fail != "" {
		return voice.DetectResult{}, injected(fail)
	}
	if stall >= 0 {
		return voice.DetectResult{}, hang(ctx)
	}
	res := voice.DetectResult{
		Segments: p.segments(req),
		TraceID:  p.traceID(seq),
		Model:    p.model,
	}
	if err := res.Check(req); err != nil {
		return voice.DetectResult{}, err
	}
	return res, nil
}

// segments runs the energy detector over req's audio.
func (p *Provider) segments(req voice.DetectRequest) []voice.SpeechSegment {
	f := req.Format
	frameBytes := f.BytesFor(vadFrame)
	if frameBytes < f.FrameBytes() {
		frameBytes = f.FrameBytes()
	}
	total := f.DurationOf(len(req.Audio))
	var segs []voice.SpeechSegment
	open := false
	var start time.Duration
	for off, idx := 0, 0; off < len(req.Audio); off, idx = off+frameBytes, idx+1 {
		end := off + frameBytes
		if end > len(req.Audio) {
			end = len(req.Audio)
		}
		speech := peak(req.Audio[off:end]) >= p.vadThreshold
		at := time.Duration(idx) * vadFrame
		switch {
		case speech && !open:
			open, start = true, at
		case !speech && open:
			open = false
			segs = appendSegment(segs, voice.SpeechSegment{Start: start, End: at})
		}
	}
	if open {
		segs = appendSegment(segs, voice.SpeechSegment{Start: start, End: total})
	}
	return segs
}

// appendSegment adds s, merging it into the previous segment when the silence
// between them is shorter than vadMergeGap.
func appendSegment(segs []voice.SpeechSegment, s voice.SpeechSegment) []voice.SpeechSegment {
	if n := len(segs); n > 0 && s.Start-segs[n-1].End < vadMergeGap {
		segs[n-1].End = s.End
		return segs
	}
	return append(segs, s)
}

// peak returns the largest absolute sample value in a PCM16 buffer.
func peak(pcm []byte) int {
	top := 0
	for i := 0; i+2 <= len(pcm); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if v < 0 {
			v = -v
		}
		if v > top {
			top = v
		}
	}
	return top
}
