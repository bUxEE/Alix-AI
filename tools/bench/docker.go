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
)

// Container memory is read with "docker stats --no-stream" and nothing else:
// one fixed argument vector, no shell, and a container name that has been
// matched against a strict pattern first (so it cannot start with a dash and
// become a flag). The figure is the cgroup's memory usage as Docker computes
// it (usage minus inactive page cache). It is the closest container-level
// proxy to RSS, not RSS itself: model weights that llama.cpp maps from the
// file are file-backed pages, and whichever of them the kernel counts as
// active cache stay in the number.

var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// maxStatsOutput bounds what one docker stats call may print.
const maxStatsOutput = 4096

// dockerStats is a MemorySource over docker stats.
type dockerStats struct {
	container string
	bin       string
	// run executes the container CLI with args and returns its standard
	// output. Tests replace it; the default is execCLI on bin.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// newDockerStats validates the container name and the CLI to run. bin is the
// docker (or podman) executable: a bare name looked up in PATH, or an absolute
// path (a PATH that Go cannot search, as on some Windows setups, needs one).
// It may only name docker or podman, so the flag cannot turn the tool into a
// way to run an arbitrary program.
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
		return MemSample{}, fmt.Errorf("unexpected docker stats output %q", truncateForError(s))
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
		return 0, fmt.Errorf("unexpected size %q in docker stats output", truncateForError(s))
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

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if t, cut := truncateRunes(s, 60); cut {
		return t + "..."
	}
	return s
}
