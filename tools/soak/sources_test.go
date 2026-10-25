package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseStatsLinesAndSizes(t *testing.T) {
	got, err := parseStatsLines("alixai-localai\t2.5GiB / 8GiB\nalixai\t96MiB / 512MiB\n", []string{"alixai-localai", "alixai"})
	if err != nil {
		t.Fatal(err)
	}
	if got["alixai-localai"] != 2560 || got["alixai"] != 96 {
		t.Errorf("parsed %v", got)
	}
	// older Docker prints decimal units
	if mib, err := parseSizeMiB("1048.576kB"); err != nil || mib < 0.99 || mib > 1.01 {
		t.Errorf("1048.576kB = %v, %v", mib, err)
	}
	for _, bad := range []string{"x", "12", "12 parsecs", ""} {
		if _, err := parseSizeMiB(bad); err == nil {
			t.Errorf("parseSizeMiB(%q) did not fail", bad)
		}
	}
	// a container docker did not report is an error, never a smaller sum
	if _, err := parseStatsLines("alixai\t96MiB / 512MiB\n", []string{"alixai", "alixai-localai"}); err == nil || !strings.Contains(err.Error(), "alixai-localai") {
		t.Errorf("a missing container: %v", err)
	}
	if _, err := parseStatsLines("garbage\n", []string{"x"}); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestParseInspectMemory(t *testing.T) {
	mem, swap, err := parseInspectMemory("8589934592 8589934592\n")
	if err != nil || mem != 8<<30 || swap != 8<<30 {
		t.Errorf("%d %d %v", mem, swap, err)
	}
	for _, bad := range []string{"", "1", "a b", "1 2 3"} {
		if _, _, err := parseInspectMemory(bad); err == nil {
			t.Errorf("parseInspectMemory(%q) did not fail", bad)
		}
	}
}

// fakeDocker answers the two commands the source runs and records what it was asked.
type fakeDocker struct {
	stats   string
	inspect map[string]string
	calls   [][]string
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "stats":
		return []byte(f.stats), nil
	case "inspect":
		out, ok := f.inspect[args[len(args)-1]]
		if !ok {
			return nil, errors.New("no such container")
		}
		return []byte(out), nil
	}
	return nil, errors.New("unexpected command")
}

func TestDockerSource(t *testing.T) {
	// the profile files set memswap_limit equal to mem_limit: swap is structurally zero
	noSwap := "8589934592 8589934592"
	d, err := newDockerSource("docker", []string{"alixai-localai", "alixai"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{
		stats:   "alixai\t96MiB / 512MiB\nalixai-localai\t2GiB / 8GiB\n",
		inspect: map[string]string{"alixai-localai": noSwap, "alixai": "536870912 536870912"},
	}
	d.run = f.run
	s, err := d.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.UsedMiB != 2144 || !s.SwapKnown || s.SwapMiB != 0 {
		t.Errorf("sample %+v", s)
	}
	if got := d.Describe(); got.Kind != MemSourceDocker || got.Target != "alixai-localai,alixai" {
		t.Errorf("describe %+v", got)
	}
	// the first call is a stats with a fixed argument vector and the names last
	if c := f.calls[0]; c[0] != "stats" || c[1] != "--no-stream" || c[len(c)-1] != "alixai" {
		t.Errorf("stats call %v", c)
	}
	// swap off is a property of the container: it is asked once
	if _, err := d.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	inspects := 0
	for _, c := range f.calls {
		if c[0] == "inspect" {
			inspects++
		}
	}
	if inspects != 2 {
		t.Errorf("%d inspect calls over two samples of two containers, want 2", inspects)
	}

	// swap that is allowed is not measured: the sample says so
	for name, inspect := range map[string]string{
		"unlimited swap":       "8589934592 -1",
		"default swap (twice)": "8589934592 17179869184",
		"unset":                "0 0",
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := newDockerSource("docker", []string{"alixai-localai"})
			f := &fakeDocker{stats: "alixai-localai\t2GiB / 8GiB\n", inspect: map[string]string{"alixai-localai": inspect}}
			d.run = f.run
			s, err := d.Sample(context.Background())
			if err != nil || s.SwapKnown {
				t.Errorf("sample %+v, err %v: swap must be unknown", s, err)
			}
		})
	}

	// a container that docker does not know is an error
	d2, _ := newDockerSource("docker", []string{"gone"})
	d2.run = (&fakeDocker{stats: "gone\t1MiB / 2MiB\n", inspect: map[string]string{}}).run
	if _, err := d2.Sample(context.Background()); err == nil {
		t.Error("an unknown container was sampled")
	}
}

func TestNewDockerSourceValidates(t *testing.T) {
	for _, c := range [][]string{{}, {"-rf"}, {"a b"}, {"a;b"}, {"ok", "ok"}, {""}} {
		if _, err := newDockerSource("docker", c); err == nil {
			t.Errorf("containers %q were accepted", c)
		}
	}
	for _, bin := range []string{"rm", "sh", "./docker", "docker; rm", "/no/such/docker"} {
		if _, err := newDockerSource(bin, []string{"a"}); err == nil {
			t.Errorf("container CLI %q was accepted", bin)
		}
	}
	for _, bin := range []string{"docker", "podman", "Docker.exe"} {
		if _, err := newDockerSource(bin, []string{"a"}); err != nil {
			t.Errorf("container CLI %q refused: %v", bin, err)
		}
	}
}

const procStatus = `Name:	llama
VmPeak:	  900000 kB
VmRSS:	  204800 kB
VmSwap:	    2048 kB
Threads:	12
`

func TestParseProcStatus(t *testing.T) {
	rss, swap, err := parseProcStatus(procStatus)
	if err != nil || rss != 204800 || swap != 2048 {
		t.Errorf("%d %d %v", rss, swap, err)
	}
	for _, bad := range []string{"", "Name: kthreadd\n", "VmRSS: 12 kB\n", "VmRSS: x kB\nVmSwap: 1 kB\n", "VmRSS: 1 MB\nVmSwap: 1 kB\n"} {
		if _, _, err := parseProcStatus(bad); err == nil {
			t.Errorf("parseProcStatus(%q) did not fail", bad)
		}
	}
}

func TestHostSource(t *testing.T) {
	h := &hostSource{pids: []int{10, 20}, read: func(pid int) (string, error) {
		if pid == 20 {
			return strings.ReplaceAll(procStatus, "2048 kB", "1024 kB"), nil
		}
		return procStatus, nil
	}}
	s, err := h.Sample(context.Background())
	if err != nil || s.UsedMiB != 400 || s.SwapMiB != 3 || !s.SwapKnown {
		t.Errorf("sample %+v, %v", s, err)
	}
	if d := h.Describe(); d.Kind != MemSourceHost || d.Target != "pid 10,20" {
		t.Errorf("describe %+v", d)
	}
	// a process that is gone is an error, not a smaller sum
	h.read = func(pid int) (string, error) {
		if pid == 20 {
			return "", errors.New("no such process")
		}
		return procStatus, nil
	}
	if _, err := h.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "pid 20") {
		t.Errorf("a vanished process: %v", err)
	}
	// construction is refused where /proc is not there, and for bad pids where it is
	if _, err := newHostSource([]int{0}); err == nil {
		t.Error("pid 0 was accepted")
	}
	if _, err := newHostSource(nil); err == nil {
		t.Error("no pid was accepted")
	}
}
