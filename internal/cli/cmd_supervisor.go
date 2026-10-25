package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/supervisor"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// serve runs the LocalAI supervisor (master plan 18: "LocalAI crash: supervisor restarts it with backoff; in-flight
// action remains uncommitted unless receipt proves otherwise"; M5-07 built the loop, this file puts it in the process).
//
// # Two places, never one
//
// The hardened native unit of AlixAI (deploy/native, M5-02, SEC-080) has an empty capability set, NoNewPrivileges and
// DynamicUser: it cannot start LocalAI under another identity and must not try. So the work is split in two:
//
//   - `alixai serve` (supervisor.runtime: unit, the default) ATTACHES to a LocalAI unit the service manager keeps
//     alive (deploy/native/systemd/localai.service). It starts nothing and executes nothing: it health-checks the
//     runtime through the LocalAI adapter, backs off while it is unhealthy, gives up after the limit, and settles the
//     in-flight actions at every end of an instance. supervisor.runtime: exec is refused in serve.
//   - `alixai supervisor exec` is the command the LocalAI unit runs. It spawns the configured absolute LocalAI binary
//     (no shell, an explicit environment, own identity, limits) and keeps it alive with the same loop. It never runs
//     inside the AlixAI unit: the unit's ExecStart is `serve`, a test pins that, and the exec runtime refuses root.
//
// # What the wrapped runtime is
//
// Supervisor implements only providers.AIRuntimeProvider (Name, Health). supervisorRuntime is the value serve
// registers for the controller role in place of the bare adapter: Chat, ChatStream and Embed are the adapter's, Name
// and Health are the supervisor's, so "starting", "backing off" and "gave up" reach every role as unavailable, and
// every model call is gated by Admit, which fails closed unless the runtime is running. The adapter's endpoint
// allowlist is neither widened nor bypassed: the supervisor probes through the adapter's own Health.
//
// # In-flight actions
//
// supervisorAdapters wraps the adapter source the action coordinator dispatches through, so every Commit, from the
// public API and from a dialogue turn alike, is a Begin and End on the ledger. A commit does not need the model, so
// it is never refused while the runtime is down; the ledger only makes sure that an action in flight when LocalAI
// dies is settled before any restart: committed only when the coordinator's own receipt for that preparation says
// committed, queued or partial, otherwise uncommitted and flagged for reconciliation. The scope of a commit is kept
// next to its preparation id, in memory, because the coordinator reads a receipt only for the session that owns it.
// The ledger is in memory by design: a crash of AlixAI itself is covered by actions.RecoverAtStartup.

const (
	supervisorSectionName = "supervisor"
	// supervisorRuntimeUnit and supervisorRuntimeExec are the values of supervisor.runtime.
	supervisorRuntimeUnit = "unit"
	supervisorRuntimeExec = "exec"
	// supervisorCheckName is the readiness check that says why the runtime is not running.
	supervisorCheckName = "runtime_supervisor"
	// supervisorMaxKeyBytes bounds a LocalAI key file, as the adapter does.
	supervisorMaxKeyBytes = 4096
)

func init() {
	config.Register(supervisorSectionName, &supervisorSection)
	Register(Command{
		Name:    "supervisor exec",
		Summary: "run LocalAI under the supervisor: the command of the separate LocalAI unit, never of the AlixAI service",
		Run:     runSupervisorExec,
	})
	registerServeProvider("supervisor", supervisorProvide)
}

// supervisorConfig is the "supervisor" section.
//
//	supervisor:
//	  enabled: true            # off by default: container installs are kept alive by the orchestrator
//	  runtime: unit            # unit: attach to a LocalAI unit (serve) | exec: spawn LocalAI (`supervisor exec` only)
//	  max_restarts: 5          # the loop's own tuning (internal/supervisor.Config), all optional
//	  backoff: {initial: 1s, max: 30s}
//	  exec:                    # read by `alixai supervisor exec` only
//	    path: /opt/localai/local-ai
//	    args: [run]
//	    env: [LOCALAI_ADDRESS=127.0.0.1:8080]
//	    api_key_env: LOCALAI_API_KEY   # the key of localai.api_key_ref (file:) reaches the child under this name
type supervisorConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Runtime           string `yaml:"runtime"`
	supervisor.Config `yaml:",inline"`
	Exec              supervisorExecConfig `yaml:"exec"`
}

