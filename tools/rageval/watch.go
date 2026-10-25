package main

import "time"

// stopwatch measures an interval with the finest clock the system offers. On
// Windows the clock behind time.Since ticks every 0.5 ms or so, which turns a
// 0.2 ms stage into 0.000 and a 3 ms query into 2.5 or 3.0; there the stopwatch
// reads the performance counter instead (watch_windows.go). Elsewhere it is
// time.Since.
type stopwatch struct {
	t0 time.Time
	c0 int64
}

func startWatch() stopwatch { return stopwatch{t0: time.Now(), c0: counterNow()} }

// elapsed is the time since startWatch.
func (s stopwatch) elapsed() time.Duration {
	if d, ok := counterSince(s.c0); ok {
		return d
	}
	return time.Since(s.t0)
}
