package supervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Limits caps what the spawned runtime may use. A zero field is "no cap from
// the supervisor" (the service manager's own limits still apply). A cap the
// platform cannot enforce is an error from NewExec, never a silent no-op:
//
//	Linux    MaxMemoryBytes (RLIMIT_DATA: private memory, not mapped model files),
//	         MaxOpenFiles (RLIMIT_NOFILE); core dumps are always disabled
//	Windows  MaxMemoryBytes (job object process memory limit)
//	others   none
type Limits struct {
	MaxMemoryBytes uint64 `yaml:"max_memory_bytes"`
	MaxOpenFiles   uint64 `yaml:"max_open_files"`
}

// Default and bounds for the polite stop. killWait bounds the wait for a killed
// process to be reaped; it does not depend on the caller's context, which may
// already have ended during a long grace period.
const (
	DefaultStopGrace = 10 * time.Second
	maxStopGrace     = 5 * time.Minute
	killWait         = 10 * time.Second
	outputWait       = 2 * time.Second
)

// ExecConfig describes the process an Exec runtime spawns.
type ExecConfig struct {
	// Path is the absolute path of the LocalAI binary. It is never searched for
	// on PATH and never run through a shell.
	Path string `yaml:"path"`
	// Args are passed as they are.
	Args []string `yaml:"args"`
	// Dir is the working directory; empty means the supervisor's.
	Dir string `yaml:"dir"`
	// Env is the child's whole environment as NAME=value entries. Nothing is
	// inherited from the supervisor (apart from what the platform needs to run a
	// process at all), so no secret of AlixAI's reaches LocalAI.
	Env []string `yaml:"env"`
	// Limits are applied to the child right after it starts.
	Limits Limits `yaml:"limits"`
	// StopGrace is how long a polite stop waits before the process is killed.
	StopGrace time.Duration `yaml:"stop_grace"`
	// Stdout and Stderr receive the child's output; nil discards it. LocalAI can
	// print prompts at debug level, so point them only at an owner-only file.
	Stdout io.Writer `yaml:"-"`
	Stderr io.Writer `yaml:"-"`
}

// Exec is the Runtime that spawns LocalAI itself. It belongs in the LocalAI
// identity's own unit, never in the hardened AlixAI service (see the package
// documentation), and refuses to run as root.
type Exec struct {
	cfg  ExecConfig
	euid func() int // the effective uid; a field so tests can run as root in a container
}

var _ Runtime = (*Exec)(nil)

// NewExec validates cfg. It starts nothing.
func NewExec(cfg ExecConfig) (*Exec, error) {
	bad := func(msg string) (*Exec, error) {
		return nil, contracts.New(contracts.ErrValidationFailed, "supervisor: "+msg)
	}
	if !filepath.IsAbs(cfg.Path) {
		return bad("exec path must be absolute")
	}
	if cfg.Dir != "" && !filepath.IsAbs(cfg.Dir) {
		return bad("exec dir must be absolute")
	}
	for _, kv := range cfg.Env {
		if i := strings.IndexByte(kv, '='); i < 1 {
			return bad("exec env entries must be NAME=value")
		}
	}
	if cfg.StopGrace < 0 || cfg.StopGrace > maxStopGrace {
		return bad("exec stop_grace out of range")
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = DefaultStopGrace
	}
	if err := limitsSupported(cfg.Limits); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "supervisor: a configured limit cannot be enforced here")
	}
	cfg.Args = append([]string(nil), cfg.Args...)
	cfg.Env = append([]string(nil), cfg.Env...)
	return &Exec{cfg: cfg, euid: currentEUID}, nil
}

// refuseRoot is the HC-41 guard: the supervisor does not run LocalAI from a
// root process. euid is negative where the platform has no such notion.
func refuseRoot(euid int) error {
	if euid == 0 {
		return contracts.New(contracts.ErrPermissionDenied, "supervisor: refusing to start the runtime from a root process (HC-41)")
	}
	return nil
}

// Start spawns the process, applies the limits and returns the instance. If a
// limit cannot be applied the process is killed and Start fails: the runtime
// never runs unlimited by accident.
func (e *Exec) Start(ctx context.Context) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "supervisor: not starting the runtime, the context ended")
	}
	if err := refuseRoot(e.euid()); err != nil {
		return nil, err
	}
	fi, err := os.Stat(e.cfg.Path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, contracts.New(contracts.ErrUnavailable, "supervisor: the runtime binary is missing or not a regular file")
	}

	cmd := exec.Command(e.cfg.Path, e.cfg.Args...)
	cmd.Dir = e.cfg.Dir
	// A nil Env would make os/exec inherit the supervisor's whole environment, so
	// the slice is never nil, even when nothing is configured.
	env := make([]string, 0, len(e.cfg.Env)+1)
	env = append(env, baseEnv()...)
	cmd.Env = append(env, e.cfg.Env...)
	cmd.Stdout, cmd.Stderr = e.cfg.Stdout, e.cfg.Stderr
	// A backend the runtime started can keep the output it inherited open after
	// the runtime died; Wait would then wait for the backend, and the sweep that
	// ends it runs only after Wait. Once the runtime is gone, its output is
	// drained for outputWait at most.
	cmd.WaitDelay = outputWait
	prepareCmd(cmd)
	if err := cmd.Start(); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "supervisor: the runtime process could not be started")
	}
	pid := cmd.Process.Pid
	g, err := guardProcess(pid, e.cfg.Limits)
	if err != nil {
		if g != nil {
			g.kill(pid)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if g != nil {
			g.sweep()
		}
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "supervisor: the runtime could not be confined, so it was not kept running")
	}

	inst := &execInstance{pid: pid, g: g, grace: e.cfg.StopGrace, done: make(chan struct{})}
	go func() {
		werr := cmd.Wait()
		inst.detail = exitDetail(werr)
		g.sweep() // the leader is gone: take its backend processes with it
		close(inst.done)
	}()
	return inst, nil
}

// execInstance is a process this supervisor started.
type execInstance struct {
	pid    int
	g      *procGuard
	grace  time.Duration
	done   chan struct{}
	detail string // written before done is closed
}

func (i *execInstance) PID() int              { return i.pid }
func (i *execInstance) Done() <-chan struct{} { return i.done }

// ExitDetail implements exitDetailer. It is empty until the process has ended.
func (i *execInstance) ExitDetail() string {
	select {
	case <-i.done:
		return i.detail
	default:
		return ""
	}
}

// Stop asks the process group to end, kills it after the grace period and
// returns once the process is confirmed gone. Only the PID this instance
// started, and the group or job created for it, are ever signalled.
func (i *execInstance) Stop(ctx context.Context) error {
	select {
	case <-i.done:
		return nil
	default:
	}
	if i.g.terminate(i.pid) {
		t := time.NewTimer(i.grace)
		defer t.Stop()
		select {
		case <-i.done:
			return nil
		case <-t.C:
		case <-ctx.Done():
		}
	}
	i.g.kill(i.pid)
	t := time.NewTimer(killWait)
	defer t.Stop()
	select {
	case <-i.done:
		return nil
	case <-t.C:
		return contracts.New(contracts.ErrTimeout, "supervisor: the runtime process was killed but is not confirmed gone")
	}
}

// exitDetail renders how a process ended in a fixed format: "exit status 3",
// "signal: killed". It never includes anything the process printed.
func exitDetail(err error) string {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		// ErrWaitDelay: it exited 0, and a backend still held its output.
		return "exit status 0"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ProcessState.String()
	}
	return "wait failed"
}
