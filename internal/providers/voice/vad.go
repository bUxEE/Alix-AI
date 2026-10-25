package voice

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// DetectRequest asks for the speech segments of one audio buffer.
type DetectRequest struct {
	// Audio is raw PCM in Format (never serialised, json:"-").
	Audio   []byte      `json:"-"`
	Format  AudioFormat `json:"format"`
	Profile string      `json:"profile,omitempty"`
}

// Validate checks the request's shape and audio against l.
func (r DetectRequest) Validate(l Limits) error {
	if err := l.CheckAudio(r.Format, r.Audio); err != nil {
		return err
	}
	return checkName("profile", r.Profile)
}

// SpeechSegment is a span of speech, as offsets from the start of the audio.
type SpeechSegment struct {
	Start time.Duration `json:"start_ns"`
	End   time.Duration `json:"end_ns"`
}

// DetectResult lists the speech segments in time order.
type DetectResult struct {
	Segments []SpeechSegment   `json:"segments"`
	Timings  providers.Timings `json:"timings"`
	TraceID  string            `json:"trace_id,omitempty"`
	Model    string            `json:"model,omitempty"`
}

// VAD serves the voice-activity-detection role. With no VAD registered the
// service falls back to explicit turn taking (master plan section 18).
// Implementations validate scope and request (fail closed), honour ctx and
// return *contracts.Error values.
type VAD interface {
	DetectSpeech(ctx context.Context, scope contracts.Scope, req DetectRequest) (DetectResult, error)
}

// vadSlack is how far past the end of the audio a segment may reach: models
// work in frames and round the last one up.
const vadSlack = 50 * time.Millisecond

// Check verifies a result answers a request: every segment non-empty, finite
// in time, inside the audio (within vadSlack) and in order without overlap. A
// nonsense segment list would cut audio at the wrong place, so this is a
// safety check, not politeness. The failure is schema_invalid.
func (res DetectResult) Check(req DetectRequest) error {
	total := req.Format.DurationOf(len(req.Audio))
	var prev time.Duration
	for i, s := range res.Segments {
		switch {
		case s.Start < 0 || s.End <= s.Start:
			return contracts.Newf(contracts.ErrSchemaInvalid, "voice: segment %d [%v, %v] is not a valid span", i, s.Start, s.End)
		case s.End > total+vadSlack:
			return contracts.Newf(contracts.ErrSchemaInvalid, "voice: segment %d ends at %v, past the %v of audio", i, s.End, total)
		case s.Start < prev:
			return contracts.Newf(contracts.ErrSchemaInvalid, "voice: segment %d overlaps or precedes segment %d", i, i-1)
		}
		prev = s.End
	}
	return nil
}
