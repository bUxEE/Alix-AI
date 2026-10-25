package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
)

func TestParseMemUsage(t *testing.T) {
	const eps = 1e-6
	tests := []struct {
		in    string
		used  float64 // MiB
		limit float64
		bad   bool
	}{
		{"1.234GiB / 8GiB", 1.234 * 1024, 8192, false},
		{"812.5MiB / 7.67GiB", 812.5, 7.67 * 1024, false},
		{"1GiB / 4GiB\n", 1024, 4096, false},
		{"  512MiB / 8GiB  ", 512, 8192, false},
		{"0B / 0B", 0, 0, false},
		{"2048KiB / 8GiB", 2, 8192, false},
		{"1.5GB / 8GB", 1.5e9 / (1 << 20), 8e9 / (1 << 20), false}, // older Docker prints decimal units
		{"250MB / 1GB", 250e6 / (1 << 20), 1e9 / (1 << 20), false},
		{"900kB / 1MB", 900e3 / (1 << 20), 1e6 / (1 << 20), false},
		{"1TiB / 2TiB", 1 << 20, 2 << 20, false},
		{"", 0, 0, true},
		{"12MiB", 0, 0, true},
		{"12 / 8GiB", 0, 0, true},
		{"abc / def", 0, 0, true},
		{"1.2.3GiB / 8GiB", 0, 0, true},
		{"-5MiB / 8GiB", 0, 0, true},
		{"5ZiB / 8GiB", 0, 0, true},
		{"--  / --", 0, 0, true},
	}
	for _, tt := range tests {
		got, err := parseMemUsage(tt.in)
		if tt.bad {
			if err == nil {
				t.Errorf("parseMemUsage(%q) = %+v, want an error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMemUsage(%q): %v", tt.in, err)
			continue
		}
		if math.Abs(got.UsedMiB-tt.used) > eps || math.Abs(got.LimitMiB-tt.limit) > eps {
			t.Errorf("parseMemUsage(%q) = %+v, want %v / %v MiB", tt.in, got, tt.used, tt.limit)
		}
	}
}

func TestDockerStatsCommand(t *testing.T) {
	t.Run("container names are checked before anything runs", func(t *testing.T) {
		for _, bad := range []string{"", "-it", "--rm", "a b", "a;rm -rf /", "$(id)", "a/b", "../x", "x\ny", strings.Repeat("a", 129), ".hidden", "_x"} {
			if _, err := newDockerStats(bad, ""); err == nil {
				t.Errorf("container name %q was accepted", bad)
			}
		}
		for _, ok := range []string{"alixai-localai", "localai_1", "a.b-c_d", "A1"} {
			if _, err := newDockerStats(ok, ""); err != nil {
				t.Errorf("container name %q was refused: %v", ok, err)
			}
		}
	})

	t.Run("the argument vector is fixed", func(t *testing.T) {
		d, err := newDockerStats("alixai-localai", "")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		d.run = func(_ context.Context, args ...string) ([]byte, error) {
			got = args
			return []byte("1.5GiB / 8GiB\n"), nil
		}
		s, err := d.Sample(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"stats", "--no-stream", "--format", "{{.MemUsage}}", "alixai-localai"}
		if !slices.Equal(got, want) {
			t.Errorf("docker was run with %q, want %q", got, want)
		}
		if s.UsedMiB != 1536 || s.LimitMiB != 8192 {
			t.Errorf("sample = %+v", s)
		}
	})

	t.Run("a failing docker is an error, not a zero reading", func(t *testing.T) {
		d, _ := newDockerStats("x", "")
		d.run = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("docker: not running") }
		if s, err := d.Sample(context.Background()); err == nil {
			t.Errorf("sample = %+v, want an error", s)
		}
		d.run = func(context.Context, ...string) ([]byte, error) { return []byte("garbage"), nil }
		if _, err := d.Sample(context.Background()); err == nil {
			t.Error("unparseable output must be an error")
		}
	})
}

