package parse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// hostLimits are the process-level limits the host imposes from outside.
type hostLimits struct {
	memory int64
	cpu    time.Duration
}

// confinement is what the host holds for one worker process: the limits it
// imposes from outside and the means to end the process. One implementation per
// operating system (conf_windows.go, conf_unix.go).
type confinement interface {
	// attach puts the started process under the host-side limits. It is called
	// before the worker is sent anything.
	attach(p *os.Process) error
	// kill ends the process and anything it started. It is safe to call twice.
	kill()
	// memory is the worker's peak memory so far and what the number counts.
	memory() (peak, workingSet int64, kind string)
	// finish is called once the process has exited.
	finish(ps *os.ProcessState)
	// memoryExceeded says whether the confinement saw the memory budget reached.
	memoryExceeded() bool
	// mechanisms names the host-side mechanisms, for the sandbox report.
	mechanisms() []string
	close()
}

// Runner starts a sandboxed worker for each document. It is safe for
// concurrent use; Config.MaxConcurrent bounds how many workers run at once.
type Runner struct {
	cfg  Config
	log  *slog.Logger
	clk  clock.Clock
	sem  chan struct{}
	dir  string
	once sync.Once

	spawned atomic.Int64

	// selfGuard makes the worker watch its own memory. On unless a test turns it
	// off to show that the OS enforcement works without it.
	selfGuard bool
	// onStart, when set (tests), is called with each worker's process id.
	onStart func(pid int)
	// args are extra arguments for the worker, which the production worker takes
	// none of: tests use them to run a hostile stand-in worker.
	args []string
}

// Option configures a Runner.
type Option func(*Runner)

// WithLogger sets the logger; the default discards everything.
func WithLogger(l *slog.Logger) Option { return func(r *Runner) { r.log = l } }

// WithClock sets the clock used for the timings in Stats. Wall-time limits use
// real timers whatever the clock.
func WithClock(c clock.Clock) Option { return func(r *Runner) { r.clk = c } }

// NewRunner validates the configuration and the worker executable and returns
// a Runner. It creates one empty directory, owner-only, that every worker uses
// as its working directory, so a relative path in a hostile document's
// processing resolves to nothing. Close removes it.
func NewRunner(cfg Config, opts ...Option) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := os.Stat(cfg.WorkerPath)
	if err != nil || !st.Mode().IsRegular() {
		return nil, contracts.Newf(contracts.ErrUnavailable, "parse: worker %q is not an executable file", cfg.WorkerPath)
	}
	dir, err := os.MkdirTemp("", "alixai-parse-")
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "parse: cannot create the worker directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil && runtime.GOOS != "windows" {
		os.RemoveAll(dir)
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "parse: cannot restrict the worker directory")
	}
	r := &Runner{
		cfg: cfg, dir: dir, selfGuard: true,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clk: clock.Real{},
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Close removes the worker directory. In-flight parses are not interrupted.
func (r *Runner) Close() error {
	var err error
	r.once.Do(func() { err = os.RemoveAll(r.dir) })
	return err
}

// Spawned is the number of worker processes started so far. A document refused
// before parsing (too large, unsupported) never starts one.
func (r *Runner) Spawned() int64 { return r.spawned.Load() }

// DefaultWorkerPath is where the worker is expected: next to the running
// executable, as alixai-parser (alixai-parser.exe on Windows). It returns an
// error if the running executable cannot be located.
func DefaultWorkerPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	name := "alixai-parser"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(exe), name), nil
}

