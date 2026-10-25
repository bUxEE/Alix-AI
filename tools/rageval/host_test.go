package main

import "testing"

func TestParseHostFiles(t *testing.T) {
	if got := parseCPUInfo("processor\t: 0\nmodel name\t: Test CPU @ 3.0GHz\nprocessor\t: 1\nmodel name\t: Other\n"); got != "Test CPU @ 3.0GHz" {
		t.Errorf("parseCPUInfo = %q", got)
	}
	if got := parseCPUInfo("nothing here"); got != "" {
		t.Errorf("parseCPUInfo = %q", got)
	}
	if got := parseMemInfo("MemFree:  1 kB\nMemTotal:       16384 kB\n"); got != 16384<<10 {
		t.Errorf("parseMemInfo = %d", got)
	}
	if got := parseMemInfo("MemTotal: x kB"); got != 0 {
		t.Errorf("parseMemInfo of garbage = %d", got)
	}
	h := CollectHost()
	if h.LogicalCores < 1 || h.OS == "" || h.Arch == "" {
		t.Errorf("CollectHost = %+v", h)
	}
}
