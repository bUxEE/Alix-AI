package voice

import (
	"fmt"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Encoding names how samples are laid out in an audio buffer.
type Encoding string

// EncodingPCM16 is signed 16-bit little-endian linear PCM, interleaved when
// there is more than one channel. It is the only encoding the seam carries:
// containers and codecs (WAV, Opus, MP3) are the adapter's concern, so the
// core never parses untrusted media formats.
const EncodingPCM16 Encoding = "pcm_s16le"

// AudioFormat describes a raw PCM buffer.
type AudioFormat struct {
	Encoding   Encoding `json:"encoding"`
	SampleRate int      `json:"sample_rate"`
	Channels   int      `json:"channels"`
}

// PCM16Mono returns the mono PCM16 format at rate Hz.
func PCM16Mono(rate int) AudioFormat {
	return AudioFormat{Encoding: EncodingPCM16, SampleRate: rate, Channels: 1}
}

// IsZero reports whether no field is set (for a TTS request: "use the
// runtime's default output format").
func (f AudioFormat) IsZero() bool { return f == AudioFormat{} }

// Sample-rate bounds. 8 kHz is telephony, 48 kHz is the highest rate speech
// models are served at.
const (
	MinSampleRate = 8000
	MaxSampleRate = 48000
	MaxChannels   = 2
)

// Validate rejects an unsupported encoding, a sample rate outside
// [MinSampleRate, MaxSampleRate] and a channel count outside [1, MaxChannels].
func (f AudioFormat) Validate() error {
	switch {
	case f.Encoding != EncodingPCM16:
		return invalid("audio encoding %q is not supported (want %s)", string(f.Encoding), EncodingPCM16)
	case f.SampleRate < MinSampleRate || f.SampleRate > MaxSampleRate:
		return invalid("sample rate %d is outside [%d, %d]", f.SampleRate, MinSampleRate, MaxSampleRate)
	case f.Channels < 1 || f.Channels > MaxChannels:
		return invalid("channel count %d is outside [1, %d]", f.Channels, MaxChannels)
	}
	return nil
}

// FrameBytes is the size of one sample frame (one sample per channel).
func (f AudioFormat) FrameBytes() int { return 2 * f.Channels }

// BytesPerSecond is the data rate of the format.
func (f AudioFormat) BytesPerSecond() int { return f.FrameBytes() * f.SampleRate }

// DurationOf returns how long n bytes of audio last. The format must be valid.
func (f AudioFormat) DurationOf(n int) time.Duration {
	bps := f.BytesPerSecond()
	if bps <= 0 || n <= 0 {
		return 0
	}
	return time.Duration(int64(n) * int64(time.Second) / int64(bps))
}

// BytesFor returns the bytes of audio lasting d, rounded down to a whole
// sample frame. The format must be valid.
func (f AudioFormat) BytesFor(d time.Duration) int {
	if f.FrameBytes() <= 0 || d <= 0 {
		return 0
	}
	n := int(int64(f.BytesPerSecond()) * int64(d) / int64(time.Second))
	return n - n%f.FrameBytes()
}

// Limits caps what one voice request may cost the runtime (master plan 17.2:
// "reject or queue work before memory exhaustion"). Every limit must be
// positive: there is no "unlimited", and a zero Limits is rejected by Validate
// rather than silently meaning "no cap".
type Limits struct {
	// MaxAudioBytes caps the audio of one batch call, and the cumulative audio
	// of one STT stream.
	MaxAudioBytes int
	// MaxChunkBytes caps one Send on an STT stream.
	MaxChunkBytes int
	// MaxTextRunes caps the text of one TTS request.
	MaxTextRunes int
}

// DefaultLimits returns conservative limits suited to the Lite profile: about
// four minutes of 16 kHz mono audio, 64 KiB per chunk and 2000 characters of
// speech text.
func DefaultLimits() Limits {
	return Limits{MaxAudioBytes: 8 << 20, MaxChunkBytes: 64 << 10, MaxTextRunes: 2000}
}

// Validate rejects a Limits with a non-positive field.
func (l Limits) Validate() error {
	switch {
	case l.MaxAudioBytes <= 0:
		return invalid("limits: MaxAudioBytes must be positive")
	case l.MaxChunkBytes <= 0:
		return invalid("limits: MaxChunkBytes must be positive")
	case l.MaxTextRunes <= 0:
		return invalid("limits: MaxTextRunes must be positive")
	}
	return nil
}

// CheckAudio validates a batch buffer: valid format, not empty, a whole number
// of sample frames, within MaxAudioBytes.
func (l Limits) CheckAudio(f AudioFormat, audio []byte) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	switch {
	case len(audio) == 0:
		return invalid("audio is empty")
	case len(audio)%f.FrameBytes() != 0:
		return invalid("audio length %d is not a whole number of %d-byte sample frames", len(audio), f.FrameBytes())
	case len(audio) > l.MaxAudioBytes:
		return limitError("max_audio_bytes", l.MaxAudioBytes, len(audio))
	}
	return nil
}

// CheckChunk validates one Send on a stream that has already received sent
// bytes: not empty, whole frames, within MaxChunkBytes, and the stream total
// within MaxAudioBytes. The format must already be valid.
func (l Limits) CheckChunk(f AudioFormat, chunk []byte, sent int) error {
	switch {
	case len(chunk) == 0:
		return invalid("audio chunk is empty")
	case len(chunk)%f.FrameBytes() != 0:
		return invalid("audio chunk length %d is not a whole number of %d-byte sample frames", len(chunk), f.FrameBytes())
	case len(chunk) > l.MaxChunkBytes:
		return limitError("max_chunk_bytes", l.MaxChunkBytes, len(chunk))
	case sent+len(chunk) > l.MaxAudioBytes:
		return limitError("max_audio_bytes", l.MaxAudioBytes, sent+len(chunk))
	}
	return nil
}

var (
	languageRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// ValidLanguage reports whether tag looks like a BCP 47 language tag ("en",
// "it-IT", "zh-Hant-TW"). The empty string is not valid here: requests treat
// "" as "the active pack's language" before calling it.
func ValidLanguage(tag string) bool { return languageRE.MatchString(tag) }

func checkLanguage(tag string) error {
	if tag != "" && !ValidLanguage(tag) {
		return invalid("language %q is not a language tag", tag)
	}
	return nil
}

// checkName validates an identifier that adapters forward to a runtime (a
// voice or model-profile name): short, no separators, no whitespace.
func checkName(what, v string) error {
	if v != "" && !nameRE.MatchString(v) {
		return invalid("%s %q is not a plain identifier", what, v)
	}
	return nil
}

func invalid(format string, args ...any) error {
	return contracts.New(contracts.ErrValidationFailed, "voice: "+fmt.Sprintf(format, args...))
}

// limitError is the typed rejection of an oversize request. validation_failed,
// not resource_exhausted (which is retryable): the identical oversize request
// can never succeed.
func limitError(which string, limit, got int) error {
	return contracts.Newf(contracts.ErrValidationFailed, "voice: request exceeds limit %s (%d > %d)", which, got, limit).
		WithDetails("limit_name", which).
		WithDetails("limit", limit).
		WithDetails("requested", got)
}
