package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/triggers"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The re-audit watcher in `alixai serve` (master plan 4.5 and 9.4, release gate 12; M5-23).
//
// internal/integrationaudit/triggers (M5-10) compares what an application is now with what its
// last audit recorded, audits again after a change and stops live traffic first when the change is
// critical. It is a library, and it has to run where traffic is served: stopping traffic means
// the capability registry, the package cache and the adapter cache of THIS process, and the
// admin client of `alixai audit quarantine` cannot confirm any of them. This file is where the
// process runs it.
//
// # What triggers it
//
//   - An install. The only way a package gets into a running server is POST /v1/applications
//     (gateway.ApplicationService.Register), so serve gives the API a wrapper of that service that
//     calls Watcher.PackageInstalled after every install (idempotent re-registrations too: a look
//     with nothing changed audits nothing). A package installed by `alixai package install`, in
//     another process, is found by the next sweep.
//   - MCP drift. An MCP adapter that finds a mapped tool changed after it was approved calls the
//     observer of transport_mcp.go (mcp.Config.OnDrift); serve registers one that runs
//     Watcher.ReportDrift on a goroutine of its own, under a context that does not end with the
//     adapter call (it descends from the stack's lifetime, not from the call), because the hook runs on
//     the call path of the adapter and the watcher's own adapter calls it while the watcher holds
//     the lock of the application. The drifted adapter has already refused every call of the
//     endpoint, sticky, so nothing reaches the application in the meantime. The poll of every
//     look (Config.Drifts) reads the adapter of the CURRENT instance only (reauditStack.drifts).
//   - The clock. A loop calls Watcher.Sweep at start and every reauditSweepEvery; Sweep queues one
//     watch job per application per period, and the job runs Watcher.HandleJob.
//
// # The job and its authority
//
// triggers.JobType runs on the RAG stack's one queue and one pool (ragStack.shareJobs; there is no
// second pool) as class 6, maintenance, so the resource governor holds it behind every live turn
// and every higher class. The pool accepts triggers.ServiceIdentity through reauditAuthority and
// nothing else: that identity, that type, the scope the watcher itself makes (triggers.JobScope)
// and a payload that names the very application of the scope. The authority does not ask for an
// active package: the job also finishes the audit of a package the watcher has just taken out of
// service. A declared identity alone grants nothing (jobs.ServiceAuthorizer).
//
// # What it can never do
//
// The Auditor the watcher holds (reauditAuditor) has no Activate and no AcceptWarning, and the
// Runner under it is built with no operator authorised to accept anything: whatever a re-audit
// finds, only an operator starts traffic again. A critical change stops traffic before anything
// is audited or announced (triggers/doc.go); if the registry of this process still serves the
// application afterwards, that is reaudit.failed, a critical notice and a retryable error, and
// the stop is confirmed again by every later look. When a hook that must stop traffic fails, a
// watch job is queued so that the confirmation is retried and not forgotten.
//
// # A known limitation (M1-17)
//
// Runner.AcceptWarning works only on a quarantined package. A re-audit of a package that is live
// and has no critical problem (a changed document, a new application version, a baseline that is
// missing) leaves the old activation in force and stores a report; if that report carries a new
// warning, nobody can accept it while the package serves: the operator has to quarantine the package
// (`alixai audit quarantine`), accept the warning and activate it again. The watcher does not work
// around this and never accepts a warning itself. TestReauditLimitationNewWarningsOfALivePackage
// states it as it is.

const (
	// reauditProviderName is the name of the serve provider. Providers run in name order and this
	// one needs the RAG stack's queue and pool, which "rag" builds.
	reauditProviderName = "reaudit"

	// reauditJobAttempts is how often the queue leases one watch job before it gives up: a registry
	// that still serves a quarantined application is an unavailable error and is retried.
	reauditJobAttempts = 5

	// reauditHookTimeout bounds the work an install or a drift triggers outside any request.
	reauditHookTimeout = 2 * time.Minute
	// reauditPollTimeout bounds the lookup of the active package for the drift poll.
	reauditPollTimeout = 5 * time.Second
	// reauditStopGrace is how long stopping waits for hooks in flight before it cancels them.
	reauditStopGrace = 10 * time.Second
	// reauditKeyMax is the longest idempotency key passed to the queue as it is; a longer one (a
	// drift names its endpoint, tool and kind) is replaced by its digest.
	reauditKeyMax = 200
)

