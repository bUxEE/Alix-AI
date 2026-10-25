package verify

import (
	"strings"
	"testing"
	"time"
)

// Hostile text of the largest accepted size must not cost more than linear work: a long run of
// separators is one claim per part, and each claim must stay cheap. The test compares the time at
// 16 KiB with the time at 1 KiB, so it measures how the cost grows and not how fast the host is
// (it holds under -race and on a loaded runner): linear work gives a ratio of 16 to 50, quadratic work
// near 256, and the bound is 100 on the best of three runs of each size.
func TestPathologicalInputsStayLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	d := confirmDirective()
	units := map[string]string{
		"digits":      "9",
		"ones":        "1 ",
		"dashes":      "1-",
		"slashes":     "1/",
		"colons":      "10:",
		"cites":       "[1]",
		"brackets":    "[",
		"words":       "twenty ",
		"hundred":     "one hundred and ",
		"ids":         "apt_1 ",
		"srcids":      "src_01J0000000000000000000000A ",
		"months":      "14 October ",
		"euro":        "\u20ac 1 ",
		"sources":     "Source:\n- a\n",
		"sourcelines": "Source:\n- a\n- b\n",
		"booked":      "not booked ",
		"dots":        "1.",
		"nbsp":        "1\u00a0000",
		"apos":        "1'",
		"wdates":      "Tuesday the ",
	}
	// best is the best of three timings of reps verifications, per verification.
	best := func(text string, opts Options, reps int) time.Duration {
		min := time.Duration(1<<63 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			for r := 0; r < reps; r++ {
				if _, err := Verify(text, d, nil, opts); err != nil {
					t.Fatal(err)
				}
			}
			if el := time.Since(start) / time.Duration(reps); el < min {
				min = el
			}
		}
		return min
	}
	for name, unit := range units {
		small := strings.Repeat(unit, (1<<10)/len(unit))
		large := strings.Repeat(unit, (MaxTextBytes)/len(unit))
		if len(large) > MaxTextBytes {
			large = large[:MaxTextBytes]
		}
		for _, opts := range []Options{en, it} {
			ts, tl := best(small, opts, 16), best(large, opts, 1)
			ratio := float64(tl) / float64(ts+1)
			t.Logf("%-12s %s 1 KiB %-10v 16 KiB %-10v ratio %.1f", name, opts.Language, ts, tl, ratio)
			// 16x the text: linear work costs 16x to 50x (allocation and cache effects), quadratic
			// work about 256x. The bound sits between, and a cost under 50 microseconds at 1 KiB is
			// below what the clock can resolve.
			if ts > 50*time.Microsecond && ratio > 100 {
				t.Errorf("%s (%s): the cost grew %.1fx for 16x the text; linear work grows 16x to 50x", name, opts.Language, ratio)
			}
		}
	}
}
