package cli

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/metrics"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/resources"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The resource governor of `alixai serve` (master plan 3 rule 12, 17.2, 18; M3-30).
//
// internal/resources is a library; this file is where it becomes part of the running process.
// wireV1 builds one Governor and one DiskGuard from the "resources" section (resourcesNew) and
// carries them on serveDeps; nothing else builds a second one.
//
//   - Queue: ragCompose gives the DiskGuard to ingest.NewPause and installs
//     gov.Policy(pause.Policy(nil)) as the jobs policy, so background work (classes 5 and 6)
//     leases nothing while a live turn is active, while memory is unknown or at its ceiling, or
//     while the disk is under pressure. There is one queue and one pool: this file builds neither.
//   - Loop: serve runs gov.Run for its whole life (resourcesWiring.start). Without Run the readings
//     stay unknown and background work holds from the start, which is the safe direction.
//   - Turns: localSelector wraps the controller's generator (resourcesGenerator). Each model call
//     is a live interactive piece of work, BeginInteractive(jobs.ClassTurn), from the call to the
//     end of its stream (Close, or the terminal Recv), and a call that may have to load the model
//     (serveWarmTracker.cold) is first admitted with AdmitModelLoad, which rejects, with a typed
//     resource_exhausted, instead of evicting anything.
//   - Boundaries: resources.interactive_up_to_class and background_from_class are not configured
//     here; they are copied from the queue's effective jobs configuration (resourcesClassBoundaries).
//
// # Unloading: one policy, LocalAI's own
//
// LocalAI's idle watchdog (LOCALAI_WATCHDOG_IDLE_TIMEOUT) is the only thing that unloads a model
// for now. The adapter's allowlist (adapters/localai) does not reach the routes that report a
// loaded model's memory or unload it, so the LocalAI client does not implement
// resources.MemoryReporter or resources.Unloader, resources.RuntimeFromRegistry finds no runtime,
// and the governor's own idle unload (pins, min_warm) has nothing to act on. Two unload policies
// therefore never run together, and M0-19's cold-load budget already covers the first turn after
// an unload. The governor's policy stays available for a runtime that can report memory and
// unload, which is a separate decision on that allowlist.
//
// # Admission uses the registry's measured memory
//
// With no runtime report the governor cannot see the model's memory, so a model load is judged by
// the model registry's own figure: the measured peak resident set (memory.peak_rss_mb), then the
// steady one, then the artifact's size. A model with none of them is not judged and the log says so.
const (
	resourcesSectionName = "resources"
	// resourcesNoEstimateWarning is logged at start when the controller model has no memory figure.
	resourcesNoEstimateWarning = "the controller model has no measured memory and no size in the model registry: its load is not admitted against the memory budget"
)

// resourcesSections are the sections this file and cmd_ops.go read: resources and metrics. On
// config.Default the packages register them; a registry other than Default gets copies from
// resourcesRegisterSections, found again by resourcesSectionsOf.
type resourcesSections struct {
	gov     resources.Config
	metrics metrics.Config
}

var resourcesSectionsByRegistry sync.Map // *config.Registry -> *resourcesSections

// resourcesRegisterSections registers the resources and metrics sections on reg, for a registry
// other than config.Default (tests, tools); on Default their own packages' init functions do it.
func resourcesRegisterSections(reg *config.Registry) {
	s := &resourcesSections{gov: resources.DefaultConfig(), metrics: metrics.DefaultConfig()}
	reg.Register(resourcesSectionName, &s.gov)
	reg.Register(metrics.SectionName, &s.metrics)
	resourcesSectionsByRegistry.Store(reg, s)
}

// resourcesSectionsOf returns the sections as the registry serve ran on loaded them.
func resourcesSectionsOf(reg *config.Registry) resourcesSections {
	if reg != nil && reg != config.Default {
		if v, ok := resourcesSectionsByRegistry.Load(reg); ok {
			return *v.(*resourcesSections)
		}
	}
	return resourcesSections{gov: resources.FromConfig(), metrics: metrics.FromConfig()}
}

