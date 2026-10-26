package dictation

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unsafe"
)

// seedWAV builds a minimal PCM s16le mono 16 kHz WAV of nSamples samples.
func seedWAV(nSamples int, mutate func([]byte)) []byte {
	pcm := make([]byte, nSamples*2)
	for i := 0; i < nSamples; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], 1000)
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], SampleRateHz)
	binary.LittleEndian.PutUint32(h[28:], SampleRateHz*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	raw := append(h, pcm...)
	if mutate != nil {
		mutate(raw)
	}
	return raw
}

// FuzzParseWAV checks the WAV parser never panics on untrusted input, never
// returns a Clip whose PCM lies outside the input buffer, and always derives
// Duration from len(PCM).
func FuzzParseWAV(f *testing.F) {
	f.Add(seedWAV(1600, nil))                           // valid ~100 ms
	f.Add(seedWAV(4800, nil))                           // valid ~300 ms
	f.Add([]byte("RIFF"))                               // truncated header
	f.Add([]byte("RIFF....WAVEfmt "))                   // truncated after WAVE
	f.Add(seedWAV(100, func(b []byte) { b[36] = 'x' })) // bad data id
	// data chunk before fmt
	f.Add([]byte{
		'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E',
		'd', 'a', 't', 'a', 4, 0, 0, 0, 1, 2, 3, 4,
		'f', 'm', 't', ' ', 16, 0, 0, 0,
		1, 0, 1, 0, 0x80, 0x3e, 0, 0, 0, 0x7d, 0, 0, 2, 0, 16, 0,
	})
	// oversized fmt
	bigFmt := make([]byte, 12+8+128)
	copy(bigFmt[0:], "RIFF....WAVE")
	copy(bigFmt[12:], "fmt ")
	binary.LittleEndian.PutUint32(bigFmt[16:], 128)
	f.Add(bigFmt)
	// odd data size
	f.Add(seedWAV(10, func(b []byte) { binary.LittleEndian.PutUint32(b[40:], 3) }))
	// 0xFFFFFFFF data size
	f.Add(seedWAV(10, func(b []byte) { binary.LittleEndian.PutUint32(b[40:], 0xFFFFFFFF) }))
	// many junk chunks before data
	var many bytes.Buffer
	many.WriteString("RIFF")
	many.Write([]byte{0, 0, 0, 0})
	many.WriteString("WAVE")
	many.WriteString("fmt ")
	fmtBody := make([]byte, 4+16)
	binary.LittleEndian.PutUint32(fmtBody[0:], 16)
	binary.LittleEndian.PutUint16(fmtBody[4:], 1)
	binary.LittleEndian.PutUint16(fmtBody[6:], 1)
	binary.LittleEndian.PutUint32(fmtBody[8:], SampleRateHz)
	binary.LittleEndian.PutUint32(fmtBody[12:], SampleRateHz*2)
	binary.LittleEndian.PutUint16(fmtBody[16:], 2)
	binary.LittleEndian.PutUint16(fmtBody[18:], 16)
	many.Write(fmtBody)
	for i := 0; i < 20; i++ {
		many.WriteString("JUNK")
		many.Write([]byte{4, 0, 0, 0, 0, 0, 0, 0})
	}
	many.WriteString("data")
	many.Write([]byte{4, 0, 0, 0, 1, 0, 2, 0})
	f.Add(many.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xfe, 0x00})

	f.Fuzz(func(t *testing.T, raw []byte) {
		clip, err := ParseWAV(raw)
		if err != nil {
			return
		}
		if len(clip.PCM) == 0 {
			t.Fatal("accepted clip with empty PCM")
		}
		if len(clip.PCM) > len(raw) {
			t.Fatalf("PCM length %d exceeds raw %d", len(clip.PCM), len(raw))
		}
		// PCM must be a contiguous subslice of raw (same backing array).
		rawBase := uintptr(unsafe.Pointer(unsafe.SliceData(raw)))
		pcmBase := uintptr(unsafe.Pointer(unsafe.SliceData(clip.PCM)))
		if pcmBase < rawBase || pcmBase+uintptr(len(clip.PCM)) > rawBase+uintptr(len(raw)) {
			t.Fatal("PCM lies outside the input buffer")
		}
		want := clip.Format.DurationOf(len(clip.PCM))
		if clip.Duration != want {
			t.Fatalf("Duration = %v, want %v from len(PCM)=%d", clip.Duration, want, len(clip.PCM))
		}
	})
}
