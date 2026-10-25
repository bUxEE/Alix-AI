package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ParseWAV returns the PCM16 payload and sample rate of a mono WAV file: 16-bit PCM as it is, 32-bit IEEE float (some FLEURS clips) converted
// to 16-bit PCM, the only format the adapter takes. Other layouts are an error: the benchmark never resamples or mixes down, so what it
// sends is what it measured.
func ParseWAV(b []byte) (pcm []byte, rate int, err error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a RIFF/WAVE file")
	}
	var fmtSeen, isFloat bool
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) {
				return nil, 0, errors.New("short fmt chunk")
			}
			tag := binary.LittleEndian.Uint16(b[body:])
			ch := binary.LittleEndian.Uint16(b[body+2:])
			rate = int(binary.LittleEndian.Uint32(b[body+4:]))
			bits := binary.LittleEndian.Uint16(b[body+14:])
			isFloat = tag == 3 && bits == 32
			if ch != 1 || !(tag == 1 && bits == 16 || isFloat) {
				return nil, 0, fmt.Errorf("want mono 16-bit PCM or 32-bit float, got format %d, %d channels, %d bits", tag, ch, bits)
			}
			fmtSeen = true
		case "data":
			if !fmtSeen {
				return nil, 0, errors.New("data chunk before fmt chunk")
			}
			end := body + size
			if end > len(b) || size < 0 { // a streamed header carries 0xFFFFFFFF: take what is there
				end = len(b)
			}
			if isFloat {
				return floatToPCM16(b[body:end]), rate, nil
			}
			return b[body:end], rate, nil
		}
		off = body + size + size%2
	}
	return nil, 0, errors.New("no data chunk")
}

// floatToPCM16 converts little-endian float32 samples in [-1, 1] to 16-bit little-endian PCM (clipped, rounded to nearest).
func floatToPCM16(f []byte) []byte {
	out := make([]byte, 2*(len(f)/4))
	for i := 0; i+4 <= len(f); i += 4 {
		x := float64(math.Float32frombits(binary.LittleEndian.Uint32(f[i:])))
		binary.LittleEndian.PutUint16(out[i/2:], uint16(int16(math.Max(-32768, math.Min(32767, math.Round(x*32767))))))
	}
	return out
}

// WriteWAV wraps mono 16-bit little-endian PCM in a canonical 44-byte WAV header.
func WriteWAV(pcm []byte, rate int) []byte {
	h := make([]byte, 44, 44+len(pcm))
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

// pcmMS is the duration of mono 16-bit PCM in milliseconds.
func pcmMS(pcm []byte, rate int) float64 {
	if rate <= 0 {
		return 0
	}
	return float64(len(pcm)/2) * 1000 / float64(rate)
}
