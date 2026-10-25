package triggers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Defaults of Config.
const (
	// SystemPrincipal is who the watcher acts as in the audit log: it is not an
	// operator, and the log says so.
	SystemPrincipal = "alixai-reaudit"
	// DefaultCallTimeout bounds each call the watcher makes to an adapter.
	DefaultCallTimeout = 30 * time.Second
	// DefaultSweepPeriod is how often one application is looked at by Sweep.
	DefaultSweepPeriod = 15 * time.Minute

	// ReasonPrefix starts the reason of every quarantine the watcher makes. It is
	// how a package the watcher took out of service is told from one an operator
	// did: the first gets a fresh report on its own, the second is the operator's
	// business.
	ReasonPrefix = "re-audit trigger: "
)

// Packages is what the watcher reads of the installed packages: the records and
// the verified package. *apppkg.Installer implements it. Open verifies the
// stored bytes against the signature, the trust store and the recorded hash, so
// a package that was edited on disk, or whose publisher key was revoked, does not
// open.
type Packages interface {
	List(ctx context.Context, applicationID string) ([]apppkg.Record, error)
	Open(ctx context.Context, applicationID, manifestHash string) (*apppkg.Package, apppkg.Record, error)
}

// Auditor is the part of *integrationaudit.Runner the watcher uses. It has no
// Activate and no AcceptWarning on purpose: the watcher can audit and can stop
// traffic, and nothing it does can start traffic or accept a warning, however
// clean a report it produces.
type Auditor interface {
	Audit(ctx context.Context, req integrationaudit.RunRequest) (*integrationaudit.Run, error)
	LatestRun(ctx context.Context, applicationID, manifestHash string) (integrationaudit.StoredRun, bool, error)
	Quarantine(ctx context.Context, applicationID, manifestHash, reason, operatorID string) error
}

var _ Auditor = (*integrationaudit.Runner)(nil)

// Registry is the in-process capability registry of the process that serves
// traffic: *capabilities.Registry. After a quarantine the watcher refreshes it
// (what the admin route /admin/registry/refresh does in a running server), drops
// the application, and asks it what it serves. A quarantine the registry still
// serves is not a quarantine, and the watcher reports it as a failure.
type Registry interface {
	Refresh(ctx context.Context, applicationID string) error
	Deactivate(applicationID string)
	Info(ctx context.Context, applicationID string) (capabilities.Info, error)
}

var _ Registry = (*capabilities.Registry)(nil)

// AdapterSource builds an adapter for an application, for the watcher's own use,
// and returns the function that releases it. It is called for every look, never
// cached: an MCP adapter that found a drift refuses every call until it is
// rebuilt, and a re-audit must talk to the application as it is now. A nil
// adapter with a nil error means the application cannot be reached for
// inspection.
type AdapterSource func(ctx context.Context, applicationID string) (adapters.AppAdapter, func(), error)

// Drift is what the MCP adapter reports when a tool no longer matches the schema
// pinned at audit time: adapters/mcp.Drift as plain data, so this package imports
// no concrete adapter. The composition root adapts mcp.Config.OnDrift to
// Watcher.ReportDrift. Kind is one of adapters/mcp's DriftKind values; any value,
// known or not, is a critical change.
type Drift struct {
	Endpoint string
	Tool     string
	Kind     string
	Pinned   string
	Observed string
}

