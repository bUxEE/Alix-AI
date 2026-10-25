package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseMemUsage(t *testing.T) {
	for _, tc := range []struct {
		in         string
		used, lim  float64
		wantErrSub string
	}{
		{"1.5GiB / 8GiB", 1536, 8192, ""},
		{"812.5MiB / 7.67GiB\n", 812.5, 7.67 * 1024, ""},
		{"262144KiB / 8192MiB", 256, 8192, ""},
		{"500MB / 8GB", 500e6 / (1 << 20), 8e9 / (1 << 20), ""},
		{"nonsense", 0, 0, "unexpected docker stats output"},
		{"12 / 8GiB", 0, 0, "unexpected size"},
		{"12XiB / 8GiB", 0, 0, "unknown size unit"},
		{"", 0, 0, "unexpected docker stats output"},
	} {
		got, err := parseMemUsage(tc.in)
		if tc.wantErrSub != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("parseMemUsage(%q) = %v, %v, want an error with %q", tc.in, got, err, tc.wantErrSub)
			}
			continue
		}
		if err != nil || abs(got.UsedMiB-tc.used) > 0.01 || abs(got.LimitMiB-tc.lim) > 0.01 {
			t.Errorf("parseMemUsage(%q) = %+v, %v, want used %.2f limit %.2f", tc.in, got, err, tc.used, tc.lim)
		}
	}
}

// TestDockerStatsUsesOneFixedCommand: the only thing the sampler ever runs is
// `docker stats --no-stream --format {{.MemUsage}} <name>`, a name that could
// become a flag is refused, and the CLI may only be docker or podman.
func TestDockerStatsUsesOneFixedCommand(t *testing.T) {
	d, err := newDockerStats("alixai-localai", "")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	d.run = func(_ context.Context, a ...string) ([]byte, error) {
		args = a
		return []byte("270MiB / 8GiB\n"), nil
	}
	s, err := d.Sample(context.Background())
	if err != nil || s.UsedMiB != 270 || s.LimitMiB != 8192 {
		t.Fatalf("sample %+v, %v", s, err)
	}
	if want := []string{"stats", "--no-stream", "--format", "{{.MemUsage}}", "alixai-localai"}; !reflect.DeepEqual(args, want) {
		t.Errorf("ran %v, want %v", args, want)
	}

	for _, name := range []string{"", "-rf", "a b", "a;b", "--privileged", strings.Repeat("a", 200), "x/y"} {
		if _, err := newDockerStats(name, ""); err == nil {
			t.Errorf("container name %q was accepted", name)
		}
	}
	for _, bin := range []string{"sh", "docker-compose", "rm", "bin/docker", "./docker", "/no/such/docker"} {
		if _, err := newDockerStats("ok", bin); err == nil {
			t.Errorf("container CLI %q was accepted", bin)
		}
	}
	for _, bin := range []string{"docker", "podman", "Docker.exe"} {
		if _, err := newDockerStats("ok", bin); err != nil {
			t.Errorf("container CLI %q was refused: %v", bin, err)
		}
	}
}

// scriptedSource returns its readings in order, then repeats the last; fail
// makes every call from the n-th on an error.
type scriptedSource struct {
	mu    sync.Mutex
	read  []MemSample
	calls int
	fail  int // 0 never; n: calls from the n-th (1-based) fail
}

func (s *scriptedSource) Sample(context.Context) (MemSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail > 0 && s.calls >= s.fail {
		return MemSample{}, errors.New("scripted failure")
	}
	i := min(s.calls-1, len(s.read)-1)
	return s.read[i], nil
}

func TestMemCollectorSummarisesBaselinePeakAndSteady(t *testing.T) {
	src := &scriptedSource{read: []MemSample{{100, 8192}, {400, 8192}, {300, 8192}, {320, 8192}}}
	m, err := startMemory(context.Background(), src, "alixai-localai", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Let it read the scripted values, then say warm-up is over, then let it read
	// the last one (repeated) for a while.
	waitFor(t, func() bool { src.mu.Lock(); defer src.mu.Unlock(); return src.calls >= 4 })
	m.markMeasure()
	waitFor(t, func() bool { src.mu.Lock(); defer src.mu.Unlock(); return src.calls >= 8 })
	info := m.stop(context.Background())
	again := m.stop(context.Background()) // stopping twice is harmless
	if info.Samples != again.Samples {
		t.Errorf("a second stop changed the readings: %d then %d", info.Samples, again.Samples)
	}
	if !info.Sampled || info.Source != MemSourceDocker || info.Container == nil || *info.Container != "alixai-localai" || info.IntervalMS == nil || *info.IntervalMS != 1 {
		t.Fatalf("info %+v", info)
	}
	if *info.BaselineMB != 100 || *info.PeakRSSMB != 400 || *info.SteadyRSSMB != 320 || *info.LimitMBObserved != 8192 || info.Samples < 8 || info.SampleErrors != 0 {
		t.Errorf("baseline %v peak %v steady %v limit %v samples %d errors %d", *info.BaselineMB, *info.PeakRSSMB, *info.SteadyRSSMB, *info.LimitMBObserved, info.Samples, info.SampleErrors)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the sampler")
		}
		time.Sleep(time.Millisecond)
	}
}

// A run asked to measure memory that cannot take the first reading fails: it
// does not silently measure nothing. A reading that fails later is counted.
func TestMemCollectorFailsClosed(t *testing.T) {
	if _, err := startMemory(context.Background(), &scriptedSource{read: []MemSample{{1, 1}}, fail: 1}, "c", time.Millisecond); err == nil || !strings.Contains(err.Error(), "container memory") {
		t.Errorf("a failing baseline: %v", err)
	}
	src := &scriptedSource{read: []MemSample{{10, 100}}, fail: 2}
	m, err := startMemory(context.Background(), src, "c", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { src.mu.Lock(); defer src.mu.Unlock(); return src.calls >= 4 })
	info := m.stop(context.Background())
	if info.Samples != 1 || info.SampleErrors < 3 || *info.PeakRSSMB != 10 {
		t.Errorf("samples %d, errors %d, peak %v", info.Samples, info.SampleErrors, info.PeakRSSMB)
	}
}

func TestSummarizeMemory(t *testing.T) {
	if b, p, s := summarizeMemory(nil, 0); b != nil || p != nil || s != nil {
		t.Error("no readings must give no figures")
	}
	// The median of an even number of readings after the mark is their mean; with
	// none after the mark it is the last reading.
	b, p, s := summarizeMemory([]float64{5, 50, 10, 20, 30, 40}, 2)
	if *b != 5 || *p != 50 || *s != 25 {
		t.Errorf("baseline %v peak %v steady %v", *b, *p, *s)
	}
	if _, _, s := summarizeMemory([]float64{5, 50, 10}, 3); *s != 10 {
		t.Errorf("steady with nothing after the mark: %v", *s)
	}
	if !limitMatches(8192, 8192) || !limitMatches(8150, 8192) || limitMatches(4096, 8192) || limitMatches(7000, 8192) {
		t.Error("limitMatches is not a 1% rule")
	}
}