// resourcesClassBoundaries are interactive_up_to_class and background_from_class as the queue
// runs with them: the jobs section's, defaulted, with interactive_up_to_class raised to the RAG
// value (ragEffectiveJobs). The governor copies them, so a live turn makes yield exactly the
// classes the queue's own yield rule treats as background.
func resourcesClassBoundaries(jc jobs.Config) (interactiveUpTo, backgroundFrom int) {
	jc = ragEffectiveJobs(jc)
	interactiveUpTo, backgroundFrom = jc.InteractiveUpTo, jc.BackgroundFrom
	if interactiveUpTo == 0 {
		interactiveUpTo = jobs.DefaultInteractiveUpTo
	}
	if backgroundFrom == 0 {
		backgroundFrom = jobs.DefaultBackgroundFrom
	}
	return interactiveUpTo, backgroundFrom
}

// resourcesInput is what resourcesNew builds the governor from.
type resourcesInput struct {
	Log       *slog.Logger
	Clock     clock.Clock
	DataDir   string
	Providers *providers.Registry
	// Config is the registry serve loaded; nil means config.Default.
	Config *config.Registry
	// Controller is the controller model's name and the memory its load is expected to take; an
	// empty name (the inference path is off) means no model-load admission.
	Controller      string
	ControllerBytes uint64
	// MemoryProbe and DiskProbe replace the operating system's readings; nil reads the system.
	// For tests, which must not depend on the host's memory or free disk.
	MemoryProbe resources.MemoryProbe
	DiskProbe   resources.DiskProbe
}

// resourcesWiring is the governor, its disk guard and what the rest of serve takes from them.
type resourcesWiring struct {
	Gov  *resources.Governor
	Disk *resources.DiskGuard
	// Metrics is the "metrics" section serve ran with.
	Metrics metrics.Config

	controller      string
	controllerBytes uint64
	log             *slog.Logger
	queue           atomic.Pointer[jobs.Queue]
}

// resourcesNew builds the Governor and the DiskGuard from the resources section.
func resourcesNew(in resourcesInput) (*resourcesWiring, error) {
	sections := resourcesSectionsOf(in.Config)
	cfg := sections.gov
	cfg.InteractiveUpTo, cfg.BackgroundFrom = resourcesClassBoundaries(ragSourcesSectionsOf(in.Config).jobs)

	gov, err := resources.New(resources.Options{
		Config: cfg, Clock: in.Clock, Logger: in.Log, Memory: in.MemoryProbe,
		// Nil unless a registered provider reports its memory and unloads (see the file comment).
		Runtime: resources.RuntimeFromRegistry(in.Providers),
	})
	if err != nil {
		return nil, err
	}
	disk, err := resources.NewDiskGuard(resources.DiskOptions{Config: cfg, Paths: []string{in.DataDir}, Probe: in.DiskProbe, Clock: in.Clock, Logger: in.Log})
	if err != nil {
		return nil, err
	}
	if in.Controller != "" && in.ControllerBytes == 0 && in.Log != nil {
		in.Log.Warn(resourcesNoEstimateWarning, slog.String("model", in.Controller))
	}
	log := in.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &resourcesWiring{
		Gov: gov, Disk: disk, Metrics: sections.metrics,
		controller: in.Controller, controllerBytes: in.ControllerBytes, log: log,
	}, nil
}

// start runs the governor loop until ctx ends and returns the function that waits for it. It
// does one refresh before its first wait, so the queue's background hold (unknown memory counts
// as pressure) lifts as serve starts.
func (w *resourcesWiring) start(ctx context.Context) (stop func()) {
	if w == nil || w.Gov == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Gov.Run(ctx)
	}()
	return func() { <-done }
}

// setQueue records the job queue ragCompose built, for the operations surface. Providers run in
// name order, so the ops provider runs before the RAG one and reads the queue when asked.
func (w *resourcesWiring) setQueue(q *jobs.Queue) { w.queue.Store(q) }

// jobQueue is the job queue, nil until the RAG stack has built it.
func (w *resourcesWiring) jobQueue() *jobs.Queue { return w.queue.Load() }

// resourcesController names the controller model and estimates the memory its load takes, from
// the model registry the binary carries. An unknown name or a registry that cannot be read gives
// no estimate (the load is then not judged) and never an error: the inference path reports its
// own failures.
func resourcesController(lcfg localai.Config, inf inferenceConfig) (name string, bytes uint64) {
	mr, err := loadModelRegistry()
	if err != nil {
		return "", 0
	}
	name = lcfg.Models.Controller
	if name == "" {
		if name, err = serveReadinessControllerModel(mr, inf.Models, ""); err != nil {
			return "", 0
		}
	}
	entry, ok := mr.Get(name)
	if !ok {
		return name, 0
	}
	return name, resourcesModelBytes(entry)
}

