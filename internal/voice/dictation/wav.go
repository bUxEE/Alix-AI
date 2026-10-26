package dictation

import (
	"encoding/binary"
	"time"

	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Only accepted shape for a dictation body: WAV wrapping PCM s16le mono 16 kHz.
const (
	SampleRateHz   = 16000
	MinDurationMS  = 300
	maxWAVChunks   = 16
	maxWAVSkip     = 256
	wavUnknownSize = 0xFFFFFFFF
)

// Clip is a validated PCM buffer taken from a WAV body.
type Clip struct {
	PCM      []byte
	Format   voice.AudioFormat
	Duration time.Duration
}

// ParseWAV accepts only PCM s16le mono 16 kHz WAV. duration is computed from the
// data length; a client-supplied duration is never trusted.
func ParseWAV(raw []byte) (Clip, error) {
	if len(raw) < 44 {
		return Clip{}, invalid("the audio body is not a WAV file")
	}
	f, dataOff, dataLen, err := parseWAVHeader(raw)
	if err != nil {
		return Clip{}, err
	}
	if f.Encoding != voice.EncodingPCM16 || f.Channels != 1 || f.SampleRate != SampleRateHz {
		return Clip{}, invalid("audio must be WAV PCM s16le mono at 16000 Hz")
	}
	if dataLen < 0 {
		return Clip{}, invalid("the WAV data size is unknown")
	}
	end := dataOff + int(dataLen)
	if dataOff < 0 || end > len(raw) || dataLen%2 != 0 {
		return Clip{}, invalid("the WAV data chunk is truncated or misaligned")
	}
	pcm := raw[dataOff:end]
	if len(pcm) == 0 {
		return Clip{}, invalid("the audio body is empty")
	}
	d := f.DurationOf(len(pcm))
	return Clip{PCM: pcm, Format: f, Duration: d}, nil
}

func parseWAVHeader(raw []byte) (f voice.AudioFormat, dataOff int, dataLen int64, err error) {
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return f, 0, 0, invalid("the audio body is not a WAV file")
	}
	off := 12
	var haveFmt bool
	for range maxWAVChunks {
		if off+8 > len(raw) {
			return f, 0, 0, invalid("the WAV header is truncated")
		}
		id := string(raw[off : off+4])
		size := binary.LittleEndian.Uint32(raw[off+4:])
		off += 8
		switch id {
		case "fmt ":
			if size < 16 || size > 64 {
				return f, 0, 0, invalid("WAV fmt chunk has an unusable size")
			}
			if off+int(size) > len(raw) {
				return f, 0, 0, invalid("the WAV header is truncated")
			}
			buf := raw[off : off+int(size)]
			if binary.LittleEndian.Uint16(buf[0:]) != 1 || binary.LittleEndian.Uint16(buf[14:]) != 16 {
				return f, 0, 0, invalid("audio must be WAV PCM s16le mono at 16000 Hz")
			}
			f = voice.AudioFormat{
				Encoding:   voice.EncodingPCM16,
				Channels:   int(binary.LittleEndian.Uint16(buf[2:])),
				SampleRate: int(binary.LittleEndian.Uint32(buf[4:])),
			}
			if verr := f.Validate(); verr != nil {
				return f, 0, 0, invalid("audio must be WAV PCM s16le mono at 16000 Hz")
			}
			haveFmt = true
			off += int(size) + int(size%2)
		case "data":
			if !haveFmt {
				return f, 0, 0, invalid("WAV data chunk precedes its fmt chunk")
			}
			if size == 0 || size == wavUnknownSize {
				return f, 0, 0, invalid("the WAV data size is unknown")
			}
			return f, off, int64(size), nil
		default:
			if size > maxWAVSkip {
				return f, 0, 0, invalid("WAV header chunk is too large")
			}
			skip := int(size) + int(size%2)
			if off+skip > len(raw) {
				return f, 0, 0, invalid("the WAV header is truncated")
			}
			off += skip
		}
	}
	return f, 0, 0, invalid("WAV header has too many chunks")
}

func invalid(msg string) *contracts.Error {
	return contracts.New(contracts.ErrValidationFailed, "dictation: "+msg)
}
