package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// The hardware profiles a run can be labelled with. Every one is a resource cap on a
// machine that is larger than the profile, so every result is emulated, never certified
// (master plan 7.4). There is deliberately no way to produce emulated: false; a
// certified-hardware profile needs a new, reviewed value here.
const (
	ProfileLite     = "lite"
	ProfileBalanced = "balanced"
	ProfileHigh     = "high"
	Profile4G       = "experimental-4g"
)

type profileSpec struct {
	id    string
	memMB int
	cpus  float64
}

// The caps mirror deploy/container/localai/compose.<profile>.yaml for lite, balanced and
// experimental-4g. "high" is the higher-memory emulated profile the CLM-8B candidate needs;
// its caps are declared HERE until the CLM compose files (D-06) define them, and must be
// aligned with them.
var profileSpecs = []profileSpec{
	{ProfileLite, 8192, 4},
	{ProfileBalanced, 16384, 8},
	{ProfileHigh, 32768, 8},
	{Profile4G, 4096, 4},
}

// ParseProfile maps the command-line spelling to a profile id (4g is the short form of
// experimental-4g).
func ParseProfile(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "4g" {
		return Profile4G, nil
	}
	for _, p := range profileSpecs {
		if p.id == s {
			return p.id, nil
		}
	}
	return "", fmt.Errorf("unknown profile %q (use lite, balanced, high or 4g)", s)
}

// IsEmulated reports whether results under profile are emulated: always, for every known
// profile.
func IsEmulated(profile string) bool {
	_, ok := LimitsFor(profile)
	return ok
}

// LimitsFor returns the container limits a profile stands for.
func LimitsFor(profile string) (ContainerLimits, bool) {
	for _, p := range profileSpecs {
		if p.id == profile {
			return ContainerLimits{MemMB: p.memMB, CPUs: p.cpus, Source: "profile-definition"}, true
		}
	}
	return ContainerLimits{}, false
}

// CollectHost describes the machine: logical cores, OS and architecture from the Go
// runtime. A scorer in a container or on another machine is not described by this.
func CollectHost() HostInfo {
	return HostInfo{LogicalCores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}

var gitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CollectGit asks git for HEAD and whether the tree is dirty, running two fixed commands
// in dir (no shell, no user-controlled argument). When git cannot answer the fields are
// null: a result without a commit says so.
func CollectGit(ctx context.Context, dir string) GitInfo {
	run := func(args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
