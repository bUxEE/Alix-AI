package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const auditSectionName = "integration_audit"

func init() {
	config.Register(auditSectionName, &auditSection)
	Register(Command{
		Name:    "audit run",
		Summary: "audit an installed application package, sign the report and activate the package if the audit is clear",
		Run:     runAuditRun,
	})
	Register(Command{
		Name:    "audit accept-warning",
		Summary: "record that an authorised operator accepted one warning of a package's latest audit report",
		Run:     runAuditAcceptWarning,
	})
	Register(Command{
		Name:    "audit quarantine",
		Summary: "take an active package out of service at once: quarantine it and tell the running server to stop serving it",
		Run:     runAuditQuarantine,
	})
}

// auditSection is the registered "integration_audit" section.
//
//	integration_audit:
//	  signing_key_id: alixai-audit-signer-2026   # registered under alixai.platform: alixai trust add-key -app alixai.platform
//	  signing_key_file: /etc/alixai/audit-signer.pem   # PKCS#8 PEM, readable by the service user only
//	  accept_operators: [alice, bob]             # who may accept audit warnings; empty: nobody
//	  profile: lite                              # the model/runtime profile the audit is recorded against
//	  max_report_age_hours: 24                   # how old a report may be when a package is activated on it
var auditSection auditConfig

type auditConfig struct {
	SigningKeyID      string   `yaml:"signing_key_id"`
	SigningKeyFile    string   `yaml:"signing_key_file"`
	AcceptOperators   []string `yaml:"accept_operators"`
	Profile           string   `yaml:"profile"`
	MaxReportAgeHours int      `yaml:"max_report_age_hours"`
}

var operatorIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:-]{0,127}$`)

// Validate implements the section validation hook.
func (c auditConfig) Validate() error {
	if (c.SigningKeyID == "") != (c.SigningKeyFile == "") {
		return errors.New("signing_key_id and signing_key_file go together")
	}
	if strings.ContainsRune(c.SigningKeyFile, 0) {
		return errors.New("signing_key_file must not contain NUL")
	}
	for i, o := range c.AcceptOperators {
		if !operatorIDRE.MatchString(o) {
			return fmt.Errorf("accept_operators[%d] is not an operator id (1 to 128 letters, digits and ._@:-)", i)
		}
	}
	switch c.Profile {
	case "", "experimental-4g", "lite", "balanced", "performance":
	default:
		return fmt.Errorf("profile %q: want experimental-4g, lite, balanced or performance", c.Profile)
	}
	if c.MaxReportAgeHours < 0 || c.MaxReportAgeHours > 24*30 {
		return errors.New("max_report_age_hours must be between 0 (the default, 24) and 720")
	}
	return nil
}

// stack is what the operator commands share: the store, the trust store and the
// audit log of the deployment, and the line to a running server (transports.go).
type stack struct {
	db         *store.DB
	clk        clock.Clock
	dataDir    string
	trustStore *trust.Store
	verifier   *trust.Verifier
	log        *audit.Log
	cfg        auditConfig
	// server tells a server running on this data directory about an activation
	// or a quarantine; the runner calls it.
	server *transportsServer
	// clearSecrets unregisters application_credentials when the stack closes.
	clearSecrets func()
}

// openStack loads the configuration at configPath and opens the deployment's
// store, creating the data directory's database on first use. allowTestKeys
// opens the trust store for the published TEST-ONLY keys, which only a
// developer's deployment does.
func openStack(ctx context.Context, env *Env, configPath string, allowTestKeys bool) (*stack, error) {
	if err := env.Config.LoadFrom(configPath, env.LookupEnv); err != nil {
		return nil, err
	}
	core := env.Config.Core()
	opts := store.FromConfig()
	opts.Path = filepath.Join(core.DataDir.String(), store.DefaultFileName)
	db, err := store.Open(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("open the store: %w", err)
	}
	s := &stack{db: db, clk: clock.Real{}, dataDir: core.DataDir.String(), cfg: auditSection, server: newTransportsServer(core.DataDir.String())}
	var tsOpts []trust.StoreOption
	if allowTestKeys {
		tsOpts = append(tsOpts, trust.AllowTestOnlyKeys())
	}
	s.trustStore = trust.NewStore(db, s.clk, tsOpts...)
	s.verifier = trust.NewVerifier(s.trustStore, s.clk)
	if s.log, err = audit.New(db, audit.Options{Clock: s.clk}); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := stackWireApplicationCredentials(ctx, s); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *stack) close() {
	if s.clearSecrets != nil {
		s.clearSecrets()
		s.clearSecrets = nil
	}
	_ = s.db.Close()
}

func (s *stack) installer(coreVersion string) (*apppkg.Installer, error) {
	return apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: s.db, Clock: s.clk, DataDir: s.dataDir, Verifier: s.verifier, CoreVersion: resolveCoreVersion(coreVersion),
	})
}

// emitJSON writes v as indented JSON.
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// loadSigningKey reads the platform key that signs audit reports.
func loadSigningKey(env *Env, spec string, cfg auditConfig) (*trust.SigningKey, error) {
	id, file := cfg.SigningKeyID, cfg.SigningKeyFile
	if spec != "" {
		var ok bool
		if id, file, ok = strings.Cut(spec, "="); !ok || id == "" || file == "" {
			return nil, Usagef("-signing-key %q is not KEYID=KEYFILE", spec)
		}
	}
	if id == "" {
		return nil, Usagef("no signing key: pass -signing-key KEYID=KEYFILE or set integration_audit.signing_key_id and signing_key_file; its public half must be registered with 'alixai trust add-key -app %s'", trust.PlatformApplicationID)
	}
	key, warnings, err := trust.LoadPrivateKeyFile(file)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		// A signing key other users can read lets them forge a report (as loadCheckpointKey).
		return nil, fmt.Errorf("the audit signing key cannot be used: %s", warnings[0])
	}
	if trust.IsTestOnlyKey(key.Public().(ed25519.PublicKey)) {
		return nil, Usagef("the signing key is a published TEST-ONLY key that anyone can sign with; generate your own with 'alixai package keygen'")
	}
	return &trust.SigningKey{KeyID: id, Key: key}, nil
}

func (s *stack) runner(env *Env, spec string, core string, needKey bool) (*integrationaudit.Runner, error) {
	inst, err := s.installer(core)
	if err != nil {
		return nil, err
	}
	cfg := integrationaudit.Config{
		DB: s.db, Clock: s.clk, Installer: inst, DataDir: s.dataDir, Verifier: s.verifier,
		CoreVersion: resolveCoreVersion(core), Log: s.log,
		Authorizer: integrationaudit.AllowOperators(s.cfg.AcceptOperators),
		Profile:    s.cfg.Profile,
		// An activation refreshes, and a quarantine evicts, the capability
		// registry of a server running on this data directory at once, through
		// its admin interface, instead of leaving it to the registry's 30 s cache.
		Capabilities: s.server,
	}
	if s.cfg.MaxReportAgeHours > 0 {
		cfg.MaxReportAge = time.Duration(s.cfg.MaxReportAgeHours) * time.Hour
	}
	if needKey {
		key, err := loadSigningKey(env, spec, s.cfg)
		if err != nil {
			return nil, err
		}
		cfg.Signing = key
	}
	return integrationaudit.NewRunner(cfg)
}

const auditRunUsage = "alixai audit run [-config FILE] [-package HASH] [-no-activate] [-signing-key KEYID=KEYFILE] [-operator ID] [-core-version V] APPLICATION"

// findingOut is a finding as the audit commands print it.
type findingOut struct {
	Key         string `json:"key"`
	Severity    string `json:"severity"`
	CheckID     string `json:"check_id"`
	Capability  string `json:"capability,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	// Accepted is set for a warning an operator accepted for this package.
	Accepted   bool   `json:"accepted,omitempty"`
	AcceptedBy string `json:"accepted_by,omitempty"`
}

type sectionOut struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type auditRunOut struct {
	RunID          string                   `json:"run_id"`
	ApplicationID  string                   `json:"application_id"`
	PackageVersion string                   `json:"package_version"`
	ManifestHash   string                   `json:"manifest_hash"`
	ReportSHA256   string                   `json:"report_sha256"`
	Outcome        string                   `json:"outcome"`
	Summary        integrationaudit.Summary `json:"summary"`
	Targets        []string                 `json:"targets"`
	Sections       []sectionOut             `json:"sections"`
	Findings       []findingOut             `json:"findings"`
	// Activated is true when this command activated the package.
	Activated bool `json:"activated"`
	// StaysInQuarantine says why the package was not activated.
	StaysInQuarantine string `json:"stays_in_quarantine,omitempty"`
	// Transport says which adapter the audit inspected, or why it inspected
	// none (the application and runtime targets are then unverified).
	Transport auditTransportOut `json:"transport"`
	// Server says what the running server did when the package was activated:
	// absent when this command did not activate one.
	Server *transportsServerOut `json:"server,omitempty"`
	// Readiness links the latest readiness report stored for the application (alixai readiness scan -app), or says there
	// is none. The audit does not read it and is not changed by it.
	Readiness *rdyCLIAuditLinkOut `json:"readiness"`
}

