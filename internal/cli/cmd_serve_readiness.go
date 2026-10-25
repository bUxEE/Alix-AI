package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Serve readiness (M0-18, from the live spike M0-13 findings F4 to F6 and the
// M0-15 resource budgets; M0-19 for the first call after an idle unload and the
// derived controller name). Everything here reaches `alixai serve` through one
// serve provider and three small hooks in cmd_serve.go: startInference derives
// an empty controller name, wireV1 builds the serveWarmTracker and hands it to
// this provider and to localSelector, and localSelector asks it for its budget.
//
//   - /readyz says whether the controller model has loaded and answered
//     (gateway.ReadinessWarmth, check "inference_warmth"), and the first call a
//     cold model gets has a longer budget than the 60 s of a user turn. So does
//     the first user turn that finds the model unloaded again (serveWarmTracker).
//   - localai.models.controller must be a verified registry id, not free text;
//     left empty it is derived from the one verified controller entry.
//   - LOCALAI_MAX_ACTIVE_BACKENDS must not be lower than the number of models
//     the configuration keeps resident (resource-budgets rule R3).
//
// The provider does nothing about inference when the inference path is off: no
// controller is registered, nothing is called, /readyz already says so.

func init() { registerServeProvider(serveReadinessProviderName, serveReadinessProvide) }

const (
	serveReadinessProviderName = "readiness"
	// serveReadinessCheckName is the /readyz check that reports the warm-up. It
	// is not "inference": that name is the check of a path that is off.
	serveReadinessCheckName = "inference_warmth"

	// serveReadinessActiveBackendsEnv is the LocalAI setting the guard reads.
	serveReadinessActiveBackendsEnv = "LOCALAI_MAX_ACTIVE_BACKENDS"

	// serveReadinessColdLoadMeasured is the longest first call after a start the
	// spike saw: 50.4 s for Qwen3.5-0.8B on a file that was in no cache yet
	// (M0-13 report.md F4, runs 8 to 50 s; M0-15 section 7.3 saw 5.8 to 14.6 s with
	// a warm host cache). The first-call budget has to cover it with its generation.
	serveReadinessColdLoadMeasured = 51 * time.Second
	// serveReadinessColdLoadAllowance is added to the user-turn timeout for the
	// call that has to load the model. Together with providers.DefaultBudget it is
	// 3 minutes: about 3.5 times the worst load measured, and still inside the
	// adapter's own 5 minute request timeout.
	serveReadinessColdLoadAllowance = 2 * time.Minute

	// The warm-up call is constant text and one token or so of output: nothing of
	// any tenant ever goes into it, and it needs no output constraint.
	serveReadinessWarmPrompt  = "Reply with the single word: ready"
	serveReadinessWarmMaxToks = 4

	// serveWarmIdleEnv is the LocalAI setting that unloads an idle model and
	// serveWarmDefaultIdleTimeout its default (the compose files set the same 15
	// minutes). The watchdog is global: it unloads the controller too, whatever
	// resource-budgets rule R3.3 would want.
	serveWarmIdleEnv            = "LOCALAI_WATCHDOG_IDLE_TIMEOUT"
	serveWarmDefaultIdleTimeout = 15 * time.Minute
)

// serveReadinessLookupEnv reads the environment the guards compare against.
// Tests replace it.
var serveReadinessLookupEnv = os.LookupEnv

// serveReadinessProvide is the serve provider.
func serveReadinessProvide(ctx context.Context, d serveDeps) error {
	return serveReadinessWire(ctx, serveReadinessInput{
		log: d.Log, clk: d.Clock, reg: d.Providers,
		localai: localaiSection, inference: inferenceSection,
		loadRegistry: loadModelRegistry, lookupEnv: serveReadinessLookupEnv,
		warm:     d.Warm,
		addCheck: d.AddCheck, addService: d.AddService,
	})
}

// serveReadinessInput is what serveReadinessWire works from, so a test can give
// it a registry, a runtime and an environment of its own.
type serveReadinessInput struct {
	log          *slog.Logger
	clk          clock.Clock
	reg          *providers.Registry
	localai      localai.Config
	inference    inferenceConfig
	loadRegistry func() (*models.Registry, error)
	lookupEnv    func(string) (string, bool)
	// warm is told when the controller answered the warm-up call and when the
	// runtime was seen unusable. Nil is allowed: nothing is recorded.
	warm       *serveWarmTracker
	addCheck   func(gateway.Check)
	addService func(start func(ctx context.Context) (stop func()))
}

// serveReadinessWire runs the three guards and wires the warm-up. An error
// refuses the start: it is a configuration that would run with eviction on every
// call, or chat with a model nobody verified.
func serveReadinessWire(_ context.Context, in serveReadinessInput) error {
	// With no controller registered the inference path is off and /readyz says
	// so through the "inference" check; there is no model to name or to warm.
	inferenceOn := in.reg.Has(providers.RoleController)

	// The models the configuration keeps resident, counted with the controller's
	// real name: a controller left empty is the verified registry id it is
	// derived from (startInference built the client with that name), so it must
	// take its slot in the LOCALAI_MAX_ACTIVE_BACKENDS guard too.
	resident := in.localai.Models
	if inferenceOn {
		mr, err := in.loadRegistry()
		if err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "readiness: the model registry could not be loaded")
		}
		// Strict in every build: the name is a verified registry id that serves a
		// controller role, or the start is refused.
		model, err := serveReadinessControllerModel(mr, in.inference.Models, in.localai.Models.Controller)
		if err != nil {
			return err
		}
		in.log.Info("the controller model is a verified registry id", slog.String("model", model))
		resident.Controller = model
	}

	raw, present := in.lookupEnv(serveReadinessActiveBackendsEnv)
	note, err := serveReadinessCheckActiveBackends(resident, raw, present)
	if err != nil {
		return err
	}
	if note != "" {
		in.log.Warn(note)
	}
	if !inferenceOn {
		return nil
	}

	budget := serveReadinessFirstCallBudget(providers.DefaultBudget())
	if rt := in.localai.RequestTimeout; rt > 0 && rt < budget.Timeout {
		in.log.Warn("localai.request_timeout is shorter than the first-call budget: a model load that takes longer than it fails the warm-up",
			slog.Duration("request_timeout", rt), slog.Duration("first_call_budget", budget.Timeout))
	}
	warmUp := serveReadinessWarmUp(func() (providers.TextGenerator, error) {
		return in.reg.TextGenerator(providers.RoleController)
	}, budget)
	warm := func(ctx context.Context) error {
		if err := warmUp(ctx); err != nil {
			return err
		}
		// The model answered: a user turn that follows within the idle window
		// does not have to load it.
		in.warm.touch()
		return nil
	}
	tracker, err := gateway.NewReadinessWarmth(gateway.ReadinessWarmthOptions{Warm: warm, Clock: in.clk, Logger: in.log})
	if err != nil {
		return err
	}
	in.addCheck(gateway.Check{
		Name: serveReadinessCheckName,
		Fn: func(ctx context.Context) error {
			// A runtime that was unavailable has restarted and has unloaded its
			// model: warm it again once it is back, and give the first user turn
			// that follows the cold-load budget until something has answered.
			if !in.reg.Usable(providers.RoleController) {
				tracker.Invalidate()
				in.warm.forget()
				return contracts.New(contracts.ErrUnavailable, "the model runtime is not usable, so the model is not warm")
			}
			return tracker.Evaluate(ctx)
		},
	})
	in.addService(tracker.Start)
	return nil
}

