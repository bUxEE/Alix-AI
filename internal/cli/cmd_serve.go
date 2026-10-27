package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/diag"
	"github.com/buxee/alix-ai/internal/dialogue"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// serveExternalCoversController reports whether external_providers.routing names
// profiles for the controller so a deployment with empty inference.models is still
// ready. The external build replaces this in init; the noexternal build leaves it false.
var serveExternalCoversController = func() bool { return false }

func init() {
	// The adapter does not register its own section; the composition root does.
	config.Register(localaiSectionName, &localaiSection)
	config.Register(inferenceSectionName, &inferenceSection)
	Register(Command{
		Name:    "serve",
		Summary: "run the AlixAI service: store, model preflight, providers and the HTTP gateway",
		Run:     runServe,
	})
}

// Section names owned by this file. The gateway registers "gateway" and "dev"
// itself (internal/gateway), the store registers "store".
const (
	localaiSectionName   = "localai"
	inferenceSectionName = "inference"
)

// localaiSection is the registered "localai" section: adapters/localai.Config
// as the adapter defines it. Read it through this pointer after a load.
var localaiSection localai.Config

// inferenceConfig is the "inference" section: what `alixai serve` verifies
// before the inference path may start (SEC-063, master plan 16.3).
//
//	inference:
//	  models_dir: data/models          # the directory the runtime loads models from
//	  models: [granite-4.0-350m-q4_k_m] # registry entry ids to verify
//
// Each id must name an entry of the shipped model registry (models/registry);
// its artifact.filename inside models_dir is hashed and compared with the
// entry. localai.models.controller is one of these ids (M0-18), or empty when
// exactly one of them serves the controller role: then it is derived. With no
// models listed the inference path stays off: there is no unverified-but-allowed
// way to run a model.
type inferenceConfig struct {
	ModelsDir string   `yaml:"models_dir"`
	Models    []string `yaml:"models"`
}

var inferenceSection inferenceConfig

var modelIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Validate implements the section validation hook.
func (c inferenceConfig) Validate() error {
	if strings.ContainsRune(c.ModelsDir, 0) {
		return errors.New("models_dir must not contain NUL")
	}
	if len(c.Models) > 0 && strings.TrimSpace(c.ModelsDir) == "" {
		return errors.New("models_dir is required when models is set")
	}
	seen := map[string]bool{}
	for i, id := range c.Models {
		if !modelIDRE.MatchString(id) {
			return fmt.Errorf("models[%d] is not a registry entry id", i)
		}
		if seen[id] {
			return fmt.Errorf("models[%d]: %q is listed twice", i, id)
		}
		seen[id] = true
	}
	return nil
}

// registerServeSections registers every section `serve` reads on reg, for a
// registry other than config.Default (tests, tools). On Default they are
// registered by the packages' own init functions (the gateway's and the
// identity package's) and by this package's (localai, inference, keys, rag).
// The RAG sections (rag, rag_ingest, rag_query, rag_vector, rag_answer, jobs)
// are registered by ragSourcesRegisterSections.
//
// The identity section is the one `serve` reads through identity.CurrentConfig:
// a registry that did not register it would refuse an `identity:` block as an
// unknown section, and the /v1 API would be left without a deployment id.
func registerServeSections(reg *config.Registry) {
	reg.Register(localaiSectionName, &localaiSection)
	reg.Register(inferenceSectionName, &inferenceSection)
	reg.Register(supervisorSectionName, &supervisorSection)
	reg.Register(serveProfileSectionName, &serveProfileSection)
	reg.Register(keysSectionName, &keysSection)
	reg.Register(auditSectionName, &auditSection)
	reg.Register(identity.SectionName, identity.ConfigSection())
	reg.Register(appCredSectionName, &appCredSection)
	// ADR 0046/0047: stream_verified + analyst (abilities, web, caps).
	reg.Register(dialogue.SectionName, dialogue.ConfigSection())
	gateway.RegisterSections(reg)
	resourcesRegisterSections(reg)
	quotaWireRegisterSections(reg)
	decWireRegisterSections(reg)
	ragSourcesRegisterSections(reg)
}

// loadModelRegistry returns the model registry carried in the binary. Tests
// replace it.
var loadModelRegistry = func() (*models.Registry, error) { return models.LoadFS(registry.FS) }

