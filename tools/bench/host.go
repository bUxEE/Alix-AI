package main

import (
	"runtime"
	"strconv"
	"strings"
)

// CollectHost describes the machine the tool runs on: logical cores, OS and
// architecture from the Go runtime; the CPU model and installed memory from
// the operating system where the tool knows how to ask (Windows and Linux),
// null elsewhere. A runtime in a container or on another machine is not
// described by this.
func CollectHost() HostInfo {
	h := HostInfo{LogicalCores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if model := strings.Join(strings.Fields(cpuModel()), " "); model != "" {
		m, _ := truncateRunes(model, 200)
		h.CPUModel = &m
	}
	if b := totalRAMBytes(); b >= 1<<20 {
		mb := int64(b >> 20)
		h.RAMMB = &mb
	}
	return h
}

// parseCPUInfo returns the first "model name" of /proc/cpuinfo text.
func parseCPUInfo(text string) string {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseMemInfo returns MemTotal of /proc/meminfo text, in bytes.
func parseMemInfo(text string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "MemTotal" {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return 0
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			n <<= 10
		}
		return n
	}
	return 0
}
