package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/internal/dialogue"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/resources"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// serve and the optional decision layer (master plan 8.5, ADR 0029, D-15).
//
// The decision layer is OPTIONAL, BOUNDED, NON-GENERATIVE and NON-AUTHORITATIVE. This file is
// where the pieces of D-03 (guarded engine), D-04 (dialogue stage and detectors), D-05 (scorer
// adapters), D-09 (status and configuration API) and D-13 (supervisor, metrics) become one thing
// inside `alixai serve`. It adds no authority: a selection only narrows what the controller is
// offered, and every selected id is re-resolved by the deterministic adapter.
//
// # Off by default, and off changes nothing
//
// With `decision.enabled: false` (the shipped state) wireV1 hands the dialogue engine
// dialogue.NoopDecisionStage and no detectors, exactly what it did before this file existed. No
// provider is built, no supervisor, no tick goroutine, nothing is registered in the router role and
// no scorer endpoint is contacted. The only visible difference is that GET /v1/decision/status
// answers {"mode":"off","provider_id":"none"} with zero counts instead of unavailable, and that a
// configuration change is refused (the deployment did not build a layer).
//
// # What is wired when it is on
//
//   - one scorer provider (decisionProvidersBuild, kind localai or clm) under the supervisor of
//     D-13, which is the provider registered in the `router` role (decision.RegisterRouter). The
//     supervisor demand-loads it behind a ResourceGate, a thin admit-only adapter over
//     resources.Governor.Admit. A Tick driver (a goroutine owned by serve) probes, restarts, unloads
//     when idle and ends probation; it stops, and the supervisor with it, when serve stops.
//   - one dialogue/decision Layer, rebuilt (and swapped atomically) when the operator changes the
//     running profile's threshold, because the engine fixes MinConfidence when it is built. The
//     layer's stage is the dialogue engine's DecisionStage, behind a mode gate (off, shadow,
//     advisory). Its detectors are the control detectors of the engine; Layer.Wrap(engine) is built
//     and provided as ddec.Turner.
//   - the DecisionService of D-09: status counters kept per application and tenant, an operator
//     list from the configuration, configuration changes audited (decision.config_changed).
//
// # Known limits, stated plainly
//
//   - The HTTP routes of sessions, AG-UI and voice call the concrete *dialogue.Engine.Turn, so
//     Layer.Wrap(engine).Turn is NOT in front of them. The stage is on the turn path (it reads the
//     scope from its request); the scorer-backed control detectors need the scope Wrap binds and stay
//     the exact rules until those routes take a Turner.
//   - Layer builds its own engines and keeps the Advice to itself, so Metrics.ObserveAdvice cannot be
//     called from here. The same counters are recorded from the decision.accepted and
//     decision.fallback audit events (decWireAudit), which the engine appends once per Score.
//   - Counters live in memory ("since" is the start of the process). Changes made through the API are
//     not written back to the file: a restart returns to the file.

const decWireSectionName = "decision"

// The two profile modes the layer actually acts on, as the status schema names them.
const (
	decWireModeOff      = gen.DecisionStatusModeOff
	decWireModeShadow   = gen.DecisionStatusModeShadow
	decWireModeAdvisory = gen.DecisionStatusModeAdvisory
)

// decWireProviderConfig selects the scorer provider (decisionProvidersSettings).
type decWireProviderConfig struct {
	// Kind is "localai" or "clm".
	Kind string `yaml:"kind"`
	// Model and ModelRevision name the scorer model and the revision the operator pins. A scorer
	// with no revision reports "unverified", which the calibration gate refuses.
	Model         string `yaml:"model"`
	ModelRevision string `yaml:"model_revision"`
	// Endpoint is the CLM service endpoint (kind "clm"; empty is the adapter's loopback default).
	Endpoint string `yaml:"endpoint"`
	// Mode is the CLM scoring route, "rank" (default) or "systemone".
	Mode string `yaml:"mode"`
	// APIKeyRef names the CLM service key ("env:NAME" or "file:PATH"); the key itself is never
	// accepted here.
	APIKeyRef string `yaml:"api_key_ref"`
}

// decWireProfile is the configuration of one approved profile.
type decWireProfile struct {
	// Mode is off, shadow or advisory.
	Mode string `yaml:"mode"`
	// MinConfidence is the calibrated-confidence floor; never below the platform floor.
	MinConfidence float64 `yaml:"min_confidence"`
}

