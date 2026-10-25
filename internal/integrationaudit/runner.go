package integrationaudit

import (
	"context"
	"crypto/ed25519"
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("integrationaudit", migrations) }

// Defaults of Config.
const (
	// DefaultMaxReportAge is how old a report may be when a package is
	// activated on it.
	DefaultMaxReportAge = 24 * time.Hour
	// DefaultCallTimeout bounds each call the audit makes to an adapter.
	DefaultCallTimeout = 30 * time.Second
	// DefaultDeploymentID is the deployment identifier in the scope the audit
	// uses for its own adapter calls. It never reaches the host application.
	DefaultDeploymentID = "alixai-audit"

	runIDPrefix = "run"
)

// CapabilityRegistry is what the audit tells when an activation changes. The
// registry of internal/capabilities implements it; the audit calls Refresh when
// a package became active and Deactivate when one stopped being, because the
// registry otherwise serves a cached entry for up to 30 seconds.
type CapabilityRegistry interface {
	Refresh(ctx context.Context, applicationID string) error
	Deactivate(applicationID string)
}

// Authorizer says which operators may accept an audit warning. It is consulted
// for every acceptance and denies by default.
type Authorizer interface {
	AuthorizeAcceptance(ctx context.Context, operatorID, applicationID string) error
}

// DenyAll authorises nobody. It is what a Runner uses when no Authorizer is
// configured: warnings cannot be accepted until an operator is named.
type DenyAll struct{}

// AuthorizeAcceptance implements Authorizer.
func (DenyAll) AuthorizeAcceptance(context.Context, string, string) error {
	return contracts.New(contracts.ErrPermissionDenied, "integrationaudit: no operator is authorised to accept audit warnings")
}

// AllowOperators authorises exactly the listed operator IDs, for every
// application. An empty list authorises nobody.
type AllowOperators []string

// AuthorizeAcceptance implements Authorizer.
func (a AllowOperators) AuthorizeAcceptance(_ context.Context, operatorID, _ string) error {
	if operatorID != "" {
		for _, o := range a {
			if o == operatorID {
				return nil
			}
		}
	}
	return contracts.New(contracts.ErrPermissionDenied, "integrationaudit: the operator is not authorised to accept audit warnings")
}

// Config configures a Runner.
type Config struct {
	// DB is the store with the "integrationaudit", "apppkg", "trust" and
	// "audit" migrations applied.
	DB *store.DB
	// Clock is the only source of time. Nil means the wall clock.
	Clock clock.Clock
	// Installer reads installed packages. Required.
	Installer *apppkg.Installer
	// DataDir is the directory the Installer was given: the audit reads the
	// stored package again from <DataDir>/<record path> to collect the
	// loader's findings. Required.
	DataDir string
	// Verifier authenticates packages and report signatures against the
	// trust store. Required, and the same trust store the Installer uses.
	Verifier *trust.Verifier
	// CoreVersion is the AlixAI core version packages are checked against.
	CoreVersion string
	// Limits bound the package, as for the Installer.
	Limits apppkg.Limits
	// Signing is the platform key that signs reports; its public half must be
	// registered under trust.PlatformApplicationID. Without it Run refuses:
	// an unsigned report activates nothing.
	Signing *trust.SigningKey
	// SignatureValidity is how long a report signature is valid. Zero means
	// DefaultReportValidity.
	SignatureValidity time.Duration
	// Log records the audit and activation events. Required: a state change
	// that cannot be recorded does not happen.
	Log *audit.Log
	// Checks is the registry of checks. Nil means Default.
	Checks *Registry
	// Capabilities is told about every activation change. Nil means nothing
	// is told (a process that does not serve traffic).
	Capabilities CapabilityRegistry
	// Authorizer decides who may accept warnings. Nil means DenyAll.
	Authorizer Authorizer
	// DeploymentID is the deployment identifier of the audit's own scope.
	DeploymentID string
	// Profile is the model and runtime profile the audit runs on, if known.
	Profile string
	// MaxReportAge is how old a report may be at activation. Zero means
	// DefaultMaxReportAge.
	MaxReportAge time.Duration
	// CallTimeout bounds each adapter call. Zero means DefaultCallTimeout.
	CallTimeout time.Duration
}

