//go:build !windows && !linux

package resources

import "runtime"

// osProcessRSS approximates the resident set with the memory the Go runtime
// holds from the OS and has not returned (Sys - HeapReleased). It is an upper
// bound, which is the safe side for admission.
func osProcessRSS() (uint64, error) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys - m.HeapReleased, nil
}
