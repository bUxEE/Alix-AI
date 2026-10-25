package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Container memory, for the runs that talk to a LocalAI container.
//
// It is read the way tools/bench reads it: `docker stats --no-stream` and
// nothing else, with one fixed argument vector, no shell, and a container name
// that has been matched against a strict pattern first (so it cannot start with
// a dash and become a flag). The figure is the cgroup's memory usage as Docker
// computes it (usage minus inactive page cache): the closest container-level
// proxy to RSS, not RSS itself. llama.cpp maps the model file, and whichever of
// those file-backed pages the kernel counts as active cache stay in the number;
// the resident set of the same processes (M0-13) is larger for that reason.
// tools/bench is a main package, so this is its sampler's small twin.

var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

const (
	// maxStatsOutput bounds what one docker stats call may print.
	maxStatsOutput = 4096
	// memSampleTimeout bounds one reading (docker stats --no-stream itself waits
	// for a sample, about a second).
	memSampleTimeout = 20 * time.Second
	// MemSourceDocker and MemSourceNone are the values of MemoryInfo.Source.
	MemSourceDocker = "docker-stats"
	MemSourceNone   = "none"
)

// MemSample is one reading of a container's memory, in MiB.
type MemSample struct {
	UsedMiB  float64
	LimitMiB float64
}

// MemorySource takes one reading. dockerStats is the real one; tests supply
// scripted sources.
type MemorySource interface {
	Sample(ctx context.Context) (MemSample, error)
}

// dockerStats is a MemorySource over docker stats.
type dockerStats struct {
	container string
	bin       string
	// run executes the container CLI with args and returns its standard output.
	// Tests replace it; the default is execCLI on bin.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// newDockerStats validates the container name and the CLI to run. bin is the
// docker (or podman) executable: a bare name looked up in PATH, or an absolute
// path. It may only name docker or podman, so the flag cannot turn the tool
// into a way to run an arbitrary program.
func newDockerStats(container, bin string) (*dockerStats, error) {
	if !containerNamePattern.MatchString(container) {
		return nil, fmt.Errorf("container name %q is not valid (letters, digits, '_', '.' and '-', starting with a letter or digit)", container)
	}
	if bin == "" {
		bin = "docker"
	}
	if err := validateCLI(bin); err != nil {
		return nil, err
	}
	d := &dockerStats{container: container, bin: bin}
	d.run = func(ctx context.Context, args ...string) ([]byte, error) { return execCLI(ctx, d.bin, args...) }
	return d, nil
}

// validateCLI accepts docker, podman and an absolute path to either (with or
// without .exe, in any case).
func validateCLI(bin string) error {
	base := strings.ToLower(filepath.Base(bin))
	base = strings.TrimSuffix(base, ".exe")
	if base != "docker" && base != "podman" {
		return fmt.Errorf("container CLI %q must be docker or podman", bin)
	}
	if bin != filepath.Base(bin) { // it has a directory part
		if !filepath.IsAbs(bin) {
			return fmt.Errorf("container CLI %q must be a bare name or an absolute path", bin)
		}
		st, err := os.Stat(bin)
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("container CLI %q is not a file", bin)
		}
	}
	return nil
}

// execCLI runs the container CLI. Only dockerStats.Sample calls it, with the
// fixed argument vector built there.
func execCLI(ctx context.Context, bin string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %w", filepath.Base(bin), args[0], err)
	}
	if len(out) > maxStatsOutput {
		return nil, errors.New("docker stats printed more than expected")
	}
	return out, nil
}

// Sample implements MemorySource.
func (d *dockerStats) Sample(ctx context.Context) (MemSample, error) {
	out, err := d.run(ctx, "stats", "--no-stream", "--format", "{{.MemUsage}}", d.container)
	if err != nil {
		return MemSample{}, err
	}
	return parseMemUsage(string(out))
}

