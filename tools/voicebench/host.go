package main

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var gitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gitTimeout bounds each git call.
const gitTimeout = 10 * time.Second

// gitOut runs one fixed git command in dir (an argument vector, no shell, no user-controlled arguments).
func gitOut(ctx context.Context, dir string, args ...string) (string, bool) {
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

// CollectGit asks git for the HEAD commit and whether the working tree has changes. When git cannot answer, the fields are null.
func CollectGit(ctx context.Context, dir string) GitInfo {
	var info GitInfo
	sha, ok := gitOut(ctx, dir, "rev-parse", "HEAD")
	sha = strings.TrimSpace(sha)
	if !ok || !gitSHAPattern.MatchString(sha) {
		return info
	}
	info.SHA = &sha
	if status, ok := gitOut(ctx, dir, "status", "--porcelain"); ok {
		dirty := strings.TrimSpace(status) != ""
		info.Dirty = &dirty
	}
	return info
}

// protocolCommit is the commit that last touched the protocol file: the one the run is held to.
func protocolCommit(ctx context.Context, dir, path string) *string {
	out, ok := gitOut(ctx, dir, "log", "-1", "--format=%H", "--", path)
	out = strings.TrimSpace(out)
	if !ok || !gitSHAPattern.MatchString(out) {
		return nil
	}
	return &out
}

// CollectHost describes the machine LocalAI ran on, as the container sees it: the logical CPUs and memory of the Docker VM (`docker info`)
// and the CPU model of /proc/cpuinfo inside the container. The OS and architecture are those of the tool's host. Any fact the tool cannot
// obtain is null.
func CollectHost(ctx context.Context, dockerBin, container string) HostInfo {
	h := HostInfo{LogicalCores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	run := func(args ...string) string {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, dockerBin, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if f := strings.Fields(run("info", "--format", "{{.NCPU}} {{.MemTotal}}")); len(f) == 2 {
		if n, err := strconv.Atoi(f[0]); err == nil && n > 0 {
			h.LogicalCores = n
		}
		if b, err := strconv.ParseInt(f[1], 10, 64); err == nil && b >= 1<<20 {
			mb := b >> 20
			h.RAMMB = &mb
		}
	}
	if containerNameRE.MatchString(container) {
		for _, line := range strings.Split(run("exec", container, "cat", "/proc/cpuinfo"), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				m := clipRunes(strings.Join(strings.Fields(v), " "), 200)
				h.CPUModel = &m
				break
			}
		}
	}
	return h
}
