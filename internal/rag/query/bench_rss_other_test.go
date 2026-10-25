//go:build !windows && !linux

package query

func benchProcessMemory() (rss, peakRSS uint64, ok bool) { return 0, 0, false }