// auditTransportOut is the adapter line of an audit's output.
type auditTransportOut struct {
	// Status is "in_use" (the audit inspected the adapter the server would
	// call) or "unverified" (there is none).
	Status string `json:"status"`
	// Kind is the transport kind the manifest declares, when it declares one.
	Kind string `json:"kind,omitempty"`
	// Reason is one of the transports reasons (not_registered,
	// no_operation_endpoint, mixed_kinds, package_unreadable) and Detail the
	// sentence that goes with it.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// EventsEndpoints are declared events endpoints, which nothing consumes yet.
	EventsEndpoints []string `json:"events_endpoints,omitempty"`
}

const (
	auditTransportInUse      = "in_use"
	auditTransportUnverified = "unverified"
)

// auditAdapter is what the audit runs on: the package it audits (resolved the
// way the runner resolves it, so the adapter and the audit are about the same
// bytes) and the adapter for it, nil when there is none.
type auditAdapter struct {
	manifestHash string
	adapter      adapters.AppAdapter
	out          auditTransportOut
}

// auditResolveAdapter builds the adapter an audit inspects, through the same
// transportsSource `alixai serve` uses. It never fails an audit for the absence
// of a transport: that is "unverified", said on stderr and in the output, and
// the framework turns it into the warnings an operator must accept. It does fail
// for a transport that is registered and cannot be opened (the application is
// unreachable or its adapter configuration is refused): the operator asked to
// audit a system that cannot be reached, and a quiet downgrade to unverified
// would hide a mistake.
func auditResolveAdapter(ctx context.Context, env *Env, inst *apppkg.Installer, dataDir, app, hash string) (auditAdapter, error) {
	res := auditAdapter{manifestHash: hash}
	unverified := func(o auditTransportOut) auditAdapter {
		o.Status = auditTransportUnverified
		res.out, res.adapter = o, nil
		fmt.Fprintf(env.Stderr, "alixai: note: no adapter is available for this application: %s, so its control surface and runtime are recorded as unverified\n", o.Detail)
		return res
	}

	rec, err := auditPackageToAudit(ctx, inst, app, hash)
	if err != nil {
		// The runner reports the same failure, with its own wording and exit
		// status; the audit cannot start without the package, so there is nothing
		// to build an adapter for and nothing more to say about one.
		res.out = auditTransportOut{Status: auditTransportUnverified, Reason: transportsReasonPackage, Detail: "the package to audit could not be found"}
		return res, nil
	}
	res.manifestHash = rec.ManifestHash

	ad, plan, err := transportsSource{installer: inst, dataDir: dataDir}.adapter(ctx, app, rec.ManifestHash)
	if plan != nil && len(plan.Events) > 0 {
		res.out.EventsEndpoints = plan.Events
		fmt.Fprintf(env.Stderr, "alixai: note: the manifest declares events endpoint(s) %s; this build consumes none of them\n", strings.Join(plan.Events, ", "))
	}
	if err == nil {
		res.adapter = ad
		res.out.Status, res.out.Kind = auditTransportInUse, plan.Kind
		return res, nil
	}
	reason, kind, typed := transportsUnavailableReason(err)
	var ce *contracts.Error
	detail := err.Error()
	if errors.As(err, &ce) && ce != nil {
		detail = ce.Message
	}
	switch {
	case transportsNoAdapter(err), typed && reason == transportsReasonPackage:
		keep := res.out
		keep.Kind, keep.Reason, keep.Detail = kind, reason, detail
		return unverified(keep), nil
	case typed && reason == transportsReasonOpenFailed:
		return auditAdapter{}, fmt.Errorf("cannot build the %s adapter for %s: %w", kind, app, err)
	}
	return auditAdapter{}, fmt.Errorf("cannot build the adapter for %s: %w", app, err)
}