// supervisorExecConfig describes the process `supervisor exec` spawns. Nothing secret lives here.
type supervisorExecConfig struct {
	// Path is the absolute path of the LocalAI binary; it is never searched for on PATH.
	Path string `yaml:"path"`
	// Args are passed as they are, with no shell.
	Args []string `yaml:"args"`
	// Dir is the working directory; empty means the supervisor's.
	Dir string `yaml:"dir"`
	// Env is the child's whole environment as NAME=value entries; nothing is inherited.
	Env []string `yaml:"env"`
	// APIKeyEnv names the environment variable that carries the LocalAI key to the child. The key is read from
	// the file localai.api_key_ref names (file: only) and is never configuration.
	APIKeyEnv string `yaml:"api_key_env"`
	// MaxMemoryBytes and MaxOpenFiles are the supervisor's own limits (see supervisor.Limits); a limit the
	// platform cannot enforce refuses the start. The service manager's limits (MemoryMax) apply as well.
	MaxMemoryBytes uint64 `yaml:"max_memory_bytes"`
	MaxOpenFiles   uint64 `yaml:"max_open_files"`
	// StopGrace is how long a polite stop waits before the process is killed; zero is the default.
	StopGrace time.Duration `yaml:"stop_grace"`
}

var supervisorSection = supervisorConfig{Runtime: supervisorRuntimeUnit}

// supervisorIsAbs accepts a path that is absolute on either family of platforms: a configuration written for the Linux unit
// must still pass `config check` on a Windows workstation. The exec runtime itself enforces the host's own rule when it starts.
func supervisorIsAbs(p string) bool { return path.IsAbs(p) || filepath.IsAbs(p) }

var supervisorEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate implements the section validation hook.
func (c supervisorConfig) Validate() error {
	switch c.Runtime {
	case supervisorRuntimeUnit, supervisorRuntimeExec:
	default:
		return fmt.Errorf("runtime %q is not %q or %q", c.Runtime, supervisorRuntimeUnit, supervisorRuntimeExec)
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	e := c.Exec
	if e.APIKeyEnv != "" && !supervisorEnvNameRE.MatchString(e.APIKeyEnv) {
		return errors.New("exec.api_key_env is not an environment variable name")
	}
	for _, kv := range e.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return errors.New("exec.env entries must be NAME=value")
		}
		if name == e.APIKeyEnv {
			return errors.New("exec.env must not set the variable exec.api_key_env names")
		}
	}
	if c.Enabled && c.Runtime == supervisorRuntimeExec {
		if !supervisorIsAbs(e.Path) {
			return errors.New("exec.path must be an absolute path")
		}
		if e.Dir != "" && !supervisorIsAbs(e.Dir) {
			return errors.New("exec.dir must be an absolute path")
		}
	}
	return nil
}

// ---- the wrapped runtime -------------------------------------------------------------------------------------------

// supervisorRuntime is the LocalAI adapter under the supervisor: the adapter's text and embedding roles, the
// supervisor's Name and Health, and Admit in front of every model call.
type supervisorRuntime struct {
	sup     *supervisor.Supervisor
	gen     providers.TextGenerator
	emb     providers.Embedder
	tracker *supervisorTracker
}

var (
	_ providers.AIRuntimeProvider = (*supervisorRuntime)(nil)
	_ providers.TextGenerator     = (*supervisorRuntime)(nil)
	_ providers.Embedder          = (*supervisorRuntime)(nil)
)

// Name implements providers.AIRuntimeProvider; it is the supervised runtime's.
func (r *supervisorRuntime) Name() string { return r.sup.Name() }

// Health implements providers.AIRuntimeProvider; it is the supervisor's, which is the adapter's own report while the
// runtime is running and a fixed "unavailable" in every other state.
func (r *supervisorRuntime) Health(ctx context.Context) (providers.HealthReport, error) {
	return r.sup.Health(ctx)
}

// Chat implements providers.TextGenerator.
func (r *supervisorRuntime) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	if err := r.sup.Admit(); err != nil {
		return providers.ChatResult{}, err
	}
	return r.gen.Chat(ctx, scope, req)
}

// ChatStream implements providers.TextGenerator.
func (r *supervisorRuntime) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	if err := r.sup.Admit(); err != nil {
		return nil, err
	}
	return r.gen.ChatStream(ctx, scope, req)
}

// Embed implements providers.Embedder.
func (r *supervisorRuntime) Embed(ctx context.Context, scope contracts.Scope, req providers.EmbedRequest) (providers.EmbedResult, error) {
	if err := r.sup.Admit(); err != nil {
		return providers.EmbedResult{}, err
	}
	return r.emb.Embed(ctx, scope, req)
}