// decWireOperator names one operator of the deployment by the identity the verified token carries.
type decWireOperator struct {
	ApplicationID string `yaml:"application_id"`
	TenantID      string `yaml:"tenant_id"`
	PrincipalID   string `yaml:"principal_id"`
}

// decWireCalibration is one measured calibration the operator allows (core.Calibration).
type decWireCalibration struct {
	ModelID         string  `yaml:"model_id"`
	ModelRevision   string  `yaml:"model_revision"`
	ContractVersion string  `yaml:"contract_version"`
	ECE             float64 `yaml:"ece"`
	MaxECE          float64 `yaml:"max_ece"`
}

// decWireLifecycle tunes the supervisor (lifecycle.Config); zero takes the lifecycle default.
type decWireLifecycle struct {
	MinWarm        time.Duration `yaml:"min_warm"`
	IdleUnload     time.Duration `yaml:"idle_unload"`
	BackoffInitial time.Duration `yaml:"backoff_initial"`
	BackoffMax     time.Duration `yaml:"backoff_max"`
	LoadTimeout    time.Duration `yaml:"load_timeout"`
	ProbeTimeout   time.Duration `yaml:"probe_timeout"`
	Probation      time.Duration `yaml:"probation"`
}

// decWireConfig is the "decision" section. The zero value, and the shipped state, is OFF.
//
//	decision:
//	  enabled: false                 # nothing below is read while this is false
//	  provider: {kind: clm, endpoint: http://127.0.0.1:8700, model: clm-v0.1-8b, model_revision: <pinned>,
//	             api_key_ref: env:ALIXAI_CLM_KEY}
//	  platform_floor: 0.5
//	  profiles:                      # approved profiles; the running deployment profile's entry governs
//	    lite: {mode: shadow, min_confidence: 0.85}
//	  operators: [{application_id: a, tenant_id: t, principal_id: p}]
//	  calibration:                   # measured values only; none configured means calibration_failed
//	    - {model_id: clm-v0.1-8b, model_revision: <pinned>, contract_version: dialogue-routing-v1, ece: 0.03, max_ece: 0.05}
//	  peak_bytes: 0                  # the scorer's measured memory, asked of the resource governor
//	  timeout: 250ms                 # one scorer call
//	  tick_interval: 5s
type decWireConfig struct {
	Enabled       bool                      `yaml:"enabled"`
	Provider      decWireProviderConfig     `yaml:"provider"`
	Timeout       time.Duration             `yaml:"timeout"`
	PlatformFloor float64                   `yaml:"platform_floor"`
	Profiles      map[string]decWireProfile `yaml:"profiles"`
	Operators     []decWireOperator         `yaml:"operators"`
	Calibration   []decWireCalibration      `yaml:"calibration"`
	PeakBytes     uint64                    `yaml:"peak_bytes"`
	TickInterval  time.Duration             `yaml:"tick_interval"`
	Lifecycle     decWireLifecycle          `yaml:"lifecycle"`
}

// decWireDefaults is the section's defaults: the layer OFF.
func decWireDefaults() decWireConfig {
	lc := lifecycle.DefaultConfig()
	return decWireConfig{
		Timeout: core.DefaultConfig().Timeout, PlatformFloor: decWireHardFloor, TickInterval: 5 * time.Second,
		Lifecycle: decWireLifecycle{
			MinWarm: lc.MinWarm, IdleUnload: lc.IdleUnload, BackoffInitial: lc.BackoffInitial, BackoffMax: lc.BackoffMax,
			LoadTimeout: lc.LoadTimeout, ProbeTimeout: lc.ProbeTimeout,
		},
	}
}

// decWireHardFloor is the lowest threshold the platform ever allows (the gateway holds the same).
const decWireHardFloor = 0.5

var (
	decWireProfileRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	decWireSection   = decWireDefaults()
	// decWireSections holds the section copy registered on each registry other than config.Default.
	decWireSections sync.Map // *config.Registry -> *decWireConfig
)

func init() {
	config.Register(decWireSectionName, &decWireSection)
}

// decWireRegisterSections registers the section on reg, for a registry other than config.Default
// (tests, tools).
func decWireRegisterSections(reg *config.Registry) {
	c := decWireDefaults()
	reg.Register(decWireSectionName, &c)
	decWireSections.Store(reg, &c)
}

// decWireSectionOf returns the section as the registry serve ran on loaded it.
func decWireSectionOf(reg *config.Registry) decWireConfig {
	if reg != nil && reg != config.Default {
		if v, ok := decWireSections.Load(reg); ok {
			return *v.(*decWireConfig)
		}
	}
	return decWireSection
}

