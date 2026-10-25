//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceTestRun starts h.Execute with channels the test controls, the way the SCM does.
type serviceTestRun struct {
	requests chan svc.ChangeRequest
	status   chan svc.Status
	result   chan serviceTestResult
}

type serviceTestResult struct {
	specific bool
	code     uint32
}

func serviceStart(t *testing.T, h *serviceHandler) *serviceTestRun {
	t.Helper()
	r := &serviceTestRun{
		requests: make(chan svc.ChangeRequest),
		status:   make(chan svc.Status, 16),
		result:   make(chan serviceTestResult, 1),
	}
	go func() {
		specific, code := h.Execute(nil, r.requests, r.status)
		r.result <- serviceTestResult{specific, code}
	}()
	return r
}

func (r *serviceTestRun) wait(t *testing.T) serviceTestResult {
	t.Helper()
	select {
	case res := <-r.result:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return")
		return serviceTestResult{}
	}
}

func (r *serviceTestRun) states() []svc.State {
	var out []svc.State
	for {
		select {
		case s := <-r.status:
			out = append(out, s.State)
		default:
			return out
		}
	}
}

func serviceTestHandler(run func(context.Context) error) *serviceHandler {
	return &serviceHandler{
		base:       context.Background(),
		run:        run,
		stopWait:   time.Second,
		startGrace: 50 * time.Millisecond,
	}
}

func TestServiceStopCancelsServeAndExitsCleanly(t *testing.T) {
	cancelled := make(chan struct{})
	r := serviceStart(t, serviceTestHandler(func(ctx context.Context) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}))
	// Running is reported once the grace passes; a Stop before that is not delivered by the SCM.
	time.Sleep(200 * time.Millisecond)
	r.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	res := r.wait(t)
	if res.specific || res.code != 0 {
		t.Fatalf("a requested stop must exit cleanly, got specific=%v code=%d", res.specific, res.code)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("serve's context was not cancelled")
	}
	if got, want := r.states(), []svc.State{svc.StartPending, svc.Running, svc.StopPending}; !slices.Equal(got, want) {
		t.Fatalf("reported states = %v, want %v", got, want)
	}
}

func TestServiceShutdownIsAStop(t *testing.T) {
	r := serviceStart(t, serviceTestHandler(func(ctx context.Context) error { <-ctx.Done(); return nil }))
	time.Sleep(200 * time.Millisecond)
	r.requests <- svc.ChangeRequest{Cmd: svc.Shutdown}
	if res := r.wait(t); res.specific || res.code != 0 {
		t.Fatalf("shutdown must exit cleanly, got %+v", res)
	}
}

// A refused configuration must fail the start: the service is never reported Running.
func TestServiceRefusedStartFailsBeforeRunning(t *testing.T) {
	r := serviceStart(t, serviceTestHandler(func(context.Context) error { return errors.New("refused") }))
	res := r.wait(t)
	if !res.specific || res.code != serviceExitFailed {
		t.Fatalf("a refused start must exit with the failure code, got %+v", res)
	}
	if slices.Contains(r.states(), svc.Running) {
		t.Fatal("a refused start was reported as Running")
	}
}

// serve returning by itself, even without an error, is a failure: the SCM restarts the service.
func TestServiceUnexpectedExitIsAFailure(t *testing.T) {
	for _, err := range []error{nil, errors.New("boom")} {
		release := make(chan struct{})
		r := serviceStart(t, serviceTestHandler(func(context.Context) error { <-release; return err }))
		time.Sleep(200 * time.Millisecond)
		close(release)
		res := r.wait(t)
		if !res.specific || res.code != serviceExitFailed {
			t.Fatalf("serve returning %v after Running = %+v, want the failure code", err, res)
		}
	}
}

func TestServiceStopTimeoutIsAFailure(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := serviceTestHandler(func(context.Context) error { <-block; return nil })
	h.stopWait = 100 * time.Millisecond
	r := serviceStart(t, h)
	time.Sleep(200 * time.Millisecond)
	r.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	res := r.wait(t)
	if !res.specific || res.code != serviceExitStopTimeout {
		t.Fatalf("a serve that ignores the stop = %+v, want the stop-timeout code", res)
	}
}