// parseMemUsage parses the MemUsage column, for example "1.234GiB / 8GiB" or
// "812.5MiB / 7.67GiB".
func parseMemUsage(s string) (MemSample, error) {
	used, limit, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return MemSample{}, fmt.Errorf("unexpected docker stats output %q", clipForError(s))
	}
	u, err := parseSizeMiB(used)
	if err != nil {
		return MemSample{}, err
	}
	l, err := parseSizeMiB(limit)
	if err != nil {
		return MemSample{}, err
	}
	return MemSample{UsedMiB: u, LimitMiB: l}, nil
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]+)$`)

// unitBytes lists the units Docker prints: binary (KiB, MiB, GiB) in current
// versions, decimal (kB, MB, GB) in older ones.
var unitBytes = map[string]float64{
	"B":  1,
	"kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
}

// parseSizeMiB converts "1.5GiB" to mebibytes.
func parseSizeMiB(s string) (float64, error) {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("unexpected size %q in docker stats output", clipForError(s))
	}
	mult, ok := unitBytes[m[2]]
	if !ok {
		return 0, fmt.Errorf("unknown size unit %q in docker stats output", m[2])
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	return v * mult / (1 << 20), nil
}

func clipForError(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "..."
	}
	return s
}

// newMemorySource builds the source of a run's container memory. It is a
// variable so that a test can script the readings: nothing else replaces it.
var newMemorySource = func(container, bin string) (MemorySource, error) {
	d, err := newDockerStats(container, bin)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// memCollector samples a MemorySource on an interval while the run goes on. The
// first reading is taken synchronously before anything else (the baseline), the
// last one in stop, so even a run shorter than one interval has both. It owns
// one goroutine, which stop always ends.
type memCollector struct {
	source    MemorySource
	container string
	interval  time.Duration
	cancel    context.CancelFunc
	done      chan struct{}

	mu     sync.Mutex
	used   []float64
	mark   int // used[mark:] was taken after warm-up
	errs   int
	limit  float64
	closed bool
}

// startMemory takes the baseline reading and starts the sampling goroutine. It
// fails when the baseline cannot be taken: a run that was asked to measure
// memory and cannot must not silently measure nothing.
func startMemory(ctx context.Context, src MemorySource, container string, interval time.Duration) (*memCollector, error) {
	m := &memCollector{source: src, container: container, interval: interval, done: make(chan struct{})}
	s, err := m.take(ctx)
	if err != nil {
		return nil, fmt.Errorf("container memory: %w", err)
	}
	m.record(s)
	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx)
	return m, nil
}

func (m *memCollector) take(ctx context.Context) (MemSample, error) {
	ctx, cancel := context.WithTimeout(ctx, memSampleTimeout)
	defer cancel()
	return m.source.Sample(ctx)
}

func (m *memCollector) record(s MemSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used = append(m.used, s.UsedMiB)
	if s.LimitMiB > 0 {
		m.limit = s.LimitMiB
	}
}

func (m *memCollector) loop(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s, err := m.take(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.mu.Lock()
				m.errs++
				m.mu.Unlock()
				continue
			}
			m.record(s)
		}
	}
}

// markMeasure notes that warm-up is over: the steady figure uses only readings
// taken from here on.
func (m *memCollector) markMeasure() {
	m.mu.Lock()
	m.mark = len(m.used)
	m.mu.Unlock()
}

// stop ends sampling, takes a last reading and returns the summary.
func (m *memCollector) stop(ctx context.Context) MemoryInfo {
	m.mu.Lock()
	already := m.closed
	m.closed = true
	m.mu.Unlock()
	if !already {
		m.cancel()
		<-m.done
		if s, err := m.take(context.WithoutCancel(ctx)); err == nil {
			m.record(s)
		} else {
			m.mu.Lock()
			m.errs++
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	base, peak, steady := summarizeMemory(m.used, m.mark)
	interval := m.interval.Milliseconds()
	container := m.container
	info := MemoryInfo{
		Sampled: true, Source: MemSourceDocker, Container: &container, IntervalMS: &interval,
		Samples: len(m.used), SampleErrors: m.errs,
		BaselineMB: base, PeakRSSMB: peak, SteadyRSSMB: steady,
	}
	if m.limit > 0 {
		info.LimitMBObserved = round3(m.limit)
	}
	return info
}

// summarizeMemory reduces readings (MiB, oldest first) to the baseline (the
// first), the peak (the largest of all, warm-up included, because loading the
// model is when memory peaks) and the steady figure: the median of the readings
// taken from index mark on, or the last reading when there are none.
func summarizeMemory(used []float64, mark int) (baseline, peak, steady *float64) {
	if len(used) == 0 {
		return nil, nil, nil
	}
	baseline = round3(used[0])
	hi := used[0]
	for _, u := range used {
		hi = max(hi, u)
	}
	peak = round3(hi)
	if mark < 0 || mark > len(used) {
		mark = len(used)
	}
	after := used[mark:]
	if len(after) == 0 {
		steady = round3(used[len(used)-1])
		return
	}
	sorted := append([]float64(nil), after...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		steady = round3(sorted[n/2])
	} else {
		steady = round3((sorted[n/2-1] + sorted[n/2]) / 2)
	}
	return
}

// limitMatches reports whether the memory limit Docker reported for the
// container (MiB) is the limit the profile stands for, within 1%: docker stats
// rounds what it prints.
func limitMatches(observedMiB float64, wantMB int) bool {
	want := float64(wantMB)
	return abs(observedMiB-want) <= want*0.01
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