// check is the readiness condition: the runtime is running, or /readyz says what state it is in.
func (r *supervisorRuntime) check(context.Context) error {
	if st := r.sup.Status().State; st != supervisor.StateRunning {
		return contracts.Newf(contracts.ErrUnavailable, "the LocalAI supervisor reports the runtime is %s", string(st))
	}
	return nil
}

// start is what serve registers as a service: Run in its own goroutine, stopped (and waited for) when serve stops.
func (r *supervisorRuntime) start(log *slog.Logger) func(ctx context.Context) (stop func()) {
	return func(ctx context.Context) func() {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := r.sup.Run(runCtx); err != nil {
				log.Error("the LocalAI supervisor gave up: the model runtime stays unavailable until alixai is restarted",
					slog.String("err_code", string(contracts.CodeOf(err))))
			}
		}()
		return func() {
			cancel()
			<-done
		}
	}
}

// supervisorNewWith builds the wrapped runtime over any runtime and probe. serve passes the LocalAI adapter as probe,
// generator and embedder; a test passes fakes.
func supervisorNewWith(cfg supervisor.Config, rt supervisor.Runtime, probe providers.AIRuntimeProvider,
	gen providers.TextGenerator, emb providers.Embedder, log *slog.Logger, clk clock.Clock, extra ...supervisor.Option) (*supervisorRuntime, error) {
	tracker := newSupervisorTracker(log)
	ledger := supervisor.NewLedger(clk, tracker)
	tracker.ledger = ledger
	opts := append([]supervisor.Option{
		supervisor.WithClock(clk), supervisor.WithLedger(ledger), supervisor.WithLogger(log), supervisor.WithEvents(supervisorLogEvent(log)),
	}, extra...)
	sup, err := supervisor.New(cfg, rt, probe, opts...)
	if err != nil {
		return nil, err
	}
	return &supervisorRuntime{sup: sup, gen: gen, emb: emb, tracker: tracker}, nil
}

// supervisorLogEvent logs the supervision cycle with fixed fields; nothing the child printed is ever in an event.
func supervisorLogEvent(log *slog.Logger) func(supervisor.Event) {
	return func(e supervisor.Event) {
		attrs := []any{slog.String("event", string(e.Type)), slog.Int("attempt", e.Attempt)}
		if e.PID != 0 {
			attrs = append(attrs, slog.Int("pid", e.PID))
		}
		if e.Reason != "" {
			attrs = append(attrs, slog.String("reason", e.Reason))
		}
		if e.Detail != "" {
			attrs = append(attrs, slog.String("detail", e.Detail))
		}
		switch e.Type {
		case supervisor.EventExited, supervisor.EventGaveUp:
			log.Warn("localai supervisor", attrs...)
		case supervisor.EventBackoff:
			log.Warn("localai supervisor", append(attrs, slog.Duration("delay", e.Delay))...)
		case supervisor.EventSettled:
			// the supervisor logs each settlement itself
		default:
			log.Info("localai supervisor", attrs...)
		}
	}
}

// supervisorRegister registers the controller role: the bare adapter when the supervisor is off (every other path of
// serve is then byte for byte what it was), the wrapped runtime when it is on. Only the attach runtime is accepted
// here: `supervisor exec` never runs inside serve.
func supervisorRegister(reg *providers.Registry, log *slog.Logger, clk clock.Clock, client *localai.Client) error {
	cfg := supervisorSection
	if !cfg.Enabled {
		return reg.Register(providers.RoleController, client)
	}
	if cfg.Runtime != supervisorRuntimeUnit {
		return contracts.New(contracts.ErrValidationFailed,
			"supervisor.runtime exec runs in the LocalAI unit (alixai supervisor exec), never inside serve")
	}
	rt, err := supervisorNewWith(cfg.Config, &supervisor.Unit{}, client, client, client, log, clk)
	if err != nil {
		return err
	}
	return reg.Register(providers.RoleController, rt)
}

// supervisorRuntimeOf returns the wrapped runtime registered for the controller role, or nil when serve runs the bare
// adapter.
func supervisorRuntimeOf(reg *providers.Registry) *supervisorRuntime {
	if reg == nil {
		return nil
	}
	p, ok := reg.Provider(providers.RoleController)
	if !ok {
		return nil
	}
	rt, _ := p.(*supervisorRuntime)
	return rt
}

// supervisorProvide is serve's provider: with the supervisor on, its readiness check and its loop (a service of the
// process, started once everything is built and stopped before the store closes).
func supervisorProvide(_ context.Context, d serveDeps) error {
	rt := supervisorRuntimeOf(d.Providers)
	if rt == nil {
		return nil
	}
	if d.AddCheck != nil {
		d.AddCheck(gateway.Check{Name: supervisorCheckName, Fn: rt.check})
	}
	if d.AddService != nil {
		d.AddService(rt.start(d.Log))
	}
	return nil
}

