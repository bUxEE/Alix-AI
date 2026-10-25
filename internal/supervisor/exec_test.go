package supervisor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// HC-41: a root process does not start LocalAI.
func TestRefuseRoot(t *testing.T) {
	if err := refuseRoot(0); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("euid 0 = %v, want permission_denied", err)
	}
	for _, euid := range []int{-1, 1, 1000, 65532} {
		if err := refuseRoot(euid); err != nil {
			t.Errorf("euid %d refused: %v", euid, err)
		}
	}
}

// Start itself applies the guard, before it touches the binary.
func TestExecStartRefusesARootProcess(t *testing.T) {
	ex, err := NewExec(ExecConfig{Path: fakeChild})
	if err != nil {
		t.Fatal(err)
	}
	ex.euid = func() int { return 0 }
	if _, err := ex.Start(context.Background()); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("Start as root = %v, want permission_denied", err)
	}
}

func TestNewExecValidatesConfig(t *testing.T) {
	abs := fakeChild
	tests := []struct {
		name string
		cfg  ExecConfig
	}{
		{"relative path", ExecConfig{Path: "localai"}},
		{"no path", ExecConfig{}},
		{"relative dir", ExecConfig{Path: abs, Dir: "work"}},
		{"env without a name", ExecConfig{Path: abs, Env: []string{"=x"}}},
		{"env without a value separator", ExecConfig{Path: abs, Env: []string{"NOEQUALS"}}},
		{"negative grace", ExecConfig{Path: abs, StopGrace: -time.Second}},
		{"absurd grace", ExecConfig{Path: abs, StopGrace: time.Hour}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewExec(tt.cfg); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("NewExec = %v, want validation_failed", err)
			}
		})
	}
	if _, err := NewExec(ExecConfig{Path: abs}); err != nil {
		t.Errorf("a valid config was refused: %v", err)
	}
}

// A limit the platform cannot enforce is an error, not a silent no-op.
func TestUnenforceableLimitsAreRefused(t *testing.T) {
	_, err := NewExec(ExecConfig{Path: fakeChild, Limits: Limits{MaxOpenFiles: 64}})
	switch runtime.GOOS {
	case "linux":
		if err != nil {
			t.Errorf("linux enforces open files: %v", err)
		}
	default:
		if !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s cannot enforce max_open_files, NewExec = %v", runtime.GOOS, err)
		}
	}
	_, err = NewExec(ExecConfig{Path: fakeChild, Limits: Limits{MaxMemoryBytes: 1 << 30}})
	switch runtime.GOOS {
	case "linux", "windows":
		if err != nil {
			t.Errorf("%s enforces memory: %v", runtime.GOOS, err)
		}
	default:
		if !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s cannot enforce memory, NewExec = %v", runtime.GOOS, err)
		}
	}
}

// startFake spawns the fake child directly and stops it at test end.
func startFake(t *testing.T, cfg ExecConfig) (Instance, string) {
	t.Helper()
	dir := t.TempDir()
	cfg.Path = fakeChild
	cfg.Args = append([]string{"-state", dir}, cfg.Args...)
	ex, err := NewExec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ex.euid = func() int { return 1000 } // CI containers often run tests as root
	inst, err := ex.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = inst.Stop(ctx)
	})
	return inst, dir
}

