//go:build windows

package cli

// The Windows service of the native install (M5-02, ADR 0012, deploy/native/README.md).
//
//	alixai service install -config C:\ProgramData\AlixAI\alixai.yaml    register the service (elevated)
//	alixai service uninstall                                            stop and remove it (elevated)
//	alixai service run -config C:\ProgramData\AlixAI\alixai.yaml        what the service control manager runs
//
// `service run` is the entry point the SCM starts, never a console command: it reports to the SCM, runs exactly
// what `alixai serve` runs, and turns Stop and Shutdown into the cancellation of that run. The service runs as the
// per-service virtual account `NT SERVICE\<name>` (the Windows counterpart of the unit's DynamicUser) or as
// LocalService; LocalSystem and named user accounts are refused. There is no systemd or launchd command: those
// platforms use the files under deploy/native, and this file is built for Windows only because
// golang.org/x/sys/windows/svc has no other platform.
//
// Every package-level name starts with "service" (internal/cli is shared by many tasks).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func init() {
	Register(Command{
		Name:    "service run",
		Summary: "run as a Windows service (started by the service control manager, not from a console)",
		Run:     serviceRunCmd,
	})
	Register(Command{
		Name:    "service install",
		Summary: "register the Windows service (needs an elevated prompt)",
		Run:     serviceInstallCmd,
	})
	Register(Command{
		Name:    "service uninstall",
		Summary: "stop and remove the Windows service, keeping its data (needs an elevated prompt)",
		Run:     serviceUninstallCmd,
	})
}

const (
	serviceDefaultName = "AlixAI"
	// serviceStopWait is how long the service gets to stop after the SCM asks; it is also the wait hint.
	serviceStopWait = 30 * time.Second
	// serviceStartGrace is how long a start waits for serve to refuse before it reports Running: a refused
	// configuration (a bad key, an address that needs TLS) is answered within it, so `Start-Service` fails
	// instead of reporting a service that is already gone.
	serviceStartGrace = 2 * time.Second

	// Service-specific exit codes; they make the SCM count the exit as a failure and apply the recovery actions.
	serviceExitFailed      uint32 = 1 // serve returned without being asked to stop, or refused to start
	serviceExitStopTimeout uint32 = 2 // serve did not stop within serviceStopWait

	serviceEventID uint32 = 1
	// serviceEventMax keeps one event inside what the event log accepts.
	serviceEventMax = 16000
)

// Seams: the SCM and the event log cannot be used from a test.
var (
	serviceIsWindowsService                                      = svc.IsWindowsService
	serviceSCMRun           func(string, svc.Handler) error      = svc.Run
	serviceServe                                                 = runServe
	serviceOpenLog          func(string) (io.WriteCloser, error) = serviceOpenEventLog
)

var serviceNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,79}$`)

// serviceCheckName refuses a service name that is not a plain identifier: it becomes the name of the virtual
// account and of the event source.
func serviceCheckName(name string) error {
	if !serviceNameRE.MatchString(name) {
		return Usagef("-name %q is not a service name (letters, digits, '.', '_' and '-', starting with a letter)", name)
	}
	return nil
}

// serviceCheckConfigPath wants an absolute path: a service starts in C:\Windows\System32, so a relative one
// would resolve there.
func serviceCheckConfigPath(path string) error {
	if path == "" {
		return Usagef("-config is required")
	}
	if strings.ContainsRune(path, 0) || !filepath.IsAbs(path) {
		return Usagef("-config must be an absolute path (a service starts in C:\\Windows\\System32)")
	}
	return nil
}

// serviceCheckAccount allows the two identities a service of this kind should have: its own virtual account, or
// LocalService. Anything else, LocalSystem above all, is refused (master plan 16.2: least privilege).
func serviceCheckAccount(name, account string) error {
	switch {
	case strings.EqualFold(account, `NT SERVICE\`+name), strings.EqualFold(account, `NT AUTHORITY\LocalService`):
		return nil
	}
	return Usagef(`-account must be "NT SERVICE\%s" or "NT AUTHORITY\LocalService"; the service never runs as LocalSystem or as a named user`, name)
}

// serviceLogLevel picks the event-log type of one log line: the service logs JSON lines, so an error or
// warning record is recognised by its level member.
func serviceLogLevel(line string) string {
	switch {
	case strings.Contains(line, `"level":"ERROR"`):
		return "error"
	case strings.Contains(line, `"level":"WARN"`):
		return "warning"
	}
	return "info"
}

// serviceEventLog adapts the Windows event log to an io.Writer: each line becomes one event.
type serviceEventLog struct {
	log interface {
		Info(uint32, string) error
		Warning(uint32, string) error
		Error(uint32, string) error
		Close() error
	}
}

func serviceOpenEventLog(name string) (io.WriteCloser, error) {
	l, err := eventlog.Open(name)
	if err != nil {
		return nil, fmt.Errorf("service: open the event log: %w", err)
	}
	return serviceEventLog{log: l}, nil
}

func (w serviceEventLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > serviceEventMax {
			line = line[:serviceEventMax]
		}
		var err error
		switch serviceLogLevel(line) {
		case "error":
			err = w.log.Error(serviceEventID, line)
		case "warning":
			err = w.log.Warning(serviceEventID, line)
		default:
			err = w.log.Info(serviceEventID, line)
		}
		if err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w serviceEventLog) Close() error { return w.log.Close() }

// serviceHandler runs one blocking function as a Windows service.
type serviceHandler struct {
	base       context.Context
	run        func(ctx context.Context) error
	stopWait   time.Duration
	startGrace time.Duration
}

// Execute implements svc.Handler. It returns a service-specific failure code whenever the run ends without
// the SCM having asked for it, so the SCM's recovery actions restart the service.
func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: uint32((h.startGrace + 5*time.Second) / time.Millisecond)}
	ctx, cancel := context.WithCancel(h.base)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	select {
	case <-done:
		return true, serviceExitFailed
	case <-time.After(h.startGrace):
	}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(h.stopWait / time.Millisecond)}
				cancel()
				select {
				case <-done:
					return false, 0
				case <-time.After(h.stopWait):
					return true, serviceExitStopTimeout
				}
			}
		case <-done:
			return true, serviceExitFailed
		}
	}
}

func serviceRunCmd(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("service run")
	name := fs.String("name", serviceDefaultName, "service name")
	path := fs.String("config", "", `absolute path of the YAML config file (required), e.g. C:\ProgramData\AlixAI\alixai.yaml`)
	if err := env.ParseFlags(fs, `alixai service run -config C:\ProgramData\AlixAI\alixai.yaml [-name AlixAI]`, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := serviceCheckName(*name); err != nil {
		return err
	}
	if err := serviceCheckConfigPath(*path); err != nil {
		return err
	}
	isService, err := serviceIsWindowsService()
	if err != nil {
		return fmt.Errorf("service run: %w", err)
	}
	if !isService {
		return errors.New("service run is started by the Windows service control manager; run `alixai serve` in a console")
	}

	logw, err := serviceOpenLog(*name)
	if err != nil {
		return err
	}
	defer logw.Close()
	svcEnv := *env
	svcEnv.Stderr = logw
	h := &serviceHandler{
		base: ctx,
		run: func(ctx context.Context) error {
			err := serviceServe(ctx, &svcEnv, []string{"-config", *path})
			if err != nil {
				fmt.Fprintf(logw, "alixai service: serve stopped: %v\n", err)
			}
			return err
		},
		stopWait:   serviceStopWait,
		startGrace: serviceStartGrace,
	}
	if err := serviceSCMRun(*name, h); err != nil {
		return fmt.Errorf("service run: %w", err)
	}
	return nil
}

func serviceInstallCmd(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("service install")
	name := fs.String("name", serviceDefaultName, "service name")
	path := fs.String("config", "", `absolute path of the YAML config file (required), e.g. C:\ProgramData\AlixAI\alixai.yaml`)
	account := fs.String("account", "", `account the service runs as: "NT SERVICE\<name>" (default) or "NT AUTHORITY\LocalService"`)
	if err := env.ParseFlags(fs, `alixai service install -config C:\ProgramData\AlixAI\alixai.yaml [-name AlixAI] [-account ...]`, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := serviceCheckName(*name); err != nil {
		return err
	}
	if err := serviceCheckConfigPath(*path); err != nil {
		return err
	}
	if *account == "" {
		*account = `NT SERVICE\` + *name
	}
	if err := serviceCheckAccount(*name, *account); err != nil {
		return err
	}
	if info, err := os.Stat(*path); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("service install: -config %s is not a readable file", *path)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("service install: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("service install: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service install: connect to the service control manager (needs an elevated prompt): %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(*name); err == nil {
		s.Close()
		return fmt.Errorf("service install: a service named %q already exists; run `alixai service uninstall -name %s` first", *name, *name)
	}
	s, err := m.CreateService(*name, exe, mgr.Config{
		DisplayName:      "AlixAI",
		Description:      "AlixAI application-control service (loopback only)",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: *account,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, "service", "run", "-name", *name, "-config", *path)
	if err != nil {
		return fmt.Errorf("service install: %w", err)
	}
	defer s.Close()
	// Restart after a failure, backing off; the failure count resets after a day without one.
	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
	undo := func(err error) error {
		_ = s.Delete()
		return fmt.Errorf("service install: %w (the service was not left registered)", err)
	}
	if err := s.SetRecoveryActions(recovery, 24*60*60); err != nil {
		return undo(err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return undo(err)
	}
	if err := eventlog.InstallAsEventCreate(*name, eventlog.Info|eventlog.Warning|eventlog.Error); err != nil {
		return undo(err)
	}
	fmt.Fprintf(env.Stdout, "service %q installed, runs as %s, not started.\n", *name, *account)
	fmt.Fprintf(env.Stdout, "Restrict the data directory and key file to that account before the first start (deploy/native/README.md), then: Start-Service %s\n", *name)
	return nil
}

func serviceUninstallCmd(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("service uninstall")
	name := fs.String("name", serviceDefaultName, "service name")
	if err := env.ParseFlags(fs, "alixai service uninstall [-name AlixAI]", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := serviceCheckName(*name); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service uninstall: connect to the service control manager (needs an elevated prompt): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(*name)
	if err != nil {
		return fmt.Errorf("service uninstall: no service named %q: %w", *name, err)
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("service uninstall: %w", err)
	}
	if st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("service uninstall: stop: %w", err)
		}
		deadline := time.Now().Add(serviceStopWait + 5*time.Second)
		for st.State != svc.Stopped {
			if time.Now().After(deadline) {
				return fmt.Errorf("service uninstall: %q did not stop in time; it was not removed", *name)
			}
			time.Sleep(300 * time.Millisecond)
			if st, err = s.Query(); err != nil {
				return fmt.Errorf("service uninstall: %w", err)
			}
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("service uninstall: %w", err)
	}
	// The event source may already be gone; the service itself is.
	_ = eventlog.Remove(*name)
	fmt.Fprintf(env.Stdout, "service %q removed; its data directory and configuration were not touched.\n", *name)
	return nil
}
