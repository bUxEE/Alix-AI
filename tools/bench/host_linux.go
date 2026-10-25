//go:build linux

package main

import "os"

func cpuModel() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	return parseCPUInfo(string(b))
}

func totalRAMBytes() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemInfo(string(b))
}
