package voice

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// wavHeaderBytes is the size of the canonical header wavHeader writes.
const wavHeaderBytes = 44

// wavHeader returns the canonical 44-byte header of a PCM16 WAV file holding
// dataLen bytes of samples. LocalAI takes uploads as files and converts them
// itself (ffmpeg), so the container is the adapter's concern: the core only
// ever carries raw PCM. The header is built in memory, never on disk.
func wavHeader(f pv.AudioFormat, dataLen int) []byte {
	h := make([]byte, wavHeaderBytes)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(f.BytesPerSecond()))
	binary.LittleEndian.PutUint16(h[32:], uint16(f.FrameBytes()))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}

// Limits on a WAV header read from the runtime: a header is a handful of small
// chunks, so anything else is not one.
const (
	maxWAVChunks    = 16
	maxWAVSkipBytes = 1 << 20
	wavUnknownSize  = 0xFFFFFFFF
)

// readWAVHeader consumes a WAV header from r up to and including the "data"
// chunk header, and returns the sample format and the number of data bytes
// that follow (-1 when the runtime does not know it: a streamed file carries
// 0xFFFFFFFF, or 0, in the size fields). Only 16-bit linear PCM, mono or
// stereo, in the rate range the voice seam carries, is accepted. A problem
// with the header's content is a *contracts.Error (schema_invalid, or
// unavailable for a body that ends inside it); a failure of the read itself is
// returned as is, for the caller to map.
func readWAVHeader(r io.Reader) (f pv.AudioFormat, dataLen int64, err error) {
	var riff [12]byte
	if err := readFull(r, riff[:]); err != nil {
		return f, 0, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return f, 0, schemaInvalid("runtime did not answer with a WAV stream")
	}
	var haveFmt bool
	for range maxWAVChunks {
		var ch [8]byte
		if err := readFull(r, ch[:]); err != nil {
			return f, 0, err
		}
		id, size := string(ch[0:4]), binary.LittleEndian.Uint32(ch[4:])
		switch id {
		case "fmt ":
			if size < 16 || size > 64 {
				return f, 0, schemaInvalid("WAV fmt chunk has an unusable size")
			}
			buf := make([]byte, size+size%2)
			if err := readFull(r, buf); err != nil {
				return f, 0, err
			}
			f = pv.AudioFormat{
				Encoding:   pv.EncodingPCM16,
				Channels:   int(binary.LittleEndian.Uint16(buf[2:])),
				SampleRate: int(binary.LittleEndian.Uint32(buf[4:])),
			}
			if binary.LittleEndian.Uint16(buf[0:]) != 1 || binary.LittleEndian.Uint16(buf[14:]) != 16 {
				return f, 0, schemaInvalid("runtime audio is not 16-bit linear PCM")
			}
			if verr := f.Validate(); verr != nil {
				return f, 0, contracts.Wrap(contracts.ErrSchemaInvalid, verr, "localai voice: runtime reported an unusable audio format")
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return f, 0, schemaInvalid("WAV data chunk precedes its fmt chunk")
			}
			if size == 0 || size == wavUnknownSize {
				return f, -1, nil
			}
			return f, int64(size), nil
		default:
			if size > maxWAVSkipBytes {
				return f, 0, schemaInvalid("WAV header chunk is too large")
			}
			if _, err := io.CopyN(io.Discard, r, int64(size)+int64(size%2)); err != nil {
				return f, 0, asShort(err)
			}
		}
	}
	return f, 0, schemaInvalid("WAV header has too many chunks")
}

func readFull(r io.Reader, buf []byte) error {
	_, err := io.ReadFull(r, buf)
	return asShort(err)
}

// asShort turns the end of the body inside a header into an interrupted
// stream and leaves every other error for the caller to map.
func asShort(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return interrupted("audio stream ended inside its header")
	}
	return err
}

// vadSampleRate is the only rate LocalAI's /v1/vad accepts: "array of audio
// samples (16kHz PCM float)", mono. Audio at any other rate is refused rather
// than resampled (resampling would be a guess the caller never asked for).
const vadSampleRate = 16000

// vadAudioJSON encodes PCM16 mono samples as the JSON float array LocalAI's
// VAD takes, scaling each sample by 1/32768. Six decimals are finer than the
// 16-bit step (about 3e-5), so nothing is lost.
func vadAudioJSON(model string, pcm []byte) []byte {
	n := len(pcm) / 2
	b := make([]byte, 0, 64+len(model)+n*9)
	b = append(b, `{"model":`...)
	b = strconv.AppendQuote(b, model)
	b = append(b, `,"audio":[`...)
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		s := int16(binary.LittleEndian.Uint16(pcm[2*i:]))
		b = strconv.AppendFloat(b, float64(s)/32768, 'f', 6, 32)
	}
	return append(b, `]}`...)
}

// secondsToDuration converts a runtime-reported time in seconds. The bound
// keeps a nonsense value from overflowing the conversion.
func secondsToDuration(s float64) (d time.Duration, ok bool) {
	if math.IsNaN(s) || s < 0 || s > 1e6 {
		return 0, false
	}
	return time.Duration(math.Round(s * 1e9)), true
}