// Runner runs integration audits and owns the transitions of an installed
// package between quarantined and active. It is safe for concurrent use.
//
// It is the only code in AlixAI that sets a package active: the installer has
// no such function, and the database refuses an activation that carries no
// audit report.
type Runner struct {
	cfg  Config
	clk  clock.Clock
	ids  *ids.Generator
	chks *Registry
	auth Authorizer
}

// NewRunner returns a Runner. It does not touch the database until it is used.
func NewRunner(cfg Config) (*Runner, error) {
	switch {
	case cfg.DB == nil:
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the runner needs a database")
	case cfg.Installer == nil:
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the runner needs an installer")
	case cfg.DataDir == "":
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the runner needs the data directory the installer uses")
	case cfg.Verifier == nil:
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the runner needs a trust verifier")
	case cfg.CoreVersion == "":
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the runner needs the core version")
	case cfg.Log == nil:
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the runner needs an audit log: a state change that cannot be recorded does not happen")
	}
	if cfg.Signing != nil && (cfg.Signing.KeyID == "" || len(cfg.Signing.Key) != ed25519.PrivateKeySize) {
		return nil, contracts.New(contracts.ErrInternal, "integrationaudit: the signing key is not an Ed25519 private key with an id")
	}
	r := &Runner{cfg: cfg, clk: cfg.Clock, chks: cfg.Checks, auth: cfg.Authorizer}
	if r.clk == nil {
		r.clk = clock.Real{}
	}
	r.ids = ids.NewGenerator(r.clk)
	if r.chks == nil {
		r.chks = Default
	}
	if r.auth == nil {
		r.auth = DenyAll{}
	}
	if r.cfg.MaxReportAge <= 0 {
		r.cfg.MaxReportAge = DefaultMaxReportAge
	}
	if r.cfg.CallTimeout <= 0 {
		r.cfg.CallTimeout = DefaultCallTimeout
	}
	if r.cfg.DeploymentID == "" {
		r.cfg.DeploymentID = DefaultDeploymentID
	}
	return r, nil
}

// RunRequest asks for an audit.
type RunRequest struct {
	// ApplicationID is the application to audit. Required.
	ApplicationID string
	// ManifestHash picks the installed package. Empty means the newest package
	// of the application that is quarantined.
	ManifestHash string
	// Adapter reaches the application. Nil means none is available: the
	// runtime and application targets are then recorded as unverified.
	Adapter adapters.AppAdapter
	// OperatorID is who asked for the audit, for the audit log. Empty means
	// the system.
	OperatorID string
}

// Run is the result of an audit.
type Run struct {
	// RunID identifies this run in the database and the audit log. It is not
	// part of the report, which is deterministic.
	RunID string
	// Signed is the signed report that was stored.
	Signed SignedReport
	// Record is the installed package as it was when the audit started.
	Record apppkg.Record
}

// Disposition is what the report permits.
func (r *Run) Disposition() Disposition { return r.Signed.Report.Summary.Outcome }

