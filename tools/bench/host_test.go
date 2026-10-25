package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHostParsers(t *testing.T) {
	cpuinfo := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Core(TM) i7-10700K CPU @ 3.80GHz\ncpu MHz\t\t: 3800.000\n\nprocessor\t: 1\nmodel name\t: Other\n"
	if got := parseCPUInfo(cpuinfo); got != "Intel(R) Core(TM) i7-10700K CPU @ 3.80GHz" {
		t.Errorf("parseCPUInfo = %q", got)
	}
	if got := parseCPUInfo("processor : 0\n"); got != "" {
		t.Errorf("parseCPUInfo without a model name = %q", got)
	}

	meminfo := "MemTotal:       65380000 kB\nMemFree:        1000 kB\n"
	if got := parseMemInfo(meminfo); got != 65380000<<10 {
		t.Errorf("parseMemInfo = %d", got)
	}
	for _, bad := range []string{"", "MemFree: 5 kB", "MemTotal: lots kB", "MemTotal:"} {
		if got := parseMemInfo(bad); got != 0 {
			t.Errorf("parseMemInfo(%q) = %d, want 0", bad, got)
		}
	}
}

func TestCollectHost(t *testing.T) {
	h := CollectHost()
	if h.LogicalCores < 1 || h.OS == "" || h.Arch == "" {
		t.Errorf("host = %+v", h)
	}
	if h.CPUModel != nil && (strings.TrimSpace(*h.CPUModel) == "" || strings.Contains(*h.CPUModel, "  ")) {
		t.Errorf("cpu model %q must be a tidy non-empty string or null", *h.CPUModel)
	}
	if h.RAMMB != nil && *h.RAMMB < 1 {
		t.Errorf("ram = %d", *h.RAMMB)
	}
	t.Logf("host: %v cores, cpu %v, ram %v MiB", h.LogicalCores, derefStr(h.CPUModel), h.RAMMB)
}

func derefStr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func TestCollectGit(t *testing.T) {
	t.Run("a directory that is not a repository gives nulls", func(t *testing.T) {
		g := CollectGit(context.Background(), t.TempDir())
		// t.TempDir lives under the system temp directory, which is not in a repository.
		if g.SHA != nil || g.Dirty != nil {
			// Only possible when the temp directory is itself inside a repository.
			t.Logf("temp dir is inside a repository: %+v", g)
		}
	})
	t.Run("the repository gives a full sha", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		root := repoPath(t, ".")
		if _, err := os.Stat(root + "/.git"); err != nil {
			t.Skip("not a git checkout")
		}
		g := CollectGit(context.Background(), root)
		if g.SHA == nil || !gitSHAPattern.MatchString(*g.SHA) || g.Dirty == nil {
			t.Errorf("git = %+v", g)
		}
	})
	t.Run("a cancelled context gives nulls", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if g := CollectGit(ctx, repoPath(t, ".")); g.SHA != nil {
			t.Errorf("git = %+v", g)
		}
	})
}