// auditPackageToAudit picks the package an audit runs on as the runner does: the
// one named, or the application's newest quarantined package.
func auditPackageToAudit(ctx context.Context, inst *apppkg.Installer, app, hash string) (apppkg.Record, error) {
	if hash != "" {
		return inst.Get(ctx, app, hash)
	}
	recs, err := inst.List(ctx, app)
	if err != nil {
		return apppkg.Record{}, err
	}
	var best *apppkg.Record
	for i := range recs {
		if recs[i].State == apppkg.StateQuarantined && (best == nil || !recs[i].InstalledAt.Before(best.InstalledAt)) {
			best = &recs[i]
		}
	}
	if best == nil {
		return apppkg.Record{}, contracts.New(contracts.ErrConflict, "the application has no quarantined package to audit")
	}
	return *best, nil
}

// runAuditRun audits the newest quarantined package of an application: it runs
// every registered check against the stored, re-verified package and, where an
// adapter is available, the application; signs the report with the platform key;
// stores it; and activates the package when the report allows it. The report
// allows it when it has no critical finding and every warning in it was accepted
// by an authorised operator (audit accept-warning). It never asks a model
// anything.
//
// Exit status 0 means the audit ran and, unless -no-activate was given, the
// package is active. Exit status 1 means the package stays in quarantine; the
// output says why and lists the exact remediation items.
func runAuditRun(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("audit run")
	path := env.ConfigFlag(fs)
	pkgHash := fs.String("package", "", "manifest hash of the installed package to audit (default: the application's newest quarantined package)")
	noActivate := fs.Bool("no-activate", false, "audit and store the report but do not activate the package")
	keySpec := fs.String("signing-key", "", "KEYID=KEYFILE: the platform key that signs the report (default: integration_audit.signing_key_id and signing_key_file)")
	operator := fs.String("operator", "", "who runs the audit, recorded in the audit log")
	core := fs.String("core-version", "", "AlixAI core version the package is checked against (default: this binary's version)")
	if err := env.ParseFlags(fs, auditRunUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one application ID (usage: %s)", auditRunUsage)
	}
	if *operator != "" && !operatorIDRE.MatchString(*operator) {
		return Usagef("-operator %q is not an operator id (1 to 128 letters, digits and ._@:-)", *operator)
	}
	app := fs.Arg(0)

	s, err := openStack(ctx, env, *path, false)
	if err != nil {
		return err
	}
	defer s.close()
	// The RAG checks of the audit see this deployment, and only for this audit (cmd_audit_rag.go).
	defer auditRAGInstall(s)()
	runner, err := s.runner(env, *keySpec, *core, true)
	if err != nil {
		return err
	}

	// The adapter is built the way `alixai serve` builds it: by the transport
	// registered for the endpoint kind the package's manifest declares
	// (transports.go). With none, the application and runtime targets are
	// recorded as unverified, and this says why.
	inst, err := s.installer(*core)
	if err != nil {
		return err
	}
	res, err := auditResolveAdapter(ctx, env, inst, s.dataDir, app, *pkgHash)
	if err != nil {
		return err
	}
	defer transportsClose(res.adapter)
	run, err := runner.Audit(ctx, integrationaudit.RunRequest{ApplicationID: app, ManifestHash: res.manifestHash, Adapter: res.adapter, OperatorID: *operator})
	if err != nil {
		return err
	}
	accepted, err := runner.Acceptances(ctx, run.Record.ApplicationID, run.Record.ManifestHash)
	if err != nil {
		return err
	}
	out := summarise(run, accepted)
	out.Transport = res.out

	var activateErr error
	switch {
	case *noActivate:
		out.StaysInQuarantine = "activation was not requested (-no-activate)"
	case run.Disposition() == integrationaudit.DispositionBlocked:
		out.StaysInQuarantine = "the audit found critical problems"
	default:
		act, err := runner.Activate(ctx, integrationaudit.ActivateRequest{
			ApplicationID: app, ManifestHash: run.Record.ManifestHash, OperatorID: *operator,
		})
		switch {
		case err == nil:
			out.Activated = true
		case contracts.HasCode(err, contracts.ErrQuarantined):
			out.StaysInQuarantine = err.Error()
			activateErr = err
		default:
			if act != nil && act.Record.State == apppkg.StateActive {
				out.Activated = true // active, but the registry of a running process was not told
				fmt.Fprintf(env.Stderr, "alixai: warning: %v\n", err)
			} else {
				return err
			}
		}
	}
	if out.Activated {
		// The runner told the server (if one is running on this data directory)
		// to reload the application; say what came of it.
		if sv, _ := s.server.result(); sv != nil {
			out.Server = sv
			fmt.Fprintf(env.Stderr, "alixai: note: %s\n", sv.note(app))
		}
	}
	out.Readiness = rdyCLIAuditLink(ctx, s.db, app)
	if err := emitJSON(env.Stdout, out); err != nil {
		return err
	}
	switch {
	case out.Activated, *noActivate && run.Disposition() != integrationaudit.DispositionBlocked:
		return nil
	case run.Disposition() == integrationaudit.DispositionBlocked:
		return fmt.Errorf("the package stays in quarantine: the audit found %d critical finding(s)", out.Summary.Critical)
	case activateErr != nil:
		return fmt.Errorf("the package stays in quarantine: %w", activateErr)
	}
	return errors.New("the package stays in quarantine")
}