func decWireModeValid(m string) bool {
	return gen.DecisionStatusMode(m).Valid()
}

// Validate implements the section validation hook. Nothing beyond the shape is checked while the
// layer is off, so a half-written block cannot stop a deployment that does not use it.
func (c decWireConfig) Validate() error {
	if c.PlatformFloor < decWireHardFloor || c.PlatformFloor > 1 || math.IsNaN(c.PlatformFloor) {
		return fmt.Errorf("platform_floor must be between %.1f and 1", decWireHardFloor)
	}
	for name, p := range c.Profiles {
		switch {
		case !decWireProfileRE.MatchString(name):
			return fmt.Errorf("profiles: %q is not a profile name (lowercase letters, digits, - and _)", name)
		case !decWireModeValid(p.Mode):
			return fmt.Errorf("profiles.%s.mode must be off, shadow or advisory", name)
		case math.IsNaN(p.MinConfidence) || p.MinConfidence < c.PlatformFloor || p.MinConfidence > 1:
			return fmt.Errorf("profiles.%s.min_confidence must be between platform_floor (%.2f) and 1", name, c.PlatformFloor)
		}
	}
	for i, o := range c.Operators {
		if o.ApplicationID == "" || o.TenantID == "" || o.PrincipalID == "" {
			return fmt.Errorf("operators[%d]: application_id, tenant_id and principal_id are all required", i)
		}
	}
	for i, cal := range c.Calibration {
		if cal.ContractVersion != ddec.ContractRouting && cal.ContractVersion != ddec.ContractAct {
			return fmt.Errorf("calibration[%d].contract_version must be %s or %s", i, ddec.ContractRouting, ddec.ContractAct)
		}
	}
	if c.Timeout < 0 || c.TickInterval < 0 {
		return errors.New("timeout and tick_interval must not be negative")
	}
	if !c.Enabled {
		return nil
	}
	if !decisionProvidersAvailable() {
		return errors.New("enabled: this binary was built without decision providers (-tags nodecision)")
	}
	if !slices.Contains(decisionProvidersKinds(), strings.ToLower(strings.TrimSpace(c.Provider.Kind))) {
		return fmt.Errorf("provider.kind must be one of %v", decisionProvidersKinds())
	}
	if c.Timeout == 0 || c.TickInterval == 0 {
		return errors.New("timeout and tick_interval must be positive")
	}
	return nil
}

// decWireBuildProvider builds the scorer provider. decision_providers.go replaces it (in the build
// that has the adapters) with one that also passes the CLM api_key_ref; tests replace it with a fake.
var decWireBuildProvider = func(p decWireProviderConfig) (any, error) {
	return decisionProvidersBuild(decisionProvidersSettings{
		Kind: p.Kind, Model: p.Model, ModelRevision: p.ModelRevision, Endpoint: p.Endpoint, Mode: p.Mode,
	})
}

// decWireOnBuilt, when set (tests), sees every stack decWireNew builds.
var decWireOnBuilt func(*decWireStack)

// ---- audit: the config event and the counting tee ----

type decWireConfigAudit struct {
	Profile                       string `json:"profile"`
	Running                       bool   `json:"running"`
	Mode                          string `json:"mode"`
	PreviousMode                  string `json:"previous_mode"`
	MinConfidencePermille         int    `json:"min_confidence_permille"`
	PreviousMinConfidencePermille int    `json:"previous_min_confidence_permille"`
}

// eventDecWireConfigChanged is appended before a change is applied; per-mille integers because the
// audit log refuses non-integer JSON numbers.
var eventDecWireConfigChanged = audit.MustRegister[decWireConfigAudit](audit.TypeSpec{
	Name:        "decision.config_changed",
	Description: "an operator changed the mode or threshold of a decision-layer profile (identifiers and per-mille integers only)",
	Levels:      audit.LevelTenant,
	Required:    []string{"profile", "mode", "previous_mode"},
})

func decWirePermille(f float64) int { return int(math.Round(f * 1000)) }

// decWireAudit forwards to the audit log and, after a successful forward, counts what the decision
// engine reports: one decision.accepted or decision.fallback event per Engine.Score. Counting only
// after the forward succeeded keeps the accepted-then-fallback(unavailable) path to one count.
type decWireAudit struct {
	inner audit.Appender
	s     *decWireStack
}

