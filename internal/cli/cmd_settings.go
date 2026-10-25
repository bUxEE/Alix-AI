package cli

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The settings area of `alixai serve` (master plan 14.2, M3-07): this file builds
// the one service the area needs, gateway.SettingsService, and hands it to the
// server through the serve provider registry. Nothing in cmd_serve.go knows about
// it, and without it the area is unmounted and serve refuses to start.
//
// What is wired, and what deliberately is not:
//
//   - the approved profiles, ceilings and approved voices are the operator's
//     "settings" configuration section (internal/settings.Config), never a
//     setting a client can write;
//   - the active package comes from the same application service the other areas
//     use, so a quarantined application serves no settings;
//   - a change is recorded in the process's audit log in the same transaction as
//     the change, and capabilities named by a setting are checked by the same
//     policy gateway that decides exposure;
//   - the history retention of a tenant or deployment document is stored by history, not
//     here (one policy, applied by the retention job serve schedules; settings/retention.go
//     and cmd_history_retention.go), with its own settings.changed event in the transaction
//     of the policy;
//   - the integration-audit read-out reads the stored, signed reports of
//     internal/integrationaudit (and verifies the signature before it reports a
//     word of one); it can not run an audit, accept a warning or activate a package;
//   - the provider-health read-out is the registry's role statuses: role, status
//     and degradation effect, no runtime name and no address.

func init() { registerServeProvider("settings", settingsServeProvider) }

// settingsServeProvider provides gateway.SettingsService.
func settingsServeProvider(_ context.Context, d serveDeps) error {
	apps, ok := gateway.Lookup[gateway.ApplicationService](d.Services)
	if !ok {
		return errors.New("settings: the application service is not provided")
	}
	deps := settings.Deps{
		DB: d.DB, Clock: d.Clock, Audit: d.Audit, Policy: d.Policy,
		Packages: settingsPackages{apps: apps},
		Limits:   settings.CurrentConfig().Limits(),
	}
	reports, err := newSettingsReports(d)
	if err != nil {
		return err
	}
	deps.Findings = reports
	// The history retention a tenant or deployment document states is history's: one stored policy,
	// applied by the retention job serve schedules (cmd_history_retention.go). The history provider
	// runs before this one.
	if h, ok := gateway.Lookup[*history.Service](d.Services); ok {
		deps.Retention = historyRetentionSettings{svc: h}
	}
	if d.Providers != nil {
		deps.Providers = settingsProviders{reg: d.Providers}
	}
	svc, err := settings.New(deps)
	if err != nil {
		return err
	}
	return gateway.Provide[gateway.SettingsService](d.Services, svc)
}

// settingsPackages gives the settings service the application's active package,
// the one the dialogue engine and the action coordinator decide against.
type settingsPackages struct{ apps gateway.ApplicationService }

func (p settingsPackages) Package(ctx context.Context, applicationID string) (settings.Package, error) {
	pkg, err := p.apps.Package(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	return pkg, nil
}

// settingsProviders reports the health of the model roles from the registry's
// last probe. Only role, status and effect leave it.
type settingsProviders struct{ reg *providers.Registry }

func (p settingsProviders) ProviderHealth(context.Context) (settings.ProviderHealth, error) {
	snap := p.reg.Snapshot()
	out := settings.ProviderHealth{Status: string(p.reg.Overall()), Roles: make([]settings.ProviderRole, 0, len(snap))}
	for _, r := range snap {
		out.Roles = append(out.Roles, settings.ProviderRole{Role: string(r.Role), Configured: r.Configured, Status: string(r.Status), Effect: r.Effect})
	}
	return out, nil
}

// settingsReports reads the stored integration-audit reports of an application.
type settingsReports struct {
	inst     *apppkg.Installer
	runner   *integrationaudit.Runner
	verifier *trust.Verifier
}

func newSettingsReports(d serveDeps) (*settingsReports, error) {
	core := apppkg.CoreVersionOrDev(version.Get().Version)
	verifier := trust.NewVerifier(trust.NewStore(d.DB, d.Clock), d.Clock)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: d.DB, Clock: d.Clock, DataDir: d.DataDir, Verifier: verifier, CoreVersion: core})
	if err != nil {
		return nil, err
	}
	// A reader only: no signing key, no capability registry, nobody authorised to
	// accept a warning. It can not run an audit or activate a package.
	runner, err := integrationaudit.NewRunner(integrationaudit.Config{
		DB: d.DB, Clock: d.Clock, Installer: inst, DataDir: d.DataDir, Verifier: verifier, CoreVersion: core, Log: d.Audit,
	})
	if err != nil {
		return nil, err
	}
	return &settingsReports{inst: inst, runner: runner, verifier: verifier}, nil
}

// IntegrationAudit implements settings.AuditSource. The package it reports is the
// active one, or else the newest; a report is read only after its signature
// verifies, and one that does not is an error, never a partial answer.
func (s *settingsReports) IntegrationAudit(ctx context.Context, applicationID string) (settings.AuditStatus, error) {
	st := settings.AuditStatus{ApplicationID: applicationID, State: "not_installed", Findings: []settings.AuditFinding{}}
	recs, err := s.inst.List(ctx, applicationID)
	if err != nil {
		return settings.AuditStatus{}, err
	}
	if len(recs) == 0 {
		return st, nil
	}
	rec := recs[len(recs)-1]
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			rec = r
		}
	}
	st.State, st.PackageVersion, st.ManifestHash = string(rec.State), rec.PackageVersion, rec.ManifestHash
	run, found, err := s.runner.LatestRun(ctx, applicationID, rec.ManifestHash)
	if err != nil {
		return settings.AuditStatus{}, err
	}
	if !found {
		return st, nil
	}
	if _, err := integrationaudit.VerifyReport(ctx, s.verifier, run.Signed); err != nil {
		return settings.AuditStatus{}, contracts.Wrap(contracts.ErrInternal, err, "settings: the stored integration-audit report does not verify")
	}
	accepted, err := s.runner.Acceptances(ctx, applicationID, rec.ManifestHash)
	if err != nil {
		return settings.AuditStatus{}, err
	}
	rep := run.Signed.Report
	st.Audited, st.RunID, st.Outcome = true, run.RunID, string(rep.Summary.Outcome)
	st.Counts = settings.AuditCounts{Critical: rep.Summary.Critical, Warnings: rep.Summary.Warnings, Info: rep.Summary.Info, Unverified: rep.Summary.Unverified}
	for _, f := range rep.Findings {
		status := settings.FindingOpen
		switch f.Severity {
		case contracts.SeverityInfo:
			status = settings.FindingInformational
		case contracts.SeverityWarning:
			if _, ok := accepted[f.Key]; ok {
				status = settings.FindingAccepted
			}
		}
		st.Findings = append(st.Findings, settings.AuditFinding{
			Key: f.Key, CheckID: f.CheckID, Severity: string(f.Severity), Capability: f.Capability, Path: f.Path,
			Message: f.Message, Remediation: f.Remediation, Status: status,
		})
	}
	return st, nil
}