func TestContainerCLIIsRestricted(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "docker.exe")
	if err := os.WriteFile(good, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	notDocker := filepath.Join(dir, "curl.exe")
	if err := os.WriteFile(notDocker, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"", "docker", "podman", "Docker.EXE", good} {
		if _, err := newDockerStats("c", ok); err != nil {
			t.Errorf("container CLI %q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"sh", "bash -c x", "curl", notDocker, "./docker", "bin/docker", filepath.Join(dir, "nope", "docker"), dir + string(filepath.Separator) + "docker", "docker2", "/usr/bin/env"} {
		if _, err := newDockerStats("c", bad); err == nil {
			t.Errorf("container CLI %q was accepted", bad)
		}
	}
}

func TestSummarizeMemory(t *testing.T) {
	tests := []struct {
		name                  string
		used                  []float64
		mark                  int
		base, peak, steadyWnt *float64
	}{
		{"no readings", nil, 0, nil, nil, nil},
		{"one reading is baseline, peak and steady", []float64{100}, 1, round3(100), round3(100), round3(100)},
		{"peak includes warm-up, steady excludes it", []float64{100, 900, 500, 520, 510}, 2, round3(100), round3(900), round3(510)},
		{"steady is the median of the measured phase", []float64{100, 800, 400, 600, 500, 1000}, 2, round3(100), round3(1000), round3(550)},
		{"no reading after warm-up: the last one", []float64{100, 700}, 2, round3(100), round3(700), round3(700)},
		{"a mark beyond the data is the same", []float64{100, 700}, 9, round3(100), round3(700), round3(700)},
		{"mark zero uses everything", []float64{10, 30, 20}, 0, round3(10), round3(30), round3(20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, p, s := summarizeMemory(tt.used, tt.mark)
			if !reflect.DeepEqual(b, tt.base) || !reflect.DeepEqual(p, tt.peak) || !reflect.DeepEqual(s, tt.steadyWnt) {
				t.Errorf("baseline %v peak %v steady %v; want %v %v %v", deref(b), deref(p), deref(s), deref(tt.base), deref(tt.peak), deref(tt.steadyWnt))
			}
		})
	}
}

func TestLimitMatches(t *testing.T) {
	for _, tt := range []struct {
		observed float64
		want     int
		ok       bool
	}{
		{8192, 8192, true},
		{8192.4, 8192, true},
		{8100, 8192, false}, // 1.1% under
		{8200, 8192, true},
		{4096, 4096, true},
		{64000, 8192, false}, // a container with no limit reports the host's memory
		{8192, 4096, false},
		{0, 8192, false},
	} {
		if got := limitMatches(tt.observed, tt.want); got != tt.ok {
			t.Errorf("limitMatches(%v, %d) = %v, want %v", tt.observed, tt.want, got, tt.ok)
		}
	}
}

// scriptedSource returns scripted readings and counts its calls.
type scriptedSource struct {
	mu       sync.Mutex
	readings []MemSample
	failFrom int // calls after this many succeed no more; -1 never fails
	failAll  bool
	calls    int
}

func (s *scriptedSource) Sample(ctx context.Context) (MemSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failAll || (s.failFrom >= 0 && s.calls > s.failFrom) {
		return MemSample{}, errors.New("docker: gone")
	}
	i := s.calls - 1
	if i >= len(s.readings) {
		i = len(s.readings) - 1
	}
	return s.readings[i], nil
}

func (s *scriptedSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestMemoryCollector(t *testing.T) {
	t.Run("baseline, marks and the last reading", func(t *testing.T) {
		src := &scriptedSource{failFrom: -1, readings: []MemSample{
			{UsedMiB: 400, LimitMiB: 8192}, {UsedMiB: 2500, LimitMiB: 8192}, {UsedMiB: 2300, LimitMiB: 8192}, {UsedMiB: 2400, LimitMiB: 8192},
		}}
		// An interval longer than the test: only the baseline and the closing reading are taken.
		m, err := startMemory(context.Background(), MemoryPlan{Source: src, Container: "c", Interval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		m.markMeasure()
		info := m.stop(context.Background())
		want := MemoryInfo{
			Sampled: true, Source: MemSourceDocker, Container: strPtr("c"), IntervalMS: i64Ptr(3600000),
			Samples: 2, BaselineMB: round3(400), PeakRSSMB: round3(2500), SteadyRSSMB: round3(2500), LimitMBObserved: round3(8192),
		}
		if diff := cmp.Diff(want, info); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
		if again := m.stop(context.Background()); again.Samples != 2 || src.count() != 2 {
			t.Errorf("stop is not idempotent: %d samples, %d calls", again.Samples, src.count())
		}
	})

	t.Run("samples on an interval and stops cleanly", func(t *testing.T) {
		src := &scriptedSource{failFrom: -1, readings: []MemSample{{UsedMiB: 100}, {UsedMiB: 200}, {UsedMiB: 300}}}
		m, err := startMemory(context.Background(), MemoryPlan{Source: src, Container: "c", Interval: 5 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for src.count() < 4 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		m.markMeasure()
		info := m.stop(context.Background())
		if info.Samples < 4 {
			t.Fatalf("only %d samples were taken", info.Samples)
		}
		if *info.PeakRSSMB != 300 || *info.BaselineMB != 100 {
			t.Errorf("baseline %v peak %v", deref(info.BaselineMB), deref(info.PeakRSSMB))
		}
		if info.LimitMBObserved != nil {
			t.Errorf("a source that reports no limit gives limit_mb_observed %v", deref(info.LimitMBObserved))
		}
		time.Sleep(20 * time.Millisecond)
		n := src.count()
		time.Sleep(30 * time.Millisecond)
		if src.count() != n {
			t.Error("the collector kept sampling after stop")
		}
	})

	t.Run("a failing baseline stops the run before it starts", func(t *testing.T) {
		src := &scriptedSource{failAll: true}
		if _, err := startMemory(context.Background(), MemoryPlan{Source: src, Container: "c", Interval: time.Second}); err == nil {
			t.Error("a run that cannot measure the memory it was asked to measure must fail")
		}
	})

	t.Run("later failures are counted, not fatal", func(t *testing.T) {
		src := &scriptedSource{failFrom: 1, readings: []MemSample{{UsedMiB: 100}}}
		m, err := startMemory(context.Background(), MemoryPlan{Source: src, Container: "c", Interval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		info := m.stop(context.Background())
		if info.Samples != 1 || info.SampleErrors != 1 || *info.PeakRSSMB != 100 {
			t.Errorf("info = %+v", info)
		}
	})
}

func TestRunWithMemorySampling(t *testing.T) {
	corpus := realCorpus(t)
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	p := newOracle(corpus)
	gen := &timedGen{inner: p, clk: clk, latency: scriptedLatency(corpus)}

	t.Run("the memory block is filled", func(t *testing.T) {
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Limit = 3
		src := &scriptedSource{failFrom: -1, readings: []MemSample{{UsedMiB: 300, LimitMiB: 8192}, {UsedMiB: 1800, LimitMiB: 8192}}}
		cfg.Memory = &MemoryPlan{Source: src, Container: "alixai-localai", Interval: time.Hour}
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		m := res.Memory
		if !m.Sampled || m.Source != MemSourceDocker || *m.Container != "alixai-localai" || m.Samples != 2 ||
			*m.BaselineMB != 300 || *m.PeakRSSMB != 1800 || *m.LimitMBObserved != 8192 {
			t.Errorf("memory = %+v", m)
		}
		_, resultSchema, _ := realSchemas(t)
		data, _ := marshalResult(res)
		if problems := validateResultBytes(resultSchema, data); len(problems) > 0 {
			t.Errorf("a result with memory fails check: %v", problems)
		}
	})

	t.Run("a container that is not under the profile's limit aborts before any case", func(t *testing.T) {
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Profile = models.ProfileBalanced // 16384 MiB
		src := &scriptedSource{failFrom: -1, readings: []MemSample{{UsedMiB: 300, LimitMiB: 8192}}}
		cfg.Memory = &MemoryPlan{Source: src, Container: "c", Interval: time.Hour}
		calls := p.Calls()
		res, err := Run(context.Background(), cfg)
		if err == nil || res != nil || !strings.Contains(err.Error(), "memory limit") {
			t.Fatalf("res %v err %v; want a limit mismatch error", res, err)
		}
		if p.Calls() != calls {
			t.Error("cases were sent although the container is not under the labelled limit")
		}
	})

	t.Run("a memory limit that disagrees with the label fails check", func(t *testing.T) {
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Limit = 2
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		res.Memory = MemoryInfo{Sampled: true, Source: MemSourceDocker, Container: strPtr("c"), IntervalMS: i64Ptr(1000), Samples: 1,
			BaselineMB: round3(10), PeakRSSMB: round3(10), SteadyRSSMB: round3(10), LimitMBObserved: round3(65000)}
		problems := VerifyResult(res)
		if len(problems) != 1 || !strings.Contains(problems[0], "not under the limit") {
			t.Errorf("problems = %v", problems)
		}
	})

	t.Run("steady memory above the peak is impossible", func(t *testing.T) {
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Limit = 2
		res, _ := Run(context.Background(), cfg)
		res.Memory = MemoryInfo{Sampled: true, Source: MemSourceDocker, Container: strPtr("c"), IntervalMS: i64Ptr(1000), Samples: 1,
			BaselineMB: round3(10), PeakRSSMB: round3(10), SteadyRSSMB: round3(20)}
		if problems := VerifyResult(res); len(problems) == 0 {
			t.Error("steady above peak must be flagged")
		}
	})
}