func (a decWireAudit) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	out, err := a.inner.AppendAt(ctx, loc, ev)
	if err == nil {
		a.s.observe(loc, ev)
	}
	return out, err
}

// AppendTx forwards without counting: the decision engine appends with AppendAt, and an event inside
// a caller's transaction is not durable until that transaction commits.
func (a decWireAudit) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	return a.inner.AppendTx(ctx, tx, loc, ev)
}

// ---- the stack ----

// decWireCounts is one (application, tenant)'s counters. rejected_count is never stored: it is the
// sum of the fallback counts, computed when read, so it cannot drift.
type decWireCounts struct {
	accepted int64
	fallback map[gen.FallbackReason]int64
}

// decWireState is what the turn path reads on every call; it is swapped as a whole.
type decWireState struct {
	layer   *ddec.Layer
	mode    gen.DecisionStatusMode
	minConf float64
	// avoidedSeen is how many scorer-avoided controller calls were already forwarded to the metrics
	// (touched only by the tick goroutine).
	avoidedSeen atomic.Int64
}

// decWireInput is what decWireNew builds from.
type decWireInput struct {
	Log   *slog.Logger
	Clock clock.Clock
	// Config is the registry serve loaded; nil means config.Default.
	Config    *config.Registry
	Providers *providers.Registry
	Audit     audit.Appender
	Policy    *policy.Gateway
	Packages  dialogue.PackageSource
	Gov       *resources.Governor
	// Profile is the running deployment profile's name.
	Profile string
}

// decWireStack is the decision layer of one serve process: the service of D-09, the supervisor and
// tick driver of D-13 and the layer of D-04. With the layer off only the service half exists.
type decWireStack struct {
	log     *slog.Logger
	clk     clock.Clock
	cfg     decWireConfig
	floor   float64
	running string
	enabled bool
	since   time.Time
	audit   audit.Appender // the real log, for configuration events

	sup     *lifecycle.Supervisor
	metrics *lifecycle.Metrics
	mem     *lifecycle.MemoryRegistry
	manif   gen.ScorerManifest
	mk      func(minConf float64) (*ddec.Layer, error)
	cur     atomic.Pointer[decWireState]

	mu       sync.Mutex
	profiles map[string]decWireProfile
	counts   map[audit.ChainKey]*decWireCounts
	events   map[string]int
	turner   ddec.Turner
}

var _ gateway.DecisionService = (*decWireStack)(nil)

// decWireInstance adapts a built provider to lifecycle.Instance: the adapters have a Health probe
// and hold no process of their own, so Close has nothing to free.
type decWireInstance struct {
	core.DecisionModelProvider
	health func(context.Context) error
	closed *atomic.Int64
}

func (i decWireInstance) Health(ctx context.Context) error { return i.health(ctx) }

func (i decWireInstance) Close(context.Context) error {
	if i.closed != nil {
		i.closed.Add(1)
	}
	return nil
}

// decWireLoader starts an instance over the one provider decWireNew built.
type decWireLoader struct {
	p      core.DecisionModelProvider
	closed *atomic.Int64
}

func (l decWireLoader) Load(ctx context.Context, _ lifecycle.Revision) (lifecycle.Instance, error) {
	h, ok := l.p.(interface{ Health(context.Context) error })
	if !ok {
		return nil, contracts.New(contracts.ErrUnavailable, "decision: the scorer provider has no health probe")
	}
	return decWireInstance{DecisionModelProvider: l.p, health: h.Health, closed: l.closed}, ctx.Err()
}

// decWireGate turns resources.Governor.Admit into the supervisor's admit-only ResourceGate.
type decWireGate struct{ gov *resources.Governor }

func (g decWireGate) Admit(ctx context.Context, bytes uint64, label string) (lifecycle.Reservation, error) {
	t, err := g.gov.Admit(ctx, resources.Request{Class: jobs.ClassTurn, Bytes: bytes, Label: label})
	if err != nil {
		// A literal nil: a nil *Ticket inside the interface would not be a nil interface.
		return nil, err
	}
	return t, nil
}