// Config configures a Watcher.
type Config struct {
	// Packages reads the installed packages. Required.
	Packages Packages
	// Auditor audits and quarantines. Required.
	Auditor Auditor
	// Verifier verifies the signed reports the watcher compares against: the same
	// trust verifier the Runner uses. Required.
	Verifier *trust.Verifier
	// Registry is the capability registry of the serving process. Required: a
	// watcher that cannot confirm a stop must not claim one.
	Registry Registry
	// Log records the ops events on the application chain. Required.
	Log *audit.Log
	// DB lists the applications Sweep looks at. Needed by Sweep only.
	DB *store.DB
	// Clock is the only source of time. Nil means the wall clock.
	Clock clock.Clock
	// Adapter builds the adapters of the applications. Nil means none: the
	// watcher cannot look at an adapter surface (it says so in Result.Unverified)
	// and audits without one.
	Adapter AdapterSource
	// Drifts, when set, is polled on every look for the drift the adapters found
	// since they were built (adapters/mcp.Adapter.Drifts), in addition to the
	// OnDrift push of ReportDrift.
	Drifts func(applicationID string) []Drift
	// Notifier receives the ops events as well as the audit log. Optional.
	Notifier Notifier
	// Enqueuer queues the slow part (the re-audit) as a job of class 6. Optional:
	// without it the re-audit runs inline in the call that found the change.
	Enqueuer Enqueuer
	// CoreVersion is the core version the Runner audits against; a change of it
	// since the baseline report is a trigger. Empty disables that trigger.
	CoreVersion string
	// DeploymentID is the deployment identifier of the scope of the watcher's
	// jobs. Empty means integrationaudit.DefaultDeploymentID.
	DeploymentID string
	// Principal is who the watcher acts as. Empty means SystemPrincipal.
	Principal string
	// CallTimeout bounds each adapter call. Zero means DefaultCallTimeout.
	CallTimeout time.Duration
	// SweepPeriod is how often Sweep queues one application. Zero means
	// DefaultSweepPeriod.
	SweepPeriod time.Duration
	// Logger receives the watcher's own diagnostics. Nil discards them.
	Logger *slog.Logger
}