// Test seams, replaced only by tests.
var (
	// reauditSweepEvery is how often serve queues the watch jobs.
	reauditSweepEvery = triggers.DefaultSweepPeriod
	// reauditOnCompose sees the stack serve builds.
	reauditOnCompose func(*reauditStack)
	// reauditOnSweep sees the result of every sweep.
	reauditOnSweep func(triggers.SweepResult, error)
)

func init() { registerServeProvider(reauditProviderName, reauditProvide) }

// reauditInput is everything reauditCompose builds from. wireV1 fills it from the process; a test
// fills it with the same parts and, where it needs to see an order, with the taps at the end.
type reauditInput struct {
	DB        *store.DB
	Clock     clock.Clock
	Log       *slog.Logger
	Audit     *audit.Log
	DataDir   string
	Installer *apppkg.Installer
	Verifier  *trust.Verifier
	// Core is the core version the Runner audits against and the Watcher compares.
	Core string
	// Registry, Apps and Adapters are the caches of this process that hold something about an
	// application: the ones traffic is served from.
	Registry *capabilities.Registry
	Apps     *gateway.Applications
	Adapters *adapterSource
	// Section is the integration_audit section (the model and runtime profile of a report, how old
	// a report may be). Who may accept a warning is not read: nobody can through this stack.
	Section auditConfig
	// Signing is the platform key that signs reports; nil leaves the stack unable to audit (a stop
	// needs no key).
	Signing *trust.SigningKey
	// DeploymentID is the deployment of the scope of the watch jobs; empty takes the audit's own.
	DeploymentID string
	// SweepEvery is the period of the sweep; zero takes reauditSweepEvery.
	SweepEvery time.Duration

	// Taps, for a test. Checks replaces the registry of checks of the Runner; Notifier sees every
	// notice as well; WrapAuditor and WrapRegistry see every call of the watcher; Drifts replaces
	// the poll of the current adapter; OnReport sees what an install or a drift hook did.
	Checks       *integrationaudit.Registry
	Notifier     triggers.Notifier
	WrapAuditor  func(triggers.Auditor) triggers.Auditor
	WrapRegistry func(triggers.Registry) triggers.Registry
	Drifts       func(applicationID string) []triggers.Drift
	OnReport     func(hook, applicationID string, res triggers.Result, err error)
}