// serveReadinessFirstCallBudget is the budget of the call that may have to load
// the model: the user-turn budget with the cold-load allowance added to its
// timeout. Every other limit is unchanged.
func serveReadinessFirstCallBudget(base providers.Budget) providers.Budget {
	return serveReadinessFirstCallBudgetFor(base, serveReadinessColdLoadAllowance)
}

func serveReadinessFirstCallBudgetFor(base providers.Budget, allowance time.Duration) providers.Budget {
	b := base
	b.Timeout = base.Timeout + allowance
	return b
}

// serveReadinessScope is the scope of the warm-up call. The adapter validates it
// and never sends it: LocalAI knows nothing about tenants, and the call carries
// no tenant data. The identifiers say what the call is.
func serveReadinessScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "alixai-service",
		ApplicationID:    "alixai-readiness",
		TenantID:         "service",
		WorkspaceID:      "service",
		PrincipalID:      "readiness-warm-up",
		Roles:            []string{"service"},
		CapabilityScopes: []string{"readiness.warm"},
		SessionID:        "readiness-warm-up",
	}
}

// serveReadinessWarmUp returns the warm-up call: one short chat on the
// controller, under budget (so it gets the first-call timeout), that succeeds
// only when the model loaded and answered. The generator is looked up on each
// call, so a registry that lost its controller fails the call closed.
func serveReadinessWarmUp(gen func() (providers.TextGenerator, error), budget providers.Budget) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		g, err := gen()
		if err != nil {
			return err
		}
		bg, err := providers.WithBudget(g, budget)
		if err != nil {
			return err
		}
		_, err = bg.Chat(ctx, serveReadinessScope(), providers.ChatRequest{
			Role:        providers.RoleController,
			Messages:    []providers.Message{{Role: providers.MsgUser, Content: serveReadinessWarmPrompt}},
			MaxTokens:   serveReadinessWarmMaxToks,
			Temperature: 0,
		})
		return err
	}
}

