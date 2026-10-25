//go:build linux

package main

import "testing"

func TestParseProcStatus(t *testing.T) {
	rss, peak, ok := parseProcStatus("Name:\trageval\nVmPeak:\t  900 kB\nVmHWM:\t  2048 kB\nVmRSS:\t  1024 kB\nThreads:\t4\n")
	if !ok || rss != 1024<<10 || peak != 2048<<10 {
		t.Errorf("parseProcStatus = %d, %d, %v", rss, peak, ok)
	}
	if _, _, ok := parseProcStatus("Name:\tx\n"); ok {
		t.Error("a status without VmRSS and VmHWM must not parse")
	}
}