// reauditStack is the re-audit watcher of the process and what feeds it.
type reauditStack struct {
	in      reauditInput
	log     *slog.Logger
	watcher *triggers.Watcher
	enq     *reauditEnqueuer

	// life is the context of everything the stack does outside a request: it descends from the
	// background, never from an adapter call, and ends when the stack is stopped for good.
	life   context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// reauditCompose builds the watcher: a Runner that can audit and quarantine, the registry of this
// process, the adapters the watcher looks at the application through, the poll of the current
// adapter instance and the enqueuer over the queue (bound later, by the provider, once the RAG
// stack has built it).
func reauditCompose(in reauditInput) (*reauditStack, error) {
	switch {
	case in.DB == nil:
		return nil, errors.New("reaudit: no store")
	case in.Installer == nil || in.Verifier == nil:
		return nil, errors.New("reaudit: no package installer or trust verifier")
	case in.Registry == nil || in.Apps == nil || in.Adapters == nil:
		return nil, errors.New("reaudit: no capability registry, package source or adapter source: a stop it cannot confirm must not be claimed")
	case in.Audit == nil:
		return nil, errors.New("reaudit: no audit log")
	case in.Core == "":
		return nil, errors.New("reaudit: no core version")
	}
	if in.Clock == nil {
		in.Clock = clock.Real{}
	}
	if in.Log == nil {
		in.Log = slog.New(slog.DiscardHandler)
	}
	if in.DeploymentID == "" {
		in.DeploymentID = integrationaudit.DefaultDeploymentID
	}
	if in.SweepEvery <= 0 {
		in.SweepEvery = reauditSweepEvery
	}

	// What this process holds about an application is dropped together, and the same hands do it
	// when the Runner quarantines and when the watcher confirms (serveAdminControl).
	control := serveAdminControl{registry: in.Registry, apps: in.Apps, adapters: in.Adapters}
	rcfg := integrationaudit.Config{
		DB: in.DB, Clock: in.Clock, Installer: in.Installer, DataDir: in.DataDir, Verifier: in.Verifier,
		CoreVersion: in.Core, Signing: in.Signing, Log: in.Audit, Checks: in.Checks,
		Capabilities: control, Profile: in.Section.Profile,
		// No Authorizer: nobody is authorised to accept a warning through this Runner.
	}
	if in.Section.MaxReportAgeHours > 0 {
		rcfg.MaxReportAge = time.Duration(in.Section.MaxReportAgeHours) * time.Hour
	}
	runner, err := integrationaudit.NewRunner(rcfg)
	if err != nil {
		return nil, err
	}

	life, cancel := context.WithCancel(context.Background())
	st := &reauditStack{in: in, log: in.Log, enq: &reauditEnqueuer{}, life: life, cancel: cancel}

	var auditor triggers.Auditor = reauditAuditor{runner: runner, rag: &stack{db: in.DB, clk: in.Clock}}
	var registry triggers.Registry = reauditRegistry{control: control, reg: in.Registry}
	if in.WrapAuditor != nil {
		auditor = in.WrapAuditor(auditor)
	}
	if in.WrapRegistry != nil {
		registry = in.WrapRegistry(registry)
	}
	var notifier triggers.Notifier = reauditNotifier(in.Log)
	if in.Notifier != nil {
		notifier = reauditTee{notifier, in.Notifier}
	}
	drifts := st.drifts
	if in.Drifts != nil {
		drifts = in.Drifts
	}
	w, err := triggers.NewWatcher(triggers.Config{
		Packages: in.Installer, Auditor: auditor, Verifier: in.Verifier, Registry: registry, Log: in.Audit, DB: in.DB, Clock: in.Clock,
		Adapter: st.adapterFor, Drifts: drifts, Notifier: notifier, Enqueuer: st.enq,
		CoreVersion: in.Core, DeploymentID: in.DeploymentID, SweepPeriod: in.SweepEvery, Logger: in.Log,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	st.watcher = w
	return st, nil
}

// reauditLoadSigning reads the platform key that signs reports, from the same two settings as
// `alixai audit run`. Not configured, unreadable or a published TEST-ONLY key is nil and said: serve
// still starts, a critical change still stops traffic, and the re-audit that follows is a
// reaudit.failed until a key is there.
func reauditLoadSigning(log *slog.Logger, cfg auditConfig) *trust.SigningKey {
	if cfg.SigningKeyID == "" {
		log.Warn("re-audit: integration_audit.signing_key_id is not configured: a change after activation stops traffic, but the package cannot be audited again until it is")
		return nil
	}
	key, warnings, err := trust.LoadPrivateKeyFile(cfg.SigningKeyFile)
	for _, w := range warnings {
		log.Warn("re-audit: signing key", slog.String("warning", w))
	}
	switch {
	case err != nil:
		log.Error("re-audit: the signing key cannot be read, so re-audits cannot be signed", slog.String("code", string(contracts.CodeOf(err))))
		return nil
	case trust.IsTestOnlyKey(key.Public().(ed25519.PublicKey)):
		log.Error("re-audit: the signing key is a published TEST-ONLY key that anyone can sign with, so re-audits are not signed with it")
		return nil
	}
	return &trust.SigningKey{KeyID: cfg.SigningKeyID, Key: key}
}

// ---- the parts the watcher is given -------------------------------------------------

// reauditAuditor is the Auditor of the watcher: the audit (with the RAG checks seeing this
// deployment, as in `alixai audit run`), the latest report and the quarantine of the Runner, and
// nothing else. It has no method that activates a package or accepts a warning.
type reauditAuditor struct {
	runner *integrationaudit.Runner
	rag    *stack
}

func (a reauditAuditor) Audit(ctx context.Context, req integrationaudit.RunRequest) (*integrationaudit.Run, error) {
	defer auditRAGInstall(a.rag)()
	return a.runner.Audit(ctx, req)
}

func (a reauditAuditor) LatestRun(ctx context.Context, applicationID, manifestHash string) (integrationaudit.StoredRun, bool, error) {
	return a.runner.LatestRun(ctx, applicationID, manifestHash)
}

func (a reauditAuditor) Quarantine(ctx context.Context, applicationID, manifestHash, reason, operatorID string) error {
	return a.runner.Quarantine(ctx, applicationID, manifestHash, reason, operatorID)
}

// reauditRegistry is the Registry of the watcher: refreshing and dropping an application go through
// the same control the admin interface acts on, so the package cache and the adapter cache go with
// the registry's entry (a drifted adapter that stayed cached under the same package hash would serve
// again the moment the package is reactivated), and Info asks the registry what it serves.
type reauditRegistry struct {
	control serveAdminControl
	reg     *capabilities.Registry
}

func (r reauditRegistry) Refresh(ctx context.Context, applicationID string) error {
	return r.control.Refresh(ctx, applicationID)
}

func (r reauditRegistry) Deactivate(applicationID string) { r.control.Deactivate(applicationID) }

func (r reauditRegistry) Info(ctx context.Context, applicationID string) (capabilities.Info, error) {
	return r.reg.Info(ctx, applicationID)
}

// reauditNotifier writes the notices to the log, where the operator reads them: critical ones at
// error level.
func reauditNotifier(log *slog.Logger) triggers.Notifier {
	return triggers.NotifierFunc(func(ctx context.Context, n triggers.Notice) {
		level := slog.LevelInfo
		switch n.Severity {
		case triggers.SeverityWarning:
			level = slog.LevelWarn
		case triggers.SeverityCritical:
			level = slog.LevelError
		}
		hash := n.ManifestHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		log.Log(ctx, level, "re-audit: "+string(n.Type),
			slog.String("application_id", n.ApplicationID), slog.String("package", hash), slog.String("severity", n.Severity),
			slog.Int("changes", len(n.Changes)), slog.String("outcome", n.Outcome), slog.String("reason", n.Reason), slog.String("code", n.Code))
	})
}

// reauditTee tells two notifiers.
type reauditTee [2]triggers.Notifier

func (t reauditTee) Notify(ctx context.Context, n triggers.Notice) {
	t[0].Notify(ctx, n)
	t[1].Notify(ctx, n)
}

// adapterFor is the watcher's AdapterSource: a fresh adapter of the application for one look,
// never the one that serves traffic (a drifted adapter refuses every call until it is rebuilt, and
// a re-audit must talk to the application as it is now). It is built from the live package, or the
// newest quarantined one when none is live. nil, nil means this build has no adapter to reach it.
func (s *reauditStack) adapterFor(ctx context.Context, applicationID string) (adapters.AppAdapter, func(), error) {
	recs, err := s.in.Installer.List(ctx, applicationID)
	if err != nil {
		return nil, nil, err
	}
	rec, ok := reauditPackageToReach(recs)
	if !ok {
		return nil, nil, nil
	}
	ad, _, err := transportsSource{installer: s.in.Installer, dataDir: s.in.DataDir}.adapter(ctx, applicationID, rec.ManifestHash)
	switch {
	case err == nil:
		return ad, func() { transportsClose(ad) }, nil
	case transportsNoAdapter(err):
		return nil, nil, nil
	}
	return nil, nil, err
}

// reauditPackageToReach picks the package the watcher's adapter is built from.
func reauditPackageToReach(recs []apppkg.Record) (apppkg.Record, bool) {
	var best *apppkg.Record
	for i := range recs {
		switch {
		case recs[i].State == apppkg.StateActive:
			return recs[i], true
		case recs[i].State == apppkg.StateQuarantined && (best == nil || !recs[i].InstalledAt.Before(best.InstalledAt)):
			best = &recs[i]
		}
	}
	if best == nil {
		return apppkg.Record{}, false
	}
	return *best, true
}

// drifts is the poll of every look (Config.Drifts): the drift found by the adapter that serves the
// active package NOW. MCP drift is sticky for the life of an adapter instance, so the instance
// must be the current one and for the current package: the cache entry of another package hash is
// ignored, and an instance the process dropped (a quarantine, a deactivation) is not in the cache
// at all. Reading anything older would re-quarantine a package right after an operator reactivated
// it. Nothing is built here.
func (s *reauditStack) drifts(applicationID string) []triggers.Drift {
	ctx, cancel := context.WithTimeout(s.life, reauditPollTimeout)
	defer cancel()
	pkg, err := s.in.Apps.Package(ctx, applicationID)
	if err != nil || pkg == nil {
		return nil // not active: nothing serves, so nothing to stop
	}
	cur, ok := s.in.Adapters.cached(applicationID, pkg.ContentHash())
	if !ok || cur.ad == nil {
		return nil
	}
	var out []triggers.Drift
	for _, d := range mcpTransportDrifts(cur.ad) {
		out = append(out, reauditDriftOf(d))
	}
	return out
}

// reauditDriftOf is an MCP adapter's drift as the plain data the watcher takes.
func reauditDriftOf(d mcptransport.Drift) triggers.Drift {
	return triggers.Drift{Endpoint: d.Endpoint, Tool: d.Tool, Kind: string(d.Kind), Pinned: d.Pinned, Observed: d.Observed}
}

// ---- the hooks ------------------------------------------------------------------------

// reauditApplications is the application service of the API with the install hook on it. Only
// Register is the wrapper's; everything else is the wrapped service's.
type reauditApplications struct {
	gateway.ApplicationService
	st *reauditStack
}

// applications wraps the application service of the API: every install is shown to the watcher
// before the API answers.
func (s *reauditStack) applications(inner gateway.ApplicationService) gateway.ApplicationService {
	return reauditApplications{ApplicationService: inner, st: s}
}

// Register implements gateway.ApplicationService. The install has happened by the time the watcher
// looks (the package is installed quarantined and serves nothing), so a watcher that fails does not
// fail the registration: the failure is logged, is a reaudit.failed in the audit log when it is
// about a stop, and queues a watch job, and a repeat of the registration is idempotent.
func (a reauditApplications) Register(ctx context.Context, scope contracts.Scope, archive []byte) (*apppkg.Installed, error) {
	inst, err := a.ApplicationService.Register(ctx, scope, archive)
	if err != nil || inst == nil {
		return inst, err
	}
	// The stop must not depend on the client staying connected.
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reauditHookTimeout)
	defer cancel()
	res, werr := a.st.watcher.PackageInstalled(hctx, inst.Record.ApplicationID, inst.Record.ManifestHash)
	a.st.reported(hctx, "install", inst.Record.ApplicationID, res, werr)
	return inst, nil
}

// onDrift is the observer of the MCP adapters (RegisterMCPDriftObserver). It returns at once: the
// stop runs on a goroutine of its own, under the stack's lifetime and a timeout.
func (s *reauditStack) onDrift(applicationID string, d mcptransport.Drift) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.life, reauditHookTimeout)
		defer cancel()
		res, err := s.watcher.ReportDrift(ctx, applicationID, reauditDriftOf(d))
		s.reported(ctx, "drift", applicationID, res, err)
	}()
}

