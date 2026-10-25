package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The applications part of the public API (master plan 14.2 "Applications and
// configuration"):
//
//	POST /v1/applications                    register an application package (it lands in quarantine)
//	GET  /v1/applications/{application_id}   inspect the caller's own application: state, packages, health
//	GET  /v1/capabilities                    the capabilities the caller may see
//	GET  /v1/health                          readiness, behind the identity middleware
//
// Registration only ever installs a package into quarantine (apppkg.Installer:
// signed by a key the operator registered, no critical finding, nothing
// active). Nothing here, and nothing reachable from a request, can activate a
// package: that transition belongs to the integration audit (M1-17). A
// quarantined application serves no traffic: every session, turn and action
// route asks Applications for the active package first and refuses with
// quarantined when there is none (SEC-054, SEC-060).

// CapabilityRegisterApplication is the capability scope a context token must
// carry to register a package: the host chooses which of its principals may
// publish, the token is how it says so (the same device audit.CapabilityExport
// uses). It names a platform operation, not an application capability.
const CapabilityRegisterApplication = "applications.register"

// DefaultPackageMaxAge is how long Applications trusts a package it opened:
// the same 30 seconds the capability registry trusts its own entries. A package
// that was deactivated is noticed at once (the record is read on every call);
// one whose publisher key was revoked is noticed when the entry ages out.
const DefaultPackageMaxAge = capabilities.DefaultMaxAge

// maxRegistrationFindings bounds the findings echoed in a refusal.
const maxRegistrationFindings = 20

// Applications is the one source of installed application packages. It backs
// the registration endpoint, the capability registry (Source), the dialogue
// engine (Package) and the action coordinator (ActivePackage), so all three
// decide against the same package at the same time (the engine checks the
// package it is given is active, the application's and the content the
// registry routes against).
//
// It never produces a package that is not active: a quarantined, rejected or
// superseded package, an application with nothing installed and a package that
// no longer verifies all answer with quarantined.
type Applications struct {
	in     *apppkg.Installer
	load   apppkg.Options
	clk    clock.Clock
	maxAge time.Duration

	mu    sync.Mutex
	cache map[string]cachedPackage
}

type cachedPackage struct {
	hash string
	at   time.Time
	pkg  capabilities.Package
}

// NewApplications returns the source over an installer. load are the options
// the installer was built with (trust verifier and core version): the
// registration endpoint loads an archive with them once, before it installs
// anything, to learn which application it declares. A zero maxAge means
// DefaultPackageMaxAge.
func NewApplications(in *apppkg.Installer, load apppkg.Options, clk clock.Clock, maxAge time.Duration) (*Applications, error) {
	if in == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "gateway: applications need a package installer")
	}
	if load.Verifier == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "gateway: applications need the installer's trust verifier; nothing is registered unsigned")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	if maxAge <= 0 {
		maxAge = DefaultPackageMaxAge
	}
	return &Applications{in: in, load: load, clk: clk, maxAge: maxAge, cache: map[string]cachedPackage{}}, nil
}

// Source is the capabilities.Source of the same installer, for the registry.
func (a *Applications) Source() capabilities.Source { return capabilities.InstallerSource(a.in) }

func errNotActive() error {
	return contracts.New(contracts.ErrQuarantined, "the application has no active package: it is quarantined, rejected or not installed")
}

// Package implements dialogue.PackageSource: the active package of an
// application, or quarantined.
func (a *Applications) Package(ctx context.Context, applicationID string) (capabilities.Package, error) {
	recs, err := a.in.List(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	var hash string
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			hash = r.ManifestHash
		}
	}
	if hash == "" {
		a.forget(applicationID)
		return nil, errNotActive()
	}
	now := a.clk.Now()
	a.mu.Lock()
	c, ok := a.cache[applicationID]
	a.mu.Unlock()
	if ok && c.hash == hash && now.Sub(c.at) < a.maxAge {
		return c.pkg, nil
	}
	p, rec, err := a.in.Open(ctx, applicationID, hash)
	if err != nil {
		a.forget(applicationID)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// An active package that no longer verifies (changed on disk, key
		// revoked or expired) serves nothing.
		return nil, contracts.Wrap(contracts.ErrQuarantined, err, "the application's installed package no longer verifies")
	}
	pkg := capabilities.FromApppkg(p)
	if pkg == nil || rec.State != apppkg.StateActive || pkg.Status() != policy.StatusActive || pkg.ApplicationID() != applicationID {
		a.forget(applicationID)
		return nil, errNotActive()
	}
	a.mu.Lock()
	a.cache[applicationID] = cachedPackage{hash: hash, at: now, pkg: pkg}
	a.mu.Unlock()
	return pkg, nil
}

