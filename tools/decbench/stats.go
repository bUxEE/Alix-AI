package main

import (
	"math"
	"sort"
)

// PercentileMethod names the percentile definition used in every result.
const PercentileMethod = "nearest-rank"

// nearestRank returns the value at 1-based rank ceil(pct*n/100) of ascending, non-empty
// sorted samples. The rank is computed in integers so 95% of 20 is exactly rank 19.
func nearestRank(sorted []float64, pct int) float64 {
	n := len(sorted)
	rank := (pct*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// Percentile returns the pct-th percentile (1 to 100) of xs by the nearest-rank method.
// There is no interpolation: the answer is always one of the samples. ok is false for an
// empty slice or a pct outside 1..100.
func Percentile(xs []float64, pct int) (float64, bool) {
	if len(xs) == 0 || pct < 1 || pct > 100 {
		return 0, false
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return nearestRank(sorted, pct), true
}

// Stats summarises n latency samples. There is deliberately no mean: below the host timer
// tick a mean says nothing, so only order statistics are reported, always with the sample
// count N. Every statistic is nil when N is 0.
type Stats struct {
	N   int      `json:"n"`
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
	P99 *float64 `json:"p99"`
	Max *float64 `json:"max"`
}

// Summarize computes Stats over xs (milliseconds), rounded to three decimals.
func Summarize(xs []float64) Stats {
	if len(xs) == 0 {
		return Stats{}
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return Stats{
		N:   len(sorted),
		P50: round3(nearestRank(sorted, 50)),
		P95: round3(nearestRank(sorted, 95)),
		P99: round3(nearestRank(sorted, 99)),
		Max: round3(sorted[len(sorted)-1]),
	}
}

func round3(x float64) *float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	r := math.Round(x*1000) / 1000
	return &r
}

func round6(x float64) *float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	r := math.Round(x*1e6) / 1e6
	return &r
}

// Ratio is passed out of total; Rate is nil when Total is 0 (unknown is never 0%).
type Ratio struct {
	Passed int      `json:"passed"`
	Total  int      `json:"total"`
	Rate   *float64 `json:"rate"`
}

func newRatio(passed, total int) Ratio {
	r := Ratio{Passed: passed, Total: total}
	if total > 0 {
		r.Rate = round6(float64(passed) / float64(total))
	}
	return r
}
