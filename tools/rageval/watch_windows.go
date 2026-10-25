//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procQPC          = kernel32.NewProc("QueryPerformanceCounter")
	procQPF          = kernel32.NewProc("QueryPerformanceFrequency")
	counterFrequency = func() int64 {
		var f int64
		if r, _, _ := procQPF.Call(uintptr(unsafe.Pointer(&f))); r == 0 {
			return 0
		}
		return f
	}()
)

// counterNow reads the performance counter, 0 when there is none.
func counterNow() int64 {
	if counterFrequency <= 0 {
		return 0
	}
	var c int64
	if r, _, _ := procQPC.Call(uintptr(unsafe.Pointer(&c))); r == 0 {
		return 0
	}
	return c
}

// counterSince converts the counter's distance from c0 to a duration.
func counterSince(c0 int64) (time.Duration, bool) {
	if counterFrequency <= 0 || c0 == 0 {
		return 0, false
	}
	c := counterNow()
	if c == 0 || c < c0 {
		return 0, false
	}
	ticks := c - c0
	// ticks * 1e9 / frequency without overflowing int64 for hours of ticks.
	sec, rem := ticks/counterFrequency, ticks%counterFrequency
	return time.Duration(sec)*time.Second + time.Duration(rem*int64(time.Second)/counterFrequency), true
}