// ActivePackage implements actions.PackageSource.
func (a *Applications) ActivePackage(ctx context.Context, applicationID string) (policy.ActivePackage, error) {
	p, err := a.Package(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (a *Applications) forget(applicationID string) {
	a.mu.Lock()
	delete(a.cache, applicationID)
	a.mu.Unlock()
}

// Records lists the installed packages of an application, oldest first.
func (a *Applications) Records(ctx context.Context, applicationID string) ([]apppkg.Record, error) {
	return a.in.List(ctx, applicationID)
}

// Register installs an archive into quarantine for the application of the
// verified scope. The archive must declare that application: a token for
// application A can never install (or reinstall) a package of application B.
// Everything else (a signature by a key the operator registered, no critical
// finding, compatibility) is the installer's, and a package that fails it is a
// *apppkg.RefusedError and nothing is written.
func (a *Applications) Register(ctx context.Context, scope contracts.Scope, archive []byte) (*apppkg.Installed, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	res, err := apppkg.Load(ctx, apppkg.FromArchive(bytes.NewReader(archive)), a.load)
	if err != nil {
		return nil, err
	}
	// The declared application is untrusted until the signature verifies, and
	// is only used here to refuse. An archive that declares none cannot verify
	// and is refused by Install with its findings.
	if res.ApplicationID != "" && res.ApplicationID != scope.ApplicationID {
		return nil, contracts.New(contracts.ErrScopeViolation, "the package belongs to another application").
			WithDetails("levels", []string{string(contracts.LevelApplication)})
	}
	inst, err := a.in.Install(ctx, apppkg.FromArchive(bytes.NewReader(archive)))
	if err != nil {
		return nil, err
	}
	if inst.Record.ApplicationID != scope.ApplicationID {
		return nil, contracts.New(contracts.ErrScopeViolation, "the package belongs to another application").
			WithDetails("levels", []string{string(contracts.LevelApplication)})
	}
	a.forget(scope.ApplicationID)
	return inst, nil
}

// ---- the area ----

func init() { RegisterArea("v1_apps", registerV1Apps) }

type appsHandler struct {
	apps   ApplicationService
	policy *policy.Gateway
	// adapters, when provided, is asked for an active application's health.
	adapters actions.AdapterSource
	// audit, when provided, records package registrations.
	audit audit.Appender
	d     *Deps
}

// registerV1Apps needs the application service and the policy gateway. The
// adapter source and the audit appender are optional: without the first an
// application's health is not reported, and without the second a registration
// is not recorded (the composition root always provides both).
func registerV1Apps(rt *Router, d *Deps) {
	n := d.Needs()
	h := &appsHandler{
		apps:   Need[ApplicationService](n),
		policy: Need[*policy.Gateway](n),
		d:      d,
	}
	if !n.Ready() {
		return
	}
	h.adapters, _ = Want[actions.AdapterSource](n)
	h.audit, _ = Want[audit.Appender](n)
	v1 := rt.V1()
	v1.HandleFunc("POST /applications", h.register)
	v1.HandleFunc("GET /applications/{application_id}", h.inspect)
	v1.HandleFunc("GET /capabilities", h.capabilities)
	v1.HandleFunc("GET /health", h.health)
}

// PackageBody is one installed package as the API reports it. It carries no
// path on disk and no file content.
type PackageBody struct {
	ManifestHash    string    `json:"manifest_hash"`
	PackageVersion  string    `json:"package_version"`
	State           string    `json:"state"`
	StatusReason    string    `json:"status_reason,omitempty"`
	SignerKeyID     string    `json:"signer_key_id,omitempty"`
	Warnings        int       `json:"warnings"`
	AuditReportID   string    `json:"audit_report_id,omitempty"`
	InstalledAt     time.Time `json:"installed_at"`
	StatusChangedAt time.Time `json:"status_changed_at"`
}

func packageBody(r apppkg.Record) PackageBody {
	return PackageBody{
		ManifestHash: r.ManifestHash, PackageVersion: r.PackageVersion, State: string(r.State),
		StatusReason: r.StatusReason, SignerKeyID: r.SignerKeyID, Warnings: r.Warnings,
		AuditReportID: r.AuditReportID, InstalledAt: r.InstalledAt.UTC(), StatusChangedAt: r.StatusChangedAt.UTC(),
	}
}

// applicationState summarises the installed packages of an application:
// active when one is, otherwise the state of the newest, not_installed when
// there is none.
func applicationState(recs []apppkg.Record) string {
	if len(recs) == 0 {
		return "not_installed"
	}
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			return string(apppkg.StateActive)
		}
	}
	return string(recs[len(recs)-1].State)
}

