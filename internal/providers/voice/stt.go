package voice

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TranscribeRequest asks for the transcript of one complete audio buffer.
type TranscribeRequest struct {
	// Audio is raw PCM in Format. It is never serialised (json:"-"): audio is
	// not retained by default and must not reach logs or the audit trail.
	Audio  []byte      `json:"-"`
	Format AudioFormat `json:"format"`
	// Language is a language tag hint; "" means the active language pack's.
	Language string `json:"language,omitempty"`
	// Profile is an optional model-profile hint, as in providers.ChatRequest:
	// the adapter's configuration decides what it maps to.
	Profile string `json:"profile,omitempty"`
}

// Validate checks the request's shape and audio against l.
func (r TranscribeRequest) Validate(l Limits) error {
	if err := l.CheckAudio(r.Format, r.Audio); err != nil {
		return err
	}
	if err := checkLanguage(r.Language); err != nil {
		return err
	}
	return checkName("profile", r.Profile)
}

// Transcript is a finished recognition. Its Text is untrusted model output:
// it enters the normal turn pipeline like typed text and never grants
// anything.
type Transcript struct {
	Text string `json:"text"`
	// Language is the language the runtime reports having recognised ("" if
	// unknown).
	Language string `json:"language,omitempty"`
	// Duration is the length of the audio that was recognised.
	Duration time.Duration     `json:"duration_ns"`
	Timings  providers.Timings `json:"timings"`
	TraceID  string            `json:"trace_id,omitempty"`
	// Model is the model the runtime reports having used.
	Model string `json:"model,omitempty"`
}

// STTStreamRequest opens a live recognition stream.
type STTStreamRequest struct {
	// Format is the format of every chunk passed to Send.
	Format   AudioFormat `json:"format"`
	Language string      `json:"language,omitempty"`
	Profile  string      `json:"profile,omitempty"`
}

// Validate checks the request's shape.
func (r STTStreamRequest) Validate() error {
	if err := r.Format.Validate(); err != nil {
		return err
	}
	if err := checkLanguage(r.Language); err != nil {
		return err
	}
	return checkName("profile", r.Profile)
}

// TranscriptEvent is one event of a live recognition.
//
//   - An interim event (Final false, Done false) carries the runtime's current
//     best hypothesis for the segment in progress; each interim replaces the
//     previous one. It is for display and may change: never act on it.
//   - A final event (Final true) carries a segment that will not change. The
//     transcript is the final segments in order, joined by single spaces.
//   - The Done event ends the stream. It carries no text; Language, Model,
//     TraceID and Timings are only meaningful on it.
//
// Seq starts at 1 and increases by one per event.
type TranscriptEvent struct {
	Seq      int    `json:"seq"`
	Text     string `json:"text,omitempty"`
	Final    bool   `json:"final,omitempty"`
	Done     bool   `json:"done,omitempty"`
	Language string `json:"language,omitempty"`
	// Offset is how far into the audio the event reaches, when the runtime
	// says (zero if unknown).
	Offset  time.Duration     `json:"offset_ns,omitempty"`
	Timings providers.Timings `json:"timings"`
	TraceID string            `json:"trace_id,omitempty"`
	Model   string            `json:"model,omitempty"`
}

// STTStream is a live recognition: audio goes in through Send, events come out
// through Recv.
//
// Send feeds one chunk of audio in the stream's format (a whole number of
// sample frames, within Limits) and fails after CloseSend or Close. CloseSend
// says that no more audio follows; the stream then delivers the remaining
// final events and Done. Send and CloseSend are for a single sender; Recv for a
// single receiver; the two sides may run concurrently.
//
// Recv returns the next event and blocks until one is available. After the
// Done event it returns io.EOF. Any other error ends the stream: what was
// received so far is partial and must not be acted on. Recv honours the context
// given to TranscribeStream: once it is cancelled or past its deadline Recv
// (and Send) return promptly with an error wrapping the context's error.
// Close releases the runtime connection, is idempotent and safe to call
// concurrently with a blocked Recv; callers must always Close.
type STTStream interface {
	Send(audio []byte) error
	CloseSend() error
	Recv() (TranscriptEvent, error)
	Close() error
}

// STT serves the speech-to-text role. Implementations validate scope and
// request (fail closed), honour ctx and return *contracts.Error values.
type STT interface {
	// Transcribe recognises one complete buffer.
	Transcribe(ctx context.Context, scope contracts.Scope, req TranscribeRequest) (Transcript, error)
	// TranscribeStream starts a live recognition and returns before it
	// completes.
	TranscribeStream(ctx context.Context, scope contracts.Scope, req STTStreamRequest) (STTStream, error)
}

// CollectTranscript drains a stream whose audio has all been sent (the caller
// has called CloseSend) into a Transcript and closes it. The transcript is
// returned only when the stream completed (a Done event, then io.EOF):
//
//   - an interrupted stream yields its error and no text (never act on partial
//     output);
//   - a stream that ends with io.EOF before Done is unavailable;
//   - events out of order, or any event after Done, are internal errors.
//
// A stream that completed with no final event is a successful, empty
// transcript: that is what silence recognises as.
func CollectTranscript(s STTStream) (Transcript, error) {
	defer s.Close()
	var (
		parts []string
		res   Transcript
		last  int
		done  bool
	)
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Transcript{}, err
		}
		if done {
			return Transcript{}, contracts.New(contracts.ErrInternal, "voice: runtime sent an event after the final one")
		}
		if ev.Seq <= last {
			return Transcript{}, contracts.Newf(contracts.ErrInternal, "voice: transcript event %d arrived after %d", ev.Seq, last)
		}
		last = ev.Seq
		switch {
		case ev.Done:
			done = true
			res.Language, res.Model, res.TraceID, res.Timings = ev.Language, ev.Model, ev.TraceID, ev.Timings
			res.Duration = ev.Offset
		case ev.Final:
			if t := strings.TrimSpace(ev.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	if !done {
		return Transcript{}, contracts.New(contracts.ErrUnavailable, "voice: transcript stream ended before completion").
			WithDetails("final_segments", len(parts))
	}
	res.Text = strings.Join(parts, " ")
	return res, nil
}