// serveReadinessControllerModel returns the LocalAI model name the controller
// role must use: the id of a verified registry entry that serves a controller
// role. The rule for an operator is that every LocalAI model definition is
// named by its registry id (tests/live/spike/localai/models.yaml, the M0-13
// spike's, is), so the name is derived from what was verified instead of typed
// next to it. The shipped deploy/container/localai/config/models.yaml defines
// only the smoke model "alixai-smoke", for smoke.sh: it is not a registry id,
// so `alixai serve` cannot address it under this rule. Configured empty derives the name when
// exactly one verified entry is a controller; a configured name must be one of
// them. Anything else (another registry entry, an embedding model, free text)
// is refused: a configuration must not verify one model file and chat with a
// different one (M0-13 F6).
func serveReadinessControllerModel(mr *models.Registry, verified []string, configured string) (string, error) {
	var controllers []string
	for _, id := range verified {
		entry, ok := mr.Get(id)
		if !ok {
			continue
		}
		for _, role := range entry.Roles {
			if role == models.RoleConversationalController || role == models.RoleTypedRouting {
				controllers = append(controllers, id)
				break
			}
		}
	}
	if configured == "" {
		switch len(controllers) {
		case 1:
			return controllers[0], nil
		case 0:
			return "", contracts.New(contracts.ErrValidationFailed,
				"readiness: none of inference.models is a controller model, so localai.models.controller has nothing to derive from")
		}
		return "", contracts.Newf(contracts.ErrValidationFailed,
			"readiness: inference.models lists several controller models (%s); set localai.models.controller to one of them",
			strings.Join(controllers, ", "))
	}
	for _, id := range controllers {
		if id == configured {
			return id, nil
		}
	}
	if len(controllers) == 0 {
		return "", contracts.Newf(contracts.ErrValidationFailed,
			"readiness: localai.models.controller %q is not a verified registry id: no entry of inference.models is a controller model", configured)
	}
	return "", contracts.Newf(contracts.ErrValidationFailed,
		"readiness: localai.models.controller %q is not a verified registry id; use one of inference.models that serves the controller role (%s)",
		configured, strings.Join(controllers, ", "))
}

