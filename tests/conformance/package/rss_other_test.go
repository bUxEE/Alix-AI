//go:build !windows && !linux

package conformancepkg

// processMemory is not implemented on this platform; the footprint test then
// only reports timings.
func processMemory() (rss, peakRSS, private uint64, ok bool) { return 0, 0, 0, false }
