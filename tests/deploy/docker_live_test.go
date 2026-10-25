//go:build live

package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run with: go test -tags live ./tests/deploy/ -run TestDockerBuildSmoke
// It builds the images with the local Docker daemon, so it needs a daemon and the base images (a network on
// the first run). The static checks of this package run without any of that.

func docker(t *testing.T, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Dir = repoRoot(t)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return out.String(), errDockerTimeout
	}
}

var errDockerTimeout = errors.New("docker command timed out")

func TestDockerBuildSmoke(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if out, err := docker(t, time.Minute, "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("no Docker daemon: %v\n%s", err, out)
	}

	type variant struct {
		tag    string
		args   []string
		target string
	}
	variants := []variant{
		{tag: "alixai:m5-01-smoke", target: "alixai"},
		{tag: "alixai:m5-01-smoke-noexternal", target: "alixai", args: []string{"--build-arg", "GO_TAGS=noexternal"}},
		{tag: "alixai-netctl:m5-01-smoke", target: "netctl"},
	}
	for _, v := range variants {
		t.Cleanup(func() { _, _ = docker(t, time.Minute, "rmi", "-f", v.tag) })
	}
	for _, v := range variants {
		t.Run(v.tag, func(t *testing.T) {
			args := append([]string{"build", "-q", "-f", dockerfile, "--target", v.target, "-t", v.tag}, v.args...)
			args = append(args, ".")
			if out, err := docker(t, 15*time.Minute, args...); err != nil {
				t.Fatalf("docker build failed: %v\n%s", err, out)
			}

			out, err := docker(t, time.Minute, "image", "inspect", v.tag)
			if err != nil {
				t.Fatalf("inspect: %v\n%s", err, out)
			}
			var inspected []struct {
				Config struct {
					User       string
					Entrypoint []string
					Volumes    map[string]struct{}
				}
			}
			if err := json.Unmarshal([]byte(out), &inspected); err != nil || len(inspected) != 1 {
				t.Fatalf("inspect output: %v\n%s", err, out)
			}
			cfg := inspected[0].Config
			if uidOf(cfg.User) <= 0 {
				t.Errorf("image user %q is not a numeric non-root uid", cfg.User)
			}
			if len(cfg.Volumes) != 0 {
				t.Errorf("image declares volumes %v; data lives in bind mounts the operator names", cfg.Volumes)
			}

			// distroless: there is no shell to exec into
			if out, err := docker(t, time.Minute, "run", "--rm", "--read-only", "--cap-drop", "ALL", "--entrypoint", "/bin/sh", v.tag, "-c", "true"); err == nil {
				t.Errorf("the image has a shell: %s", out)
			}
		})
	}

	t.Run("alixai runs read-only without capabilities", func(t *testing.T) {
		out, err := docker(t, 2*time.Minute, "run", "--rm", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "alixai:m5-01-smoke", "version")
		if err != nil || !strings.Contains(out, "go") {
			t.Fatalf("alixai version: %v\n%s", err, out)
		}
	})
	t.Run("netctl refuses a probe that leaves loopback", func(t *testing.T) {
		out, err := docker(t, 2*time.Minute, "run", "--rm", "--read-only", "--cap-drop", "ALL", "--network", "none", "alixai-netctl:m5-01-smoke", "probe", "http://example.com/")
		if err == nil {
			t.Fatalf("a probe of a non-loopback URL was accepted: %s", out)
		}
	})
}