func runServe(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("serve")
	path := env.ConfigFlag(fs)
	if err := env.ParseFlags(fs, "alixai serve [-config file.yaml]", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := env.Config.LoadFrom(*path, env.LookupEnv); err != nil {
		return err
	}
	if err := configCheckAfterLoad(); err != nil {
		return err
	}
	core := env.Config.Core()
	// The ring behind the diagnostics bundle's recent errors sees every record from here on.
	log, errRing := opsWrapLogger(alog.NewWithWriter(core.Log, env.Stderr), core.Log.RedactKeys)

	gcfg, dev := *gateway.ConfigSection(), *gateway.DevSection()
	policy := gateway.ListenPolicy{TLS: gcfg.TLS.Enabled(), SpikeEndpoint: dev.SpikeEndpoint}

	// Everything that can be refused from the configuration alone is refused
	// first, before a file is created or a port is touched.
	if core.Server.UnixSocket != "" {
		return errors.New("server.unix_socket is not served by this build of the gateway yet; unset it")
	}
	if err := policy.CheckAddr(core.Server.Listen); err != nil {
		return err
	}
	// The deployment profile is resolved with the other refusals that need only the
	// configuration: an unknown name stops here, with nothing created.
	profile, err := serveProfileResolve(string(serveProfileSection))
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	// The platform checkpoint signing key is read before the store is opened or
	// a port is touched: a key that is configured and cannot be used is a refused
	// start, and nothing is created for a service that will not run.
	ckey, err := loadCheckpointKey(keysSection, core.DataDir.String())
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	// An existing data directory that other users can read is warned about before
	// the store opens it (SEC-081).
	hardeningCheckDataDir(log, core.DataDir.String())

	// A restore cut while it swapped files leaves the directory part backup and
	// part what was there before; it is not served from.
	if err := backup.InterruptedRestore(core.DataDir.String()); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	clk := clock.Real{}
	opts := store.FromConfig()
	opts.Path = filepath.Join(core.DataDir.String(), store.DefaultFileName)
	opts.Logger = log
	db, err := store.Open(ctx, opts)
	if err != nil {
		return fmt.Errorf("serve: open the store: %w", err)
	}
	defer db.Close()
	if ckey != nil {
		if err := checkCheckpointKeyRegistered(ctx, db, clk, ckey); err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		log.Info("audit checkpoints are signed", slog.String("key_id", ckey.signing.KeyID), slog.Int64("every", keysSection.CheckpointEvery),
			slog.Duration("interval", keysSection.CheckpointInterval))
	} else {
		log.Warn("audit checkpoints are not signed: no platform checkpoint signing key is provisioned (run `alixai keys init`)")
	}

	checks := []gateway.Check{{
		Name: "store",
		Fn: func(ctx context.Context) error {
			var one int
			if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
				return contracts.Wrap(contracts.ErrUnavailable, err, "the store is not answering")
			}
			return nil
		},
	}}

	reg, off := startInference(ctx, log, clk, localaiSection, inferenceSection)
	if off != nil {
		// Empty inference.models with the controller routed externally is a light
		// deployment (no local LLM): keep the local selector fail-closed, mark the
		// controller covered so /readyz is green, and do not add a failing check.
		// A configured local model that failed pin/hash verification still blocks.
		if len(inferenceSection.Models) == 0 && serveExternalCoversController() {
			log.Info("local inference path unused: text controller routed externally",
				slog.String("reason", providers.ExternalCoverReason),
				slog.String("inference_off", off.reason))
			reg.MarkExternallyCovered(providers.RoleController)
		} else {
			log.Error("the inference path is off", slog.String("reason", off.reason), slog.String("err_text", off.err.Error()))
			checks = append(checks, gateway.Check{
				Name: "inference",
				Fn: func(context.Context) error {
					return contracts.New(contracts.ErrUnavailable, "inference path is off: "+off.reason)
				},
			})
		}
	}

	// The public API (milestone M1-16): every service the /v1 handlers use is
	// built here, from one package source, before anything listens.
	w, err := wireV1(ctx, wireInput{
		log: log, clk: clk, db: db, dataDir: core.DataDir.String(), providers: reg, off: off, checkpointKey: ckey, keys: keysSection,
		config: env.Config, profile: profile, errorRing: errRing,
	})
	if err != nil {
		return fmt.Errorf("serve: wire the public API: %w", err)
	}
	defer w.adapters.close()
	// Which transports this build can reach an application through, said once
	// at the start: an application whose declared kind is not here is unavailable
	// (adapterSource says so on its first call), and an operator reading the log
	// should not have to wait for a request to learn it.
	if kinds := transportsDefault.kinds(); len(kinds) == 0 {
		log.Warn("no adapter transport is registered in this build: every application is unavailable")
	} else {
		log.Info("adapter transports", slog.Any("kinds", kinds))
	}
	// Commits that a process that died left half-way are resolved before a
	// request is served: this process has none in flight, so every row in
	// committing belongs to one that is gone. Nothing is ever committed again
	// by recovery; the host's own Status says what happened (SEC-046).
	rec, err := w.coordinator.RecoverAtStartup(ctx)
	if err != nil {
		return fmt.Errorf("serve: resolve interrupted commits: %w", err)
	}
	if rec.Examined > 0 {
		log.Warn("interrupted commits resolved before serving", slog.Int("examined", rec.Examined),
			slog.Int("recorded", rec.Recorded), slog.Int("reopened", rec.Reopened), slog.Int("uncertain", rec.Uncertain))
	}
	// The periodic sweeps are service jobs: they work across tenants, so they
	// are started here and are not reachable from a request (the API sees the
	// coordinator only as gateway.ActionService, which has no sweep).
	// They stop when this function returns for any reason: a start that is
	// refused after this point (a port in use, an area without its services, the
	// admin interface) must exit, not wait for a context nobody cancels.
	jobsCtx, stopJobs := context.WithCancel(ctx)
	waitJobs := startServiceJobs(jobsCtx, log, w.serviceJobs(log)...)
	var stopServices []func()
	// The governor's loop runs for as long as serve does: without it every reading stays unknown
	// and background work holds.
	stopServices = append(stopServices, w.resources.start(jobsCtx))
	for _, start := range w.extraStarts {
		stopServices = append(stopServices, start(jobsCtx))
	}
	served := false
	defer func() {
		stopJobs()
		waitJobs()
		for i := len(stopServices) - 1; i >= 0; i-- {
			stopServices[i]()
		}
		if served {
			// Anchor what was appended since the last checkpoint before the process
			// goes, once nothing that appends is still running and before the store
			// closes: the window of unsigned events is then empty after a clean stop.
			w.finalCheckpoint(log)
		}
	}()
	checks = append(checks, w.extraChecks...)

	srv, err := gateway.NewServer(gateway.Options{
		Config:    gcfg,
		Dev:       dev,
		Logger:    log,
		Clock:     clk,
		Providers: reg,
		Checks:    checks,
		V1Auth:    w.auth,
		Services:  w.services,
	})
	if err != nil {
		return err
	}
	// An area that registered itself but whose services nobody provided would be
	// a /v1 path that answers 404 in production: refuse to start instead.
	if un := srv.Unmounted(); len(un) > 0 {
		return fmt.Errorf("serve: route areas without their services: %s", describeUnmounted(un))
	}
	ln, err := gateway.Listen(core.Server.Listen, srv.Policy())
	if err != nil {
		return err
	}
	defer lastWireDenyListener(ln)() // PT-001: a connector can never be aimed at the public listener
	// The operator admin interface (loopback only, its own listener, never the
	// public mux): `alixai audit run` and `audit quarantine` reach it through the
	// endpoint file it writes in the data directory. Its address is logged, not
	// printed: the "listening on" line below is the public API's.
	adminStop, adminToken, err := serveAdminStartToken(ctx, log, clk, core.DataDir.String(), w.admin)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("serve: the operator admin interface: %w", err)
	}
	defer adminStop()
	// The operator's door of the operations surface (cmd_ops.go): loopback only, behind the same
	// credential, and only when metrics.enabled is set.
	metricsStop, err := opsStartMetrics(ctx, log, w.resources.metricsConfig(), w.services, adminToken)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("serve: the metrics listener: %w", err)
	}
	defer metricsStop()
	scheme := "http"
	if srv.TLSEnabled() {
		scheme = "https"
	}
	log.Info("serving",
		slog.String("addr", ln.Addr().String()),
		slog.Bool("tls", srv.TLSEnabled()),
		slog.Bool("spike_endpoint", dev.SpikeEndpoint),
		slog.Bool("inference", off == nil),
		slog.Bool("api", w.auth != nil))
	if _, err := fmt.Fprintf(env.Stdout, "alixai: listening on %s://%s\n", scheme, ln.Addr()); err != nil {
		_ = ln.Close()
		return err
	}
	serveErr := srv.Serve(ctx, ln)
	served = true
	return serveErr
}