// reported is what a hook does with its outcome: shows it to a test tap, says a failure and queues a
// watch job so that a stop that could not be confirmed is confirmed again and not forgotten. A full
// queue is not retried (the next sweep looks again).
func (s *reauditStack) reported(ctx context.Context, hook, applicationID string, res triggers.Result, err error) {
	if s.in.OnReport != nil {
		s.in.OnReport(hook, applicationID, res, err)
	}
	if err == nil {
		return
	}
	s.log.Error("re-audit: the watcher could not complete what an "+hook+" asked of it",
		slog.String("application_id", applicationID), slog.String("code", string(contracts.CodeOf(err))), slog.String("err_text", err.Error()))
	if contracts.CodeOf(err) == contracts.ErrResourceExhausted {
		return
	}
	payload, _ := json.Marshal(triggers.JobPayload{ApplicationID: applicationID})
	key := fmt.Sprintf("retry:%s:%d", hook, s.in.Clock.Now().UnixMilli())
	if _, qerr := s.enq.EnqueueWatch(context.WithoutCancel(ctx), triggers.JobScope(s.in.DeploymentID, applicationID), key, payload); qerr != nil {
		s.log.Error("re-audit: the retry of the watcher could not be queued; the next sweep looks again",
			slog.String("application_id", applicationID), slog.String("code", string(contracts.CodeOf(qerr))))
	}
}