// resourcesModelBytes is the memory a model is expected to hold once loaded: the registry's
// measured peak resident set, else its steady one, else the size of the artifact. Zero means the
// registry says nothing.
func resourcesModelBytes(e models.Entry) uint64 {
	const mib = 1 << 20
	for _, mb := range []*float64{e.Memory.PeakRSSMB, e.Memory.SteadyRSSMB} {
		if mb != nil && *mb > 0 {
			return uint64(math.Ceil(*mb * mib))
		}
	}
	if e.Artifact.SizeBytes != nil && *e.Artifact.SizeBytes > 0 {
		return uint64(*e.Artifact.SizeBytes)
	}
	return 0
}

// generator wraps the controller's generator so every model call is arbitrated by the governor.
// A nil wiring returns gen as it is.
func (w *resourcesWiring) generator(gen providers.TextGenerator, warm *serveWarmTracker) providers.TextGenerator {
	if w == nil || w.Gov == nil {
		return gen
	}
	return resourcesGenerator{w: w, inner: gen, warm: warm}
}

// resourcesGenerator is the controller's generator under the governor. Each Chat, and each
// ChatStream until it ends, is a live turn: background work yields to it. A call that may have to
// load the model is admitted against the memory budget first.
type resourcesGenerator struct {
	w     *resourcesWiring
	inner providers.TextGenerator
	warm  *serveWarmTracker
}

// begin registers the call with the governor and returns the release, which is safe to call more
// than once and also runs when ctx ends, so a stream nobody closes cannot hold background work
// back for good. An admission that fails returns the governor's typed error and registers nothing.
func (g resourcesGenerator) begin(ctx context.Context) (release func(), err error) {
	gov := g.w.Gov
	var ticket *resources.Ticket
	if g.w.controller != "" && g.w.controllerBytes > 0 && g.warm.cold() {
		// The model may have to be loaded now. Rejected, never made room for by unloading.
		if ticket, err = gov.AdmitModelLoad(ctx, resources.ModelLoad{
			ID: g.w.controller, Role: providers.RoleController, Bytes: g.w.controllerBytes,
		}); err != nil {
			return nil, err
		}
	}
	endTurn := gov.BeginInteractive(jobs.ClassTurn)
	endUse := func() {}
	if g.w.controller != "" {
		endUse = gov.UseModel(g.w.controller)
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			endUse()
			endTurn()
			ticket.Release()
		})
	}
	// The context's own end runs finish; the caller's release also stops that watch. finish never
	// reads stop, so the two cannot race on it.
	stop := context.AfterFunc(ctx, finish)
	return func() {
		stop()
		finish()
	}, nil
}

// Chat implements providers.TextGenerator.
func (g resourcesGenerator) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	release, err := g.begin(ctx)
	if err != nil {
		return providers.ChatResult{}, err
	}
	defer release()
	return g.inner.Chat(ctx, scope, req)
}

// ChatStream implements providers.TextGenerator. The turn lasts until the stream ends.
func (g resourcesGenerator) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	release, err := g.begin(ctx)
	if err != nil {
		return nil, err
	}
	s, err := g.inner.ChatStream(ctx, scope, req)
	if err != nil {
		release()
		return nil, err
	}
	return resourcesStream{inner: s, release: release}, nil
}

// resourcesStream ends the turn when the stream ends: on its last chunk, on any error and on Close.
type resourcesStream struct {
	inner   providers.ChatStream
	release func()
}

func (s resourcesStream) Recv() (providers.ChatChunk, error) {
	c, err := s.inner.Recv()
	if err != nil || c.Done {
		s.release()
	}
	return c, err
}

func (s resourcesStream) Close() error {
	err := s.inner.Close()
	s.release()
	return err
}

// metricsConfig is the "metrics" section serve ran with; the default (disabled) for a nil wiring.
func (w *resourcesWiring) metricsConfig() metrics.Config {
	if w == nil {
		return metrics.DefaultConfig()
	}
	return w.Metrics
}