// describeUnmounted renders Server.Unmounted for an error: "area (needs a, b); ...".
func describeUnmounted(un map[string][]string) string {
	names := make([]string, 0, len(un))
	for n := range un {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s (needs %s)", n, strings.Join(un[n], ", ")))
	}
	return strings.Join(parts, "; ")
}

// inferenceOff says why the inference path did not start: reason is a fixed
// phrase safe to show an anonymous caller (it appears in /readyz), err is the
// detail for the log.
type inferenceOff struct {
	reason string
	err    error
}

// startInference verifies the configured models and, only if every check
// passes, builds the LocalAI client and registers it as the controller. On any
// failure it returns an empty registry and the reason: the service stays up,
// /readyz reports it, and nothing can reach a runtime (master plan 3 rule 14).
// An empty localai.models.controller is derived from the verified registry id
// first; a name that is configured is not judged here, the readiness provider
// refuses the start when it is not a verified id.
//
// The runtime itself is registered raw. The per-call budget is applied by the
// route that calls the model, so the registry's health refresh reaches the
// client's own Health (a wrapper would hide it).
func startInference(ctx context.Context, log *slog.Logger, clk clock.Clock, lcfg localai.Config, inf inferenceConfig) (*providers.Registry, *inferenceOff) {
	reg := providers.NewRegistry(clk)
	off := func(reason string, err error) (*providers.Registry, *inferenceOff) {
		return reg, &inferenceOff{reason: reason, err: err}
	}

	if len(inf.Models) == 0 {
		return off("no model is configured for verification (inference.models)", errors.New("inference.models is empty"))
	}
	mr, err := loadModelRegistry()
	if err != nil {
		return off("the model registry could not be loaded", err)
	}
	begin := time.Now()
	versions, err := verifyModels(ctx, mr, inf.ModelsDir, inf.Models)
	if err != nil {
		return off("model verification failed", err)
	}
	log.Info("models verified", slog.Int("models", len(inf.Models)), slog.Duration("dur", time.Since(begin)))

	// An empty localai.models.controller is the verified registry id that serves
	// the controller role, when there is exactly one (M0-18). A name that is
	// configured is checked by the readiness provider, which refuses the start
	// when it is not a verified id.
	if lcfg.Models.Controller == "" {
		name, derr := serveReadinessControllerModel(mr, inf.Models, "")
		if derr != nil {
			return off("no controller model is configured and none can be derived from inference.models (localai.models.controller)", derr)
		}
		lcfg.Models.Controller = name
		log.Info("localai.models.controller is derived from the verified registry id", slog.String("model", name))
	}

	// QualifiedVersions: the LocalAI versions every verified entry was
	// qualified with. An operator list may narrow that set and never widen it.
	// An empty result must not reach the adapter, where "empty" means "no
	// check" and would fail open.
	if len(lcfg.QualifiedVersions) > 0 {
		versions = intersectVersions(versions, lcfg.QualifiedVersions)
	}
	if len(versions) == 0 {
		return off("no LocalAI version is qualified for the configured models",
			errors.New("the allowed_runtime_versions of the verified models (narrowed by localai.qualified_versions) share no version"))
	}
	lcfg.QualifiedVersions = versions

	client, err := localai.New(lcfg, localai.WithLogger(log), localai.WithClock(clk))
	if err != nil {
		return off("the LocalAI adapter is misconfigured", err)
	}
	// The bare adapter, or the adapter under the LocalAI supervisor when supervisor.enabled is set (cmd_supervisor.go).
	if err := supervisorRegister(reg, log, clk, client); err != nil {
		return off("the controller role could not be registered", err)
	}
	// Make the first /readyz truthful: registration marks a role ok, and only
	// a probe says otherwise. Refresh bounds each probe by its own timeout.
	reg.Refresh(ctx)
	return reg, nil
}

// verifyModels checks every named registry entry against the file installed in
// dir (models.Verify: entry valid and activatable, hash pinned, size and
// SHA-256 equal) and returns the LocalAI versions all of them are qualified
// for. Any failure refuses the lot: one unverified model is enough to keep the
// inference path off (SEC-063).
func verifyModels(ctx context.Context, mr *models.Registry, dir string, ids []string) ([]string, error) {
	var qualified []string
	for i, id := range ids {
		entry, ok := mr.Get(id)
		if !ok {
			return nil, contracts.Newf(contracts.ErrUnavailable, "model %q is not in the model registry", id)
		}
		if entry.Backend.Runtime != localai.ProviderName {
			return nil, contracts.Newf(contracts.ErrUnavailable, "model %s is registered for runtime %q, not %q", id, entry.Backend.Runtime, localai.ProviderName)
		}
		if entry.Artifact.Filename == nil {
			return nil, contracts.Newf(contracts.ErrUnavailable, "model %s has no artifact file name in the registry", id)
		}
		name := *entry.Artifact.Filename
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return nil, contracts.Newf(contracts.ErrUnavailable, "model %s: the registry's file name %q is not a plain file name", id, name)
		}
		if err := models.Verify(entry, filepath.Join(dir, name), models.WithContext(ctx)); err != nil {
			return nil, err
		}
		if i == 0 {
			qualified = append([]string(nil), entry.Backend.AllowedRuntimeVersions...)
		} else {
			qualified = intersectVersions(qualified, entry.Backend.AllowedRuntimeVersions)
		}
	}
	if len(qualified) == 0 {
		return nil, contracts.New(contracts.ErrUnavailable, "no LocalAI version is qualified for every configured model (backend.allowed_runtime_versions)")
	}
	return qualified, nil
}