// idle waits for the drift hooks in flight.
func (s *reauditStack) idle() { s.wg.Wait() }

// start is the service of the process (serveDeps.AddService): it registers the drift observer and
// runs the sweep at once and every period until ctx ends. The function it returns removes the
// observer, stops the loop and waits for it and for the hooks in flight, before the store closes.
func (s *reauditStack) start(ctx context.Context) (stop func()) {
	remove := RegisterMCPDriftObserver(s.onDrift)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(s.in.SweepEvery)
		defer tick.Stop()
		for {
			s.sweep(rctx)
			select {
			case <-rctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		remove()
		cancel()
		<-done
		s.drain()
	}
}

// sweep is one pass of the loop. A failed pass is logged and the next one goes on; a full queue stops
// a pass with resource_exhausted, which is the same.
func (s *reauditStack) sweep(ctx context.Context) {
	res, err := s.watcher.Sweep(ctx)
	if reauditOnSweep != nil {
		reauditOnSweep(res, err)
	}
	switch {
	case err != nil && ctx.Err() == nil:
		s.log.Warn("re-audit: the sweep failed; the next pass goes on", slog.String("code", string(contracts.CodeOf(err))), slog.String("err_text", err.Error()))
	case err == nil && res.Enqueued > 0:
		s.log.Info("re-audit: watch jobs queued", slog.Int("applications", res.Applications), slog.Int("queued", res.Enqueued), slog.Int("already_queued", res.Existing))
	}
}

// drain refuses new hooks and waits for the ones in flight, cancelling them after a grace.
func (s *reauditStack) drain() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(reauditStopGrace):
		s.cancel()
		<-done
	}
	s.cancel()
}

