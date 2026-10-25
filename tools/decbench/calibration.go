package main

import "math"

// CalibrationBins is the fixed number of equal-width confidence bins of the expected
// calibration error. Bin i holds confidences in [i/10, (i+1)/10); the last bin is closed
// on the right, so a confidence of exactly 1 lands in it.
const CalibrationBins = 10

// CalPoint is one scored selection: the calibrated confidence the scorer reported for the
// candidate it selected, and whether that candidate was the right one.
type CalPoint struct {
	Confidence float64
	Correct    bool
}

// Calibration is the calibration block of a configuration. Only selections the engine
// ACCEPTED are points: a fallback carries no selection, so it has no confidence to
// calibrate. N is the number of points. Brier and ECE are nil when N is 0.
type Calibration struct {
	N     int      `json:"n"`
	Bins  int      `json:"bins"`
	Brier *float64 `json:"brier"`
	ECE   *float64 `json:"ece"`
}

// Brier is the mean squared difference between the confidence and the 0/1 outcome:
// (1/N) * sum (confidence - correct)^2. ok is false for no points.
func Brier(ps []CalPoint) (float64, bool) {
	if len(ps) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, p := range ps {
		o := 0.0
		if p.Correct {
			o = 1
		}
		d := p.Confidence - o
		sum += d * d
	}
	return sum / float64(len(ps)), true
}

// binOf returns the bin of a confidence in [0, 1].
func binOf(c float64) int {
	b := int(math.Floor(c * CalibrationBins))
	if b >= CalibrationBins {
		b = CalibrationBins - 1
	}
	if b < 0 {
		b = 0
	}
	return b
}

// ECE is the expected calibration error over CalibrationBins equal-width bins:
// sum over bins of (n_b / N) * |accuracy_b - mean confidence_b|. ok is false for no points.
func ECE(ps []CalPoint) (float64, bool) {
	if len(ps) == 0 {
		return 0, false
	}
	var n [CalibrationBins]int
	var correct [CalibrationBins]int
	var conf [CalibrationBins]float64
	for _, p := range ps {
		b := binOf(p.Confidence)
		n[b]++
		conf[b] += p.Confidence
		if p.Correct {
			correct[b]++
		}
	}
	ece := 0.0
	for b := 0; b < CalibrationBins; b++ {
		if n[b] == 0 {
			continue
		}
		acc := float64(correct[b]) / float64(n[b])
		avg := conf[b] / float64(n[b])
		ece += float64(n[b]) / float64(len(ps)) * math.Abs(acc-avg)
	}
	return ece, true
}

func calibrationOf(ps []CalPoint) Calibration {
	c := Calibration{N: len(ps), Bins: CalibrationBins}
	if b, ok := Brier(ps); ok {
		c.Brier = round6(b)
	}
	if e, ok := ECE(ps); ok {
		c.ECE = round6(e)
	}
	return c
}
