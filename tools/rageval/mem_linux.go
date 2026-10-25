//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// processMemory reports this process's resident set and its high-water mark
// since the process started, in bytes, from /proc/self/status (VmRSS, VmHWM).
func processMemory() (rss, peak uint64, ok bool) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0, false
	}
	return parseProcStatus(string(b))
}

// parseProcStatus reads VmRSS and VmHWM (kB) out of /proc/<pid>/status text.
func parseProcStatus(text string) (rss, peak uint64, ok bool) {
	var gotRSS, gotPeak bool
	for _, line := range strings.Split(text, "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key := strings.TrimSpace(k)
		if key != "VmRSS" && key != "VmHWM" {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && strings.EqualFold(f[1], "kB") {
			n <<= 10
		}
		if key == "VmRSS" {
			rss, gotRSS = n, true
		} else {
			peak, gotPeak = n, true
		}
	}
	return rss, peak, gotRSS && gotPeak
}