// ---- the job ----------------------------------------------------------------------------

// reauditJobSpec is the job type of the watch: class 6, maintenance.
func reauditJobSpec() jobs.TypeSpec {
	return jobs.TypeSpec{Name: triggers.JobType, Class: jobs.ClassMaintenance, MaxAttempts: reauditJobAttempts}
}

// reauditProvide puts the watch job on the RAG stack's queue and pool and starts the loop that
// schedules it. The stack itself was built by wireV1 (the install hook has to be on the application
// service before anything else reads it).
func reauditProvide(_ context.Context, d serveDeps) error {
	st := d.Reaudit
	if st == nil {
		return nil // serveDeps built by hand: nothing to run
	}
	rs, ok := gateway.Lookup[*ragStack](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "cli: the re-audit watch job runs on the RAG stack's queue and pool, which were not provided")
	}
	if err := rs.queue.Register(reauditJobSpec()); err != nil {
		return err
	}
	st.enq.bind(rs.queue)
	err := rs.shareJobs(triggers.ServiceIdentity, reauditAuthority{}, func(p *jobs.Pool) error {
		return p.Handle(triggers.JobType, st.handle)
	})
	if err != nil {
		return err
	}
	if d.AddService != nil {
		d.AddService(st.start)
	}
	if reauditOnCompose != nil {
		reauditOnCompose(st)
	}
	return nil
}

