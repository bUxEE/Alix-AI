//go:build !windows && !linux

package main

// On other systems the CPU model and installed memory are reported as null.

func cpuModel() string { return "" }

func totalRAMBytes() uint64 { return 0 }
