//go:build windows

package resources

import (
	"errors"
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

// osProcessRSS is the working set of this process.
func osProcessRSS() (uint64, error) {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, err
	}
	r, _, callErr := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		if callErr == nil {
			callErr = errors.New("GetProcessMemoryInfo failed")
		}
		return 0, callErr
	}
	return uint64(c.workingSetSize), nil
}