// Audit audits an installed package (master plan 4.5, 9.4): it loads the stored
// bytes with full verification, gathers what the application exposes, runs
// every registered check in stage order, assembles the deterministic report,
// signs it with the platform key and stores it. It never asks a model anything.
//
// An audit does not change the package's state: only Activate does, from a
// stored report. Auditing a package that is rejected or superseded is refused;
// auditing an active one (a re-audit) is allowed and leaves it active.
func (r *Runner) Audit(ctx context.Context, req RunRequest) (*Run, error) {
	if r.cfg.Signing == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "integrationaudit: no platform signing key is configured, so no report can be signed")
	}
	rec, err := r.resolve(ctx, req.ApplicationID, req.ManifestHash)
	if err != nil {
		return nil, err
	}
	if rec.State != apppkg.StateQuarantined && rec.State != apppkg.StateActive {
		return nil, contracts.Newf(contracts.ErrConflict, "integrationaudit: the package is %s and is not audited", rec.State).
			WithDetails("application_id", rec.ApplicationID).WithDetails("manifest_hash", rec.ManifestHash)
	}
	runID := r.ids.New(runIDPrefix)
	loc, err := audit.ForApplication(rec.ApplicationID, req.OperatorID)
	if err != nil {
		return nil, err
	}
	if _, err := r.cfg.Log.AppendAt(ctx, loc, audit.NewEvent{
		Type: audit.EventAuditRunStarted,
		Data: map[string]any{"run_id": runID, "manifest_hash": rec.ManifestHash, "package_version": rec.PackageVersion},
	}); err != nil {
		return nil, err
	}

	in, err := r.gather(ctx, rec, req.Adapter, runID)
	if err != nil {
		return nil, err
	}
	results := execute(ctx, in, r.chks.Checks())
	report, err := assemble(in, results)
	if err != nil {
		return nil, err
	}
	signed, err := SignReport(*report, r.cfg.Signing.KeyID, r.cfg.Signing.Key, r.clk.Now(), r.cfg.SignatureValidity)
	if err != nil {
		return nil, err
	}
	// The report must verify before it is stored: a key that is not registered
	// for the platform, or a revoked one, is found now and not at activation.
	if _, err := VerifyReport(ctx, r.cfg.Verifier, signed); err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "integrationaudit: the platform signing key is not accepted by the trust store")
	}
	doc, err := signed.Marshal()
	if err != nil {
		return nil, err
	}
	issued := r.clk.Now().UTC()
	if err := r.cfg.DB.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO integrationaudit_runs (run_id, application_id, manifest_hash, report_sha256, disposition, issued_at, document)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			runID, rec.ApplicationID, rec.ManifestHash, signed.Attestation.ReportSHA256,
			string(report.Summary.Outcome), issued.UnixMilli(), string(doc)); err != nil {
			return err
		}
		_, err := r.cfg.Log.AppendTx(ctx, tx, loc, audit.NewEvent{
			Type: audit.EventAuditRunCompleted,
			Data: map[string]any{
				"run_id": runID, "manifest_hash": rec.ManifestHash, "report_sha256": signed.Attestation.ReportSHA256,
				"outcome": string(report.Summary.Outcome), "critical": report.Summary.Critical,
				"warnings": report.Summary.Warnings, "unverified": report.Summary.Unverified,
			},
		})
		return err
	}); err != nil {
		return nil, wrapStore(err, "cannot record the audit report")
	}
	return &Run{RunID: runID, Signed: signed, Record: rec}, nil
}

// resolve finds the installed package to audit or activate.
func (r *Runner) resolve(ctx context.Context, app, hash string) (apppkg.Record, error) {
	if app == "" {
		return apppkg.Record{}, contracts.New(contracts.ErrScopeMissing, "integrationaudit: no application given")
	}
	if hash != "" {
		return r.cfg.Installer.Get(ctx, app, hash)
	}
	recs, err := r.cfg.Installer.List(ctx, app)
	if err != nil {
		return apppkg.Record{}, err
	}
	var best *apppkg.Record
	for i := range recs {
		if recs[i].State != apppkg.StateQuarantined {
			continue
		}
		if best == nil || !recs[i].InstalledAt.Before(best.InstalledAt) {
			best = &recs[i]
		}
	}
	if best == nil {
		return apppkg.Record{}, contracts.New(contracts.ErrConflict, "integrationaudit: the application has no quarantined package to audit").
			WithDetails("application_id", app)
	}
	return *best, nil
}

