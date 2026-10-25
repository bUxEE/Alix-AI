//go:build darwin

package parse

import "syscall"

// rssSupported is false: reading another process's resident set on macOS needs
// libproc (C). The host has no memory watchdog there; see conf_unix.go.
const rssSupported = false

func rssBytes(pid int) (rss, peak int64) { return -1, -1 }

// rusageMaxRSSIsTheWorkers is true on macOS: the only peak the host can read is
// the one rusage reports after exit. That it is the worker's alone and does not
// start from the host's resident set (as it does on Linux) is NOT verified: this
// platform is untested.
const rusageMaxRSSIsTheWorkers = true

// maxRSSBytes is the peak resident set from rusage (bytes on macOS).
func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) }