// ---- in-flight actions ---------------------------------------------------------------------------------------------

// supervisorReceiptReader is the action store as the receipt source needs it: *actions.Coordinator.
type supervisorReceiptReader interface {
	Receipt(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.ActionReceipt, error)
}

var _ supervisorReceiptReader = (*actions.Coordinator)(nil)

// supervisorTracker is the ledger's receipt source over the actions store and the book of what is in flight. The
// coordinator reads a receipt only for the session that owns the preparation, so the scope of each commit is kept
// from its Begin to its End.
type supervisorTracker struct {
	log    *slog.Logger
	ledger *supervisor.Ledger

	mu     sync.Mutex
	scopes map[string]contracts.Scope
	reader supervisorReceiptReader
}

var _ supervisor.ReceiptSource = (*supervisorTracker)(nil)

func newSupervisorTracker(log *slog.Logger) *supervisorTracker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &supervisorTracker{log: log, scopes: map[string]contracts.Scope{}}
}

// bind gives the tracker the action store. Until then every lookup fails, which leaves an action uncommitted.
func (t *supervisorTracker) bind(r supervisorReceiptReader) {
	t.mu.Lock()
	t.reader = r
	t.mu.Unlock()
}

// begin records a commit in flight and returns what ends it. A commit that cannot be recorded (the same preparation
// is already tracked by a concurrent call, or the ledger is full) still goes ahead: the ledger only observes, it
// never grants or refuses a commit.
func (t *supervisorTracker) begin(scope contracts.Scope, preparationID string) (end func()) {
	if preparationID == "" {
		return func() {}
	}
	t.mu.Lock()
	if _, dup := t.scopes[preparationID]; dup {
		t.mu.Unlock()
		return func() {}
	}
	t.scopes[preparationID] = scope
	t.mu.Unlock()
	if err := t.ledger.Begin(preparationID); err != nil {
		t.mu.Lock()
		delete(t.scopes, preparationID)
		t.mu.Unlock()
		t.log.Warn("localai supervisor: a commit is not tracked", slog.String("err_code", string(contracts.CodeOf(err))))
		return func() {}
	}
	return func() {
		t.ledger.End(preparationID)
		t.mu.Lock()
		delete(t.scopes, preparationID)
		t.mu.Unlock()
	}
}

// Receipt implements supervisor.ReceiptSource. The receipt comes from the action store, never from anything the model
// said. No receipt yet (the coordinator has not written one) is "not found"; anything else is a failed lookup, which
// the ledger also settles as uncommitted.
func (t *supervisorTracker) Receipt(ctx context.Context, preparationID string) (contracts.ActionReceipt, bool, error) {
	t.mu.Lock()
	scope, tracked := t.scopes[preparationID]
	reader := t.reader
	t.mu.Unlock()
	if !tracked || reader == nil {
		return contracts.ActionReceipt{}, false, contracts.New(contracts.ErrUnavailable, "supervisor: the action store cannot be read for this preparation")
	}
	rc, err := reader.Receipt(ctx, scope, preparationID)
	switch {
	case err == nil:
		return rc, true, nil
	case contracts.HasCode(err, contracts.ErrConflict):
		return contracts.ActionReceipt{}, false, nil
	default:
		return contracts.ActionReceipt{}, false, err
	}
}

// supervisorAdapters wraps the adapter source the action coordinator dispatches through, when the supervisor is on:
// each Commit is a Begin and End on the ledger. With no supervisor it returns src unchanged.
func supervisorAdapters(reg *providers.Registry, src actions.AdapterSource) actions.AdapterSource {
	rt := supervisorRuntimeOf(reg)
	if rt == nil {
		return src
	}
	return supervisorAdapterSource{inner: src, tracker: rt.tracker}
}

// supervisorBindReceipts hands the action store to the receipt source, once the coordinator exists.
func supervisorBindReceipts(reg *providers.Registry, r supervisorReceiptReader) {
	if rt := supervisorRuntimeOf(reg); rt != nil {
		rt.tracker.bind(r)
	}
}

type supervisorAdapterSource struct {
	inner   actions.AdapterSource
	tracker *supervisorTracker
}

