//go:build linux

package store

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// processMemory reports VmRSS, VmHWM and VmData (private data) in bytes.
func processMemory() (rss, peakRSS, private uint64, ok bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "VmRSS":
			rss = kb << 10
		case "VmHWM":
			peakRSS = kb << 10
		case "VmData":
			private = kb << 10
		}
	}
	return rss, peakRSS, private, rss > 0
}
