package main

import (
	"math"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func seq(from, to int) []float64 {
	var out []float64
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}

func TestPercentiles(t *testing.T) {
	tests := []struct {
		name string
		xs   []float64
		pct  int
		want float64
		ok   bool
	}{
		// 1..10: rank = ceil(pct*10/100)
		{"p50 of ten", seq(1, 10), 50, 5, true},
		{"p90 of ten", seq(1, 10), 90, 9, true},
		{"p95 of ten rounds the rank up", seq(1, 10), 95, 10, true},
		{"p99 of ten", seq(1, 10), 99, 10, true},
		{"p100 is the max", seq(1, 10), 100, 10, true},
		{"p1 is the min", seq(1, 10), 1, 1, true},
		// 1..20: 95% of 20 is exactly 19; floating point would put it at 19.000000000000004 or 18.99999...
		{"p95 of twenty is exact", seq(1, 20), 95, 19, true},
		{"p50 of twenty", seq(1, 20), 50, 10, true},
		{"p50 of odd count", seq(1, 5), 50, 3, true},
		{"p95 of five", seq(1, 5), 95, 5, true},
		{"one sample is every percentile", []float64{42}, 95, 42, true},
		{"one sample p1", []float64{42}, 1, 42, true},
		{"unsorted input", []float64{9, 1, 5, 3, 7, 2, 8, 4, 10, 6}, 50, 5, true},
		{"ties", []float64{3, 3, 3, 9}, 50, 3, true},
		{"ties at the top", []float64{3, 3, 3, 9}, 95, 9, true},
		{"large n p99", seq(1, 1000), 99, 990, true},
		{"large n p95", seq(1, 1000), 95, 950, true},
		{"empty has no percentile", nil, 50, 0, false},
		{"percentile 0 is refused", seq(1, 10), 0, 0, false},
		{"percentile above 100 is refused", seq(1, 10), 101, 0, false},
		{"negative percentile is refused", seq(1, 10), -5, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := slices.Clone(tt.xs)
			got, ok := Percentile(tt.xs, tt.pct)
			if ok != tt.ok || got != tt.want {
				t.Errorf("Percentile(%v, %d) = %v, %v; want %v, %v", tt.xs, tt.pct, got, ok, tt.want, tt.ok)
			}
			if diff := cmp.Diff(orig, tt.xs); diff != "" {
				t.Errorf("Percentile modified its input (-before +after):\n%s", diff)
			}
		})
	}

	t.Run("the answer is always one of the samples", func(t *testing.T) {
		xs := []float64{1.5, 2.5, 10, 11.25, 100}
		for pct := 1; pct <= 100; pct++ {
			got, _ := Percentile(xs, pct)
			if !slices.Contains(xs, got) {
				t.Fatalf("p%d = %v is not a sample (nearest rank never interpolates)", pct, got)
			}
		}
	})

	t.Run("percentiles never decrease", func(t *testing.T) {
		xs := []float64{5, 1, 9, 2, 8, 3, 7, 4, 6, 100, 0.5}
		prev := math.Inf(-1)
		for pct := 1; pct <= 100; pct++ {
			got, _ := Percentile(xs, pct)
			if got < prev {
				t.Fatalf("p%d = %v is below p%d = %v", pct, got, pct-1, prev)
			}
			prev = got
		}
	})
}

func TestSummarize(t *testing.T) {
	t.Run("empty has no numbers", func(t *testing.T) {
		got := Summarize(nil)
		if diff := cmp.Diff(Stats{}, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
		if got.Mean != nil || got.P95 != nil {
			t.Error("an empty summary must carry nil statistics, not zero")
		}
	})
	t.Run("ten samples", func(t *testing.T) {
		got := Summarize(seq(1, 10))
		want := Stats{N: 10, Mean: round3(5.5), Min: round3(1), P50: round3(5), P90: round3(9), P95: round3(10), P99: round3(10), Max: round3(10)}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})
	t.Run("rounds to microseconds", func(t *testing.T) {
		got := Summarize([]float64{1.00049, 2.00051})
		if !floatEq(got.Min, 1) || !floatEq(got.Max, 2.001) {
			t.Errorf("min %v max %v; want 1 and 2.001", got.Min, got.Max)
		}
	})
	t.Run("not a number is dropped, never written", func(t *testing.T) {
		if round3(math.NaN()) != nil || round3(math.Inf(1)) != nil || round6(math.Inf(-1)) != nil {
			t.Error("NaN and infinity must become nil: encoding/json cannot marshal them")
		}
	})
}

func TestMedian(t *testing.T) {
	for _, tt := range []struct {
		xs   []float64
		want float64
		ok   bool
	}{
		{nil, 0, false},
		{[]float64{7}, 7, true},
		{[]float64{3, 1, 2}, 2, true},
		{[]float64{4, 1, 3, 2}, 2.5, true},
	} {
		got, ok := median(tt.xs)
		if got != tt.want || ok != tt.ok {
			t.Errorf("median(%v) = %v, %v; want %v, %v", tt.xs, got, ok, tt.want, tt.ok)
		}
	}
}