// serveReadinessResidentModels lists the distinct LocalAI model names the
// configuration keeps resident, sorted. A router that shares the controller's
// model needs no second slot.
func serveReadinessResidentModels(set localai.ModelSet) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range []string{set.Controller, set.Router, set.Embedder} {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// serveReadinessCheckActiveBackends is resource-budgets rule R3.1: a LocalAI
// that may keep fewer backends resident than the configuration needs unloads a
// model on every call (15.2 s for a chat after an embedding on the 4 GiB
// profile, M0-15). It refuses a value lower than the number of resident models,
// a negative one and one that is not an integer. 0 is LocalAI's "unlimited": it
// cannot evict, so it passes, with a note, because HC-29 wants a bound. A value
// AlixAI cannot see (the variable belongs to the LocalAI container) cannot be
// checked: that is a note too, and only when more than one model is resident,
// because with one there is nothing to evict. A note is returned for the caller
// to log; a non-nil error refuses the start.
func serveReadinessCheckActiveBackends(set localai.ModelSet, raw string, present bool) (note string, err error) {
	resident := serveReadinessResidentModels(set)
	raw = strings.TrimSpace(raw)
	if !present || raw == "" {
		if len(resident) > 1 {
			return fmt.Sprintf("%s is not set in AlixAI's environment: cannot confirm that LocalAI keeps the %d configured models resident at once",
				serveReadinessActiveBackendsEnv, len(resident)), nil
		}
		return "", nil
	}
	n, perr := strconv.ParseUint(raw, 10, 16)
	if perr != nil {
		return "", contracts.Newf(contracts.ErrValidationFailed,
			"readiness: %s must be a non-negative integer (0 is unlimited)", serveReadinessActiveBackendsEnv)
	}
	switch {
	case n == 0:
		return fmt.Sprintf("%s=0 is unlimited: nothing is evicted, but HC-29 wants a bound for production", serveReadinessActiveBackendsEnv), nil
	case int(n) < len(resident):
		return "", contracts.Newf(contracts.ErrValidationFailed,
			"readiness: %s=%d is lower than the %d models the configuration keeps resident (%s): LocalAI would unload one on every call; set it to at least %d (resource budgets R3)",
			serveReadinessActiveBackendsEnv, n, len(resident), strings.Join(resident, ", "), len(resident))
	}
	return "", nil
}

// serveWarmTracker knows whether the controller model is probably still loaded,
// so that a user turn that may have to load it again gets the cold-load budget
// instead of 60 s.
//
// LocalAI unloads an idle model after LOCALAI_WATCHDOG_IDLE_TIMEOUT (15 minutes
// by default) and the watchdog is global, so the controller goes with it: the
// next user turn pays a cold load, which the spike measured at 8 to 50 s, close
// to the edge of a 60 s budget. AlixAI cannot ask LocalAI what is loaded, so it
// remembers when the controller last answered, a warm-up call or a user turn,
// and calls the model cold when nothing has answered since the start or for
// longer than the window. Cold errs on the generous side: the larger budget is
// only an upper bound, so a model that was still loaded answers at once and the
// budget costs nothing.
//
// The window is two thirds of the idle timeout, so that a turn that arrives just
// as the watchdog fires is still given the larger budget. A call that failed
// proves nothing and does not count: a cold turn that timed out leaves the next
// one cold too. It is safe for concurrent use; a nil tracker records nothing and
// always answers with the base budget.
type serveWarmTracker struct {
	clk    clock.Clock
	window time.Duration

	mu       sync.Mutex
	answered time.Time // when the controller last answered; zero: not since the start
}

// serveWarmNewTracker builds the tracker. The idle timeout is the one in the
// environment AlixAI shares with LocalAI, or LocalAI's default when it cannot
// see one or cannot read it.
func serveWarmNewTracker(clk clock.Clock, lookupEnv func(string) (string, bool)) *serveWarmTracker {
	idle := serveWarmDefaultIdleTimeout
	if lookupEnv != nil {
		if raw, ok := lookupEnv(serveWarmIdleEnv); ok {
			if d, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && d > 0 {
				idle = d
			}
		}
	}
	if clk == nil {
		clk = clock.Real{}
	}
	return &serveWarmTracker{clk: clk, window: idle * 2 / 3}
}

// touch records that the controller has just answered.
func (t *serveWarmTracker) touch() {
	if t == nil {
		return
	}
	now := t.clk.Now()
	t.mu.Lock()
	t.answered = now
	t.mu.Unlock()
}

// forget records that the runtime restarted or went away: nothing is loaded.
func (t *serveWarmTracker) forget() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.answered = time.Time{}
	t.mu.Unlock()
}

// cold reports whether the next call may have to load the model.
func (t *serveWarmTracker) cold() bool {
	if t == nil {
		return false
	}
	now := t.clk.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.answered.IsZero() || now.Sub(t.answered) >= t.window
}

// budgeted wraps gen for one user turn: the call gets base, or the first-call
// budget when the model may have to be loaded, and a call that answers is
// recorded.
func (t *serveWarmTracker) budgeted(gen providers.TextGenerator, base providers.Budget) (providers.TextGenerator, error) {
	if t == nil {
		return providers.WithBudget(gen, base)
	}
	if t.cold() {
		base = serveReadinessFirstCallBudget(base)
	}
	return providers.WithBudget(serveWarmGenerator{inner: gen, tracker: t}, base)
}

// serveWarmGenerator records the controller's answers.
type serveWarmGenerator struct {
	inner   providers.TextGenerator
	tracker *serveWarmTracker
}

func (g serveWarmGenerator) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	res, err := g.inner.Chat(ctx, scope, req)
	if err == nil {
		g.tracker.touch()
	}
	return res, err
}

func (g serveWarmGenerator) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	s, err := g.inner.ChatStream(ctx, scope, req)
	if err != nil {
		return nil, err
	}
	return serveWarmStream{inner: s, tracker: g.tracker}, nil
}

// serveWarmStream records every chunk the model produces: a chunk proves that
// the model is loaded, and the last one is when it was last in use.
type serveWarmStream struct {
	inner   providers.ChatStream
	tracker *serveWarmTracker
}

func (s serveWarmStream) Recv() (providers.ChatChunk, error) {
	c, err := s.inner.Recv()
	if err == nil {
		s.tracker.touch()
	}
	return c, err
}

func (s serveWarmStream) Close() error { return s.inner.Close() }