// Parse extracts text and structure from doc, which the caller has already
// authorised and identified as the given format. It never panics and never
// returns a partial result: the result is complete and validated, or the error
// says why not. A document that crashes, hangs or exhausts memory costs one
// disposable process and one error; the caller and every other parse go on.
//
// Errors carry a stable contracts code and a "reason" detail:
//
//	validation_failed  the document is refused, deterministically: input_too_large,
//	                   output_too_large, malformed, encrypted, archive_bomb,
//	                   nesting_too_deep, too_many_pages, format_mismatch,
//	                   unsupported, needs_ocr (OCR is off and no page has text)
//	timeout            wall_timeout, cpu_time or startup_timeout
//	resource_exhausted memory_limit
//	unavailable        sandbox_unavailable: the confinement could not be installed
//	                   or verified; nothing was parsed
//	internal           worker_crashed, parser_internal, protocol
func (r *Runner) Parse(ctx context.Context, format wire.Format, doc []byte) (*Result, error) {
	if !format.Valid() {
		return nil, docError(wire.ReasonUnsupported, "format %q is not supported", format)
	}
	res, err := r.parse(ctx, string(format), doc)
	if err != nil {
		return nil, err
	}
	if len(res.NeedsOCR) > 0 && !hasText(res) {
		// A scanned document has no text layer. OCR is off by default, so this is
		// an error rather than an empty success that would index nothing.
		return nil, docError(wire.ReasonNeedsOCR, "the document has no text layer and OCR is not enabled").WithDetails("pages", len(res.NeedsOCR))
	}
	return res, nil
}

func hasText(r *Result) bool {
	for _, b := range r.Blocks {
		if !b.Hidden && strings.TrimSpace(r.Text[b.Start:b.End]) != "" {
			return true
		}
	}
	return false
}

// parse is Parse without the format check, so tests can drive the worker's
// fault-injection formats.
func (r *Runner) parse(ctx context.Context, format string, doc []byte) (*Result, error) {
	if int64(len(doc)) > r.cfg.MaxInputBytes {
		return nil, docError(wire.ReasonInputTooLarge, "the document is %d bytes, over the limit of %d", len(doc), r.cfg.MaxInputBytes)
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	begin := r.clk.Now()
	w, err := r.start(ctx)
	if err != nil {
		return nil, err
	}
	defer w.finish()
	spawn := r.clk.Now().Sub(begin)

	res, err := w.run(ctx, format, doc)
	total := r.clk.Now().Sub(begin)
	if err != nil {
		w.finish()
		return nil, w.classify(ctx, err)
	}
	res.Sandbox = w.report
	res.Stats.Spawn, res.Stats.Total = spawn, total
	// Sample while the process is alive (Windows reads the working set from the
	// live process), let it exit, and take the final peak (Unix reads the peak
	// resident set from the exit status).
	w.conf.memory()
	w.reap()
	res.Stats.PeakMemoryBytes, res.Stats.PeakWorkingSetBytes, res.Stats.PeakKind = w.conf.memory()
	return res, nil
}

// Probe starts a worker, reads its sandbox report and ends it, without parsing
// anything. It is the readiness check: it says what confinement a real parse
// would get here, and fails if the sandbox cannot be installed at all.
func (r *Runner) Probe(ctx context.Context) (SandboxReport, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return SandboxReport{}, ctx.Err()
	}
	w, err := r.start(ctx)
	if err != nil {
		return SandboxReport{}, err
	}
	defer w.finish()
	return w.report, nil
}

// ---- one worker process ----

type worker struct {
	r      *Runner
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *tailBuffer
	conf   confinement
	report SandboxReport

	killedBy atomic.Pointer[string]
	timer    *time.Timer
	stopCtx  func() bool
	doneOnce sync.Once
	ps       *os.ProcessState
	waitErr  error
	started  time.Time
}

// kill ends the process and records why, unless something already did.
func (w *worker) kill(reason string) {
	w.killedBy.CompareAndSwap(nil, &reason)
	w.conf.kill()
}

func (w *worker) killedReason() string {
	if p := w.killedBy.Load(); p != nil {
		return *p
	}
	return ""
}