// handle is the handler of the watch job. HandleJob refuses a job that does not declare the service
// identity and a payload that is not exactly {"application_id": ...}, with codes that are not
// retried; an unavailable store or a registry that still serves a quarantined application is.
func (s *reauditStack) handle(ctx context.Context, job jobs.Job) error {
	return s.watcher.HandleJob(ctx, job.ServiceIdentity, job.Payload)
}

// reauditAuthority is the pool's authority for triggers.ServiceIdentity: that identity, the watch
// job type, exactly the scope the watcher itself makes for an application (triggers.JobScope) and a
// payload that names that application and nothing else. A job that carries the identity but another
// payload or scope is denied, so a watch job queued for one application can never make the watcher
// look at, quarantine or re-audit another.
type reauditAuthority struct{}

// AuthorizeService implements jobs.ServiceAuthorizer.
func (reauditAuthority) AuthorizeService(_ context.Context, identity string, job jobs.Job) error {
	switch {
	case identity != triggers.ServiceIdentity:
		return contracts.New(contracts.ErrPermissionDenied, "unknown service identity")
	case job.Type != triggers.JobType:
		return contracts.New(contracts.ErrPermissionDenied, "the service identity does not run this job type")
	}
	if err := job.Scope.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "the watch job has no valid scope")
	}
	var p triggers.JobPayload
	dec := json.NewDecoder(bytes.NewReader(job.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || p.ApplicationID == "" || dec.More() {
		return contracts.New(contracts.ErrPermissionDenied, "the watch job names no application")
	}
	want := triggers.JobScope(job.Scope.DeploymentID, p.ApplicationID)
	got := job.Scope
	if got.ApplicationID != want.ApplicationID || got.TenantID != want.TenantID || got.WorkspaceID != want.WorkspaceID ||
		got.PrincipalID != want.PrincipalID || got.SessionID != want.SessionID ||
		!slices.Equal(got.Roles, want.Roles) || !slices.Equal(got.CapabilityScopes, want.CapabilityScopes) {
		return contracts.New(contracts.ErrPermissionDenied, "the watch job's scope is not the application's own watch scope")
	}
	return nil
}

// reauditEnqueuer is triggers.Enqueuer over the one jobs queue. The queue exists only once the RAG
// stack is built, which is after the watcher is, so it is bound by the provider; until then a
// request to queue is unavailable.
type reauditEnqueuer struct {
	mu sync.RWMutex
	q  *jobs.Queue
}

func (e *reauditEnqueuer) bind(q *jobs.Queue) {
	e.mu.Lock()
	e.q = q
	e.mu.Unlock()
}

// EnqueueWatch implements triggers.Enqueuer: one watch job under the service identity. A full class
// is the queue's resource_exhausted, untouched.
func (e *reauditEnqueuer) EnqueueWatch(ctx context.Context, scope contracts.Scope, idempotencyKey string, payload json.RawMessage) (bool, error) {
	e.mu.RLock()
	q := e.q
	e.mu.RUnlock()
	if q == nil {
		return false, contracts.New(contracts.ErrUnavailable, "cli: the job queue of the re-audit watch is not built yet")
	}
	res, err := q.Enqueue(ctx, scope, jobs.EnqueueRequest{
		Type: triggers.JobType, Payload: payload, IdempotencyKey: reauditJobKey(idempotencyKey), ServiceIdentity: triggers.ServiceIdentity,
	})
	if err != nil {
		return false, err
	}
	return res.Duplicate, nil
}

// reauditJobKey is the idempotency key as the queue takes it: what the watcher chose, or, when that
// is longer than the queue allows or not plain text, its digest (the same key still gives the same
// job).
func reauditJobKey(key string) string {
	if len(key) <= reauditKeyMax && utf8.ValidString(key) && strings.IndexFunc(key, unicode.IsControl) < 0 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}
