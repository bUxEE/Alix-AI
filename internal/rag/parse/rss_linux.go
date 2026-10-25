//go:build linux

package parse

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

const rssSupported = true

// rusageMaxRSSIsTheWorkers is false on Linux: ru_maxrss of a process started
// by fork and exec begins at the high-water mark of the address space it was
// forked from (Go starts the child with CLONE_VM, so that is the host's own),
// which put the host's resident set in every worker's reported peak. The peak
// comes from /proc/<pid>/status (VmHWM) instead.
const rusageMaxRSSIsTheWorkers = false

// rssBytes is the current resident set of a process and its high-water mark
// since it started (VmRSS and VmHWM of /proc/<pid>/status), or -1, -1 when the
// process is gone or /proc cannot be read.
func rssBytes(pid int) (rss, peak int64) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return -1, -1
	}
	rss, peak = -1, -1
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || (name != "VmRSS" && name != "VmHWM") {
			continue
		}
		f := strings.Fields(rest) // "  1234 kB"
		if len(f) < 1 {
			continue
		}
		kb, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		if name == "VmRSS" {
			rss = kb << 10
		} else {
			peak = kb << 10
		}
	}
	return rss, peak
}

// maxRSSBytes is the peak resident set from rusage (kilobytes on Linux).
func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) * 1024 }