// start launches the worker, confines it, sends Init, and verifies the Hello.
func (r *Runner) start(ctx context.Context) (*worker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lim := hostLimits{memory: r.cfg.MemoryBytes, cpu: r.cfg.cpuTime()}
	cmd := exec.Command(r.cfg.WorkerPath, r.args...)
	cmd.Dir = r.dir
	cmd.Env = []string{} // empty: no inherited secret, no PATH, nothing
	cmd.WaitDelay = 5 * time.Second
	conf, err := newConfinement(cmd, lim)
	if err != nil {
		return nil, sandboxUnavailable("cannot prepare the sandbox: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		conf.close()
		return nil, sandboxUnavailable("cannot create a pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		conf.close()
		return nil, sandboxUnavailable("cannot create a pipe: %v", err)
	}
	w := &worker{r: r, cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 64<<10), stderr: &tailBuffer{max: 4096}, conf: conf}
	cmd.Stderr = w.stderr
	r.spawned.Add(1)
	if err := cmd.Start(); err != nil {
		conf.close()
		return nil, sandboxUnavailable("cannot start the worker: %v", err)
	}
	w.started = r.clk.Now()
	if r.onStart != nil {
		r.onStart(cmd.Process.Pid)
	}
	// Anything that goes wrong from here ends the process.
	w.stopCtx = context.AfterFunc(ctx, func() { w.kill("canceled") })
	w.timer = time.AfterFunc(r.cfg.StartupTimeout, func() { w.kill("startup_timeout") })
	if err := conf.attach(cmd.Process); err != nil {
		w.kill("attach")
		w.finish()
		return nil, sandboxUnavailable("cannot confine the worker: %v", err)
	}

	init := wire.Init{
		Protocol:    wire.ProtocolVersion,
		MemoryBytes: lim.memory,
		CPUMillis:   lim.cpu.Milliseconds(),
		WallMillis:  r.cfg.WallTimeout.Milliseconds(),
		SelfGuard:   r.selfGuard,
		Threads:     1,
	}
	if err := writeFrameJSON(stdin, wire.FrameInit, init); err != nil {
		w.finish()
		return nil, w.startFailure(ctx, err)
	}
	t, payload, err := wire.ReadFrame(w.stdout, 1<<16)
	if err != nil || t != wire.FrameHello {
		w.finish()
		return nil, w.startFailure(ctx, err)
	}
	var hello wire.Hello
	if err := json.Unmarshal(payload, &hello); err != nil {
		w.kill("protocol")
		w.finish()
		return nil, sandboxUnavailable("the worker's report is not valid: %v", err)
	}
	report, err := verifyHello(hello, lim, conf.mechanisms())
	if err != nil {
		w.kill("sandbox")
		w.finish()
		return nil, err
	}
	w.report = report
	w.timer.Stop()
	return w, nil
}

// startFailure explains a worker that never completed the handshake.
func (w *worker) startFailure(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch w.killedReason() {
	case "startup_timeout":
		return contracts.Newf(contracts.ErrTimeout, "parse: the worker did not start within %s", w.r.cfg.StartupTimeout).WithDetails("reason", "startup_timeout")
	}
	if code := w.exitCode(); code == 6 {
		return sandboxUnavailable("the worker could not confine itself: %s", w.stderr.sanitised())
	}
	return sandboxUnavailable("the worker did not complete its start: %v (exit %d)", cause, w.exitCode())
}

// exitCode is the worker's exit code, or -1 if it has not exited.
func (w *worker) exitCode() int {
	w.reap()
	if w.ps == nil {
		return -1
	}
	return w.ps.ExitCode()
}

// reap waits for the process once.
func (w *worker) reap() {
	w.doneOnce.Do(func() {
		w.stdin.Close()
		done := make(chan struct{})
		go func() {
			w.waitErr = w.cmd.Wait()
			w.ps = w.cmd.ProcessState
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			w.conf.kill()
			<-done
		}
		w.conf.finish(w.ps)
	})
}

// finish ends the worker for good: the process is gone, the limits released.
func (w *worker) finish() {
	if w.timer != nil {
		w.timer.Stop()
	}
	if w.stopCtx != nil {
		w.stopCtx()
	}
	w.reap()
	w.conf.close()
}

// run sends the document and reads the result.
func (w *worker) run(ctx context.Context, format string, doc []byte) (*Result, error) {
	cfg := w.r.cfg
	w.timer = time.AfterFunc(cfg.WallTimeout, func() { w.kill("wall_timeout") })
	req := wire.Request{Protocol: wire.ProtocolVersion, Format: wire.Format(format), Size: int64(len(doc)), Limits: cfg.wireLimits()}
	if err := writeFrameJSON(w.stdin, wire.FrameRequest, req); err != nil {
		return nil, err
	}
	if err := wire.WriteFrame(w.stdin, wire.FrameBody, doc); err != nil {
		return nil, err
	}
	t, payload, err := wire.ReadFrame(w.stdout, wire.MaxJSONFrame)
	if err != nil {
		return nil, w.frameError(err)
	}
	switch t {
	case wire.FrameError:
		var f wire.Failure
		if err := json.Unmarshal(payload, &f); err != nil {
			w.kill("protocol")
			return nil, protocolError("bad failure frame: %v", err)
		}
		return nil, &workerFailure{f}
	case wire.FrameResult:
	default:
		w.kill("protocol")
		return nil, protocolError("unexpected %q frame instead of a result", t)
	}
	var wr wire.Result
	if err := json.Unmarshal(payload, &wr); err != nil {
		w.kill("protocol")
		return nil, protocolError("bad result frame: %v", err)
	}
	if wr.TextSize < 0 || wr.TextSize > cfg.MaxOutputBytes {
		w.kill("protocol")
		return nil, protocolError("the result declares %d bytes of text, over the limit of %d", wr.TextSize, cfg.MaxOutputBytes)
	}
	tt, n, err := wire.ReadHeader(w.stdout, cfg.MaxOutputBytes)
	if err != nil {
		return nil, w.frameError(err)
	}
	if tt != wire.FrameText || n != wr.TextSize {
		w.kill("protocol")
		return nil, protocolError("the text frame does not match the result")
	}
	text, err := wire.ReadPayload(w.stdout, n)
	if err != nil {
		return nil, w.frameError(err)
	}
	w.timer.Stop()
	res, err := newResult(wr, text, cfg.wireLimits())
	if err != nil {
		w.kill("protocol")
		return nil, err
	}
	return res, nil
}

// knownRefusals are the failure reasons a worker may report for a document.
var knownRefusals = map[string]bool{
	wire.ReasonMalformed: true, wire.ReasonUnsupported: true, wire.ReasonFormatMismatch: true,
	wire.ReasonEncrypted: true, wire.ReasonNeedsOCR: true, wire.ReasonArchiveBomb: true,
	wire.ReasonOutputTooLarge: true, wire.ReasonInputTooLarge: true, wire.ReasonNestingTooDeep: true,
	wire.ReasonTooManyPages: true,
}

// safeMessage limits a message from the worker to short printable ASCII: the
// worker is untrusted, and its message ends up in errors and logs.
func safeMessage(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 200 {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// frameError turns a read failure into what it means: a frame over its limit is
// a worker breaking the protocol (it is killed), anything else is a worker that
// stopped talking, which classify explains from how it ended.
func (w *worker) frameError(err error) error {
	if errors.Is(err, wire.ErrOversize) {
		w.kill("protocol")
		return protocolError("%v", err)
	}
	return err
}

// workerFailure is a Failure frame as an error, until it is classified.
type workerFailure struct{ f wire.Failure }

func (e *workerFailure) Error() string { return e.f.Reason + ": " + e.f.Message }

// classify turns whatever ended a parse into the typed error the caller sees,
// with the worker's peak memory in its details (numbers only: useful in audit,
// nothing from the document).
func (w *worker) classify(ctx context.Context, err error) error {
	out := w.classifyCause(ctx, err)
	var ce *contracts.Error
	if errors.As(out, &ce) {
		peak, _, kind := w.conf.memory()
		ce.WithDetails("peak_memory_bytes", peak).WithDetails("peak_kind", kind)
	}
	return out
}

func (w *worker) classifyCause(ctx context.Context, err error) error {
	w.reap()
	var wf *workerFailure
	if errors.As(err, &wf) {
		switch wf.f.Reason {
		case wire.ReasonInternal:
			return contracts.New(contracts.ErrInternal, "parse: the parser failed on this document").WithDetails("reason", "parser_internal")
		case wire.ReasonBadRequest:
			return contracts.New(contracts.ErrInternal, "parse: the worker refused the request: "+wf.f.Message).WithDetails("reason", "bad_request")
		}
		if !knownRefusals[wf.f.Reason] {
			return protocolError("the worker reported an unknown failure")
		}
		return docError(wf.f.Reason, "%s", safeMessage(wf.f.Message))
	}
	var ce *contracts.Error
	if errors.As(err, &ce) && ce.Details["reason"] == "protocol" {
		return ce
	}
	// The worker died or was killed. Host decisions first, then how it died.
	if cerr := ctx.Err(); cerr != nil && w.killedReason() == "canceled" {
		return cerr
	}
	code, signal := w.exitDetail()
	switch {
	case w.killedReason() == "wall_timeout" || code == 3:
		return contracts.Newf(contracts.ErrTimeout, "parse: the document took longer than %s and its parser was stopped", w.r.cfg.WallTimeout).WithDetails("reason", "wall_timeout")
	case w.killedReason() == "startup_timeout":
		return contracts.New(contracts.ErrTimeout, "parse: the worker did not respond in time").WithDetails("reason", "startup_timeout")
	case w.conf.memoryExceeded() || code == 4 || mentionsMemory(w.stderr.String()):
		return contracts.Newf(contracts.ErrResourceExhausted, "parse: the document needed more than %d bytes of memory and its parser was stopped", w.r.cfg.MemoryBytes).WithDetails("reason", "memory_limit")
	case w.cpuExhausted():
		return contracts.Newf(contracts.ErrTimeout, "parse: the document used more than %s of CPU time and its parser was stopped", w.r.cfg.cpuTime()).WithDetails("reason", "cpu_time")
	case signal == "killed" && w.killedReason() == "":
		return contracts.New(contracts.ErrResourceExhausted, "parse: the operating system stopped the parser (a resource limit)").WithDetails("reason", "killed_by_os")
	}
	e := contracts.New(contracts.ErrInternal, "parse: the parser worker crashed on this document").WithDetails("reason", "worker_crashed")
	e.WithDetails("exit_code", code)
	if signal != "" {
		e.WithDetails("signal", signal)
	}
	w.r.log.Warn("parser worker crashed", "exit_code", code, "signal", signal, "stderr_bytes", w.stderr.n)
	return e
}

// cpuExhausted reports whether the process ended having used most of the CPU
// time it was allowed, which is how the OS limit shows up: the process is
// killed and nothing else says why. The threshold is 80 %, not 100 %, because
// the kernel enforces RLIMIT_CPU on its exact accounting while the rusage the
// host reads afterwards is tick-sampled: a worker stopped at the 2 s limit was
// measured at 1.83 s (92 %) on Linux. A kill that is none of the host's, not
// a memory kill, after more than 80 % of the budget is far more likely the
// CPU limit than anything else, and the error says "the operating system
// stopped the parser" in the remaining cases (killed_by_os).
func (w *worker) cpuExhausted() bool {
	if w.ps == nil {
		return false
	}
	used := w.ps.UserTime() + w.ps.SystemTime()
	limit := w.r.cfg.cpuTime()
	return !w.ps.Success() && used >= limit/100*80
}

// exitDetail is the exit code and, on Unix, the signal that ended the process.
func (w *worker) exitDetail() (code int, signal string) {
	if w.ps == nil {
		return -1, ""
	}
	return w.ps.ExitCode(), signalName(w.ps)
}

func mentionsMemory(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "out of memory") || strings.Contains(s, "cannot allocate memory") ||
		strings.Contains(s, "failed to reserve") || strings.Contains(s, "cannot reserve")
}

// ---- helpers ----

func writeFrameJSON(w io.Writer, t wire.FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return wire.WriteFrame(w, t, b)
}

func docError(reason, format string, args ...any) *contracts.Error {
	return contracts.Newf(contracts.ErrValidationFailed, "parse: "+format, args...).WithDetails("reason", reason)
}

func sandboxUnavailable(format string, args ...any) error {
	return contracts.Newf(contracts.ErrUnavailable, "parse: the sandbox is unavailable, nothing was parsed: "+format, args...).WithDetails("reason", "sandbox_unavailable")
}

// tailBuffer keeps the first max bytes written to it and counts the rest. The
// worker's stderr is for diagnosing a crash; it is never put in an error
// message or a log line, because a crash message can echo document content.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
	n   int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n += len(p)
	if room := t.max - len(t.buf); room > 0 {
		t.buf = append(t.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// sanitised is stderr limited to printable ASCII on one line, for the worker's
// own confinement failure message (which carries no document content: the
// worker had not read any).
func (t *tailBuffer) sanitised() string {
	s := t.String()
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// verifyHello checks the worker's first frame against what the host expects:
// the same protocol, the same parser version this build of the core expects, the
// same platform, every mandatory mechanism applied and, where the host can check
// it, the limits the worker read back. A worker that does not pass is never sent
// a document.
func verifyHello(h wire.Hello, lim hostLimits, hostMech []string) (SandboxReport, error) {
	bad := func(format string, args ...any) (SandboxReport, error) {
		return SandboxReport{}, sandboxUnavailable("the worker's report was refused: "+format, args...)
	}
	if h.Protocol != wire.ProtocolVersion {
		return bad("protocol %d, expected %d", h.Protocol, wire.ProtocolVersion)
	}
	if want := formats.Component(); h.Component != want {
		return bad("the worker is %s %s, this build expects %s (a stale alixai-parser next to a newer service?)", h.Component.Name, h.Component.Version, want.Version)
	}
	if h.GoOS != runtime.GOOS || h.GoArch != runtime.GOARCH {
		return bad("the worker is for %s/%s, not %s/%s", h.GoOS, h.GoArch, runtime.GOOS, runtime.GOARCH)
	}
	req := wire.RequiredMechanisms(runtime.GOOS)
	if len(req) == 0 {
		return bad("this operating system has no supported sandbox")
	}
	for _, m := range req {
		if !contains(h.Sandbox.Applied, m) {
			return bad("mandatory mechanism %s is not applied", m)
		}
	}
	if err := checkObserved(h.Sandbox.Observed, lim); err != nil {
		return bad("%v", err)
	}
	rep := SandboxReport{
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Host:     hostMech,
		Worker:   h.Sandbox.Applied,
		Missing:  h.Sandbox.Missing,
	}
	rep.Level = levelFor(runtime.GOOS, h.Sandbox.Applied)
	return rep, nil
}

// checkObserved compares limits the worker read back from the OS with the
// limits the host set. A mismatch means the OS applied something else than
// intended.
func checkObserved(obs map[string]int64, lim hostLimits) error {
	switch runtime.GOOS {
	case "windows":
		if got := obs["job_process_memory_bytes"]; got != lim.memory {
			return fmt.Errorf("job memory limit %d, expected %d", got, lim.memory)
		}
		if got := obs["job_active_process_limit"]; got != 1 {
			return fmt.Errorf("job process limit %d, expected 1", got)
		}
	case "linux", "darwin":
		cpu := int64((lim.cpu + time.Second - 1) / time.Second)
		if cpu < 1 {
			cpu = 1
		}
		if got := obs[wire.RlimitCPU]; got != cpu {
			return fmt.Errorf("CPU limit %d s, expected %d s", got, cpu)
		}
		if got, ok := obs[wire.RlimitFsize]; !ok || got != 0 {
			return fmt.Errorf("file size limit %d, expected 0", got)
		}
		if runtime.GOOS == "linux" {
			// The limit is relative to the address space the worker already held;
			// the host cannot know that number, so it checks the arithmetic and that
			// the baseline is plausible (a Go process reserves well under 8 GiB).
			base := obs[wire.VMBaseline]
			if base <= 0 || base > 8<<30 {
				return fmt.Errorf("address-space baseline %d is not plausible", base)
			}
			if got := obs[wire.RlimitAS]; got != base+lim.memory+wire.ASSlack {
				return fmt.Errorf("address-space limit %d, expected %d (baseline %d, budget %d, slack %d)", got, base+lim.memory+wire.ASSlack, base, lim.memory, wire.ASSlack)
			}
		}
	}
	return nil
}

// levelFor is the sandbox level a worker's applied mechanisms amount to.
func levelFor(goos string, applied []string) string {
	if goos == "linux" && contains(applied, wire.LandlockFS) && contains(applied, wire.SeccompNet) && contains(applied, wire.SeccompExec) {
		return LevelL2
	}
	return LevelL1
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