// intersectVersions returns the versions of a that b also lists, compared the
// way the adapter compares them (case and a leading "v" are cosmetic) and
// spelt as in a.
func intersectVersions(a, b []string) []string {
	norm := func(v string) string { return strings.TrimPrefix(strings.ToLower(v), "v") }
	in := map[string]bool{}
	for _, v := range b {
		in[norm(v)] = true
	}
	var out []string
	for _, v := range a {
		if in[norm(v)] {
			out = append(out, v)
		}
	}
	return out
}

// ---- the public API (M1-16) -------------------------------------------------

// v1Wiring is what `serve` hands the gateway for the public API.
type v1Wiring struct {
	// auth is identity.Middleware over the context-token verifier, or nil when
	// identity is not configured: with no middleware every /v1 route refuses
	// every request (gateway.Router.V1), which is what an API that cannot
	// verify a caller must do.
	auth func(http.Handler) http.Handler
	// services is what the route areas take their services from, by type.
	services *gateway.Services
	// coordinator is the action coordinator, whose service sweeps run as jobs.
	coordinator *actions.Coordinator
	// audit is the audit log. checkpointing is true when it holds the platform
	// signing key, and checkpointInterval is how often the sweep that signs the
	// chains whose head moved runs.
	audit              *audit.Log
	checkpointing      bool
	checkpointInterval time.Duration
	// admin is what the operator admin interface acts on: the process's caches
	// of what the store says is active (serveAdminControl).
	admin gateway.AdminControl
	// adapters builds and keeps the application adapters; the process closes
	// them as it stops.
	adapters *adapterSource
	// extraChecks and extraStarts are what the serve providers added (serveDeps.AddCheck,
	// AddService).
	extraChecks []gateway.Check
	extraStarts []func(ctx context.Context) (stop func())
	// resources is the governor and the disk guard (cmd_resources.go).
	resources *resourcesWiring
	// quotas is the per-tenant quota check in front of the /v1 routes (quotas_wire.go).
	quotas *quotaWire
}

// serveDeps is what a later route area's service provider may build from: the
// shared infrastructure of the process, already built and already checked. It
// is a struct of things that exist, not of services: a provider turns them into
// its own service and gives that to Services.
type serveDeps struct {
	Log       *slog.Logger
	Clock     clock.Clock
	DB        *store.DB
	DataDir   string
	Providers *providers.Registry
	Audit     *audit.Log
	Policy    *policy.Gateway
	// Services is where the provider puts what it builds: gateway.Provide.
	Services *gateway.Services
	// Config is the configuration registry serve loaded, nil when it ran on
	// config.Default.
	Config *config.Registry
	// AddCheck adds a readiness condition to /readyz.
	AddCheck func(gateway.Check)
	// AddService registers a long-running part of the process: start runs once
	// everything is built and returns the function that stops it and waits; that
	// function runs before the store closes.
	AddService func(start func(ctx context.Context) (stop func()))
	// Warm knows whether the controller model is probably loaded; the same one
	// gives a user turn its budget (localSelector).
	Warm *serveWarmTracker
	// Resources is the resource governor and its disk guard, built once in wireV1: the RAG stack
	// installs them on the job queue and the ops provider reads their figures (cmd_resources.go).
	// Nil in a test that builds serveDeps by hand.
	Resources *resourcesWiring
	// ErrorRing keeps the recent warnings and errors the logger saw, for the diagnostics bundle.
	ErrorRing *diag.ErrorRing
	// Reaudit is the re-audit watcher (cmd_reaudit.go). It is built here, with the caches it stops
	// traffic through, because its install hook is on the application service; its provider runs
	// the job on the RAG stack's pool. Nil in a test that builds serveDeps by hand.
	Reaudit *reauditStack
	// Actions is the action coordinator. A provider that runs a service operation of it as a job (the
	// reconciliation of uncertain outcomes, cmd_reconcile.go) takes it from here; the sweeps that need
	// no queue stay in serviceJobs. Nil in a test that builds serveDeps by hand.
	Actions *actions.Coordinator
	// External is what extWireEngineSeams handed back (nil with external mode off or not built in); the
	// external-mode provider of providers_external.go is the only reader.
	External any
}

// extWireSeamsIn is what the engine is built with before External Provider Mode may replace the
// selector and the guard.
type extWireSeamsIn struct {
	Selector dialogue.ProviderSelector
	Guard    dialogue.PromptGuard
	Packages dialogue.PackageSource
	Budget   providers.Budget
}

// extWireSeamsOut is the selector and the guard the engine is built with, and the handle the provider of
// the same area takes from serveDeps.External.
type extWireSeamsOut struct {
	Selector dialogue.ProviderSelector
	Guard    dialogue.PromptGuard
	Handle   any
}

// extWireEngineSeams is set by providers_external.go (the default build only). Nil means the engine keeps
// the local selector and LocalOnlyGuard, byte for byte.
var extWireEngineSeams func(extWireSeamsIn) extWireSeamsOut

// serveProvider provides the services of one route area (gateway.Provide into
// d.Services). It runs in wireV1 after the services of the public API are
// provided, in name order, and an error stops `serve` from starting.
type serveProvider func(ctx context.Context, d serveDeps) error

var (
	serveProvidersMu sync.Mutex
	serveProviders   = map[string]serveProvider{}
)

// registerServeProvider is how a route area outside this file gets its services
// built: its cmd_<area>.go calls it from init(), next to its routes_<area>.go
// in internal/gateway, and neither edits cmd_serve.go or wireV1. It panics on a
// duplicate name, which is a programming error found when the binary starts.
func registerServeProvider(name string, p serveProvider) {
	serveProvidersMu.Lock()
	defer serveProvidersMu.Unlock()
	if name == "" || p == nil {
		panic("cli: a serve provider needs a name and a function")
	}
	if _, dup := serveProviders[name]; dup {
		panic("cli: serve provider " + name + " registered twice")
	}
	serveProviders[name] = p
}