// decWireNew builds the stack. It always returns one (the service answers "off" when the layer is
// not enabled); the layer, the supervisor and the router registration exist only when enabled.
func decWireNew(in decWireInput) (*decWireStack, error) {
	cfg := decWireSectionOf(in.Config)
	log := in.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clk := in.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	s := &decWireStack{
		log: log, clk: clk, cfg: cfg, running: in.Profile, enabled: cfg.Enabled, since: clk.Now(), audit: in.Audit,
		floor: math.Max(cfg.PlatformFloor, decWireHardFloor), profiles: map[string]decWireProfile{},
		counts: map[audit.ChainKey]*decWireCounts{}, events: map[string]int{},
	}
	for n, p := range cfg.Profiles {
		s.profiles[n] = p
	}
	if cfg.Enabled && in.Profile != "" {
		if _, ok := s.profiles[in.Profile]; !ok {
			// The running profile is always approved; it starts in shadow, the staged-rollout default.
			s.profiles[in.Profile] = decWireProfile{Mode: string(decWireModeShadow), MinConfidence: core.DefaultConfig().MinConfidence}
		}
	}
	if !cfg.Enabled {
		for n, p := range s.profiles {
			p.Mode = string(decWireModeOff)
			s.profiles[n] = p
		}
		if decWireOnBuilt != nil {
			decWireOnBuilt(s)
		}
		return s, nil
	}
	if in.Audit == nil || in.Policy == nil || in.Packages == nil || in.Gov == nil {
		return nil, errors.New("the decision layer needs the audit log, the policy gateway, the package source and the resource governor")
	}

	built, err := decWireBuildProvider(cfg.Provider)
	if err != nil {
		return nil, err
	}
	prov, ok := built.(core.DecisionModelProvider)
	if !ok {
		return nil, contracts.Newf(contracts.ErrInternal, "decision: the %T provider is not a DecisionModelProvider", built)
	}
	s.manif = prov.Manifest()
	if cfg.PeakBytes == 0 {
		log.Warn("decision.peak_bytes is not set: the scorer's load is arbitrated by the resource governor but reserves no memory")
	}
	lc := lifecycle.DefaultConfig()
	set := func(dst *time.Duration, v time.Duration) {
		if v > 0 {
			*dst = v
		}
	}
	set(&lc.MinWarm, cfg.Lifecycle.MinWarm)
	set(&lc.IdleUnload, cfg.Lifecycle.IdleUnload)
	set(&lc.BackoffInitial, cfg.Lifecycle.BackoffInitial)
	set(&lc.BackoffMax, cfg.Lifecycle.BackoffMax)
	set(&lc.LoadTimeout, cfg.Lifecycle.LoadTimeout)
	set(&lc.ProbeTimeout, cfg.Lifecycle.ProbeTimeout)
	lc.Probation = cfg.Lifecycle.Probation
	lc.Profile = in.Profile
	s.mem = lifecycle.NewMemoryRegistry()
	s.sup, err = lifecycle.New(lifecycle.Deps{
		Clock: clk, Loader: decWireLoader{p: prov, closed: decWireClosed}, Gate: decWireGate{gov: in.Gov}, Registry: s.mem,
		Revision: lifecycle.Revision{Manifest: s.manif, PeakBytes: cfg.PeakBytes}, Config: lc,
	})
	if err != nil {
		return nil, err
	}
	s.metrics = s.sup.Metrics()

	// The supervisor occupies the router role, so the layer follows the role's health.
	if in.Providers != nil {
		if err := core.RegisterRouter(in.Providers, s.sup); err != nil {
			return nil, fmt.Errorf("register the scorer in the router role: %w", err)
		}
		if !in.Providers.Usable(providers.RoleRouter) {
			return nil, errors.New("the scorer registered in the router role is not usable")
		}
	}
	teed := decWireAudit{inner: in.Audit, s: s}
	s.mk = func(minConf float64) (*ddec.Layer, error) {
		cc := core.DefaultConfig()
		cc.Enabled = true
		cc.MinConfidence = minConf
		cc.Timeout = cfg.Timeout
		cc.SupportedTypes = []gen.DecisionType{gen.DecisionTypeCapabilityRouting, gen.DecisionTypeDialogueAct}
		for _, c := range cfg.Calibration {
			cc.Calibration = append(cc.Calibration, core.Calibration{
				ModelID: c.ModelID, ModelRevision: c.ModelRevision, ContractVersion: c.ContractVersion, ECE: c.ECE, MaxECE: c.MaxECE,
			})
		}
		d := ddec.Deps{Clock: clk, Core: cc, Audit: teed, Policy: in.Policy, Packages: in.Packages, Observer: s.metrics}
		if in.Providers != nil {
			d.Registry = in.Providers
		} else {
			d.Provider = s.sup
		}
		return ddec.New(d)
	}
	run := s.profiles[in.Profile]
	layer, err := s.mk(run.MinConfidence)
	if err != nil {
		return nil, err
	}
	s.cur.Store(&decWireState{layer: layer, mode: gen.DecisionStatusMode(run.Mode), minConf: run.MinConfidence})
	log.Info("decision layer is on", slog.String("profile", in.Profile), slog.String("mode", run.Mode),
		slog.String("provider", string(s.manif.ProviderID)), slog.String("model", string(s.manif.ModelID)))
	if decWireOnBuilt != nil {
		decWireOnBuilt(s)
	}
	return s, nil
}

