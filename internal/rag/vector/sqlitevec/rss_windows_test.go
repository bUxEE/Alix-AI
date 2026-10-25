//go:build windows

package sqlitevec_test

import (
	"syscall"
	"unsafe"
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// processMemory reports the working set (resident), its peak and the private
// commit charge, in bytes, for this process.
func processMemory() (rss, peakRSS, private uint64, ok bool) {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, 0, 0, false
	}
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		return 0, 0, 0, false
	}
	return uint64(c.workingSetSize), uint64(c.peakWorkingSetSize), uint64(c.pagefileUsage), true
}