// gather collects everything the checks look at, once.
func (r *Runner) gather(ctx context.Context, rec apppkg.Record, adapter adapters.AppAdapter, runID string) (*Input, error) {
	in := &Input{
		Record:      rec,
		CoreVersion: r.cfg.CoreVersion,
		Profile:     r.cfg.Profile,
		Clock:       r.clk,
		Scope:       r.auditScope(rec.ApplicationID, runID),
	}
	for _, c := range r.chks.Checks() {
		in.Registered = append(in.Registered, CheckInfo{ID: c.ID(), Section: c.Section(), Stage: c.Stage(), Target: c.Target()})
	}

	// The package is opened through the Installer, which verifies the stored
	// bytes against the signature, the trust store and the recorded hash, and
	// loaded once more for the loader's findings (warnings and information that
	// Open does not return). Both must agree; either failing blocks.
	pkg, _, err := r.cfg.Installer.Open(ctx, rec.ApplicationID, rec.ManifestHash)
	var refused *apppkg.RefusedError
	switch {
	case errors.As(err, &refused):
		in.LoadFindings = append([]contracts.Finding(nil), refused.Findings...)
		if !contracts.HasCritical(in.LoadFindings) {
			in.LoadFindings = append(in.LoadFindings, newFinding(fdPackageRefused, "", "", "%s", refused.Reason))
		}
		in.Authenticity = apppkg.AuthFailed
		return in, nil
	case err != nil:
		return nil, err
	}
	res, err := apppkg.Load(ctx, apppkg.FromInstalledDir(filepath.Join(r.cfg.DataDir, filepath.FromSlash(rec.Path))), apppkg.Options{
		Verifier: r.cfg.Verifier, CoreVersion: r.cfg.CoreVersion, Limits: r.cfg.Limits,
	})
	if err != nil {
		return nil, err
	}
	in.LoadFindings = res.Findings
	in.Authenticity = res.Authenticity
	in.Signer = res.Signer
	if res.Critical() || res.Package == nil || res.ManifestHash != rec.ManifestHash {
		in.LoadFindings = append(in.LoadFindings, newFinding(fdPackageRefused, "", "", "the stored package changed while it was being audited"))
		in.Authenticity = apppkg.AuthFailed
		return in, nil
	}
	in.Package = pkg

	if adapter != nil {
		in.Adapter = adapter
		r.inspect(ctx, in)
	}
	return in, nil
}

// inspect asks the adapter, once, what it describes and exposes. Failures are
// recorded as typed codes; the checks turn them into findings.
func (r *Runner) inspect(ctx context.Context, in *Input) {
	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, r.cfg.CallTimeout) }

	cctx, cancel := call()
	d, err := in.Adapter.Describe(cctx)
	cancel()
	switch {
	case err != nil:
		in.DescribeErr = contracts.CodeOf(err)
	default:
		if verr := d.Validate(); verr != nil {
			in.DescribeErr = contracts.ErrSchemaInvalid
		} else {
			in.Description = &d
		}
	}

	req := adapters.InspectRequest{}
	cctx, cancel = call()
	ins, err := in.Adapter.Inspect(cctx, in.Scope, req)
	cancel()
	switch {
	case err != nil:
		in.InspectErr = contracts.CodeOf(err)
	default:
		if checked, cerr := adapters.CheckInspection(ins, req); cerr != nil {
			in.InspectErr = contracts.ErrSchemaInvalid
		} else {
			in.Inspection = &checked
		}
	}

	cctx, cancel = call()
	h, err := in.Adapter.Health(cctx)
	cancel()
	switch {
	case err != nil:
		in.HealthErr = contracts.CodeOf(err)
	default:
		if verr := h.Validate(); verr != nil {
			in.HealthErr = contracts.ErrSchemaInvalid
		} else {
			in.Health = &h
		}
	}
}

// auditScope is the identity of the audit's own calls: valid at every level and
// bound to the audited application. It is not a tenant of the application.
func (r *Runner) auditScope(app, runID string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     r.cfg.DeploymentID,
		ApplicationID:    app,
		TenantID:         "alixai-audit",
		WorkspaceID:      "alixai-audit",
		PrincipalID:      "alixai-audit",
		Roles:            []string{"auditor"},
		CapabilityScopes: []string{"audit.inspect"},
		SessionID:        "audit-" + runID,
	}
}

// wrapStore classifies a failure of the store: a typed error passes through,
// anything else is unavailable.
func wrapStore(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, fmt.Sprintf("integrationaudit: %s", msg))
}