// Watcher enforces the re-audit triggers of master plan 4.5: it compares what an
// application is now with what its last audit recorded, audits again after any
// change and stops traffic at once after a critical one. It is safe for
// concurrent use; the work on one application is serialised.
type Watcher struct {
	cfg Config

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewWatcher returns a Watcher.
func NewWatcher(cfg Config) (*Watcher, error) {
	switch {
	case cfg.Packages == nil:
		return nil, contracts.New(contracts.ErrInternal, "triggers: the watcher needs the installed packages")
	case cfg.Auditor == nil:
		return nil, contracts.New(contracts.ErrInternal, "triggers: the watcher needs an auditor")
	case cfg.Verifier == nil:
		return nil, contracts.New(contracts.ErrInternal, "triggers: the watcher needs a trust verifier to check the reports it compares against")
	case cfg.Registry == nil:
		return nil, contracts.New(contracts.ErrInternal, "triggers: the watcher needs the capability registry: a quarantine it cannot confirm must not be claimed")
	case cfg.Log == nil:
		return nil, contracts.New(contracts.ErrInternal, "triggers: the watcher needs an audit log: a stop that cannot be recorded must be reported")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Principal == "" {
		cfg.Principal = SystemPrincipal
	}
	if cfg.DeploymentID == "" {
		cfg.DeploymentID = integrationaudit.DefaultDeploymentID
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	if cfg.SweepPeriod <= 0 {
		cfg.SweepPeriod = DefaultSweepPeriod
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Watcher{cfg: cfg, locks: map[string]*sync.Mutex{}}, nil
}

// lock serialises the work on one application and returns the unlock.
func (w *Watcher) lock(applicationID string) func() {
	w.mu.Lock()
	m := w.locks[applicationID]
	if m == nil {
		m = &sync.Mutex{}
		w.locks[applicationID] = m
	}
	w.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// AuditRef names one re-audit that ran.
type AuditRef struct {
	ManifestHash string
	RunID        string
	Outcome      integrationaudit.Disposition
}

// Result is what one look at an application found and did.
type Result struct {
	ApplicationID string
	// Changes are the differences found, sorted by kind and subject. Empty means
	// nothing differed from the last audit.
	Changes []Change
	// Quarantined lists the packages the watcher took out of service.
	Quarantined []string
	// Audits lists the re-audits that ran and stored a report.
	Audits []AuditRef
	// Deferred is true when the re-audit was queued as a job instead of run.
	Deferred bool
	// Unverified lists what the watcher could not look at (an adapter that could
	// not be reached, say). Unverified is not unchanged.
	Unverified []string
}

// candidate is an installed package of the application that is not live: one
// uploaded after the live package was activated.
type candidate struct {
	rec     apppkg.Record
	changes []Change
	audited bool
}

// plan is everything one look found, before anything is done about it.
type plan struct {
	app    string
	active *apppkg.Record
	// activeChanges are the differences of the live package from its baseline.
	activeChanges []Change
	candidates    []candidate
	// followUps are packages this watcher quarantined that no report postdates.
	followUps []apppkg.Record
	// ours are the packages this watcher quarantined and that are still quarantined.
	ours       []apppkg.Record
	unverified []string
}

// critical returns the critical changes of the live package and of the
// candidates, each tagged with the package it is about.
func (p *plan) critical() []Change {
	var out []Change
	for _, c := range p.activeChanges {
		if c.Critical {
			out = append(out, c)
		}
	}
	for _, cd := range p.candidates {
		for _, c := range cd.changes {
			if c.Critical {
				out = append(out, c)
			}
		}
	}
	return out
}

func (p *plan) all() []Change {
	out := append([]Change(nil), p.activeChanges...)
	for _, cd := range p.candidates {
		out = append(out, cd.changes...)
	}
	return out
}

// auditWork says whether anything calls for an audit.
func (p *plan) auditWork() bool {
	if len(p.activeChanges) > 0 || len(p.followUps) > 0 {
		return true
	}
	for _, cd := range p.candidates {
		if !cd.audited {
			return true
		}
	}
	return false
}

// Check looks at one application: it compares the live package with its last
// audit and with the packages uploaded since, stops traffic at once when a
// critical change is found (signature, scope, permission model, a pinned MCP
// schema) and then audits again whatever changed. While a re-audit runs the
// previous activation stays in force unless a critical change was found, in which
// case it was stopped before the audit began.
//
// Check is what a scheduled job runs (HandleJob) and is idempotent: with nothing
// changed it audits nothing and emits nothing. It never activates a package and
// never accepts a warning.
func (w *Watcher) Check(ctx context.Context, applicationID string) (Result, error) {
	return w.check(ctx, applicationID, true, "check")
}

// PackageInstalled is the event hook for a package upload (apppkg.Installer
// .Install returned a new record). The part that must be immediate happens before
// it returns: a critical change stops traffic. The re-audit is queued as a job
// when an Enqueuer is configured, and run inline otherwise.
func (w *Watcher) PackageInstalled(ctx context.Context, applicationID, manifestHash string) (Result, error) {
	if manifestHash == "" {
		return Result{}, contracts.New(contracts.ErrValidationFailed, "triggers: a package upload names the package")
	}
	return w.check(ctx, applicationID, w.cfg.Enqueuer == nil, "installed:"+manifestHash)
}

func (w *Watcher) check(ctx context.Context, applicationID string, audits bool, enqueueKey string) (Result, error) {
	if applicationID == "" {
		return Result{}, contracts.New(contracts.ErrScopeMissing, "triggers: no application to check")
	}
	unlock := w.lock(applicationID)
	defer unlock()

	res := Result{ApplicationID: applicationID}
	p, err := w.assess(ctx, applicationID)
	if err != nil {
		return res, err
	}
	res.Changes, res.Unverified = p.all(), p.unverified
	sortChanges(res.Changes)

	// A package this watcher quarantined earlier stays unserved: a stop the registry
	// did not confirm on the pass that made it is confirmed again on every later one,
	// and does not wait for the registry's cache to expire.
	if p.active == nil && len(p.ours) > 0 {
		if err := w.reconfirm(ctx, p); err != nil {
			return res, err
		}
	}

	// Stop first. Nothing is audited while a package known to be critically
	// different is still serving.
	quarantinedNow := false
	if crit := p.critical(); len(crit) > 0 && p.active != nil {
		stopped, err := w.stopTraffic(ctx, *p.active, crit, &res)
		if err != nil {
			return res, err
		}
		quarantinedNow = stopped
	}
	if !p.auditWork() && !quarantinedNow {
		return res, nil
	}
	if !audits {
		if _, err := w.enqueue(ctx, applicationID, enqueueKey); err != nil {
			return res, err
		}
		res.Deferred = true
		return res, nil
	}

	changes := append([]Change(nil), res.Changes...)
	if quarantinedNow {
		c := newChange(KindQuarantined, "package", "audited again so that a report postdates the quarantine")
		changes = append(changes, w.tagged(*p.active, c))
	}
	_ = w.emit(ctx, EventTriggered, SeverityInfo,
		Notice{ApplicationID: applicationID, ManifestHash: activeHash(p), Changes: changes},
		eventData{ManifestHash: activeHash(p), Critical: quarantinedNow, Changes: nonNil(changes)})

	// What to audit, in the order of how much the operator is waiting for it: the
	// uploaded candidates, the packages quarantined earlier without a report since,
	// then the live package (a re-audit after a non-critical change) or the one
	// just quarantined.
	var targets []apppkg.Record
	for _, cd := range p.candidates {
		if !cd.audited {
			targets = append(targets, cd.rec)
		}
	}
	targets = append(targets, p.followUps...)
	if p.active != nil && (quarantinedNow || len(p.activeChanges) > 0) {
		targets = append(targets, *p.active)
	}

	var errs []error
	for _, rec := range targets {
		run, err := w.reaudit(ctx, rec, &res)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// A re-audit of the live package that finds a critical problem stops it
		// too: the activation was cleared by an audit that no longer holds.
		if p.active != nil && rec.ManifestHash == p.active.ManifestHash && !quarantinedNow &&
			run.Disposition() == integrationaudit.DispositionBlocked {
			c := newChange(KindAuditBlocked, "report", "the re-audit of the live package found critical problems")
			c.Package = rec.ManifestHash
			if _, err := w.stopTraffic(ctx, rec, []Change{c}, &res); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return res, errors.Join(errs...)
}

func activeHash(p *plan) string {
	if p.active == nil {
		return ""
	}
	return p.active.ManifestHash
}

func nonNil(cs []Change) []Change {
	if cs == nil {
		return []Change{}
	}
	return cs
}

// assess looks at the application and decides nothing: it reads the installed
// packages, verifies the live one, compares it with its baseline, the adapter
// and the packages uploaded after it, and returns what it found.
func (w *Watcher) assess(ctx context.Context, applicationID string) (*plan, error) {
	recs, err := w.cfg.Packages.List(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	p := &plan{app: applicationID}
	var ours []apppkg.Record
	for i := range recs {
		switch {
		case recs[i].State == apppkg.StateActive:
			p.active = &recs[i]
		case recs[i].State == apppkg.StateQuarantined && strings.HasPrefix(recs[i].StatusReason, ReasonPrefix):
			ours = append(ours, recs[i])
		}
	}
	if p.active == nil && len(ours) == 0 {
		return p, nil // nothing was ever live, or nothing the watcher stopped: not its business
	}
	p.ours = ours

	var live []facet
	if p.active != nil {
		pkg, changes, unverified, err := w.inspectActive(ctx, *p.active)
		if err != nil {
			return nil, err
		}
		p.activeChanges, p.unverified = changes, unverified
		if pkg != nil {
			if live, err = facetsOf(pkg, *p.active); err != nil {
				p.activeChanges = append(p.activeChanges, w.tagged(*p.active, newChange(integrationaudit.TriggerPermissionModel, "permissions", "the live package cannot be hashed")))
			}
		}
	}

	for _, rec := range ours {
		if w.needsReport(ctx, rec) {
			p.followUps = append(p.followUps, rec)
		}
	}
	for _, rec := range recs {
		if rec.State != apppkg.StateQuarantined || strings.HasPrefix(rec.StatusReason, ReasonPrefix) {
			continue
		}
		if p.active != nil && !rec.InstalledAt.After(p.active.StatusChangedAt) {
			continue // installed before the live package was activated: it was not a change after activation
		}
		cd, err := w.assessCandidate(ctx, rec, p.active, live)
		if err != nil {
			return nil, err
		}
		p.candidates = append(p.candidates, cd)
	}
	return p, nil
}

func (w *Watcher) tagged(rec apppkg.Record, c Change) Change {
	c.Package = rec.ManifestHash
	return c
}

// needsReport reports whether a package the watcher quarantined has no report
// made after the quarantine. A report made before it is no evidence about the
// package as it is, and activation refuses it (integrationaudit.ReasonStaleReport).
func (w *Watcher) needsReport(ctx context.Context, rec apppkg.Record) bool {
	run, ok, err := w.cfg.Auditor.LatestRun(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil || !ok {
		return true
	}
	v, err := integrationaudit.VerifyReport(ctx, w.cfg.Verifier, run.Signed)
	if err != nil {
		return true
	}
	return v.IssuedAt.Before(rec.StatusChangedAt.Truncate(time.Second))
}

// assessCandidate compares an uploaded package with the live one. A candidate
// that does not verify is only audited (the report will be blocked): its
// signature is not the live package's, and a bad upload must not be able to take
// a good application down.
func (w *Watcher) assessCandidate(ctx context.Context, rec apppkg.Record, active *apppkg.Record, live []facet) (candidate, error) {
	cd := candidate{rec: rec}
	_, ok, err := w.cfg.Auditor.LatestRun(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil && contracts.HasCode(err, contracts.ErrUnavailable) {
		return cd, err
	}
	cd.audited = ok && err == nil
	if active == nil || live == nil {
		cd.changes = []Change{w.tagged(rec, newChange(integrationaudit.TriggerPackage, rec.ApplicationID, "uploaded"))}
		return cd, nil
	}
	pkg, _, err := w.cfg.Packages.Open(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		if opened := openFailureIsFinal(err); !opened {
			return cd, err
		}
		cd.changes = []Change{w.tagged(rec, newChange(integrationaudit.TriggerPackage, rec.ApplicationID, "uploaded and does not verify"))}
		return cd, nil
	}
	other, err := facetsOf(pkg, rec)
	if err != nil {
		cd.changes = []Change{
			w.tagged(rec, newChange(integrationaudit.TriggerPackage, rec.ApplicationID, "uploaded")),
			w.tagged(rec, newChange(integrationaudit.TriggerPermissionModel, "permissions", "the uploaded package cannot be compared")),
		}
		return cd, nil
	}
	for _, c := range diffPackages(live, other) {
		cd.changes = append(cd.changes, w.tagged(rec, c))
	}
	return cd, nil
}

// openFailureIsFinal reports whether an Open error says the stored package is not
// what was installed (it was edited, its signature or key no longer verify, its
// files are gone) as opposed to the store being unavailable for a moment: a trust
// store that could not be consulted (the loader's signature.unverifiable finding)
// or a check that was cancelled or ran out of time says nothing about the package.
func openFailureIsFinal(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var refused *apppkg.RefusedError
	if errors.As(err, &refused) {
		for _, f := range refused.Findings {
			if f.CheckID == "signature.unverifiable" {
				return false
			}
		}
		return true
	}
	return !contracts.HasCode(err, contracts.ErrUnavailable)
}

// inspectActive verifies the live package and compares it with what its audit
// recorded. It returns the verified package, or nil when it does not verify.
func (w *Watcher) inspectActive(ctx context.Context, rec apppkg.Record) (*apppkg.Package, []Change, []string, error) {
	pkg, _, err := w.cfg.Packages.Open(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		if !openFailureIsFinal(err) {
			return nil, nil, nil, err
		}
		return nil, []Change{w.tagged(rec, newChange(KindSignature, "package", "the installed package no longer verifies"))}, nil, nil
	}
	var changes []Change
	var unverified []string
	add := func(k integrationaudit.TriggerKind, subject, detail string) {
		changes = append(changes, w.tagged(rec, newChange(k, subject, detail)))
	}

	if w.cfg.Drifts != nil {
		for _, d := range w.cfg.Drifts(rec.ApplicationID) {
			changes = append(changes, w.tagged(rec, driftChange(d)))
		}
	}

	base, ok, err := w.baselineOf(ctx, rec)
	if err != nil {
		return nil, nil, nil, err
	}
	if !ok {
		add(KindBaseline, "report", "no verifiable audit report of the live package to compare with")
	} else if w.cfg.CoreVersion != "" && base.Subject.CoreVersion != w.cfg.CoreVersion {
		add(integrationaudit.TriggerCoreVersion, "core", "changed")
	}

	if w.cfg.Adapter != nil {
		d, derr := w.describe(ctx, rec.ApplicationID)
		switch {
		case derr != nil:
			unverified = append(unverified, "adapter_surface: the adapter could not be reached")
		case ok:
			var at *integrationaudit.Trigger
			for i := range base.ReauditTriggers {
				if base.ReauditTriggers[i].Kind == integrationaudit.TriggerAdapterSurface {
					at = &base.ReauditTriggers[i]
				}
			}
			switch {
			case at == nil:
				add(integrationaudit.TriggerAdapterSurface, d.Transport, "added")
			case at.Subject != d.Transport || at.Hash != SurfaceHash(d):
				add(integrationaudit.TriggerAdapterSurface, d.Transport, "changed")
			}
			if base.Surface.Inspected && base.Surface.ApplicationVersion != d.ApplicationVersion {
				add(integrationaudit.TriggerApplicationVersion, "adapter", "changed")
			}
		}
	}
	return pkg, changes, unverified, nil
}

// baselineOf returns the report the live package was last audited with, once its
// signature verifies. ok is false when there is none or it does not verify:
// nothing can then be said to be unchanged. A store that is unavailable is an
// error, not a missing baseline.
func (w *Watcher) baselineOf(ctx context.Context, rec apppkg.Record) (integrationaudit.Report, bool, error) {
	run, ok, err := w.cfg.Auditor.LatestRun(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		if contracts.HasCode(err, contracts.ErrUnavailable) {
			return integrationaudit.Report{}, false, err
		}
		return integrationaudit.Report{}, false, nil
	}
	if !ok {
		return integrationaudit.Report{}, false, nil
	}
	if _, err := integrationaudit.VerifyReport(ctx, w.cfg.Verifier, run.Signed); err != nil {
		return integrationaudit.Report{}, false, nil
	}
	rep := run.Signed.Report
	if rep.Subject.ApplicationID != rec.ApplicationID || rep.Subject.ManifestHash != rec.ManifestHash {
		return integrationaudit.Report{}, false, nil
	}
	return rep, true, nil
}

// describe asks a fresh adapter what the application exposes now.
func (w *Watcher) describe(ctx context.Context, applicationID string) (adapters.Description, error) {
	ad, release, err := w.cfg.Adapter(ctx, applicationID)
	if err != nil {
		return adapters.Description{}, err
	}
	if release != nil {
		defer release()
	}
	if ad == nil {
		return adapters.Description{}, contracts.New(contracts.ErrUnavailable, "triggers: no adapter for the application")
	}
	cctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()
	d, err := ad.Describe(cctx)
	if err != nil {
		return adapters.Description{}, err
	}
	if err := d.Validate(); err != nil {
		return adapters.Description{}, err
	}
	return d, nil
}

func driftChange(d Drift) Change {
	subject := d.Endpoint
	if d.Tool != "" {
		subject += "/" + d.Tool
	}
	return newChange(KindMCPSchema, clip(subject, 160), clip(d.Kind, 40))
}

// reaudit runs the audit of one package against a fresh adapter and records the
// outcome. An application that cannot be reached is audited without an adapter:
// the report then says the application and runtime targets are unverified and
// carries the warning that an operator must accept before anything activates.
func (w *Watcher) reaudit(ctx context.Context, rec apppkg.Record, res *Result) (*integrationaudit.Run, error) {
	var ad adapters.AppAdapter
	if w.cfg.Adapter != nil {
		a, release, err := w.cfg.Adapter(ctx, rec.ApplicationID)
		switch {
		case err != nil || a == nil:
			res.Unverified = append(res.Unverified, "audit: no adapter; the application and runtime targets are unverified")
		default:
			ad = a
			if release != nil {
				defer release()
			}
		}
	}
	run, err := w.cfg.Auditor.Audit(ctx, integrationaudit.RunRequest{
		ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, Adapter: ad, OperatorID: w.cfg.Principal,
	})
	if err != nil {
		code := string(contracts.CodeOf(err))
		_ = w.emit(ctx, EventFailed, SeverityWarning,
			Notice{ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, Reason: "the re-audit could not be completed", Code: code},
			eventData{ManifestHash: rec.ManifestHash, Changes: []Change{}, Reason: "the re-audit could not be completed", Code: code})
		return nil, fmt.Errorf("triggers: re-audit of %s: %w", clip(rec.ManifestHash, 12), err)
	}
	outcome := string(run.Disposition())
	res.Audits = append(res.Audits, AuditRef{ManifestHash: rec.ManifestHash, RunID: run.RunID, Outcome: run.Disposition()})
	sev := SeverityInfo
	if run.Disposition() != integrationaudit.DispositionClear {
		sev = SeverityWarning
	}
	_ = w.emit(ctx, EventCompleted, sev,
		Notice{ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, RunID: run.RunID, Outcome: outcome},
		eventData{ManifestHash: rec.ManifestHash, Changes: []Change{}, RunID: run.RunID, Outcome: outcome})
	return run, nil
}

// stopTraffic takes the live package out of service and does not return until
// the capability registry confirms it serves nothing for the application. The
// order matters and is the same everywhere: quarantine in the database (which also
// evicts through the Runner's own registry hook), refresh and drop the entry in
// the registry, ask the registry what it serves, and only then write the ops
// event. stopped is false when the package was no longer the live one.
func (w *Watcher) stopTraffic(ctx context.Context, rec apppkg.Record, changes []Change, res *Result) (stopped bool, err error) {
	reason := quarantineReason(changes)
	qerr := w.cfg.Auditor.Quarantine(ctx, rec.ApplicationID, rec.ManifestHash, reason, w.cfg.Principal)
	switch {
	case qerr == nil:
		stopped = true
		res.Quarantined = append(res.Quarantined, rec.ManifestHash)
	case contracts.HasCode(qerr, contracts.ErrConflict):
		// Not active any more: an operator quarantined it or another package was
		// activated since this look. If another package is live now, this look is
		// out of date and the next one decides; if nothing is, confirm below.
		recs, lerr := w.cfg.Packages.List(ctx, rec.ApplicationID)
		if lerr != nil {
			return false, lerr
		}
		for _, r := range recs {
			if r.State == apppkg.StateActive && r.ManifestHash != rec.ManifestHash {
				return false, nil
			}
		}
	default:
		_ = w.emit(ctx, EventFailed, SeverityCritical,
			Notice{ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, Changes: changes, Reason: "traffic could not be stopped", Code: string(contracts.CodeOf(qerr))},
			eventData{ManifestHash: rec.ManifestHash, Critical: true, Changes: changes, Reason: "traffic could not be stopped", Code: string(contracts.CodeOf(qerr))})
		return false, qerr
	}
	if err := w.confirmStopped(ctx, rec.ApplicationID); err != nil {
		_ = w.emit(ctx, EventFailed, SeverityCritical,
			Notice{ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, Changes: changes, Reason: "the registry still serves the application after the quarantine", Code: string(contracts.CodeOf(err))},
			eventData{ManifestHash: rec.ManifestHash, Critical: true, Changes: changes, Reason: "the registry still serves the application after the quarantine", Code: string(contracts.CodeOf(err))})
		return stopped, err
	}
	werr := w.emit(ctx, EventQuarantined, SeverityCritical,
		Notice{ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, Changes: changes, Reason: reason},
		eventData{ManifestHash: rec.ManifestHash, Critical: true, Changes: changes, Reason: reason})
	if werr != nil {
		// The package is quarantined and confirmed unserved; only the record of it
		// failed. Say so, and do not undo anything.
		w.cfg.Logger.Error("the quarantine could not be recorded as an ops event", slog.String("application_id", rec.ApplicationID), slog.String("code", string(contracts.CodeOf(werr))))
	}
	return stopped, nil
}

// confirmStopped refreshes the registry (the immediate in-process refresh the
// admin route performs), drops the application from it and requires that it no
// longer serves it. Whatever the registry says about its own refresh, the check
// that counts is Info: an error there means the application is not served.
func (w *Watcher) confirmStopped(ctx context.Context, applicationID string) error {
	reg := w.cfg.Registry
	if err := reg.Refresh(ctx, applicationID); err != nil {
		// A registry that cannot reload drops the entry (fail closed); the Info
		// check below is what decides.
		w.cfg.Logger.Warn("the registry could not refresh after a quarantine", slog.String("application_id", applicationID), slog.String("code", string(contracts.CodeOf(err))))
	}
	reg.Deactivate(applicationID)
	if _, err := reg.Info(ctx, applicationID); err == nil {
		// Unavailable, so that the job that found it is retried, not failed.
		return contracts.New(contracts.ErrUnavailable, "triggers: the capability registry still serves the application after it was quarantined").
			WithDetails("application_id", applicationID)
	}
	return nil
}

// reconfirm checks, for an application whose live package the watcher quarantined,
// that the registry still serves nothing, and runs the stop again when it does. A
// package an operator activated since the look was made is not a failure.
func (w *Watcher) reconfirm(ctx context.Context, p *plan) error {
	if _, err := w.cfg.Registry.Info(ctx, p.app); err != nil {
		return nil
	}
	recs, err := w.cfg.Packages.List(ctx, p.app)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			return nil // reactivated since: the next look compares it
		}
	}
	rec := p.ours[0]
	if err := w.confirmStopped(ctx, p.app); err != nil {
		reason := "the registry still serves the application of a package that was quarantined"
		_ = w.emit(ctx, EventFailed, SeverityCritical,
			Notice{ApplicationID: p.app, ManifestHash: rec.ManifestHash, Reason: reason, Code: string(contracts.CodeOf(err))},
			eventData{ManifestHash: rec.ManifestHash, Critical: true, Changes: []Change{}, Reason: reason, Code: string(contracts.CodeOf(err))})
		return err
	}
	return nil
}

// quarantineReason is the reason stored with the quarantine: the prefix and the
// critical changes, bounded.
func quarantineReason(changes []Change) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, string(c.Kind)+" "+c.Subject)
	}
	return clip(ReasonPrefix+"critical change ("+strings.Join(parts, "; ")+")", 480)
}

// clip shortens s to at most n runes, never inside one.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// ReportDrift is the push hook for the schema pinning of the MCP adapter
// (adapters/mcp.Config.OnDrift): a tool the package maps no longer matches what
// was pinned at audit time (SEC-049). A drift is a critical change, so the live
// package is quarantined and the registry confirmed before this returns. The
// re-audit is not run here (the hook runs on the call path of the adapter): it is
// queued as a job, or left to the next Check.
//
// It must be called with a context that outlives the adapter call that found the
// drift, and it never calls an adapter.
func (w *Watcher) ReportDrift(ctx context.Context, applicationID string, d Drift) (Result, error) {
	if applicationID == "" {
		return Result{}, contracts.New(contracts.ErrScopeMissing, "triggers: a drift is about an application")
	}
	unlock := w.lock(applicationID)
	defer unlock()

	res := Result{ApplicationID: applicationID}
	recs, err := w.cfg.Packages.List(ctx, applicationID)
	if err != nil {
		return res, err
	}
	var active *apppkg.Record
	for i := range recs {
		if recs[i].State == apppkg.StateActive {
			active = &recs[i]
		}
	}
	c := driftChange(d)
	res.Changes = []Change{c}
	if active == nil {
		// Nothing is live, so nothing to stop; the operator is told, and the
		// pinned-schema flag stays on the adapter until it is rebuilt.
		_ = w.emit(ctx, EventTriggered, SeverityWarning,
			Notice{ApplicationID: applicationID, Changes: res.Changes, Reason: "no active package"},
			eventData{ManifestHash: "", Changes: res.Changes, Reason: "no active package"})
		return res, nil
	}
	res.Changes = []Change{w.tagged(*active, c)}
	if _, err := w.stopTraffic(ctx, *active, res.Changes, &res); err != nil {
		return res, err
	}
	if _, err := w.enqueue(ctx, applicationID, "drift:"+active.ManifestHash+":"+c.Subject+":"+c.Detail); err != nil {
		// The package is stopped; the follow-up audit is found by the next Check.
		w.cfg.Logger.Warn("the re-audit after a drift could not be queued", slog.String("application_id", applicationID), slog.String("code", string(contracts.CodeOf(err))))
	} else if w.cfg.Enqueuer != nil {
		res.Deferred = true
	}
	return res, nil
}
