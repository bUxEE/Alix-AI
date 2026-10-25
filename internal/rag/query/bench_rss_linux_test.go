//go:build linux

package query

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// benchProcessMemory reports VmRSS and VmHWM in bytes.
func benchProcessMemory() (rss, peakRSS uint64, ok bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, 0, false
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
		}
	}
	return rss, peakRSS, rss > 0
}
