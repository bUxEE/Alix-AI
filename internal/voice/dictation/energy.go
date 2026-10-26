package dictation

import (
	"encoding/binary"
	"math"
)

// Silence thresholds for PCM s16le. A clip below both is refused so STT cannot
// invent text on near-empty audio (ADR 0039).
const (
	silentPeakMax = 64   // |sample| peak
	silentRMSMax  = 32.0 // root-mean-square
)

// IsSilent reports whether pcm (s16le mono) has negligible energy.
func IsSilent(pcm []byte) bool {
	if len(pcm) < 2 {
		return true
	}
	var peak int32
	var sumSquares float64
	n := len(pcm) / 2
	for i := 0; i < n; i++ {
		s := int32(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		if s < 0 {
			s = -s
		}
		if s > peak {
			peak = s
		}
		sumSquares += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSquares / float64(n))
	return peak <= silentPeakMax && rms <= silentRMSMax
}
