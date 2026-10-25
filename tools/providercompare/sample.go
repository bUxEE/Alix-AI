package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MemSample is one reading of the harness process, the optional core process and, inside a
// cgroup-capped container, the cgroup. A value of -1 means the operating system cannot say.
type MemSample struct {
	TMS int64 `json:"t_ms"`
	// Go runtime view of the harness process (every OS).
	GoHeapInuseMiB float64 `json:"go_heap_inuse_mib"`
	GoSysMiB       float64 `json:"go_sys_mib"`
	Goroutines     int     `json:"goroutines"`
	// Linux only: resident set (VmRSS) of this process and of the core child, in MiB.
	SelfRSSMiB float64 `json:"self_rss_mib"`
	CoreRSSMiB float64 `json:"core_rss_mib"`
	// cgroup v2 memory.current of the container, in MiB (everything in it, page cache included).
	CgroupCurrentMiB float64 `json:"cgroup_current_mib"`
}

// CgroupFacts are the limits the container really runs under, read from inside it.
type CgroupFacts struct {
	Present     bool    `json:"present"`
	MemMaxMiB   float64 `json:"memory_max_mib"`
	SwapMaxMiB  float64 `json:"memory_swap_max_mib"`
	PeakMiB     float64 `json:"memory_peak_mib"`
	OOMKill     int64   `json:"oom_kill"`
	CPUMax      string  `json:"cpu_max"`
	SwapCurrent float64 `json:"memory_swap_current_mib"`
}

const miB = 1 << 20

func readFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// cgroupInt reads a single-integer cgroup file; "max" and an unreadable file give -1.
func cgroupInt(name string) float64 {
	s, ok := readFile("/sys/fs/cgroup/" + name)
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return float64(n) / miB
}

// vmRSSMiB reads VmRSS of a process from /proc (Linux), in MiB, or -1.
func vmRSSMiB(pid string) float64 {
	s, ok := readFile("/proc/" + pid + "/status")
	if !ok {
		return -1
	}
	for _, line := range strings.Split(s, "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found || k != "VmRSS" {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			return -1
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return -1
		}
		return float64(n) / 1024 // kB to MiB
	}
	return -1
}

// readCgroupFacts reads the container's limits. Present is false outside a cgroup v2 container.
func readCgroupFacts() CgroupFacts {
	f := CgroupFacts{MemMaxMiB: cgroupInt("memory.max"), SwapMaxMiB: cgroupInt("memory.swap.max"),
		PeakMiB: cgroupInt("memory.peak"), SwapCurrent: cgroupInt("memory.swap.current")}
	if _, ok := readFile("/sys/fs/cgroup/memory.current"); !ok {
		return f
	}
	f.Present = true
	f.CPUMax, _ = readFile("/sys/fs/cgroup/cpu.max")
	if ev, ok := readFile("/sys/fs/cgroup/memory.events"); ok {
		for _, line := range strings.Split(ev, "\n") {
			if k, v, found := strings.Cut(line, " "); found && k == "oom_kill" {
				f.OOMKill, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			}
		}
	}
	return f
}

// sampler reads memory on an interval until stopped.
type sampler struct {
	every   time.Duration
	corePID func() string // "" when no core child runs
	start   time.Time

	mu      sync.Mutex
	samples []MemSample
	stop    chan struct{}
	done    chan struct{}
}

func newSampler(every time.Duration, corePID func() string) *sampler {
	if every <= 0 {
		every = 250 * time.Millisecond
	}
	return &sampler{every: every, corePID: corePID, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
}

func (s *sampler) read() MemSample {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	out := MemSample{
		TMS:            time.Since(s.start).Milliseconds(),
		GoHeapInuseMiB: float64(m.HeapInuse) / miB, GoSysMiB: float64(m.Sys) / miB, Goroutines: runtime.NumGoroutine(),
		SelfRSSMiB: -1, CoreRSSMiB: -1, CgroupCurrentMiB: -1,
	}
	if runtime.GOOS == "linux" {
		out.SelfRSSMiB = vmRSSMiB("self")
		out.CgroupCurrentMiB = cgroupInt("memory.current")
		if s.corePID != nil {
			if pid := s.corePID(); pid != "" {
				out.CoreRSSMiB = vmRSSMiB(pid)
			}
		}
	}
	return out
}

// now takes one reading and records it.
func (s *sampler) now() MemSample {
	m := s.read()
	s.mu.Lock()
	s.samples = append(s.samples, m)
	s.mu.Unlock()
	return m
}

func (s *sampler) run() {
	defer close(s.done)
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.now()
		}
	}
}

// begin starts sampling in the background; end stops it and returns everything read.
func (s *sampler) begin() { go s.run() }

func (s *sampler) end() []MemSample {
	close(s.stop)
	<-s.done
	s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MemSample(nil), s.samples...)
}
