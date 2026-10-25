package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The resident-set sampler. docker stats cannot give the figure the memory budget is written in (R of docs/operations/resource-budgets.md
// section 3: the summed VmRSS of the LocalAI processes, memory-mapped model pages included) and it cannot tell the voice workers apart, so
// the sampler runs one fixed script inside the container every interval (docker exec, an argument vector, no shell on the host) that reads
// /proc for every LocalAI process, and the cgroup's memory.current minus inactive_file (W, what docker stats shows). Each process is
// classified by its command line: service (the LocalAI process itself), whisper (the whisper.cpp backend: speech to text, or the VAD), piper
// (text to speech) and llama (the text backend of the co-residency run). With one model resident at a time (the per-candidate phases),
// the class sum IS the worker's figure.

var containerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// sampleScript is the constant script of one sample. Lines: "P|pid|VmRSS|RssAnon|RssFile|cmdline" per process, "C|memory.current|inactive_file".
const sampleScript = `for p in /proc/[0-9]*; do
  n=$(cat "$p/comm" 2>/dev/null) || continue
  case "$n" in sh|bash|cat|tr|awk|sleep) continue;; esac
  c=$(tr '\0' ' ' <"$p/cmdline" 2>/dev/null)
  case "$c" in *local-ai*|*/backends/*) ;; *) continue;; esac
  awk -v pid="${p#/proc/}" -v c="$c" '/^VmRSS:/{r=$2} /^RssAnon:/{a=$2} /^RssFile:/{f=$2} END{print "P|" pid "|" r "|" a "|" f "|" c}' "$p/status" 2>/dev/null
done
echo "C|$(cat /sys/fs/cgroup/memory.current 2>/dev/null)|$(awk '/^inactive_file /{print $2}' /sys/fs/cgroup/memory.stat 2>/dev/null)"`

type procSample struct {
	PID                   int
	Class                 string
	RSSKB, AnonKB, FileKB int64
}

type memSample struct {
	T       time.Time
	Procs   []procSample
	CurrB   int64 // cgroup memory.current, bytes
	InactFB int64 // cgroup inactive_file, bytes
}

// classOf names the process by its command line: the directory of its backend under /backends (whisper, piper, kokoros), "llama" for any
// llama.cpp backend, "service" for the LocalAI process itself. Runs made before kokoros was named have it as "other-backend".
func classOf(cmd string) string {
	i := strings.Index(cmd, "/backends/")
	if i < 0 {
		return "service"
	}
	rest := cmd[i+len("/backends/"):]
	if j := strings.IndexAny(rest, "/ "); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "llama") {
		return "llama"
	}
	if rest == "" {
		return "other-backend"
	}
	return rest
}

// parseSample parses the output of sampleScript.
func parseSample(out string, at time.Time) memSample {
	s := memSample{T: at}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 6)
		switch {
		case len(f) == 6 && f[0] == "P":
			pid, _ := strconv.Atoi(f[1])
			r, _ := strconv.ParseInt(f[2], 10, 64)
			a, _ := strconv.ParseInt(f[3], 10, 64)
			fl, _ := strconv.ParseInt(f[4], 10, 64)
			s.Procs = append(s.Procs, procSample{PID: pid, Class: classOf(f[5]), RSSKB: r, AnonKB: a, FileKB: fl})
		case len(f) == 3 && f[0] == "C":
			s.CurrB, _ = strconv.ParseInt(f[1], 10, 64)
			s.InactFB, _ = strconv.ParseInt(f[2], 10, 64)
		}
	}
	return s
}

// Sampler samples the container until Stop.
type Sampler struct {
	container, docker string
	interval          time.Duration
	mu                sync.Mutex
	samples           []memSample
	errs              int
	measureFrom       time.Time
	cancel            context.CancelFunc
	done              chan struct{}
}