// summarise turns a run into what the command prints: the critical findings and
// warnings with the keys an operator needs, and the status of every section.
func summarise(run *integrationaudit.Run, accepted map[string]integrationaudit.Acceptance) auditRunOut {
	rep := run.Signed.Report
	out := auditRunOut{
		RunID: run.RunID, ApplicationID: rep.Subject.ApplicationID, PackageVersion: rep.Subject.PackageVersion,
		ManifestHash: rep.Subject.ManifestHash, ReportSHA256: run.Signed.Attestation.ReportSHA256,
		Outcome: string(rep.Summary.Outcome), Summary: rep.Summary,
		Targets: []string{}, Sections: []sectionOut{}, Findings: []findingOut{},
	}
	for _, t := range rep.Targets {
		out.Targets = append(out.Targets, fmt.Sprintf("%s: %s", t.Target, t.Status))
	}
	for _, sec := range rep.Sections {
		out.Sections = append(out.Sections, sectionOut{Number: sec.Number, Title: sec.Title, Status: string(sec.Status)})
	}
	for _, f := range rep.Findings {
		if f.Severity == contracts.SeverityInfo {
			continue
		}
		fo := findingOut{
			Key: f.Key, Severity: string(f.Severity), CheckID: f.CheckID, Capability: f.Capability,
			Message: f.Message, Remediation: f.Remediation,
		}
		if a, ok := accepted[f.Key]; ok && f.Severity == contracts.SeverityWarning {
			fo.Accepted, fo.AcceptedBy = true, a.OperatorID
		}
		out.Findings = append(out.Findings, fo)
	}
	return out
}

const acceptUsage = "alixai audit accept-warning [-config FILE] [-package HASH] -finding KEY -operator ID -reason TEXT APPLICATION"

type acceptOut struct {
	ApplicationID string    `json:"application_id"`
	ManifestHash  string    `json:"manifest_hash"`
	FindingKey    string    `json:"finding_key"`
	CheckID       string    `json:"check_id"`
	Capability    string    `json:"capability,omitempty"`
	OperatorID    string    `json:"operator_id"`
	Reason        string    `json:"reason"`
	RunID         string    `json:"run_id"`
	AcceptedAt    time.Time `json:"accepted_at"`
	// AlreadyAccepted is true when the warning had been accepted before; the
	// first acceptance stands.
	AlreadyAccepted bool `json:"already_accepted"`
}

// runAuditAcceptWarning records that an authorised operator accepted one warning
// of the package's latest signed report. The operator must be listed in
// integration_audit.accept_operators (nobody is, by default) and must give a
// reason; the acceptance records who, when and which finding, and is written to
// the audit log with it. A critical finding cannot be accepted. Accepting never
// widens the tools or scope the model receives: the next 'alixai audit run'
// activates the package with exactly the capabilities it declares.
func runAuditAcceptWarning(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("audit accept-warning")
	path := env.ConfigFlag(fs)
	pkgHash := fs.String("package", "", "manifest hash of the installed package (default: the application's newest quarantined package)")
	finding := fs.String("finding", "", "the key of the warning in the audit report (required)")
	operator := fs.String("operator", "", "the authorised operator accepting the warning (required)")
	reason := fs.String("reason", "", "why the warning is acceptable (required)")
	if err := env.ParseFlags(fs, acceptUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one application ID (usage: %s)", acceptUsage)
	}
	if *finding == "" || *operator == "" || strings.TrimSpace(*reason) == "" {
		return Usagef("-finding, -operator and -reason are all required (usage: %s)", acceptUsage)
	}
	s, err := openStack(ctx, env, *path, false)
	if err != nil {
		return err
	}
	defer s.close()
	runner, err := s.runner(env, "", "", false)
	if err != nil {
		return err
	}
	acc, err := runner.AcceptWarning(ctx, integrationaudit.AcceptRequest{
		ApplicationID: fs.Arg(0), ManifestHash: *pkgHash, FindingKey: *finding, OperatorID: *operator, Reason: *reason,
	})
	if err != nil {
		return err
	}
	return emitJSON(env.Stdout, acceptOut{
		ApplicationID: acc.ApplicationID, ManifestHash: acc.ManifestHash, FindingKey: acc.FindingKey, CheckID: acc.CheckID,
		Capability: acc.Capability, OperatorID: acc.OperatorID, Reason: acc.Reason, RunID: acc.RunID,
		AcceptedAt: acc.AcceptedAt, AlreadyAccepted: !acc.Created,
	})
}

