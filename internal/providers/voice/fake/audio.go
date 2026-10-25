package fake

import (
	"encoding/binary"
	"hash/fnv"
	"time"

	"github.com/buxee/alix-ai/internal/providers/voice"
)

// Silence returns d of silent PCM in format f. The format must be valid.
func Silence(f voice.AudioFormat, d time.Duration) []byte {
	return make([]byte, f.BytesFor(d))
}

// Tone returns d of square-wave PCM in format f at the given peak amplitude
// (0 to 32767), a stand-in for speech that the fake VAD detects. The format
// must be valid.
func Tone(f voice.AudioFormat, d time.Duration, amplitude int16) []byte {
	out := make([]byte, f.BytesFor(d))
	period := f.SampleRate / 200 // a 200 Hz square wave
	if period < 2 {
		period = 2
	}
	for i := 0; i+f.FrameBytes() <= len(out); i += f.FrameBytes() {
		s := amplitude
		if (i/f.FrameBytes()/(period/2))%2 == 1 {
			s = -amplitude
		}
		for ch := 0; ch < f.Channels; ch++ {
			binary.LittleEndian.PutUint16(out[i+2*ch:], uint16(s))
		}
	}
	return out
}

// Speech concatenates audio: handy for building silence, speech, silence.
func Speech(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// synthPCM returns n bytes of deterministic, non-silent PCM that is a pure
// function of the text, the chunk number and the format.
func synthPCM(text string, chunk int, f voice.AudioFormat, n int) []byte {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	seed := int(h.Sum32()%7919) + chunk*131
	out := make([]byte, n)
	for i := 0; i+f.FrameBytes() <= n; i += f.FrameBytes() {
		frame := i / f.FrameBytes()
		s := int16((seed+frame*37)%8000 - 4000)
		for ch := 0; ch < f.Channels; ch++ {
			binary.LittleEndian.PutUint16(out[i+2*ch:], uint16(s))
		}
	}
	return out
}
