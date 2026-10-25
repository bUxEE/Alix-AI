package voice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ttsReadBytes is how much audio one read asks for: a quarter of a second of
// 16 kHz mono, small enough that the first chunk is not held back.
const ttsReadBytes = 8 << 10

// speechWire is the body of POST /v1/audio/speech (LocalAI v4.10.0,
// core/schema TTSRequest). The format is always "wav": the runtime converts
// any other codec with ffmpeg, and the voice seam carries raw PCM.
type speechWire struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice,omitempty"`
	Language       string `json:"language,omitempty"`
	ResponseFormat string `json:"response_format"`
	Stream         bool   `json:"stream,omitempty"`
	SampleRate     int    `json:"sample_rate,omitempty"`
}

// SynthesizeStream implements voice.TTS. It sends the request and returns once
// the runtime has answered 200 with audio; the audio header and the samples are
// read by Recv, so Close can cancel a synthesis that has not produced anything
// yet. Closing the stream drops the connection, which stops the runtime's
// output at once.
//
// The text must already be verified response text: the voice session service
// owns that gate (master plan section 13).
func (c *Client) SynthesizeStream(ctx context.Context, scope contracts.Scope, req pv.SynthesizeRequest) (pv.AudioStream, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(c.limits); err != nil {
		return nil, err
	}
	model, err := c.model(providers.RoleTTS, req.Profile)
	if err != nil {
		return nil, err
	}
	wire := speechWire{
		Model:          model,
		Input:          req.Text,
		Voice:          req.Voice,
		Language:       primaryLanguage(req.Language),
		ResponseFormat: "wav",
		Stream:         !c.cfg.TTSNoStream,
	}
	if !req.Format.IsZero() {
		wire.SampleRate = req.Format.SampleRate
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "localai voice: cannot encode the request")
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	started := c.clk.Now()
	resp, err := c.post(rctx, pathSpeech, "application/json", "audio/wav", body)
	if err != nil {
		cancel()
		return nil, err
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); !strings.HasPrefix(mt, "audio/") {
		_ = resp.Body.Close()
		cancel()
		return nil, schemaInvalid("runtime did not answer with audio")
	}
	return &ttsStream{
		c:       c,
		ctx:     rctx,
		cancel:  cancel,
		body:    resp.Body,
		started: started,
		model:   model,
		trace:   traceID(resp.Header),
		buf:     make([]byte, ttsReadBytes),
	}, nil
}

// ttsStream implements voice.AudioStream. The runtime answers with a WAV
// header followed by raw 16-bit PCM; a streamed answer carries 0xFFFFFFFF in
// its size fields and ends with the end of the chunked body, a complete file
// states its length. Recv must not be called concurrently with itself; Close
// may be called at any time.
type ttsStream struct {
	c       *Client
	ctx     context.Context
	cancel  context.CancelFunc
	body    io.ReadCloser
	started time.Time
	model   string
	trace   string

	headerRead bool
	format     pv.AudioFormat
	remaining  int64 // data bytes still expected; -1 when unknown
	buf        []byte
	carry      []byte // bytes read that are not yet a whole sample frame
	pend       error  // a read error (or io.EOF) to act on once carry is drained
	seq        int
	total      int
	firstAudio time.Time
	emitted    bool // the Done chunk has been returned
	err        error

	closed  atomic.Bool
	release sync.Once
}

func (s *ttsStream) free() {
	s.release.Do(func() {
		s.cancel()
		_ = s.body.Close()
	})
}

// fail ends the stream with err. Later Recv calls return the same error.
func (s *ttsStream) fail(err error) (pv.AudioChunk, error) {
	if s.closed.Load() {
		return pv.AudioChunk{}, providers.ErrStreamClosed
	}
	s.err = err
	s.free()
	return pv.AudioChunk{}, err
}

// Close implements voice.AudioStream: it cancels playback. Idempotent, and it
// unblocks a pending Recv.
func (s *ttsStream) Close() error {
	s.closed.Store(true)
	s.free()
	return nil
}

func (s *ttsStream) mapRead(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return interrupted("audio stream ended before completion")
	}
	return mapTransportError(s.ctx, err)
}

// Recv implements voice.AudioStream. Every chunk holds a whole number of
// sample frames in the format the runtime's header states. The stream is
// complete only when the Done chunk is returned: a body that ends early (a cut
// connection, a short file) is unavailable, a partial sample frame or an empty
// answer is schema_invalid, and audio past Config.MaxResponseBytes is
// resource_exhausted.
func (s *ttsStream) Recv() (pv.AudioChunk, error) {
	if s.closed.Load() {
		return pv.AudioChunk{}, providers.ErrStreamClosed
	}
	if s.err != nil {
		return pv.AudioChunk{}, s.err
	}
	if s.emitted {
		return pv.AudioChunk{}, io.EOF
	}
	if err := s.ctx.Err(); err != nil {
		return s.fail(providers.ContextError(err))
	}
	if !s.headerRead {
		f, n, err := readWAVHeader(s.body)
		if err != nil {
			var ce *contracts.Error
			if !errors.As(err, &ce) {
				err = s.mapRead(err)
			}
			return s.fail(err)
		}
		s.format, s.remaining, s.headerRead = f, n, true
	}
	frame := s.format.FrameBytes()
	for {
		if s.pend == nil {
			if s.remaining == 0 {
				s.pend = io.EOF
			} else {
				want := len(s.buf)
				if s.remaining > 0 && int64(want) > s.remaining {
					want = int(s.remaining)
				}
				n, err := s.body.Read(s.buf[:want])
				if n > 0 {
					if s.remaining > 0 {
						s.remaining -= int64(n)
					}
					s.total += n
					if int64(s.total) > s.c.maxResp {
						return s.fail(contracts.Newf(contracts.ErrResourceExhausted, "localai voice: synthesized audio exceeds %d bytes", s.c.maxResp))
					}
					s.carry = append(s.carry, s.buf[:n]...)
				}
				s.pend = err
			}
		}
		if usable := len(s.carry) - len(s.carry)%frame; usable > 0 {
			data := append([]byte(nil), s.carry[:usable]...)
			s.carry = append([]byte(nil), s.carry[usable:]...)
			if s.firstAudio.IsZero() {
				s.firstAudio = s.c.clk.Now()
			}
			s.seq++
			return pv.AudioChunk{Seq: s.seq, Data: data, Format: s.format}, nil
		}
		if s.pend == nil {
			continue
		}
		if !errors.Is(s.pend, io.EOF) {
			return s.fail(s.mapRead(s.pend))
		}
		switch {
		case s.remaining > 0:
			return s.fail(interrupted("audio stream ended before its declared length"))
		case len(s.carry) != 0:
			return s.fail(schemaInvalid("synthesized audio is not a whole number of sample frames"))
		case s.total == 0:
			return s.fail(schemaInvalid("runtime returned no audio"))
		}
		return s.done()
	}
}

// done builds the final chunk after a complete stream.
func (s *ttsStream) done() (pv.AudioChunk, error) {
	now := s.c.clk.Now()
	t := providers.Timings{Total: now.Sub(s.started)}
	if !s.firstAudio.IsZero() {
		t.TimeToFirstToken = s.firstAudio.Sub(s.started)
	}
	s.emitted = true
	s.free()
	s.seq++
	return pv.AudioChunk{
		Seq:     s.seq,
		Format:  s.format,
		Done:    true,
		Timings: t,
		TraceID: s.trace,
		Model:   s.model,
	}, nil
}
