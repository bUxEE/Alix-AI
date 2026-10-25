package integrationaudit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	appID         = "demo-library"
	platformKeyID = "alixai-audit-signer-test"
	// validMinimalDir is the committed fixture package: signed with the
	// published TEST-ONLY publisher key on 2026-10-01, for ten years.
	validMinimalDir = "../../examples/packages/valid-minimal"
	// clinicDir is the reference application's real signed package.
	clinicDir = "../../examples/reference-apps/clinic-booking/package"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

var ctx = context.Background()

// harness is a Runner over a throwaway database and data directory, with the
// TEST-ONLY publisher key registered for demo-library (as a developer's trust
// store would) and a generated platform key registered for signing reports.
type harness struct {
	t       *testing.T
	db      *store.DB
	clk     *clock.Fake
	trust   *trust.Store
	inst    *apppkg.Installer
	log     *audit.Log
	reg     *capabilities.Registry
	runner  *Runner
	cfg     Config
	dataDir string
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
}

// options of a harness.
type hopts struct {
	probes     bool
	authorizer Authorizer
	maxAge     time.Duration
	noRegistry bool
	profile    string
}

func newChecks(t testing.TB, probes bool) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, c := range Builtins() {
		if err := r.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	if probes {
		for _, c := range stubProbes() {
			if err := r.Register(c); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r
}

// stubProbes stands in for the runtime probes of M1-18: one passing check for
// each section the built-in checks do not implement. It also proves that a
// check can be added through the extension point without editing the framework.
func stubProbes() []Check {
	probe := func(section int, stage Stage, id string) Check {
		return NewCheck(Spec{ID: id, Title: SectionTitle(section), Section: section, Stage: stage, Target: TargetApplication},
			func(_ context.Context, in *Input) (*Outcome, error) {
				o := &Outcome{}
				for _, cid := range in.Package.SortedCapabilityIDs() {
					o.Covered = append(o.Covered, cid)
				}
				o.Probes = []ProbeResult{{Kind: ProbeNegativeScope, Capability: "-", Subject: id, Expected: "refused", Observed: "refused", Passed: true}}
				return o, nil
			})
	}
	return []Check{
		probe(SectionDeterministic, StageResults, "stub.deterministic_results"),
		probe(SectionScope, StageScope, "stub.scope"),
		probe(SectionSafeExecution, StageSafety, "stub.safe_execution"),
		probe(SectionDegradation, StageSafety, "stub.degradation"),
		probe(SectionRuntimeSmoke, StageBenchmark, "stub.smoke"),
	}
}

func newHarness(t *testing.T, mutate ...func(*hopts)) *harness {
	t.Helper()
	o := hopts{probes: true, authorizer: AllowOperators{"alice"}}
	for _, m := range mutate {
		m(&o)
	}
	h := &harness{t: t, clk: clock.NewFake(t0), dataDir: t.TempDir()}
	h.db = storetest.Open(t, nil)
	h.trust = trust.NewStore(h.db, h.clk, trust.AllowTestOnlyKeys())
	if _, err := h.trust.RegisterKey(ctx, appID, devkeys.KeyID, devkeys.PublicKey(), "test publisher"); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.pub, h.priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := h.trust.RegisterKey(ctx, trust.PlatformApplicationID, platformKeyID, h.pub, "audit report signer"); err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewVerifier(h.trust, h.clk)
	if h.inst, err = apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: h.db, Clock: h.clk, DataDir: h.dataDir, Verifier: verifier, CoreVersion: "0.1.0",
	}); err != nil {
		t.Fatal(err)
	}
	if h.log, err = audit.New(h.db, audit.Options{Clock: h.clk}); err != nil {
		t.Fatal(err)
	}
	h.reg = capabilities.NewRegistry(capabilities.InstallerSource(h.inst), capabilities.WithClock(h.clk))
	h.cfg = Config{
		DB: h.db, Clock: h.clk, Installer: h.inst, DataDir: h.dataDir, Verifier: verifier, CoreVersion: "0.1.0",
		Signing: &trust.SigningKey{KeyID: platformKeyID, Key: h.priv}, Log: h.log,
		Checks: newChecks(t, o.probes), Authorizer: o.authorizer, MaxReportAge: o.maxAge, Profile: o.profile,
	}
	if !o.noRegistry {
		h.cfg.Capabilities = h.reg
	}
	if h.runner, err = NewRunner(h.cfg); err != nil {
		t.Fatal(err)
	}
	return h
}