func registeredServeProviders() (names []string, byName map[string]serveProvider) {
	serveProvidersMu.Lock()
	defer serveProvidersMu.Unlock()
	byName = make(map[string]serveProvider, len(serveProviders))
	for n, p := range serveProviders {
		names = append(names, n)
		byName[n] = p
	}
	sort.Strings(names)
	return names, byName
}

// requiredAuditTypes are the event types the public API's components append.
// Every one must be registered in the registry the log is built with: an
// unregistered type is refused at append time, which would turn the first
// action of a running service into a failure. The core registry holds the
// action.* family; a registry assembled by hand must too.
var requiredAuditTypes = []audit.EventType{
	audit.EventPolicyDecision,
	audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionCommitting, audit.EventActionCommitted,
	audit.EventActionRejected, audit.EventActionExpired, audit.EventActionInvalidated, audit.EventActionUncertain,
	audit.EventPackageInstalled,
	"dialogue.turn",
}

// checkAuditRegistry fails when reg lacks an event type the service appends.
func checkAuditRegistry(reg *audit.Registry) error {
	for _, name := range requiredAuditTypes {
		if _, ok := reg.Lookup(name); !ok {
			return fmt.Errorf("the audit registry has no %q event type", name)
		}
	}
	return nil
}

// wireInput is what wireV1 builds the public API from.
type wireInput struct {
	log       *slog.Logger
	clk       clock.Clock
	db        *store.DB
	dataDir   string
	providers *providers.Registry
	off       *inferenceOff
	// checkpointKey signs the audit log's checkpoints; nil means they are not
	// signed. keys carries how often.
	checkpointKey *checkpointKey
	keys          keysConfig
	// config is the registry serve loaded; the providers read the sections of
	// packages that cannot hand their live pointer to a registry of their own
	// through it (ragSourcesSectionsOf). nil means config.Default.
	config *config.Registry
	// profile is the deployment profile the engine and the runtime boundary are
	// built from (cmd_serve_profile.go). The zero value is Lite.
	profile dctx.Profile
	// errorRing is the ring the logger feeds (opsWrapLogger); nil keeps no recent errors.
	errorRing *diag.ErrorRing
}

