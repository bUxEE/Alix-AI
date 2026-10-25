package main

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// CollectHost describes the machine the tool runs on: logical cores, OS and
// architecture from the Go runtime; the CPU model and installed memory from the
// operating system where the tool knows how to ask (Windows and Linux), null
// elsewhere. It is the same description as tools/bench's, so results from the
// two tools name hosts the same way.
func CollectHost() HostInfo {
	h := HostInfo{LogicalCores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if model := strings.Join(strings.Fields(cpuModel()), " "); model != "" {
		m := clipRunes(model, 200)
		h.CPUModel = &m
	}
	if b := totalRAMBytes(); b >= 1<<20 {
		mb := int64(b >> 20)
		h.RAMMB = &mb
	}
	return h
}

// parseCPUInfo returns the first "model name" of /proc/cpuinfo text.
func parseCPUInfo(text string) string {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseMemInfo returns MemTotal of /proc/meminfo text, in bytes.
func parseMemInfo(text string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "MemTotal" {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return 0
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			n <<= 10
		}
		return n
	}
	return 0
}

var gitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gitTimeout bounds each git call.
const gitTimeout = 10 * time.Second

// CollectGit asks git for the HEAD commit and whether the working tree has
// changes, running two fixed commands in dir (no shell, no user-controlled
// arguments). When git cannot answer, the fields are null: a result without a
// commit is still a result, but it says so.
func CollectGit(ctx context.Context, dir string) GitInfo {
	run := func(args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(ctx, gitTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return "", false
		}
		return string(out), true
	}
	var info GitInfo
	sha, ok := run("rev-parse", "HEAD")
	sha = strings.TrimSpace(sha)
	if !ok || !gitSHAPattern.MatchString(sha) {
		return info
	}
	info.SHA = &sha
	if status, ok := run("status", "--porcelain"); ok {
		dirty := strings.TrimSpace(status) != ""
		info.Dirty = &dirty
	}
	return info
}

// clipRunes cuts s to at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// GitInfo describes the source tree.
type GitInfo struct {
	SHA   *string `json:"sha"`
	Dirty *bool   `json:"dirty"`
}

// HostInfo describes the machine that ran the tool.
type HostInfo struct {
	CPUModel     *string `json:"cpu_model"`
	LogicalCores int     `json:"logical_cores"`
	RAMMB        *int64  `json:"ram_mb"`
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
}

// ContainerLimits are the caps a profile stands for.
type ContainerLimits struct {
	MemMB  int     `json:"mem_mb"`
	CPUs   float64 `json:"cpus"`
	Source string  `json:"source"`
}