// install installs a package directory into quarantine.
func (h *harness) install(dir string) apppkg.Record {
	h.t.Helper()
	res, err := h.inst.Install(ctx, apppkg.FromDir(dir))
	if err != nil {
		h.t.Fatalf("Install(%s): %v", dir, err)
	}
	return res.Record
}

// audit runs an audit of the newest quarantined package with the adapter.
func (h *harness) audit(ad adapters.AppAdapter) *Run {
	h.t.Helper()
	run, err := h.runner.Audit(ctx, RunRequest{ApplicationID: appID, Adapter: ad})
	if err != nil {
		h.t.Fatalf("Audit: %v", err)
	}
	return run
}

func (h *harness) state(rec apppkg.Record) apppkg.State {
	h.t.Helper()
	got, err := h.inst.Get(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		h.t.Fatal(err)
	}
	return got.State
}

// events lists the event types and principals of the application chain, in
// order.
func (h *harness) events(app string) []string {
	h.t.Helper()
	rows, err := h.db.QueryContext(ctx,
		`SELECT event_type, principal_id FROM audit_events WHERE application_id = ? AND tenant_id = '' ORDER BY chain_seq`, app)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, who string
		if err := rows.Scan(&typ, &who); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, typ+"|"+who)
	}
	return out
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

// findings returns the findings of a report with a check ID.
func findings(r Report, checkID string) []KeyedFinding {
	var out []KeyedFinding
	for _, f := range r.Findings {
		if f.CheckID == checkID {
			out = append(out, f)
		}
	}
	return out
}

// hasFinding reports whether the report holds a finding with the check ID and
// severity.
func hasFinding(r Report, checkID string, sev contracts.Severity) bool {
	for _, f := range findings(r, checkID) {
		if f.Severity == sev {
			return true
		}
	}
	return false
}

func findingIDs(r Report, sev contracts.Severity) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Severity == sev {
			out = append(out, f.CheckID)
		}
	}
	return out
}

// copyDir copies a package tree so a test can change it and sign it again.
func copyDir(t testing.TB, from string) string {
	t.Helper()
	to := t.TempDir()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// resign signs a package directory with the TEST-ONLY publisher key.
func resign(t testing.TB, dir, app, version string) {
	t.Helper()
	_, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: app, PackageVersion: version},
		Keys:            []trust.SigningKey{devkeys.SigningKey()},
		Now:             time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2036, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("sign %s: %v", dir, err)
	}
}