// decWireClosed counts instance closes for tests; nil in production.
var decWireClosed *atomic.Int64

// ---- the turn path ----

// stage is the dialogue engine's DecisionStage: the off stage while the layer is not built, else
// the mode gate over the layer's stage.
func (s *decWireStack) stage() dialogue.DecisionStage {
	if s == nil || !s.enabled {
		return dialogue.NoopDecisionStage{}
	}
	return decWireStage{s: s}
}

// detectors are the engine's control detectors; the zero Detectors (the rules) while off.
func (s *decWireStack) detectors() dialogue.Detectors {
	if s == nil || !s.enabled {
		return dialogue.Detectors{}
	}
	rules := dialogue.RuleDetectors()
	gate := func(rule dialogue.ControlDetector, pick func(dialogue.Detectors) dialogue.ControlDetector) dialogue.ControlDetector {
		return dialogue.ControlDetectorFunc(func(ctx context.Context, req dialogue.ControlRequest) dialogue.ControlDetection {
			st := s.cur.Load()
			switch st.mode {
			case decWireModeAdvisory:
				return pick(st.layer.Detectors()).Detect(ctx, req)
			case decWireModeShadow:
				_ = pick(st.layer.Detectors()).Detect(ctx, req) // the scorer runs and is recorded; the rule decides
			}
			return rule.Detect(ctx, req)
		})
	}
	return dialogue.Detectors{
		Help:    gate(rules.Help, func(d dialogue.Detectors) dialogue.ControlDetector { return d.Help }),
		Cancel:  gate(rules.Cancel, func(d dialogue.Detectors) dialogue.ControlDetector { return d.Cancel }),
		Reject:  gate(rules.Reject, func(d dialogue.Detectors) dialogue.ControlDetector { return d.Reject }),
		Confirm: gate(rules.Confirm, func(d dialogue.Detectors) dialogue.ControlDetector { return d.Confirm }),
	}
}

type decWireStage struct{ s *decWireStack }

// Decide implements dialogue.DecisionStage. Off: the stage is not consulted. Shadow: the scorer runs
// and is recorded, its selection is discarded (the typed reason is disabled). Advisory: the layer's
// answer, unchanged.
func (d decWireStage) Decide(ctx context.Context, req dialogue.DecisionRequest) dialogue.DecisionResult {
	st := d.s.cur.Load()
	switch st.mode {
	case decWireModeAdvisory:
		return st.layer.Stage().Decide(ctx, req)
	case decWireModeShadow:
		_ = st.layer.Stage().Decide(ctx, req)
	}
	return dialogue.DecisionResult{Fallback: dialogue.FallbackDisabled}
}

// decWireTurner is Layer.Wrap(engine) over the layer that is current at the time of the turn.
type decWireTurner struct {
	s     *decWireStack
	inner ddec.Turner
}

func (t decWireTurner) Turn(ctx context.Context, scope contracts.Scope, in dialogue.TurnInput) (dialogue.Result, error) {
	return t.s.cur.Load().layer.Wrap(t.inner).Turn(ctx, scope, in)
}

// bind records the dialogue engine; Turner is then Layer.Wrap(engine). No-op while off.
func (s *decWireStack) bind(e ddec.Turner) {
	if s == nil || !s.enabled || e == nil {
		return
	}
	s.mu.Lock()
	s.turner = decWireTurner{s: s, inner: e}
	s.mu.Unlock()
}

// Turner returns Layer.Wrap(engine).Turn as the composition root's turn entry point; nil while off.
func (s *decWireStack) Turner() ddec.Turner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turner
}

// provide gives the gateway the DecisionService (always: it answers "off" when the layer is off) and,
// when the layer is on, the wrapped Turner.
func (s *decWireStack) provide(services *gateway.Services) error {
	if err := gateway.Provide[gateway.DecisionService](services, s); err != nil {
		return err
	}
	if t := s.Turner(); t != nil {
		return gateway.Provide[ddec.Turner](services, t)
	}
	return nil
}

