package main

import (
	"math"
	"sort"
)

// The statistics types are the ones of schemas/bench/result.schema.json
// (stats, ratio) with the same names, the same nearest-rank percentile and the
// same rounding, so that a number read from a rageval result and one read from
// a bench result mean the same thing. tools/bench is a main package and cannot
// be imported; tools/rageval/envelope_test.go runs the real `bench check` over
// a rageval result so that drift between the two copies cannot go unseen.

// PercentileMethod names the percentile definition used everywhere in a result.
const PercentileMethod = "nearest-rank"

// nearestRank assumes sorted is ascending and non-empty and pct is 1..100.
func nearestRank(sorted []float64, pct int) float64 {
	n := len(sorted)
	rank := (pct*n + 99) / 100 // ceil(pct*n/100), in integers
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// Percentile returns the pct-th percentile (1 to 100) of xs by the
// nearest-rank method: the value at 1-based rank ceil(pct/100 * n) of the n
// sorted samples, with no interpolation. ok is false when xs is empty or pct is
// outside 1..100.
func Percentile(xs []float64, pct int) (v float64, ok bool) {
	if len(xs) == 0 || pct < 1 || pct > 100 {
		return 0, false
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return nearestRank(sorted, pct), true
}

// Stats is the summary of n samples. Every statistic is nil when n is 0, so a
// result never carries NaN or a made-up zero.
type Stats struct {
	N    int      `json:"n"`
	Mean *float64 `json:"mean"`
	Min  *float64 `json:"min"`
	P50  *float64 `json:"p50"`
	P90  *float64 `json:"p90"`
	P95  *float64 `json:"p95"`
	P99  *float64 `json:"p99"`
	Max  *float64 `json:"max"`
}

// Summarize computes Stats over xs, rounded to three decimals.
func Summarize(xs []float64) Stats {
	if len(xs) == 0 {
		return Stats{}
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, x := range sorted {
		sum += x
	}
	return Stats{
		N:    len(sorted),
		Mean: round3(sum / float64(len(sorted))),
		Min:  round3(sorted[0]),
		P50:  round3(nearestRank(sorted, 50)),
		P90:  round3(nearestRank(sorted, 90)),
		P95:  round3(nearestRank(sorted, 95)),
		P99:  round3(nearestRank(sorted, 99)),
		Max:  round3(sorted[len(sorted)-1]),
	}
}

// round3 rounds to three decimals and returns a pointer, or nil for NaN and
// infinities (which JSON cannot carry).
func round3(x float64) *float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	r := math.Round(x*1000) / 1000
	return &r
}

// round6 is round3 with six decimals, for rates.
func round6(x float64) *float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	r := math.Round(x*1e6) / 1e6
	return &r
}

// Ratio is passed out of total; Rate is nil when Total is 0.
type Ratio struct {
	Passed int      `json:"passed"`
	Total  int      `json:"total"`
	Rate   *float64 `json:"rate"`
}

// newRatio builds a Ratio with its rate rounded to six decimals.
func newRatio(passed, total int) Ratio {
	r := Ratio{Passed: passed, Total: total}
	if total > 0 {
		r.Rate = round6(float64(passed) / float64(total))
	}
	return r
}