// StartSampler begins sampling every interval (at least 500 ms).
func StartSampler(ctx context.Context, dockerBin, container string, interval time.Duration) (*Sampler, error) {
	if !containerNameRE.MatchString(container) {
		return nil, fmt.Errorf("container name %q is not a plain name", container)
	}
	if interval < 500*time.Millisecond {
		interval = 500 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Sampler{container: container, docker: dockerBin, interval: interval, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			s.once(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return s, nil
}

func (s *Sampler) once(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	at := time.Now()
	out, err := exec.CommandContext(cctx, s.docker, "exec", s.container, "sh", "-c", sampleScript).Output()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			s.errs++
		}
		return
	}
	s.samples = append(s.samples, parseSample(string(out), at))
}

// MarkMeasure says the measured part of the phase starts now: steady figures use only the samples taken after it.
func (s *Sampler) MarkMeasure() {
	s.mu.Lock()
	s.measureFrom = time.Now()
	s.mu.Unlock()
}

// ClassMem is the resident set of one process class, in KiB.
type ClassMem struct {
	PeakKB   int64 `json:"peak_kb"`
	SteadyKB int64 `json:"steady_kb"`
	PeakAnon int64 `json:"peak_anon_kb"`
	PeakFile int64 `json:"peak_file_kb"`
	MaxProcs int   `json:"max_processes"`
}

// MemorySummary is what a phase's memory sampling reports.
type MemorySummary struct {
	Source        string              `json:"source"`
	Container     string              `json:"container"`
	IntervalMS    int64               `json:"interval_ms"`
	Samples       int                 `json:"samples"`
	SampleErrors  int                 `json:"sample_errors"`
	SteadySamples int                 `json:"steady_samples"`
	Classes       map[string]ClassMem `json:"classes"`
	// RTotalPeakKB is the peak of the summed resident set of every LocalAI process (R); WPeakKB the peak of memory.current minus inactive_file (W).
	RTotalPeakKB   int64 `json:"r_total_peak_kb"`
	RTotalSteadyKB int64 `json:"r_total_steady_kb"`
	WPeakKB        int64 `json:"w_peak_kb"`
}

// Stop ends sampling and summarises it.
func (s *Sampler) Stop() MemorySummary {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return summarizeMemory(s.samples, s.measureFrom, s.container, s.interval, s.errs)
}

func medianInt(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int64(nil), xs...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[(len(c)-1)/2]
}

// summarizeMemory is pure so it can be tested. Peak: the largest value over every sample (loading is when memory peaks). Steady: the median of the
// samples at or after measureFrom (all of them when it is zero).
func summarizeMemory(samples []memSample, measureFrom time.Time, container string, interval time.Duration, errs int) MemorySummary {
	m := MemorySummary{Source: "proc-rss", Container: container, IntervalMS: interval.Milliseconds(), Samples: len(samples), SampleErrors: errs, Classes: map[string]ClassMem{}}
	steady := map[string][]int64{}
	var steadyTotal []int64
	for _, sm := range samples {
		inSteady := measureFrom.IsZero() || !sm.T.Before(measureFrom)
		sums := map[string]ClassMem{}
		var total int64
		for _, p := range sm.Procs {
			c := sums[p.Class]
			c.PeakKB += p.RSSKB
			c.PeakAnon += p.AnonKB
			c.PeakFile += p.FileKB
			c.MaxProcs++
			sums[p.Class] = c
			total += p.RSSKB
		}
		for name, c := range sums {
			agg := m.Classes[name]
			agg.PeakKB = max(agg.PeakKB, c.PeakKB)
			agg.PeakAnon = max(agg.PeakAnon, c.PeakAnon)
			agg.PeakFile = max(agg.PeakFile, c.PeakFile)
			agg.MaxProcs = max(agg.MaxProcs, c.MaxProcs)
			m.Classes[name] = agg
			if inSteady {
				steady[name] = append(steady[name], c.PeakKB)
			}
		}
		m.RTotalPeakKB = max(m.RTotalPeakKB, total)
		if w := (sm.CurrB - sm.InactFB) / 1024; sm.CurrB > 0 {
			m.WPeakKB = max(m.WPeakKB, w)
		}
		if inSteady {
			steadyTotal = append(steadyTotal, total)
			m.SteadySamples++
		}
	}
	for name, xs := range steady {
		agg := m.Classes[name]
		agg.SteadyKB = medianInt(xs)
		m.Classes[name] = agg
	}
	m.RTotalSteadyKB = medianInt(steadyTotal)
	return m
}