// ---- counters ----

// observe counts one decision.accepted or decision.fallback event the engine appended, per
// application and tenant. The metrics registry is fed by the layer's Observer (ObserveAdvice),
// not from here, so nothing is counted twice.
func (s *decWireStack) observe(loc audit.Locator, ev audit.NewEvent) {
	var (
		accepted bool
		reason   gen.FallbackReason
	)
	switch d := ev.Data.(type) {
	case core.AcceptedAudit:
		accepted = true
	case core.FallbackAudit:
		reason = gen.FallbackReason(d.Reason)
		if !reason.Valid() {
			return
		}
	default:
		return
	}
	key := loc.Chain()
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.counts[key]
	if c == nil {
		c = &decWireCounts{fallback: map[gen.FallbackReason]int64{}}
		s.counts[key] = c
	}
	if accepted {
		c.accepted++
	} else {
		c.fallback[reason]++
	}
}

// ---- gateway.DecisionService ----

const decWireTimestamp = "2006-01-02T15:04:05.000Z"

// Status implements gateway.DecisionService.
func (s *decWireStack) Status(_ context.Context, scope contracts.Scope) (gen.DecisionStatus, error) {
	if scope.ApplicationID == "" || scope.TenantID == "" {
		return gen.DecisionStatus{}, contracts.New(contracts.ErrScopeMissing, "the decision status needs an application and a tenant")
	}
	now := s.clk.Now().UTC()
	st := gen.DecisionStatus{
		Mode: decWireModeOff, ProviderID: "none", FallbackCounts: []gen.DecisionStatusFallbackCount{},
		Since: common.Timestamp(s.since.UTC().Format(decWireTimestamp)), GeneratedAt: common.Timestamp(now.Format(decWireTimestamp)),
	}
	if s.enabled {
		st.Mode = gen.DecisionStatusMode(s.cur.Load().mode)
		st.ProviderID = s.manif.ProviderID
		model, rev := s.manif.ModelID, s.manif.ModelRevision
		st.ModelID, st.ModelRevision = &model, &rev
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.counts[audit.ChainKey{ApplicationID: scope.ApplicationID, TenantID: scope.TenantID}]
	if c == nil {
		return st, nil
	}
	st.AcceptedCount = c.accepted
	for r, n := range c.fallback {
		st.FallbackCounts = append(st.FallbackCounts, gen.DecisionStatusFallbackCount{Reason: r, Count: n})
		st.RejectedCount += n
	}
	sort.Slice(st.FallbackCounts, func(i, j int) bool { return st.FallbackCounts[i].Reason < st.FallbackCounts[j].Reason })
	return st, nil
}

// AuthorizeOperator implements gateway.DecisionService: only an operator of the deployment's own
// configuration (application, tenant and principal all equal), never a role or capability the token
// asserts. With no operator configured nobody passes.
func (s *decWireStack) AuthorizeOperator(_ context.Context, scope contracts.Scope) error {
	for _, o := range s.cfg.Operators {
		if scope.ApplicationID != "" && scope.TenantID != "" && scope.PrincipalID != "" &&
			o.ApplicationID == scope.ApplicationID && o.TenantID == scope.TenantID && o.PrincipalID == scope.PrincipalID {
			return nil
		}
	}
	return contracts.New(contracts.ErrPermissionDenied, "the decision configuration is for operators")
}

// PlatformFloor implements gateway.DecisionService: the configured floor, never below 0.5.
func (s *decWireStack) PlatformFloor() float64 { return s.floor }

func (s *decWireStack) profileViewLocked() []gateway.DecisionProfileConfig {
	names := make([]string, 0, len(s.profiles))
	for n := range s.profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]gateway.DecisionProfileConfig, 0, len(names))
	for _, n := range names {
		p := s.profiles[n]
		out = append(out, gateway.DecisionProfileConfig{Profile: n, Mode: gen.DecisionStatusMode(p.Mode), MinConfidence: p.MinConfidence})
	}
	return out
}

// Config implements gateway.DecisionService: every approved profile. The configuration is the
// deployment's, so every application sees the same.
func (s *decWireStack) Config(context.Context, contracts.Scope) ([]gateway.DecisionProfileConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profileViewLocked(), nil
}