func TestServiceInterrogateEchoesTheCurrentStatus(t *testing.T) {
	r := serviceStart(t, serviceTestHandler(func(ctx context.Context) error { <-ctx.Done(); return nil }))
	time.Sleep(200 * time.Millisecond)
	want := svc.Status{State: svc.Running, Accepts: svc.AcceptStop}
	r.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: want}
	r.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	r.wait(t)
	var echoed bool
	for len(r.status) > 0 {
		if s := <-r.status; s == want {
			echoed = true
		}
	}
	if !echoed {
		t.Fatal("Interrogate was not answered with the current status")
	}
}

// SEC-080: the Windows service runs as its own virtual account or LocalService, never LocalSystem or a named user.
func TestServiceAccountsAreLeastPrivilege(t *testing.T) {
	cases := []struct {
		account string
		ok      bool
	}{
		{`NT SERVICE\AlixAI`, true},
		{`nt service\alixai`, true},
		{`NT AUTHORITY\LocalService`, true},
		{`LocalSystem`, false},
		{`NT AUTHORITY\SYSTEM`, false},
		{`NT AUTHORITY\NetworkService`, false},
		{`NT SERVICE\SomethingElse`, false},
		{`.\Administrator`, false},
		{`BUILTIN\Administrators`, false},
		{``, false},
	}
	for _, c := range cases {
		err := serviceCheckAccount("AlixAI", c.account)
		if (err == nil) != c.ok {
			t.Errorf("account %q: err = %v, want ok=%v", c.account, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrUsage) {
			t.Errorf("account %q: a refusal must be a usage error, got %v", c.account, err)
		}
	}
}

func TestServiceNameAndConfigPathAreChecked(t *testing.T) {
	for name, ok := range map[string]bool{"AlixAI": true, "alixai-2": true, "": false, `a\b`: false, "1abc": false, "a b": false, strings.Repeat("a", 81): false} {
		if err := serviceCheckName(name); (err == nil) != ok {
			t.Errorf("name %q: err = %v, want ok=%v", name, err, ok)
		}
	}
	abs := filepath.Join(t.TempDir(), "alixai.yaml")
	for path, ok := range map[string]bool{abs: true, "": false, "alixai.yaml": false, `..\alixai.yaml`: false} {
		if err := serviceCheckConfigPath(path); (err == nil) != ok {
			t.Errorf("config %q: err = %v, want ok=%v", path, err, ok)
		}
	}
}

func TestServiceLogLinesBecomeEventsOfTheirLevel(t *testing.T) {
	var got []string
	w := serviceEventLog{log: &serviceFakeEventLog{record: func(kind, msg string) { got = append(got, kind+":"+msg) }}}
	long := strings.Repeat("x", serviceEventMax+50)
	in := `{"level":"INFO","msg":"up"}` + "\n" + `{"level":"WARN","msg":"w"}` + "\n\n" + `{"level":"ERROR","msg":"e"}` + "\n" + long + "\n"
	n, err := w.Write([]byte(in))
	if err != nil || n != len(in) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	want := []string{`info:{"level":"INFO","msg":"up"}`, `warning:{"level":"WARN","msg":"w"}`, `error:{"level":"ERROR","msg":"e"}`, "info:" + long[:serviceEventMax]}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

type serviceFakeEventLog struct{ record func(kind, msg string) }

func (f *serviceFakeEventLog) Info(_ uint32, m string) error    { f.record("info", m); return nil }
func (f *serviceFakeEventLog) Warning(_ uint32, m string) error { f.record("warning", m); return nil }
func (f *serviceFakeEventLog) Error(_ uint32, m string) error   { f.record("error", m); return nil }
func (f *serviceFakeEventLog) Close() error                     { return nil }

func serviceRunCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Default.Run(context.Background(), args, &Env{Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader("")})
	return code, out.String() + errOut.String()
}

// `service run` from a console must refuse, never start a server of its own.
func TestServiceRunRefusesAConsoleAndARelativeConfig(t *testing.T) {
	serves := 0
	oldIs, oldServe := serviceIsWindowsService, serviceServe
	t.Cleanup(func() { serviceIsWindowsService, serviceServe = oldIs, oldServe })
	serviceIsWindowsService = func() (bool, error) { return false, nil }
	serviceServe = func(context.Context, *Env, []string) error { serves++; return nil }

	cfg := filepath.Join(t.TempDir(), "alixai.yaml")
	if code, out := serviceRunCLI(t, "service", "run", "-config", cfg); code != ExitError || !strings.Contains(out, "alixai serve") {
		t.Fatalf("from a console: code=%d out=%q, want ExitError pointing at `alixai serve`", code, out)
	}
	if code, _ := serviceRunCLI(t, "service", "run", "-config", "alixai.yaml"); code != ExitUsage {
		t.Fatalf("relative config: code=%d, want ExitUsage", code)
	}
	if code, _ := serviceRunCLI(t, "service", "run"); code != ExitUsage {
		t.Fatalf("no config: code=%d, want ExitUsage", code)
	}
	if serves != 0 {
		t.Fatalf("serve ran %d times from a refused `service run`", serves)
	}
}

// Under the SCM, `service run` hands serve the config path and the event-log writer, and the SCM's handler runs it.
func TestServiceRunWiresServeToTheHandler(t *testing.T) {
	oldIs, oldRun, oldServe, oldLog := serviceIsWindowsService, serviceSCMRun, serviceServe, serviceOpenLog
	t.Cleanup(func() {
		serviceIsWindowsService, serviceSCMRun, serviceServe, serviceOpenLog = oldIs, oldRun, oldServe, oldLog
	})
	serviceIsWindowsService = func() (bool, error) { return true, nil }
	var logged bytes.Buffer
	serviceOpenLog = func(name string) (io.WriteCloser, error) {
		if name != "TestSvc" {
			t.Errorf("event source = %q", name)
		}
		return serviceNopWriteCloser{&logged}, nil
	}
	var gotArgs []string
	serviceServe = func(_ context.Context, env *Env, args []string) error {
		gotArgs = args
		env.Stderr.Write([]byte("hello\n"))
		return errors.New("refused")
	}
	var h svc.Handler
	serviceSCMRun = func(name string, handler svc.Handler) error {
		if name != "TestSvc" {
			t.Errorf("service name = %q", name)
		}
		h = handler
		specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 8))
		if !specific || code != serviceExitFailed {
			t.Errorf("Execute = %v, %d", specific, code)
		}
		return nil
	}
	cfg := filepath.Join(t.TempDir(), "alixai.yaml")
	if code, out := serviceRunCLI(t, "service", "run", "-name", "TestSvc", "-config", cfg); code != ExitOK {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if h == nil || !slices.Equal(gotArgs, []string{"-config", cfg}) {
		t.Fatalf("serve args = %v", gotArgs)
	}
	if s := logged.String(); !strings.Contains(s, "hello") || !strings.Contains(s, "serve stopped: refused") {
		t.Fatalf("event log got %q", s)
	}
}

