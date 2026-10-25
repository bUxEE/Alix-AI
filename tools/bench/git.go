package main

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

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
