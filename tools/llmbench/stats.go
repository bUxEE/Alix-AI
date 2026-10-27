package main

import (
	"math"
	"sort"
	"time"
)

// Sample is one completed (or failed) request.
type Sample struct {
	TurnID           string        `json:"turn_id"`
	Concurrency      int           `json:"concurrency"`
	TTFT             time.Duration `json:"-"`
	Full             time.Duration `json:"-"`
	TTFT_ms          float64       `json:"ttft_ms"`
	Full_ms          float64       `json:"full_ms"`
	CompletionTokens int           `json:"completion_tokens"`
	PromptTokens     int           `json:"prompt_tokens"`
	Err              string        `json:"error,omitempty"`
}

// Aggregate summarises samples at one concurrency.
type Aggregate struct {
	Concurrency        int     `json:"concurrency"`
	N                  int     `json:"n"`
	Errors             int     `json:"errors"`
	TTFT_P50_ms        float64 `json:"ttft_p50_ms"`
	TTFT_P95_ms        float64 `json:"ttft_p95_ms"`
	Full_P50_ms        float64 `json:"full_answer_p50_ms"`
	Full_P95_ms        float64 `json:"full_answer_p95_ms"`
	TokPerSecPerUser   float64 `json:"tokens_per_sec_per_user"`
	TokPerSecAggregate float64 `json:"tokens_per_sec_aggregate"`
	WithinTarget       bool    `json:"within_target_latency"`
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	w := idx - float64(lo)
	return sorted[lo]*(1-w) + sorted[hi]*w
}

// Summarise builds aggregates per concurrency. targetFull is the p95 full-answer budget.
func Summarise(samples []Sample, targetFull time.Duration) []Aggregate {
	by := map[int][]Sample{}
	for _, s := range samples {
		by[s.Concurrency] = append(by[s.Concurrency], s)
	}
	var conc []int
	for c := range by {
		conc = append(conc, c)
	}
	sort.Ints(conc)
	var out []Aggregate
	for _, c := range conc {
		group := by[c]
		var ttft, full []float64
		var tok int
		var wall time.Duration
		var errs int
		var okFull []time.Duration
		for _, s := range group {
			if s.Err != "" {
				errs++
				continue
			}
			ttft = append(ttft, float64(s.TTFT.Milliseconds()))
			full = append(full, float64(s.Full.Milliseconds()))
			tok += s.CompletionTokens
			wall += s.Full
			okFull = append(okFull, s.Full)
		}
		sort.Float64s(ttft)
		sort.Float64s(full)
		agg := Aggregate{
			Concurrency: c,
			N:           len(group),
			Errors:      errs,
			TTFT_P50_ms: percentile(ttft, 0.50),
			TTFT_P95_ms: percentile(ttft, 0.95),
			Full_P50_ms: percentile(full, 0.50),
			Full_P95_ms: percentile(full, 0.95),
		}
		if wall > 0 && len(okFull) > 0 {
			// Per-user average decode rate: total completion tokens / sum of per-request durations.
			agg.TokPerSecPerUser = float64(tok) / wall.Seconds()
			// Aggregate estimate: per-request p50 rate × concurrency.
			if agg.Full_P50_ms > 0 {
				agg.TokPerSecAggregate = float64(tok) / (float64(len(okFull)) * agg.Full_P50_ms / 1000.0) * float64(c)
			}
		}
		if targetFull > 0 && !math.IsNaN(agg.Full_P95_ms) {
			agg.WithinTarget = errs == 0 && time.Duration(agg.Full_P95_ms)*time.Millisecond <= targetFull
		}
		out = append(out, agg)
	}
	return out
}

// MaxConcurrentWithinTarget returns the largest concurrency whose aggregate is within target.
func MaxConcurrentWithinTarget(aggs []Aggregate) int {
	best := 0
	for _, a := range aggs {
		if a.WithinTarget && a.Concurrency > best {
			best = a.Concurrency
		}
	}
	return best
}