// Installing and removing change the host, so the tests stop at the checks that run before the SCM is touched.
func TestServiceInstallRefusesBeforeTouchingTheSCM(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "missing.yaml")
	cases := [][]string{
		{"service", "install"},
		{"service", "install", "-config", "alixai.yaml"},
		{"service", "install", "-config", cfg, "-account", "LocalSystem"},
		{"service", "install", "-config", cfg, "-name", `bad\name`},
	}
	for _, args := range cases {
		if code, out := serviceRunCLI(t, args...); code != ExitUsage {
			t.Errorf("%v: code=%d out=%q, want ExitUsage", args, code, out)
		}
	}
	// A valid request for a config that is not there stops at the file check, still before the SCM.
	if code, out := serviceRunCLI(t, "service", "install", "-config", cfg); code != ExitError || !strings.Contains(out, "not a readable file") {
		t.Errorf("missing config: code=%d out=%q", code, out)
	}
}

func TestServiceCommandsAreRegistered(t *testing.T) {
	names := Default.Names()
	for _, want := range []string{"service install", "service run", "service uninstall"} {
		if !slices.Contains(names, want) {
			t.Errorf("command %q is not registered", want)
		}
	}
}

type serviceNopWriteCloser struct{ io.Writer }

func (serviceNopWriteCloser) Close() error { return nil }
