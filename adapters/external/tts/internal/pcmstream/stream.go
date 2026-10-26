// Package pcmstream turns sequential PCM synthesis of text pieces into a
// voice.AudioStream.
package pcmstream

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Synth synthesises one text piece to raw PCM s16le mono in Format.
type Synth func(ctx context.Context, text string) ([]byte, error)

// Params configures a stream.
type Params struct {
	Pieces    []string
	Format    voice.AudioFormat
	Model     string
	TraceID   string
	Synth     Synth
	Clock     clock.Clock
	ReadBytes int
	Started   time.Time
}

// Stream is a pull-style voice.AudioStream over synthesised PCM.
type Stream struct {
	ctx    context.Context
	cancel context.CancelFunc
	p      Params

	mu         sync.Mutex
	pieceIdx   int
	carry      []byte
	seq        int
	total      int
	firstAudio time.Time
	doneEmit   bool
	term       error
	closed     atomic.Bool
	release    sync.Once
}

// New starts a stream. pieces must be non-empty; Synth must be set.
func New(parent context.Context, p Params) (*Stream, error) {
	if p.Synth == nil || len(p.Pieces) == 0 {
		return nil, contracts.New(contracts.ErrInternal, "pcmstream: pieces and Synth are required")
	}
	if err := p.Format.Validate(); err != nil {
		return nil, err
	}
	if p.ReadBytes <= 0 {
		p.ReadBytes = 8 << 10
	}
	if p.Clock == nil {
		p.Clock = clock.Real{}
	}
	if p.Started.IsZero() {
		p.Started = p.Clock.Now()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Stream{ctx: ctx, cancel: cancel, p: p}, nil
}

func (s *Stream) free() {
	s.release.Do(func() { s.cancel() })
}

// Close cancels synthesis. Idempotent.
func (s *Stream) Close() error {
	s.closed.Store(true)
	s.free()
	return nil
}

func (s *Stream) fail(err error) (voice.AudioChunk, error) {
	if s.closed.Load() {
		return voice.AudioChunk{}, providers.ErrStreamClosed
	}
	s.mu.Lock()
	s.term = err
	s.mu.Unlock()
	s.free()
	return voice.AudioChunk{}, err
}

// Recv returns the next PCM chunk.
func (s *Stream) Recv() (voice.AudioChunk, error) {
	if s.closed.Load() {
		return voice.AudioChunk{}, providers.ErrStreamClosed
	}
	s.mu.Lock()
	if s.term != nil {
		err := s.term
		s.mu.Unlock()
		return voice.AudioChunk{}, err
	}
	if s.doneEmit {
		s.mu.Unlock()
		return voice.AudioChunk{}, io.EOF
	}
	s.mu.Unlock()

	if err := s.ctx.Err(); err != nil {
		return s.fail(providers.ContextError(err))
	}

	frame := s.p.Format.FrameBytes()
	for {
		s.mu.Lock()
		if usable := len(s.carry) - len(s.carry)%frame; usable > 0 {
			n := usable
			if n > s.p.ReadBytes {
				n = s.p.ReadBytes - s.p.ReadBytes%frame
				if n == 0 {
					n = frame
				}
				if n > usable {
					n = usable
				}
			}
			data := append([]byte(nil), s.carry[:n]...)
			s.carry = append([]byte(nil), s.carry[n:]...)
			if s.firstAudio.IsZero() {
				s.firstAudio = s.p.Clock.Now()
			}
			s.seq++
			seq := s.seq
			s.mu.Unlock()
			return voice.AudioChunk{Seq: seq, Data: data, Format: s.p.Format}, nil
		}
		if s.pieceIdx >= len(s.p.Pieces) {
			if len(s.carry) != 0 {
				s.mu.Unlock()
				return s.fail(contracts.New(contracts.ErrSchemaInvalid, "tts: synthesized audio is not a whole number of sample frames"))
			}
			if s.total == 0 {
				s.mu.Unlock()
				return s.fail(contracts.New(contracts.ErrSchemaInvalid, "tts: provider returned no audio"))
			}
			now := s.p.Clock.Now()
			t := providers.Timings{Total: now.Sub(s.p.Started)}
			if !s.firstAudio.IsZero() {
				t.TimeToFirstToken = s.firstAudio.Sub(s.p.Started)
			}
			s.seq++
			seq := s.seq
			s.doneEmit = true
			s.mu.Unlock()
			s.free()
			return voice.AudioChunk{
				Seq: seq, Format: s.p.Format, Done: true,
				Timings: t, TraceID: s.p.TraceID, Model: s.p.Model,
			}, nil
		}
		text := s.p.Pieces[s.pieceIdx]
		s.pieceIdx++
		s.mu.Unlock()

		if err := s.ctx.Err(); err != nil {
			return s.fail(providers.ContextError(err))
		}
		pcm, err := s.p.Synth(s.ctx, text)
		if err != nil {
			return s.fail(err)
		}
		if len(pcm)%frame != 0 {
			return s.fail(contracts.New(contracts.ErrSchemaInvalid, "tts: synthesized audio is not a whole number of sample frames"))
		}
		s.mu.Lock()
		s.carry = append(s.carry, pcm...)
		s.total += len(pcm)
		s.mu.Unlock()
	}
}

var _ voice.AudioStream = (*Stream)(nil)