// RegistrationBody is the body of a successful POST /v1/applications.
type RegistrationBody struct {
	ApplicationID string      `json:"application_id"`
	Package       PackageBody `json:"package"`
	// Created is false when the very same content was already installed.
	Created bool `json:"created"`
	// Findings are the warnings and information of the checks. There is no
	// critical finding: such a package is refused.
	Findings []contracts.Finding `json:"findings"`
}

func (h *appsHandler) register(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	if !scope.Permits(CapabilityRegisterApplication) {
		WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "the context token does not permit registering an application package").
			WithDetails("capability", CapabilityRegisterApplication))
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != "application/gzip" && mt != "application/x-gzip") {
		WriteErrorStatus(w, r, http.StatusUnsupportedMediaType,
			contracts.New(contracts.ErrValidationFailed, "Content-Type must be application/gzip: a signed .tar.gz application package"))
		return
	}
	// The gateway's body limit (gateway.max_body_bytes) bounds the upload; the
	// loader bounds what the archive expands to.
	archive, err := io.ReadAll(r.Body)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(archive) == 0 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the request body is empty: send the package archive"))
		return
	}
	inst, err := h.apps.Register(r.Context(), scope, archive)
	if err != nil {
		var refused *apppkg.RefusedError
		if errors.As(err, &refused) {
			WriteErrorStatus(w, r, http.StatusUnprocessableEntity, refusalError(refused))
			return
		}
		WriteError(w, r, err)
		return
	}
	if h.audit != nil {
		loc, lerr := audit.ForApplication(scope.ApplicationID, scope.PrincipalID)
		if lerr == nil {
			_, lerr = h.audit.AppendAt(r.Context(), loc, audit.NewEvent{Type: audit.EventPackageInstalled, Data: map[string]any{
				"application_id": scope.ApplicationID, "manifest_hash": inst.Record.ManifestHash,
				"package_version": inst.Record.PackageVersion, "state": string(inst.Record.State),
				"created": inst.Created, "principal_id": scope.PrincipalID,
			}})
		}
		if lerr != nil {
			// The package is installed (quarantined, serving nothing) but the
			// registration could not be recorded: it is not acknowledged. A
			// repeat is idempotent and records it.
			WriteError(w, r, contracts.Wrap(contracts.ErrUnavailable, lerr, "the registration could not be recorded"))
			return
		}
	}
	status := http.StatusOK
	if inst.Created {
		status = http.StatusCreated
	}
	findings := inst.Findings
	if findings == nil {
		findings = []contracts.Finding{}
	}
	writeJSON(w, status, RegistrationBody{
		ApplicationID: scope.ApplicationID, Package: packageBody(inst.Record), Created: inst.Created, Findings: findings,
	})
}

// refusalError is the error envelope of a package that was refused: the
// critical findings, one line each, in details.
func refusalError(e *apppkg.RefusedError) error {
	lines := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		if f.Severity != contracts.SeverityCritical {
			continue
		}
		line := f.CheckID + ": " + f.Message
		if f.Path != "" {
			line += " (" + f.Path + ")"
		}
		if len(line) > 300 {
			line = line[:300]
		}
		lines = append(lines, line)
		if len(lines) == maxRegistrationFindings {
			break
		}
	}
	err := contracts.New(contracts.ErrValidationFailed, "the package was refused: "+e.Reason)
	if len(lines) > 0 {
		err = err.WithDetails("findings", lines)
	}
	return err
}

// ApplicationBody is the body of GET /v1/applications/{application_id}.
type ApplicationBody struct {
	ApplicationID string `json:"application_id"`
	// Status is active, quarantined, rejected, superseded or not_installed.
	Status   string        `json:"status"`
	Packages []PackageBody `json:"packages"`
	// Health is the application's own health report, read only for an active
	// application (a quarantined one is never called).
	Health *ApplicationHealth `json:"health,omitempty"`
}

// ApplicationHealth is the adapter's health report.
type ApplicationHealth struct {
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	ApplicationVersion string `json:"application_version,omitempty"`
}

// healthTimeout bounds the call to an application's own health check.
const healthTimeout = 5 * time.Second

