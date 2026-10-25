//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// cpuModel reads the processor name from the registry
// (HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0\ProcessorNameString).
func cpuModel() string {
	key, err := syscall.UTF16PtrFromString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`)
	if err != nil {
		return ""
	}
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, key, 0, syscall.KEY_READ, &h); err != nil {
		return ""
	}
	defer syscall.RegCloseKey(h)
	name, err := syscall.UTF16PtrFromString("ProcessorNameString")
	if err != nil {
		return ""
	}
	var typ, size uint32
	if err := syscall.RegQueryValueEx(h, name, nil, &typ, nil, &size); err != nil || size == 0 || size > 1024 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	size = uint32(len(buf)) * 2
	if err := syscall.RegQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// totalRAMBytes asks kernel32 for the installed physical memory.
func totalRAMBytes() uint64 {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return 0
	}
	return ms.TotalPhys
}
