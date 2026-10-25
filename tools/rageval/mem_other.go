//go:build !windows && !linux

package main

// processMemory is not available on this system: the result carries null.
func processMemory() (rss, peak uint64, ok bool) { return 0, 0, false }