// Nothing of the supervisor's environment, which holds AlixAI's secrets, reaches
// the child: it gets exactly the variables it is configured with, and none when
// none is configured (the default; a nil Env must not mean "inherit").
func TestExecDoesNotInheritTheEnvironment(t *testing.T) {
	t.Setenv("ALIXAI_TEST_SECRET", "hunter2")
	for _, tt := range []struct {
		name           string
		env            []string
		wantConfigured bool
	}{
		{"no env configured", nil, false},
		{"empty env configured", []string{}, false},
		{"one variable configured", []string{"LOCALAI_TEST=1"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dumped := filepath.Join(t.TempDir(), "env")
			inst, _ := startFake(t, ExecConfig{Args: []string{"-dump-env", dumped}, Env: tt.env})
			// The dump is renamed into place whole; it is read once it can be opened
			// (Windows briefly holds a file just renamed, e.g. for a virus scan).
			var b []byte
			waitFor(t, 60*time.Second, "the child to write its environment", func() bool {
				var err error
				b, err = os.ReadFile(dumped)
				return err == nil
			})
			names := strings.Fields(string(b))
			var hasConfigured bool
			for _, n := range names {
				if n == "LOCALAI_TEST" {
					hasConfigured = true
				}
				if strings.Contains(strings.ToUpper(n), "ALIXAI") {
					t.Errorf("the child inherited %s", n)
				}
			}
			if hasConfigured != tt.wantConfigured {
				t.Errorf("LOCALAI_TEST present = %v, want %v (child saw %v)", hasConfigured, tt.wantConfigured, names)
			}
			if !tt.wantConfigured && runtime.GOOS != "windows" && len(names) != 0 {
				t.Errorf("an unconfigured child inherited %v", names)
			}
			if inst.PID() == os.Getpid() || inst.PID() == 0 {
				t.Errorf("pid = %d", inst.PID())
			}
		})
	}
}

// Stop returns only once the process is gone, and says how it ended in a fixed
// format.
func TestExecStopConfirmsTheProcessIsGone(t *testing.T) {
	inst, _ := startFake(t, ExecConfig{StopGrace: time.Second})
	select {
	case <-inst.Done():
		t.Fatal("the child exited on its own")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := inst.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.Done():
	default:
		t.Fatal("Stop returned but Done is not closed")
	}
	if d, ok := inst.(exitDetailer); !ok || d.ExitDetail() == "" {
		t.Errorf("no exit detail after the stop")
	}
	if err := inst.Stop(ctx); err != nil {
		t.Errorf("a second Stop must be a no-op, got %v", err)
	}
}

// A stop_grace longer than the caller's deadline ends the grace wait on the
// context; the kill that follows is still confirmed, not reported as a timeout
// (which would make the supervisor give up on a process that is gone).
func TestExecStopConfirmsTheKillAfterTheContextEnded(t *testing.T) {
	inst, _ := startFake(t, ExecConfig{StopGrace: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := inst.Stop(ctx); err != nil {
		t.Fatalf("Stop after the context ended = %v, want nil", err)
	}
	select {
	case <-inst.Done():
	default:
		t.Fatal("Stop returned but Done is not closed")
	}
}

func TestExecStartFailures(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	ex, err := NewExec(ExecConfig{Path: missing})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Start(context.Background()); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("missing binary: Start = %v, want unavailable", err)
	}
	dirAsBinary, err := NewExec(ExecConfig{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dirAsBinary.Start(context.Background()); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("directory as binary: Start = %v, want unavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ex2, _ := NewExec(ExecConfig{Path: fakeChild})
	if _, err := ex2.Start(ctx); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("cancelled context: Start = %v, want unavailable", err)
	}
}

// With a memory limit set the child is confined (a job object on Windows,
// prlimit on Linux) and still runs; Start would have failed otherwise.
func TestExecAppliesLimits(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no limit is enforceable on " + runtime.GOOS)
	}
	lim := Limits{MaxMemoryBytes: 8 << 30}
	inst, dir := startFake(t, ExecConfig{Limits: lim})
	waitFor(t, 60*time.Second, "the confined child to be ready", func() bool {
		_, err := os.Stat(readyFile(dir, inst.PID()))
		return err == nil
	})
	select {
	case <-inst.Done():
		t.Fatal("the confined child exited")
	default:
	}
}

// A runtime that dies while a backend it started still holds the output it
// inherited is seen gone at once: waiting for that output is bounded, and the
// sweep then takes the backend with it.
func TestExecDeathIsSeenWhileABackendHoldsItsOutput(t *testing.T) {
	var out bytes.Buffer // not a file: the output goes through a pipe the backend inherits
	inst, _ := startFake(t, ExecConfig{Args: []string{"-spawn-holder"}, Stdout: &out, Stderr: &out})
	select {
	case <-inst.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the runtime exited but the supervisor still waits on the output its backend holds")
	}
}

func TestExitDetailOfACleanExitWithHeldOutput(t *testing.T) {
	if got := exitDetail(exec.ErrWaitDelay); got != "exit status 0" {
		t.Errorf("exitDetail(ErrWaitDelay) = %q, want the clean exit", got)
	}
}
