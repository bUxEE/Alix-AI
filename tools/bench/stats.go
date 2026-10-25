package main

import (
	"math"
	"sort"
)

// PercentileMethod names the percentile definition used everywhere in a
// result. It is recorded in config.percentile_method so that two result files
// are only compared when they agree.
const PercentileMethod = "nearest-rank"

// Percentile returns the pct-th percentile (1 to 100) of xs by the nearest-rank
// method: the value at 1-based rank ceil(pct/100 * n) of the n sorted samples.
// There is no interpolation, so the answer is always one of the samples, and
// p100 is the maximum. The rank is computed in integers, which keeps results
// exact where floating point would round (95/100*20 is not exactly 19). xs
// need not be sorted and is not modified. ok is false when xs is empty or pct
// is outside 1..100.
func Percentile(xs []float64, pct int) (v float64, ok bool) {
	if len(xs) == 0 || pct < 1 || pct > 100 {
		return 0, false
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return nearestRank(sorted, pct), true
}

// nearestRank assumes sorted is ascending and non-empty and pct is 1..100.
func nearestRank(sorted []float64, pct int) float64 {
	n := len(sorted)
	rank := (pct*n + 99) / 100 // ceil(pct*n/100)
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// Stats is the summary of n samples. Every statistic is nil when n is 0, so a
// result never carries NaN (which encoding/json cannot marshal) or a made-up
// zero.
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

// Summarize computes Stats over xs. Values are rounded to three decimals (a
// microsecond when the unit is a millisecond), so results are stable text.
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

// median returns the median of xs (the mean of the two middle values for an
// even count). ok is false for an empty slice.
func median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2], true
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2, true
}
