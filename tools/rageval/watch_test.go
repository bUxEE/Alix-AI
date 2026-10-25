package main

import (
	"testing"
	"time"
)

func TestStopwatch(t *testing.T) {
	w := startWatch()
	if d := w.elapsed(); d < 0 || d > 50*time.Millisecond {
		t.Errorf("an interval of nothing measured %v", d)
	}
	time.Sleep(5 * time.Millisecond)
	if d := w.elapsed(); d < 4*time.Millisecond || d > 2*time.Second {
		t.Errorf("an interval of 5ms measured %v", d)
	}
	// It resolves intervals far finer than a millisecond where the system's
	// ordinary clock does not (Windows): sixty tiny intervals must not all be
	// zero or all land on whole half-milliseconds.
	nonzero, distinct := 0, map[time.Duration]bool{}
	for i := 0; i < 60; i++ {
		w := startWatch()
		x := 0
		for j := 0; j < 2000; j++ {
			x += j
		}
		_ = x
		d := w.elapsed()
		if d > 0 {
			nonzero++
		}
		distinct[d] = true
	}
	if nonzero == 0 || len(distinct) < 3 {
		t.Errorf("sixty small intervals gave %d non-zero and %d distinct values", nonzero, len(distinct))
	}
}
