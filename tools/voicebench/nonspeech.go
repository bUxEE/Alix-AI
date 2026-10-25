package main

import (
	"encoding/binary"
	"math"
	"math/rand"
)

// NonSpeechRate is the sample rate of the non-speech clips (16 kHz mono, the only rate LocalAI's VAD takes).
const NonSpeechRate = 16000

// NonSpeechKinds are the four deterministic synthetic clips of the protocol, in order. They contain no person's voice.
var NonSpeechKinds = []string{"silence", "white", "hum", "lowpass"}

// NonSpeechPCM returns the PCM16 mono samples (little endian) of one kind. The fixed seed makes every run produce the same bytes
// (the results record their sha256, so drift is visible).
//
//	silence  exact digital silence, 3 s
//	white    white noise at -50 dBFS RMS, 3 s
//	hum      50 Hz sine at -40 dBFS RMS plus white noise at -55 dBFS RMS, 3 s
//	lowpass  one-pole low-pass noise scaled to -30 dBFS RMS, 4 s
func NonSpeechPCM(kind string) []byte {
	rng := rand.New(rand.NewSource(20261004))
	dbfs := func(db float64) float64 { return 32768 * math.Pow(10, db/20) }
	var x []float64
	switch kind {
	case "silence":
		x = make([]float64, 3*NonSpeechRate)
	case "white":
		x = make([]float64, 3*NonSpeechRate)
		s := dbfs(-50)
		for i := range x {
			x[i] = rng.NormFloat64() * s
		}
	case "hum":
		x = make([]float64, 3*NonSpeechRate)
		peak, s := dbfs(-40)*math.Sqrt2, dbfs(-55)
		for i := range x {
			x[i] = peak*math.Sin(2*math.Pi*50*float64(i)/NonSpeechRate) + rng.NormFloat64()*s
		}
	case "lowpass":
		x = make([]float64, 4*NonSpeechRate)
		var y float64
		for i := range x {
			y = 0.95*y + 0.05*rng.NormFloat64()
			x[i] = y
		}
		var sum float64
		for _, v := range x {
			sum += v * v
		}
		if rms := math.Sqrt(sum / float64(len(x))); rms > 0 {
			k := dbfs(-30) / rms
			for i := range x {
				x[i] *= k
			}
		}
	default:
		return nil
	}
	out := make([]byte, 2*len(x))
	for i, v := range x {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(math.Max(-32768, math.Min(32767, math.Round(v))))))
	}
	return out
}