// wireV1 builds the services of the public API and returns what the gateway
// needs. It is the composition root of M1-14 and M1-16: one package source
// (gateway.Applications) backs the capability registry, the dialogue engine and
// the action coordinator, so all three decide against the same verified,
// active package; the policy gateway records every decision in the audit log
// (fail closed: a decision that cannot be recorded is a denial); the host's
// adapter, wrapped by adapters.Wrap, is the only path to an application; and
// the optional seams (decision stage, prompt guard, provider selector) are
// given their defaults explicitly: no decision layer, local runtime only.
//
// Every service goes into one gateway.Services by the type its area asks for
// (the action coordinator as gateway.ActionService, never as itself), the
// route areas that are not in this file add theirs through registerServeProvider,
// and the issuer keys that verify context tokens are the operator-provisioned
// ones in the store (identity.IssuerKeyStore), read on every token.
func wireV1(ctx context.Context, in wireInput) (*v1Wiring, error) {
	log, clk, db := in.log, in.clk, in.db
	auditReg := audit.DefaultRegistry
	if err := checkAuditRegistry(auditReg); err != nil {
		return nil, err
	}
	// Packages: signed by a publisher key the operator registered, installed
	// into quarantine, active only after the integration audit. The same trust
	// store holds the platform key that signs audit checkpoints.
	verifier := trust.NewVerifier(trust.NewStore(db, clk), clk)
	auditOpts := audit.Options{Clock: clk, Registry: auditReg, Logger: log, Verifier: verifier}
	if in.checkpointKey != nil {
		signing := in.checkpointKey.signing
		auditOpts.Signing = &signing
		auditOpts.CheckpointEvery = in.keys.CheckpointEvery
	}
	alg, err := audit.New(db, auditOpts)
	if err != nil {
		return nil, err
	}
	gw := policy.NewGateway(clk, policy.WithSink(audit.NewRecordSink[policy.Record](alg, audit.EventPolicyDecision,
		func(r policy.Record) (audit.Locator, error) {
			s := r.Subject
			return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
		})))

	core := apppkg.CoreVersionOrDev(version.Get().Version)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: in.dataDir, Verifier: verifier, CoreVersion: core})
	if err != nil {
		return nil, err
	}
	apps, err := gateway.NewApplications(inst, apppkg.Options{Verifier: verifier, CoreVersion: core}, clk, 0)
	if err != nil {
		return nil, err
	}
	registry := capabilities.NewRegistry(apps.Source(), capabilities.WithClock(clk))
	// One lookup of the transport registry, shared with `alixai audit run`.
	adapterSource := &adapterSource{
		apps: apps, log: log, cache: map[string]cachedAdapter{},
		transports: transportsSource{installer: inst, dataDir: in.dataDir},
	}
	// The re-audit watcher runs here, where traffic is served: it stops it through these caches
	// and sees every install the API takes (cmd_reaudit.go).
	reaudit, err := reauditCompose(reauditInput{
		DB: db, Clock: clk, Log: log, Audit: alg, DataDir: in.dataDir, Installer: inst, Verifier: verifier, Core: core,
		Registry: registry, Apps: apps, Adapters: adapterSource,
		Section: auditSection, Signing: reauditLoadSigning(log, auditSection), DeploymentID: identity.CurrentConfig().DeploymentID,
	})
	if err != nil {
		return nil, err
	}

	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: alg, Clock: clk, Logger: log,
		Packages: actions.PackageSourceFunc(apps.ActivePackage), Adapters: supervisorAdapters(in.providers, adapterSource),
	})
	if err != nil {
		return nil, err
	}
	supervisorBindReceipts(in.providers, co)
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		return nil, err
	}
	warm := serveWarmNewTracker(clk, serveReadinessLookupEnv)
	// The resource governor and its disk guard: one of each, here, before the providers (the RAG
	// stack installs them on the queue) and the engine (its turns are arbitrated by the governor).
	ctrl, ctrlBytes := "", uint64(0)
	if in.off == nil {
		ctrl, ctrlBytes = resourcesController(localaiSection, inferenceSection)
	}
	res, err := resourcesNew(resourcesInput{
		Log: log, Clock: clk, DataDir: in.dataDir, Providers: in.providers, Config: in.config,
		Controller: ctrl, ControllerBytes: ctrlBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("the resource governor: %w", err)
	}
	profile := serveProfileOrLite(in.profile)
	serveProfileLog(log, profile)
	// The optional decision layer (cmd_decision.go): off by default, and then the stage and the detectors below are the
	// no-op stage and the rules, exactly as before.
	dec, err := decWireNew(decWireInput{
		Log: log, Clock: clk, Config: in.config, Providers: in.providers, Audit: alg, Policy: gw, Packages: apps, Gov: res.Gov, Profile: profile.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("the decision layer: %w", err)
	}
	// External Provider Mode (providers_external.go, absent from the noexternal build and off by default)
	// may swap the selector and the guard for ones that can route a turn outward; off, they stay as they are.
	seams := extWireSeamsOut{Selector: localSelector{reg: in.providers, budget: profile.Budget(), off: in.off, warm: warm, res: res}, Guard: dialogue.LocalOnlyGuard{}}
	if extWireEngineSeams != nil {
		seams = extWireEngineSeams(extWireSeamsIn{Selector: seams.Selector, Guard: seams.Guard, Packages: apps, Budget: profile.Budget()})
	}
	eng, err := serveProfileNewEngine(dialogue.Deps{
		Sessions: sess, Registry: registry, Packages: apps, Policy: gw, Actions: co, Adapters: adapterSource,
		Config:    dialogue.Config{Profile: profile, StreamVerified: dialogue.Loaded().StreamVerified},
		Providers: seams.Selector,
		Audit:     alg, Clock: clk, Logger: log,
		Decision: dec.stage(), Detectors: dec.detectors(), Guard: seams.Guard,
	})
	if err != nil {
		return nil, err
	}
	dec.bind(eng) // Layer.Wrap(engine), the turn entry point the gateway is given as a ddec.Turner

	services := gateway.NewServices()
	qw, err := quotaWireNew(quotaWireInput{
		DB: db, Clock: clk, Log: log, Config: quotaWireSectionOf(in.config), Profile: profile.Name,
		Files: func() (gateway.FilesService, bool) { return gateway.Lookup[gateway.FilesService](services) },
	})
	if err != nil {
		return nil, fmt.Errorf("the tenant quotas: %w", err)
	}
	for _, perr := range []error{
		gateway.Provide(services, sess),
		gateway.Provide(services, eng),
		gateway.Provide[gateway.ActionService](services, co),
		gateway.Provide[gateway.ApplicationService](services, reaudit.applications(apps)),
		gateway.Provide(services, gw),
		gateway.Provide[actions.AdapterSource](services, adapterSource),
		gateway.Provide[audit.Appender](services, alg),
		dec.provide(services),
	} {
		if perr != nil {
			return nil, perr
		}
	}
	names, providersByName := registeredServeProviders()
	deps := serveDeps{Log: log, Clock: clk, DB: db, DataDir: in.dataDir, Providers: in.providers, Audit: alg, Policy: gw, Services: services, Config: in.config, Warm: warm,
		Resources: res, ErrorRing: in.errorRing, Reaudit: reaudit, Actions: co, External: seams.Handle}
	var addedChecks []gateway.Check
	var addedServices []func(ctx context.Context) (stop func())
	deps.AddCheck = func(c gateway.Check) { addedChecks = append(addedChecks, c) }
	deps.AddService = func(start func(ctx context.Context) (stop func())) { addedServices = append(addedServices, start) }
	dec.register(deps.AddService)
	for _, name := range names {
		if err := providersByName[name](ctx, deps); err != nil {
			return nil, fmt.Errorf("provide the services of %s: %w", name, err)
		}
	}

	w := &v1Wiring{
		services: services, coordinator: co, audit: alg, checkpointing: in.checkpointKey != nil, checkpointInterval: in.keys.CheckpointInterval,
		admin: serveAdminControl{registry: registry, apps: apps, adapters: adapterSource}, adapters: adapterSource,
		extraChecks: addedChecks, extraStarts: addedServices, resources: res, quotas: qw,
	}
	idc := identity.CurrentConfig()
	if idc.DeploymentID == "" {
		log.Warn("the /v1 API refuses every request: identity.deployment_id is not configured")
		return w, nil
	}
	keys := identity.NewIssuerKeyStore(db, clk, identity.IssuerKeyStoreOptions{Logger: log})
	if err := reportIssuerKeys(ctx, log, keys); err != nil {
		return nil, err
	}
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: idc.DeploymentID, Audience: idc.Audience, MaxLifetime: idc.MaxTokenLifetime,
		Keys: keys, Replay: identity.NewSQLiteReplayCache(db, clk, log), Clock: clk, Logger: log,
	})
	if err != nil {
		return nil, err
	}
	// Identity first, quotas behind it: only a verified scope ever reaches a bucket.
	w.auth = quotaWireAfter(identity.Middleware(v, identity.WithLogger(log)), qw)
	return w, nil
}

// reportIssuerKeys says at startup how many issuer keys the /v1 API will accept
// tokens from, and warns about the two states an operator should not miss: none
// at all (every token is refused until `alixai identity add-issuer-key`), and a
// published TEST-ONLY key (anyone can mint a token for it).
func reportIssuerKeys(ctx context.Context, log *slog.Logger, keys *identity.IssuerKeyStore) error {
	all, err := keys.List(ctx, "")
	if err != nil {
		return fmt.Errorf("read the issuer keys: %w", err)
	}
	active, dev := 0, 0
	for _, k := range all {
		if k.Status != identity.IssuerKeyActive {
			continue
		}
		active++
		if k.DevKey {
			dev++
		}
	}
	log.Info("issuer keys", slog.Int("active", active))
	if active == 0 {
		log.Warn("no issuer key is provisioned: the /v1 API refuses every token until `alixai identity add-issuer-key` adds one")
	}
	if dev > 0 {
		log.Warn("a published TEST-ONLY issuer key is active: anyone can mint tokens for it; never use it in production", slog.Int("keys", dev))
	}
	return nil
}

