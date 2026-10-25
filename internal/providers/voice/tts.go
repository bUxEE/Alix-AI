package voice

import (
	"context"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SynthesizeRequest asks for speech for one piece of text.
//
// Text must be verified response text: speech is produced only after the
// material facts in it have been verified (master plan section 13). This
// package cannot know that; the voice session service owns the gate.
type SynthesizeRequest struct {
	Text string `json:"text"`
	// Voice names a voice of the active language pack; "" is the pack's
	// default.
	Voice    string `json:"voice,omitempty"`
	Language string `json:"language,omitempty"`
	// Format is the wanted output format; the zero value means the runtime's
	// default. The format actually used is reported on every chunk.
	Format  AudioFormat `json:"format"`
	Profile string      `json:"profile,omitempty"`
}

// Validate checks the request's shape and text against l.
func (r SynthesizeRequest) Validate(l Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Text) == "" {
		return invalid("synthesize text is empty")
	}
	if !utf8.ValidString(r.Text) {
		return invalid("synthesize text is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(r.Text); n > l.MaxTextRunes {
		return limitError("max_text_runes", l.MaxTextRunes, n)
	}
	if !r.Format.IsZero() {
		if err := r.Format.Validate(); err != nil {
			return err
		}
	}
	if err := checkLanguage(r.Language); err != nil {
		return err
	}
	if err := checkName("voice", r.Voice); err != nil {
		return err
	}
	return checkName("profile", r.Profile)
}

// AudioChunk is one piece of synthesized speech.
type AudioChunk struct {
	// Seq starts at 1 and increases by one per chunk.
	Seq int `json:"seq"`
	// Data is raw PCM in Format. It is never serialised (json:"-").
	Data []byte `json:"-"`
	// Format is the format of Data; every chunk of a stream carries the same
	// one.
	Format AudioFormat `json:"format"`
	// Done marks the final chunk, which may carry data. Timings, TraceID and
	// Model are only meaningful on it.
	Done    bool              `json:"done,omitempty"`
	Timings providers.Timings `json:"timings"`
	TraceID string            `json:"trace_id,omitempty"`
	Model   string            `json:"model,omitempty"`
}

// AudioStream is a pull-style stream of synthesized audio.
//
// Recv returns the next chunk and blocks until one is available. After the
// chunk with Done set it returns io.EOF. Any other error ends the stream:
// the audio received so far is incomplete and must not be presented as a full
// answer. Recv honours the context given to SynthesizeStream: once it is
// cancelled or past its deadline Recv returns promptly with an error wrapping
// the context's error and no further chunk is produced.
//
// Close is how playback is cancelled: it stops synthesis at once, releases the
// runtime connection, is idempotent and is safe to call concurrently with a
// blocked Recv, which then returns providers.ErrStreamClosed. Callers must
// always Close.
type AudioStream interface {
	Recv() (AudioChunk, error)
	Close() error
}

// TTS serves the text-to-speech role. Implementations validate scope and
// request (fail closed), honour ctx and return *contracts.Error values.
type TTS interface {
	// SynthesizeStream starts synthesis and returns before it completes.
	SynthesizeStream(ctx context.Context, scope contracts.Scope, req SynthesizeRequest) (AudioStream, error)
}

// Audio is a complete synthesis.
type Audio struct {
	Format  AudioFormat `json:"format"`
	Data    []byte      `json:"-"`
	Timings providers.Timings
	TraceID string `json:"trace_id,omitempty"`
	Model   string `json:"model,omitempty"`
}

// CollectAudio drains a stream into one buffer and closes it. The audio is
// returned only when the stream completed (a Done chunk, then io.EOF), every
// chunk agreed on one valid format and the data is a whole number of sample
// frames. An interrupted stream yields its error and no audio. maxBytes bounds
// the buffer: a runtime that sends more is resource_exhausted, never silently
// truncated.
func CollectAudio(s AudioStream, maxBytes int) (Audio, error) {
	defer s.Close()
	if maxBytes <= 0 {
		return Audio{}, invalid("CollectAudio needs a positive byte cap")
	}
	var (
		res  Audio
		last int
		done bool
	)
	for {
		c, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Audio{}, err
		}
		if done {
			return Audio{}, contracts.New(contracts.ErrInternal, "voice: runtime sent audio after the final chunk")
		}
		if c.Seq <= last {
			return Audio{}, contracts.Newf(contracts.ErrInternal, "voice: audio chunk %d arrived after %d", c.Seq, last)
		}
		last = c.Seq
		if err := c.Format.Validate(); err != nil {
			return Audio{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "voice: runtime reported an unusable audio format")
		}
		if res.Format.IsZero() {
			res.Format = c.Format
		} else if res.Format != c.Format {
			return Audio{}, contracts.New(contracts.ErrSchemaInvalid, "voice: runtime changed the audio format mid-stream")
		}
		if len(res.Data)+len(c.Data) > maxBytes {
			return Audio{}, contracts.Newf(contracts.ErrResourceExhausted, "voice: synthesized audio exceeds %d bytes", maxBytes)
		}
		res.Data = append(res.Data, c.Data...)
		if c.Done {
			done = true
			res.Timings, res.TraceID, res.Model = c.Timings, c.TraceID, c.Model
		}
	}
	if !done {
		return Audio{}, contracts.New(contracts.ErrUnavailable, "voice: audio stream ended before completion").
			WithDetails("partial_bytes", len(res.Data))
	}
	if len(res.Data)%res.Format.FrameBytes() != 0 {
		return Audio{}, contracts.New(contracts.ErrSchemaInvalid, "voice: synthesized audio is not a whole number of sample frames")
	}
	return res, nil
}