func (h *appsHandler) inspect(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	id := r.PathValue("application_id")
	if !validPathID(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the application id is malformed"))
		return
	}
	if id != scope.ApplicationID {
		// A token speaks for one application: any other is the caller reaching
		// beyond its scope, whether or not that application exists.
		WriteError(w, r, scopeLevelError(contracts.LevelApplication))
		return
	}
	recs, err := h.apps.Records(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	body := ApplicationBody{ApplicationID: id, Status: applicationState(recs), Packages: make([]PackageBody, 0, len(recs))}
	for _, rec := range recs {
		body.Packages = append(body.Packages, packageBody(rec))
	}
	if body.Status == string(apppkg.StateActive) && h.adapters != nil {
		body.Health = h.applicationHealth(r.Context(), id)
	}
	writeJSON(w, http.StatusOK, body)
}

// applicationHealth asks the adapter for the application's health. A failure
// is reported as unavailable with a fixed reason: the cause stays in the log.
func (h *appsHandler) applicationHealth(ctx context.Context, applicationID string) *ApplicationHealth {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	ad, err := h.adapters.Adapter(ctx, applicationID)
	if err == nil {
		var rep adapters.HealthReport
		if rep, err = ad.Health(ctx); err == nil {
			return &ApplicationHealth{Status: string(rep.Status), Reason: rep.Reason, ApplicationVersion: rep.ApplicationVersion}
		}
	}
	loggerFrom(ctx).Warn("application health check failed", "code", string(contracts.CodeOf(err)))
	return &ApplicationHealth{Status: string(adapters.HealthUnavailable), Reason: "the application did not answer its health check"}
}

// CapabilityBody is one capability the caller may see.
type CapabilityBody struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// RiskLevel is the effective risk level the policy gateway assigns (0 to 3).
	RiskLevel int `json:"risk_level"`
	// RequiresConfirmation and RequiresStepUp say what a commit will need.
	RequiresConfirmation bool `json:"requires_confirmation"`
	RequiresStepUp       bool `json:"requires_step_up"`
	// Fields are the fields the caller may supply, in declaration order.
	Fields []string `json:"fields"`
}

// CapabilitiesBody is the body of GET /v1/capabilities.
type CapabilitiesBody struct {
	Capabilities []CapabilityBody `json:"capabilities"`
}

func (h *appsHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	pkg, ok := activePackage(w, r, h.apps, scope)
	if !ok {
		return
	}
	// What the caller may see is the policy gateway's decision per capability
	// (token scope, roles, host rules); it is the same exposure the model gets.
	exp := h.policy.Expose(r.Context(), policy.ExposeRequest{Scope: scope, Package: pkg, Binding: policy.BindingOf(scope)})
	out := CapabilitiesBody{Capabilities: make([]CapabilityBody, 0, len(exp.Visible))}
	for _, dec := range exp.Visible {
		id := dec.Record.CapabilityID
		c := CapabilityBody{
			ID: id, RiskLevel: int(dec.EffectiveRisk),
			RequiresConfirmation: dec.Requires.Confirmation, RequiresStepUp: dec.Requires.StepUp,
			Fields: append([]string{}, dec.VisibleFields...),
		}
		if spec, found := pkg.Spec(id); found {
			c.Title, c.Description = string(spec.Title), string(spec.Purpose)
		}
		out.Capabilities = append(out.Capabilities, c)
	}
	writeJSON(w, http.StatusOK, out)
}

// HealthBody is the body of GET /v1/health: the readiness report of /readyz
// plus the state of the caller's application.
type HealthBody struct {
	ReadyReport
	Application HealthApplication `json:"application"`
}

// HealthApplication is the caller's application in HealthBody.
type HealthApplication struct {
	ApplicationID string `json:"application_id"`
	Status        string `json:"status"`
}

func (h *appsHandler) health(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	recs, err := h.apps.Records(r.Context(), scope.ApplicationID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rep := h.d.ready.Report(r.Context())
	status := http.StatusOK
	if !rep.Ready {
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, HealthBody{ReadyReport: rep, Application: HealthApplication{
		ApplicationID: scope.ApplicationID, Status: applicationState(recs),
	}})
}

// validPathID reports whether a path parameter is a plausible identifier:
// printable ASCII identifier characters only, at most 128 of them. It keeps
// odd bytes out of logs and queries; whether the thing exists and belongs to
// the caller is decided by the store, which filters on the verified scope.
func validPathID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// scopeLevelError is the refusal for reaching beyond the verified scope at one
// level. It names the level and never a value.
func scopeLevelError(level contracts.ScopeLevel) error {
	return contracts.New(contracts.ErrScopeViolation, fmt.Sprintf("the %s in the path is not the one the context token is bound to", level)).
		WithDetails("levels", []string{string(level)})
}

// ApplicationService is what the V1 handlers need of the application
// registry: the active package of an application (the same source the dialogue
// engine and the coordinator decide against), its installed records and
// registration. *Applications is the implementation; tests stand in their own.
type ApplicationService interface {
	dialogue.PackageSource
	// Records lists the installed packages of an application, oldest first.
	Records(ctx context.Context, applicationID string) ([]apppkg.Record, error)
	// Register installs an archive into quarantine for the application of the scope.
	Register(ctx context.Context, scope contracts.Scope, archive []byte) (*apppkg.Installed, error)
}

var _ ApplicationService = (*Applications)(nil)
