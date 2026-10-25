package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Container memory is read with `docker stats --no-stream` and swap with
// `docker inspect`, and nothing else: fixed argument vectors, no shell, and a
// container name matched against a strict pattern first (so it cannot start
// with a dash and become a flag). The figure is the cgroup's memory usage as
// Docker computes it (usage minus inactive page cache), the closest
// container-level proxy to RSS, the same one tools/bench reports.
//
// docker stats has no swap column. What the harness can say about swap from
// the container runtime is whether the container is allowed any: with
// HostConfig.MemorySwap equal to HostConfig.Memory (what the profile files set:
// memswap_limit equal to mem_limit) the cgroup's swap limit is zero and swap in
// use is structurally zero. Any other setting leaves swap possible and
// unmeasured, and the run then says so (SwapKnown false) rather than report a
// zero nobody read. A host process on Linux is measured directly (source_host.go).

var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// maxCLIOutput bounds what one container CLI call may print.
const maxCLIOutput = 64 << 10

// dockerSource is a MemorySource over docker (or podman) stats and inspect.
type dockerSource struct {
	bin        string
	containers []string
	// run executes the container CLI with args and returns its standard
	// output. Tests replace it.
	run func(ctx context.Context, args ...string) ([]byte, error)

	mu           sync.Mutex
	swapDisabled map[string]bool
}

// newDockerSource validates the container names and the CLI. bin is docker or
// podman (a bare name looked up in PATH, or an absolute path to either).
func newDockerSource(bin string, containers []string) (*dockerSource, error) {
	if len(containers) == 0 {
		return nil, errors.New("at least one --container is needed to sample a container's memory")
	}
	seen := map[string]bool{}
	for _, c := range containers {
		if !containerNamePattern.MatchString(c) {
			return nil, fmt.Errorf("container name %q is not valid (letters, digits, '_', '.' and '-', starting with a letter or digit)", c)
		}
		if seen[c] {
			return nil, fmt.Errorf("container %q is listed twice", c)
		}
		seen[c] = true
	}
	if bin == "" {
		bin = "docker"
	}
	if err := validateCLI(bin); err != nil {
		return nil, err
	}
	d := &dockerSource{bin: bin, containers: append([]string(nil), containers...), swapDisabled: map[string]bool{}}
	d.run = func(ctx context.Context, args ...string) ([]byte, error) { return execCLI(ctx, d.bin, args...) }
	return d, nil
}

// validateCLI accepts docker, podman and an absolute path to either (with or
// without .exe, in any case): the flag cannot turn the tool into a way to run
// an arbitrary program.
func validateCLI(bin string) error {
	base := strings.ToLower(filepath.Base(bin))
	base = strings.TrimSuffix(base, ".exe")
	if base != "docker" && base != "podman" {
		return fmt.Errorf("container CLI %q must be docker or podman", bin)
	}
	if bin != filepath.Base(bin) {
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

// execCLI runs the container CLI. Only dockerSource calls it, with the fixed
// argument vectors built there.
func execCLI(ctx context.Context, bin string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %w", filepath.Base(bin), args[0], err)
	}
	if len(out) > maxCLIOutput {
		return nil, errors.New("the container CLI printed more than expected")
	}
	return out, nil
}

// Describe implements MemorySource.
func (d *dockerSource) Describe() MemoryDescriptor {
	return MemoryDescriptor{Kind: MemSourceDocker, Target: strings.Join(d.containers, ",")}
}

// Sample implements MemorySource: the memory of every container, summed, and
// whether swap is structurally off in all of them.
func (d *dockerSource) Sample(ctx context.Context) (MemSample, error) {
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}"}, d.containers...)
	out, err := d.run(ctx, args...)
	if err != nil {
		return MemSample{}, err
	}
	used, err := parseStatsLines(string(out), d.containers)
	if err != nil {
		return MemSample{}, err
	}
	var total float64
	for _, c := range d.containers {
		total += used[c]
	}
	s := MemSample{UsedMiB: total}
	known := true
	for _, c := range d.containers {
		off, err := d.swapIsOff(ctx, c)
		if err != nil {
			return MemSample{}, err
		}
		if !off {
			known = false
		}
	}
	s.SwapKnown = known
	return s, nil
}

// swapIsOff reports whether the container's swap limit is zero. The answer is a
// property of how the container was created, so a positive one is kept.
func (d *dockerSource) swapIsOff(ctx context.Context, name string) (bool, error) {
	d.mu.Lock()
	off := d.swapDisabled[name]
	d.mu.Unlock()
	if off {
		return true, nil
	}
	out, err := d.run(ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", name)
	if err != nil {
		return false, err
	}
	mem, swap, err := parseInspectMemory(string(out))
	if err != nil {
		return false, err
	}
	off = mem > 0 && swap == mem
	if off {
		d.mu.Lock()
		d.swapDisabled[name] = true
		d.mu.Unlock()
	}
	return off, nil
}

// parseInspectMemory parses "<memory bytes> <memory+swap bytes>".
func parseInspectMemory(s string) (mem, swap int64, err error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected docker inspect output %q", clipRunes(strings.TrimSpace(s), 60))
	}
	if mem, err = strconv.ParseInt(f[0], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("unexpected docker inspect memory %q", clipRunes(f[0], 30))
	}
	if swap, err = strconv.ParseInt(f[1], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("unexpected docker inspect swap %q", clipRunes(f[1], 30))
	}
	return mem, swap, nil
}

// parseStatsLines parses `name<TAB>1.2GiB / 8GiB` lines into MiB per container.
// Every wanted container must be reported: a missing one is an error, never a
// smaller sum.
func parseStatsLines(text string, want []string) (map[string]float64, error) {
	got := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		name, usage, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			return nil, fmt.Errorf("unexpected docker stats line %q", clipRunes(line, 60))
		}
		used, _, ok := strings.Cut(usage, "/")
		if !ok {
			return nil, fmt.Errorf("unexpected docker stats usage %q", clipRunes(usage, 60))
		}
		mib, err := parseSizeMiB(used)
		if err != nil {
			return nil, err
		}
		got[strings.TrimSpace(name)] = mib
	}
	for _, c := range want {
		if _, ok := got[c]; !ok {
			return nil, fmt.Errorf("docker stats did not report container %q (is it running? use its name, not its id)", c)
		}
	}
	return got, nil
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
		return 0, fmt.Errorf("unexpected size %q in docker stats output", clipRunes(strings.TrimSpace(s), 40))
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