// finalCheckpoint signs a checkpoint for every chain whose head moved since its
// newest one, as the service stops. It never fails the stop.
func (w *v1Wiring) finalCheckpoint(log *slog.Logger) {
	if w == nil || w.audit == nil || !w.checkpointing {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	n, err := w.audit.CheckpointAll(ctx)
	if err != nil {
		log.Error("the final audit checkpoint failed", slog.String("code", string(contracts.CodeOf(err))))
		return
	}
	if n > 0 {
		log.Info("audit checkpoints signed at shutdown", slog.Int("chains", n))
	}
}

// localSelector is the provider selector of Local mode, and the default of the
// external-provider seam: every text role is served by the one local
// controller, behind the request budget. The runtime is looked up on each call
// so a runtime that is off (inference.models not verified) fails each turn
// closed with unavailable, and no other provider is ever tried.
type localSelector struct {
	reg    *providers.Registry
	budget providers.Budget
	off    *inferenceOff
	// warm gives the turn that may have to load the controller (nothing has
	// answered since the start, or the runtime unloaded it after sitting idle)
	// the cold-load budget instead of budget. Nil: budget always.
	warm *serveWarmTracker
	// res puts every model call under the resource governor: it is a live turn that background
	// work yields to, and a call that may load the model is admitted first. Nil: not arbitrated.
	res *resourcesWiring
}

func (s localSelector) Select(_ context.Context, _ contracts.Scope, role providers.Role) (dialogue.Provider, error) {
	if s.off != nil {
		return dialogue.Provider{}, contracts.New(contracts.ErrUnavailable, "the inference path is off: "+s.off.reason)
	}
	gen, err := s.reg.TextGenerator(providers.RoleController)
	if err != nil {
		return dialogue.Provider{}, err
	}
	if !s.reg.Usable(providers.RoleController) {
		return dialogue.Provider{}, contracts.New(contracts.ErrUnavailable, "the controller model is not available").
			WithDetails("role", string(role))
	}
	g, err := s.warm.budgeted(gen, s.budget)
	if err != nil {
		return dialogue.Provider{}, err
	}
	return dialogue.Provider{Name: "local", Local: true, Generator: s.res.generator(g, s.warm)}, nil
}

// ---- adapters ---------------------------------------------------------------

type cachedAdapter struct {
	hash string
	ad   adapters.AppAdapter
	// err is a refusal that is the same on every call for this package: there
	// is no transport for what it declares (transportsNoAdapter). It is kept so
	// a package nobody can serve costs one open, not one per request.
	err error
}

// adapterSource is the actions.AdapterSource of the service: the adapter of an
// application is built for the package that is active now (a new package gets a
// new adapter), by the transport registered for the endpoint kind its manifest
// declares (transports.go: the same lookup `alixai audit run` uses), wrapped
// with adapters.Wrap so the scope check, the time and concurrency bounds and the
// result re-validation hold whatever the transport, and refused for an
// application that has no active package.
//
// An application whose declared kind has no transport has no adapter, and the
// service says so: every call for it is unavailable with the reason, and the
// reason is logged once per package. There is no default adapter and no skip.
type adapterSource struct {
	apps       *gateway.Applications
	log        *slog.Logger
	transports transportsSource

	mu    sync.Mutex
	cache map[string]cachedAdapter
	// building holds one lock per application, so the builds of one application
	// are serialised (see Adapter).
	building map[string]*sync.Mutex
}

func (s *adapterSource) buildLock(applicationID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.building == nil {
		s.building = map[string]*sync.Mutex{}
	}
	m := s.building[applicationID]
	if m == nil {
		m = new(sync.Mutex)
		s.building[applicationID] = m
	}
	return m
}

func (s *adapterSource) cached(applicationID, hash string) (cachedAdapter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[applicationID]
	return c, ok && c.hash == hash
}

// store keeps c for the application and closes the adapter it replaces.
func (s *adapterSource) store(applicationID string, c cachedAdapter) {
	s.mu.Lock()
	old, had := s.cache[applicationID]
	s.cache[applicationID] = c
	s.mu.Unlock()
	if had {
		transportsClose(old.ad)
	}
}

func (s *adapterSource) Adapter(ctx context.Context, applicationID string) (adapters.AppAdapter, error) {
	pkg, err := s.apps.Package(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	hash := pkg.ContentHash()
	if c, ok := s.cached(applicationID, hash); ok {
		return c.ad, c.err
	}
	// One build per application at a time: two first requests for a package must
	// not both call the factory, or a transport that opens a connection or a
	// session when it is built would leak one. The second waits and finds the
	// first one's adapter.
	lk := s.buildLock(applicationID)
	lk.Lock()
	defer lk.Unlock()
	if c, ok := s.cached(applicationID, hash); ok {
		return c.ad, c.err
	}
	ad, plan, err := s.transports.adapter(ctx, applicationID, hash)
	if err != nil {
		reason, kind, _ := transportsUnavailableReason(err)
		s.log.Warn("no adapter for the application", slog.String("application_id", applicationID),
			slog.String("reason", reason), slog.String("kind", kind), slog.String("code", string(contracts.CodeOf(err))))
		if transportsNoAdapter(err) {
			s.store(applicationID, cachedAdapter{hash: hash, err: err})
			if plan != nil && len(plan.Events) > 0 {
				s.log.Warn("events endpoints are declared and not consumed by this build", slog.String("application_id", applicationID), slog.Any("endpoints", plan.Events))
			}
			return nil, err
		}
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the application's adapter could not be opened")
	}
	if plan != nil && len(plan.Events) > 0 {
		s.log.Warn("events endpoints are declared and not consumed by this build", slog.String("application_id", applicationID), slog.Any("endpoints", plan.Events))
	}
	s.store(applicationID, cachedAdapter{hash: hash, ad: ad})
	return ad, nil
}

// forget drops the adapter built for an application (closing it, when it can
// be closed), so the next call builds one again from whatever the store says is
// active.
func (s *adapterSource) forget(applicationID string) {
	s.mu.Lock()
	old, had := s.cache[applicationID]
	delete(s.cache, applicationID)
	s.mu.Unlock()
	if had {
		transportsClose(old.ad)
	}
}

// close closes every adapter the source built, as the service stops.
func (s *adapterSource) close() {
	s.mu.Lock()
	all := s.cache
	s.cache = map[string]cachedAdapter{}
	s.mu.Unlock()
	for _, c := range all {
		transportsClose(c.ad)
	}
}

// serveAdminControl is what the admin interface acts on: every cache of this
// process that holds something about an application. Refresh and Deactivate
// bring all of them back in line with the store at once; none can add anything
// the store does not say is active.
type serveAdminControl struct {
	registry *capabilities.Registry
	apps     *gateway.Applications
	adapters *adapterSource
}

func (c serveAdminControl) Refresh(ctx context.Context, applicationID string) error {
	c.apps.AdminForget(applicationID)
	return c.registry.Refresh(ctx, applicationID)
}

func (c serveAdminControl) Deactivate(applicationID string) {
	c.registry.Deactivate(applicationID)
	c.apps.AdminForget(applicationID)
	c.adapters.forget(applicationID)
}

// serveAdminStart brings the operator admin interface up: a loopback-only
// listener of its own, a fresh credential, and the endpoint file the CLI reads
// to find both. It returns the function that stops it and removes the file. An
// admin interface that cannot start refuses the start of the service: without it
// the claim that a quarantine takes effect at once would quietly not hold.
func serveAdminStart(ctx context.Context, log *slog.Logger, clk clock.Clock, dataDir string, control gateway.AdminControl) (stop func(), err error) {
	stop, _, err = serveAdminStartToken(ctx, log, clk, dataDir, control)
	return stop, err
}

// serveAdminStartToken is serveAdminStart that also returns the operator credential it made, which
// the metrics listener's /diagnostics (cmd_ops.go) checks too: one credential for the operator.
func serveAdminStartToken(ctx context.Context, log *slog.Logger, clk clock.Clock, dataDir string, control gateway.AdminControl) (stop func(), token string, err error) {
	token, err = gateway.NewAdminToken()
	if err != nil {
		return nil, "", err
	}
	adm, err := gateway.NewAdminServer(gateway.AdminOptions{Control: control, Token: token, Logger: log, Clock: clk})
	if err != nil {
		return nil, "", err
	}
	ln, err := gateway.AdminListen()
	if err != nil {
		return nil, "", err
	}
	denyRelease := lastWireDenyListener(ln) // PT-001: nor at the operator admin interface
	if err := gateway.WriteAdminEndpoint(dataDir, gateway.AdminEndpoint{
		URL: "http://" + ln.Addr().String(), Token: token, PID: os.Getpid(), StartedAt: clk.Now().UTC(),
	}); err != nil {
		_ = ln.Close()
		denyRelease()
		return nil, "", err
	}
	log.Info("admin interface", slog.String("addr", ln.Addr().String()))
	actx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := adm.Serve(actx, ln); err != nil {
			log.Error("the admin interface stopped", slog.String("code", string(contracts.CodeOf(err))))
		}
	}()
	return func() {
		cancel()
		<-done
		gateway.RemoveAdminEndpoint(dataDir, token)
		denyRelease()
	}, token, nil
}

// ---- service jobs -----------------------------------------------------------

// Periods of the service sweeps. Variables so a test can shorten them.
var (
	recoverEvery = time.Minute
	expireEvery  = 30 * time.Second
)

// serviceJob is a periodic maintenance job of the process itself. It works
// across tenants, as the dispatcher side of internal/jobs does, and is never
// reachable from a request.
type serviceJob struct {
	name  string
	every time.Duration
	run   func(ctx context.Context) error
}

// serviceJobs are the sweeps of the action coordinator: resolving commits that
// a dead worker left in committing (never by committing again) and expiring
// preparations that lapsed; and, when the platform checkpoint signing key is
// provisioned, signing a checkpoint for every audit chain whose head moved
// (the events appended inside a caller's transaction, the write-ahead ones, do
// not trigger a checkpoint of their own).
func (w *v1Wiring) serviceJobs(log *slog.Logger) []serviceJob {
	report := func(name string, rep actions.RecoverReport) {
		if rep.Examined > 0 {
			log.Info("service job", slog.String("job", name), slog.Int("examined", rep.Examined), slog.Int("recorded", rep.Recorded),
				slog.Int("reopened", rep.Reopened), slog.Int("uncertain", rep.Uncertain), slog.Int("expired", rep.Expired), slog.Int("failed", rep.Failed))
		}
	}
	jobs := []serviceJob{
		{name: "actions.recover", every: recoverEvery, run: func(ctx context.Context) error {
			rep, err := w.coordinator.Recover(ctx, actions.RecoverOptions{})
			report("actions.recover", rep)
			return err
		}},
		{name: "actions.expire_due", every: expireEvery, run: func(ctx context.Context) error {
			rep, err := w.coordinator.ExpireDue(ctx)
			report("actions.expire_due", rep)
			return err
		}},
	}
	if w.quotas != nil {
		jobs = append(jobs, serviceJob{name: "quotas.prune", every: time.Hour, run: w.quotas.prune})
	}
	if w.audit != nil && w.checkpointing && w.checkpointInterval > 0 {
		jobs = append(jobs, serviceJob{name: "audit.checkpoint", every: w.checkpointInterval, run: func(ctx context.Context) error {
			signed, err := w.audit.CheckpointAll(ctx)
			if signed > 0 {
				log.Info("service job", slog.String("job", "audit.checkpoint"), slog.Int("chains", signed))
			}
			return err
		}})
	}
	return jobs
}

// startServiceJobs runs each job on its own period until ctx ends and returns a
// function that waits for them to stop. A job never overlaps itself, a failure
// or a panic is logged and the next period tries again, and nothing a job does
// stops the service.
func startServiceJobs(ctx context.Context, log *slog.Logger, jobs ...serviceJob) (wait func()) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(j.every)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Error("service job panicked", slog.String("job", j.name), slog.Any("panic", r))
						}
					}()
					if err := j.run(ctx); err != nil && ctx.Err() == nil {
						log.Error("service job failed", slog.String("job", j.name), slog.String("code", string(contracts.CodeOf(err))))
					}
				}()
			}
		}()
	}
	return wg.Wait
}