// Adapter implements actions.AdapterSource.
func (s supervisorAdapterSource) Adapter(ctx context.Context, applicationID string) (adapters.AppAdapter, error) {
	ad, err := s.inner.Adapter(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	return supervisorAdapter{AppAdapter: ad, tracker: s.tracker}, nil
}

// supervisorAdapter is an application adapter whose Commit is tracked as in flight. Every other call is the adapter's.
type supervisorAdapter struct {
	adapters.AppAdapter
	tracker *supervisorTracker
}

// Commit implements adapters.AppAdapter.
func (a supervisorAdapter) Commit(ctx context.Context, scope contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	end := a.tracker.begin(scope, string(req.Prepared.PreparationID))
	defer end()
	return a.AppAdapter.Commit(ctx, scope, req)
}

// ---- `alixai supervisor exec` --------------------------------------------------------------------------------------

// runSupervisorExec is the command of the LocalAI unit: it spawns LocalAI and keeps it alive until the unit stops.
// It returns an error when the supervisor gives up, so the service manager restarts the unit with a fresh budget.
func runSupervisorExec(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("supervisor exec")
	cfgPath := env.ConfigFlag(fs)
	if err := env.ParseFlags(fs, "alixai supervisor exec [-config file.yaml]", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := env.Config.LoadFrom(*cfgPath, env.LookupEnv); err != nil {
		return err
	}
	log := alog.NewWithWriter(env.Config.Core().Log, env.Stderr)
	return supervisorExecRun(ctx, log, clock.Real{}, supervisorSection, localaiSection)
}

// supervisorExecRun builds and runs the exec supervisor. The probe is the LocalAI adapter, so health is whatever its
// Health says and no LocalAI route is called here.
func supervisorExecRun(ctx context.Context, log *slog.Logger, clk clock.Clock, scfg supervisorConfig, lcfg localai.Config) error {
	if !scfg.Enabled || scfg.Runtime != supervisorRuntimeExec {
		return Usagef("supervisor exec needs supervisor.enabled: true and supervisor.runtime: exec in its configuration")
	}
	client, err := localai.New(lcfg, localai.WithLogger(log), localai.WithClock(clk))
	if err != nil {
		return fmt.Errorf("supervisor exec: %w", err)
	}
	ecfg, err := supervisorExecConfigFrom(scfg.Exec, lcfg.APIKeyRef)
	if err != nil {
		return fmt.Errorf("supervisor exec: %w", err)
	}
	rt, err := supervisor.NewExec(ecfg)
	if err != nil {
		return fmt.Errorf("supervisor exec: %w", err)
	}
	sup, err := supervisor.New(scfg.Config, rt, client, supervisor.WithClock(clk), supervisor.WithLogger(log), supervisor.WithEvents(supervisorLogEvent(log)))
	if err != nil {
		return fmt.Errorf("supervisor exec: %w", err)
	}
	log.Info("localai supervisor: starting the runtime", slog.String("runtime", supervisorRuntimeExec))
	return sup.Run(ctx)
}

// supervisorExecConfigFrom maps the section to the exec runtime's configuration. The child's environment is exactly
// the configured entries, plus the LocalAI key under api_key_env when one is named; its output is discarded (LocalAI
// can print prompts at debug level).
func supervisorExecConfigFrom(e supervisorExecConfig, apiKeyRef string) (supervisor.ExecConfig, error) {
	cfg := supervisor.ExecConfig{
		Path:      e.Path,
		Args:      append([]string(nil), e.Args...),
		Dir:       e.Dir,
		Env:       append([]string(nil), e.Env...),
		Limits:    supervisor.Limits{MaxMemoryBytes: e.MaxMemoryBytes, MaxOpenFiles: e.MaxOpenFiles},
		StopGrace: e.StopGrace,
	}
	if e.APIKeyEnv != "" {
		key, err := supervisorReadKey(apiKeyRef)
		if err != nil {
			return supervisor.ExecConfig{}, err
		}
		cfg.Env = append(cfg.Env, e.APIKeyEnv+"="+key)
	}
	return cfg, nil
}

// supervisorReadKey reads the first line of the file a file: reference names. The adapter has already refused a key
// file that group or others can read (localai.New resolves the same reference); no message here repeats the value.
func supervisorReadKey(ref string) (string, error) {
	p, ok := strings.CutPrefix(ref, "file:")
	if !ok || p == "" {
		return "", errors.New("exec.api_key_env needs localai.api_key_ref to be a file: reference")
	}
	f, err := os.Open(p)
	if err != nil {
		return "", errors.New("the LocalAI key file cannot be read")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, supervisorMaxKeyBytes))
	if err != nil {
		return "", errors.New("the LocalAI key file cannot be read")
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("the LocalAI key file is empty")
	}
	return key, nil
}