// SetConfig implements gateway.DecisionService. The audit event is appended first, and the change is
// applied only if it was; a layer the deployment did not build cannot be enabled from here.
func (s *decWireStack) SetConfig(ctx context.Context, scope contracts.Scope, up gateway.DecisionConfigUpdate) (gateway.DecisionProfileConfig, error) {
	if !s.enabled {
		return gateway.DecisionProfileConfig{}, contracts.New(contracts.ErrUnavailable,
			"the decision layer is not built in this deployment: set decision.enabled and restart")
	}
	if !up.Mode.Valid() {
		return gateway.DecisionProfileConfig{}, contracts.New(contracts.ErrValidationFailed, "mode must be off, shadow or advisory")
	}
	loc, err := audit.ForScope(scope)
	if err != nil {
		return gateway.DecisionProfileConfig{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.profiles[up.Profile]
	if !ok {
		return gateway.DecisionProfileConfig{}, contracts.New(contracts.ErrValidationFailed, "the profile is not approved by this deployment")
	}
	next := decWireProfile{Mode: string(up.Mode), MinConfidence: prev.MinConfidence}
	if up.MinConfidence != nil {
		if math.IsNaN(*up.MinConfidence) || *up.MinConfidence < s.floor || *up.MinConfidence > 1 {
			return gateway.DecisionProfileConfig{}, contracts.New(contracts.ErrValidationFailed, "min_confidence is below the platform floor").
				WithDetails("reason", "below_platform_floor")
		}
		next.MinConfidence = *up.MinConfidence
	}
	running := up.Profile == s.running
	var layer *ddec.Layer
	if running && next.MinConfidence != prev.MinConfidence {
		// The engine fixes MinConfidence when it is built: build the new layer before anything is recorded.
		if layer, err = s.mk(next.MinConfidence); err != nil {
			return gateway.DecisionProfileConfig{}, contracts.Wrap(contracts.ErrInternal, err, "the decision layer could not be rebuilt")
		}
	}
	if _, err := eventDecWireConfigChanged.Append(ctx, s.audit, loc, decWireConfigAudit{
		Profile: up.Profile, Running: running, Mode: next.Mode, PreviousMode: prev.Mode,
		MinConfidencePermille: decWirePermille(next.MinConfidence), PreviousMinConfidencePermille: decWirePermille(prev.MinConfidence),
	}); err != nil {
		return gateway.DecisionProfileConfig{}, contracts.Wrap(contracts.ErrUnavailable, err, "the configuration change could not be audited, so it was not applied")
	}
	s.profiles[up.Profile] = next
	if running {
		old := s.cur.Load()
		st := &decWireState{layer: old.layer, mode: gen.DecisionStatusMode(next.Mode), minConf: next.MinConfidence}
		st.avoidedSeen.Store(old.avoidedSeen.Load())
		if layer != nil {
			st.layer = layer
			st.avoidedSeen.Store(0)
		}
		s.cur.Store(st)
	}
	return gateway.DecisionProfileConfig{Profile: up.Profile, Mode: up.Mode, MinConfidence: next.MinConfidence}, nil
}

// ---- the tick driver ----

// start runs the Tick driver for as long as serve runs. The function it returns cancels the driver,
// waits for it and then stops the supervisor, which unloads the scorer and releases its reservation.
func (s *decWireStack) start(ctx context.Context) (stop func()) {
	if !s.enabled || s.sup == nil {
		return func() {}
	}
	tctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.cfg.TickInterval)
		defer t.Stop()
		for {
			select {
			case <-tctx.Done():
				return
			case <-t.C:
				s.tick(tctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		s.sup.Stop(context.Background())
	}
}

// tick is one beat: the supervisor's Tick, and the controller calls the scorer's control detections
// avoided since the last beat, forwarded to the metrics. Only scorer detections count: an exact rule
// would have avoided the call anyway.
func (s *decWireStack) tick(ctx context.Context) {
	for _, ev := range s.sup.Tick(ctx) {
		s.log.Info("decision scorer", slog.String("event", ev))
		s.mu.Lock()
		s.events[ev]++
		s.mu.Unlock()
	}
}

// tickEvents returns how many times each Tick event happened (tests, diagnostics).
func (s *decWireStack) tickEvents() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.events)
}

// register adds the tick driver to the services serve runs; nothing while the layer is off.
func (s *decWireStack) register(add func(start func(ctx context.Context) (stop func()))) {
	if s.enabled && add != nil {
		add(s.start)
	}
}