const quarantineUsage = "alixai audit quarantine [-config FILE] [-package HASH] -reason TEXT [-operator ID] APPLICATION"

type quarantineOut struct {
	ApplicationID string `json:"application_id"`
	ManifestHash  string `json:"manifest_hash"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	// Server says what the running server did: evicted (it serves nothing for
	// the application from now on), no_server (none is running for this data
	// directory; one started later reads the store) or failed.
	Server *transportsServerOut `json:"server"`
}

// runAuditQuarantine takes an active package out of service: it becomes
// quarantined again, with its audit report cleared, and a package can be
// activated again only by a new clean audit. The change is made in the store,
// which is the authority, and then the server running on the same data
// directory, if there is one, is told through its admin interface to evict the
// application from its capability registry at once; without that the registry
// would keep serving it for up to 30 seconds.
//
// Exit status 0 means the package is quarantined and either the running server
// confirmed the eviction or no server is running. Exit status 1 means a server
// is reachable (or its endpoint record cannot be used) and did not confirm:
// the package IS quarantined in the store, and the server's own cache ages out
// within 30 seconds, but the command does not claim more than it knows.
func runAuditQuarantine(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("audit quarantine")
	path := env.ConfigFlag(fs)
	pkgHash := fs.String("package", "", "manifest hash of the package to take out of service (default: the application's active package)")
	reason := fs.String("reason", "", "why the package is quarantined, at most 500 characters (required)")
	operator := fs.String("operator", "", "who quarantines the package, recorded in the audit log")
	if err := env.ParseFlags(fs, quarantineUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one application ID (usage: %s)", quarantineUsage)
	}
	if strings.TrimSpace(*reason) == "" {
		return Usagef("-reason is required (usage: %s)", quarantineUsage)
	}
	if *operator != "" && !operatorIDRE.MatchString(*operator) {
		return Usagef("-operator %q is not an operator id (1 to 128 letters, digits and ._@:-)", *operator)
	}
	app := fs.Arg(0)

	s, err := openStack(ctx, env, *path, false)
	if err != nil {
		return err
	}
	defer s.close()
	runner, err := s.runner(env, "", "", false)
	if err != nil {
		return err
	}
	hash := *pkgHash
	if hash == "" {
		inst, err := s.installer("")
		if err != nil {
			return err
		}
		recs, err := inst.List(ctx, app)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.State == apppkg.StateActive {
				hash = r.ManifestHash
			}
		}
		if hash == "" {
			return fmt.Errorf("the application %s has no active package to quarantine", app)
		}
	}
	if err := runner.Quarantine(ctx, app, hash, *reason, *operator); err != nil {
		return err
	}
	sv, serr := s.server.result()
	if sv == nil {
		// The runner has no registry to tell only when the CLI built it without
		// one; it always has here, so this is unreachable and said if it is not.
		sv, serr = &transportsServerOut{Status: transportsServerFailed, Detail: "the server was not contacted"}, errors.New("the server was not contacted")
	}
	fmt.Fprintf(env.Stderr, "alixai: note: %s\n", sv.note(app))
	if err := emitJSON(env.Stdout, quarantineOut{
		ApplicationID: app, ManifestHash: hash, State: string(apppkg.StateQuarantined), Reason: strings.TrimSpace(*reason), Server: sv,
	}); err != nil {
		return err
	}
	if serr != nil {
		return fmt.Errorf("the package is quarantined in the store, but %w", serr)
	}
	return nil
}