// editFile replaces text in a file of a package directory.
func editFile(t testing.TB, dir, rel, from, to string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), from) {
		t.Fatalf("%s does not contain %q", rel, from)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- a scripted application adapter ----

// stub is an adapters.AppAdapter whose Describe, Inspect and Health answers are
// scripted. It carries no behaviour of a real application: the audit's static
// and inspection checks need only these three.
type stub struct {
	desc      adapters.Description
	descErr   error
	insp      adapters.Inspection
	inspErr   error
	health    adapters.HealthReport
	healthErr error
	calls     []string
}

var _ adapters.AppAdapter = (*stub)(nil)

// okDescription is what a healthy application with the package's operations
// describes: exactly what valid-minimal maps, plus cancel.
func okDescription() adapters.Description {
	return adapters.Description{
		Transport:          "fake",
		ContractVersions:   []int{adapters.ContractVersion},
		ApplicationVersion: "2.4.0",
		Capabilities: []adapters.CapabilityOps{
			{CapabilityID: "catalog.find_title", Operations: []adapters.Operation{adapters.OpRead}},
			{CapabilityID: "reservations.create", Operations: []adapters.Operation{
				adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus, adapters.OpCancel}},
		},
	}
}

const okInspection = `{"reports":[
 {"aspect":"versions","state":"reported","facts":[{"field":"application_version","value":{"kind":"text","value":"2.4.0"}}]},
 {"aspect":"health","state":"reported","facts":[{"field":"status","value":{"kind":"text","value":"ok"}}]},
 {"aspect":"limits","state":"reported","facts":[{"field":"max_title_length","value":{"kind":"integer","value":128}}]}
]}`

func inspectionOf(t testing.TB, raw string) adapters.Inspection {
	t.Helper()
	ins, err := adapters.NormaliseInspection([]byte(raw), adapters.InspectRequest{}, 0)
	if err != nil {
		t.Fatalf("inspection fixture: %v", err)
	}
	return ins
}

func newStub(t testing.TB, mutate ...func(*stub)) *stub {
	t.Helper()
	s := &stub{
		desc:   okDescription(),
		insp:   inspectionOf(t, okInspection),
		health: adapters.HealthReport{Status: adapters.HealthOK, ApplicationVersion: "2.4.0"},
	}
	for _, m := range mutate {
		m(s)
	}
	return s
}

func (s *stub) Describe(context.Context) (adapters.Description, error) {
	s.calls = append(s.calls, "describe")
	return s.desc, s.descErr
}
func (s *stub) Health(context.Context) (adapters.HealthReport, error) {
	s.calls = append(s.calls, "health")
	return s.health, s.healthErr
}
func (s *stub) Inspect(context.Context, contracts.Scope, adapters.InspectRequest) (adapters.Inspection, error) {
	s.calls = append(s.calls, "inspect")
	return s.insp, s.inspErr
}

func (s *stub) refuse(op string) (actions.DeterministicResult, error) {
	s.calls = append(s.calls, op)
	return actions.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "the stub application answers no "+op)
}
func (s *stub) Read(context.Context, contracts.Scope, adapters.ReadRequest) (actions.DeterministicResult, error) {
	return s.refuse("read")
}
func (s *stub) Validate(context.Context, contracts.Scope, adapters.ValidateRequest) (actions.DeterministicResult, error) {
	return s.refuse("validate")
}
func (s *stub) Prepare(context.Context, contracts.Scope, adapters.PrepareRequest) (actions.DeterministicResult, error) {
	return s.refuse("prepare")
}
func (s *stub) Commit(context.Context, contracts.Scope, adapters.CommitRequest) (actions.DeterministicResult, error) {
	return s.refuse("commit")
}
func (s *stub) Status(context.Context, contracts.Scope, adapters.StatusRequest) (actions.DeterministicResult, error) {
	return s.refuse("status")
}
func (s *stub) Cancel(context.Context, contracts.Scope, adapters.CancelRequest) (actions.DeterministicResult, error) {
	return s.refuse("cancel")
}

// reportJSON is the report as indented JSON, for failure messages.
func reportJSON(r Report) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// registerPublisher trusts the TEST-ONLY publisher key for another application.
func (h *harness) registerPublisher(app string) {
	h.t.Helper()
	if _, err := h.trust.RegisterKey(ctx, app, devkeys.KeyID, devkeys.PublicKey(), "test publisher"); err != nil {
		h.t.Fatal(err)
	}
}

// descriptionFor is what a faithful application describes for a package: every
// declared operation of every capability, plus cancel for the capabilities
// that prepare.
func descriptionFor(pkg *apppkg.Package) adapters.Description {
	d := adapters.Description{
		Transport:          "fake",
		ContractVersions:   []int{adapters.ContractVersion},
		ApplicationVersion: pkg.Manifest.Application.Version,
	}
	for _, id := range pkg.SortedCapabilityIDs() {
		c := pkg.Capabilities[id]
		var ops []adapters.Operation
		for _, op := range declaredOperations(c) {
			ops = append(ops, adapters.Operation(op))
		}
		if c.Operations.Prepare != nil {
			ops = append(ops, adapters.OpCancel)
		}
		d.Capabilities = append(d.Capabilities, adapters.CapabilityOps{CapabilityID: id, Operations: ops})
	}
	return d
}
