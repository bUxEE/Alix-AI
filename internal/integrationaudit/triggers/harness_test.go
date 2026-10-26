package triggers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	appID         = "demo-library"
	platformKeyID = "alixai-triggers-signer-test"
	coreVersion   = apppkg.DevCoreVersion
	// validMinimalDir is the committed fixture package, signed with the published
	// TEST-ONLY publisher key.
	validMinimalDir = "../../../examples/packages/valid-minimal"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

var ctx = context.Background()

// harness is a Runner and a Watcher over a throwaway database and data directory,
// with the TEST-ONLY publisher key registered for demo-library and a generated
// platform key registered for signing reports. The adapter is a scripted stand-in
// for the application; the registry is the real capability registry over the
// installed packages, with its 30 s cache on a clock the test controls.
type harness struct {
	t       *testing.T
	db      *store.DB
	clk     *clock.Fake
	trust   *trust.Store
	inst    *apppkg.Installer
	log     *audit.Log
	reg     *capabilities.Registry
	runner  *integrationaudit.Runner
	w       *Watcher
	dataDir string

	app     *stub
	notices *noticeLog
	enq     *fakeEnqueuer
	// drifts is what the polled Drifts hook returns.
	drifts []Drift
}

type hopts struct {
	// watcherRegistry replaces the registry the watcher confirms against; the
	// Runner keeps no registry then, so nothing but the watcher can evict.
	watcherRegistry func(*capabilities.Registry) Registry
	queue           bool
	noAdapter       bool
	coreVersion     string
}

func newHarness(t *testing.T, mutate ...func(*hopts)) *harness {
	t.Helper()
	o := hopts{coreVersion: coreVersion}
	for _, m := range mutate {
		m(&o)
	}
	h := &harness{t: t, clk: clock.NewFake(t0), dataDir: t.TempDir(), notices: &noticeLog{}, enq: &fakeEnqueuer{}}
	h.db = storetest.Open(t, nil)
	h.trust = trust.NewStore(h.db, h.clk, trust.AllowTestOnlyKeys())
	if _, err := h.trust.RegisterKey(ctx, appID, devkeys.KeyID, devkeys.PublicKey(), "test publisher"); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.trust.RegisterKey(ctx, trust.PlatformApplicationID, platformKeyID, pub, "audit report signer"); err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewVerifier(h.trust, h.clk)
	if h.inst, err = apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: h.db, Clock: h.clk, DataDir: h.dataDir, Verifier: verifier, CoreVersion: coreVersion,
	}); err != nil {
		t.Fatal(err)
	}
	if h.log, err = audit.New(h.db, audit.Options{Clock: h.clk}); err != nil {
		t.Fatal(err)
	}
	h.reg = capabilities.NewRegistry(capabilities.InstallerSource(h.inst), capabilities.WithClock(h.clk))
	h.app = newStub(t)

	checks := integrationaudit.NewRegistry()
	for _, c := range append(integrationaudit.Builtins(), stubProbes()...) {
		if err := checks.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	cfg := integrationaudit.Config{
		DB: h.db, Clock: h.clk, Installer: h.inst, DataDir: h.dataDir, Verifier: verifier, CoreVersion: coreVersion,
		Signing: &trust.SigningKey{KeyID: platformKeyID, Key: priv}, Log: h.log, Checks: checks,
		Authorizer: integrationaudit.AllowOperators{"alice"},
	}
	var watcherReg Registry = h.reg
	if o.watcherRegistry != nil {
		watcherReg = o.watcherRegistry(h.reg)
	} else {
		cfg.Capabilities = h.reg
	}
	if h.runner, err = integrationaudit.NewRunner(cfg); err != nil {
		t.Fatal(err)
	}
	wc := Config{
		Packages: h.inst, Auditor: h.runner, Verifier: verifier, Registry: watcherReg, Log: h.log, DB: h.db, Clock: h.clk,
		Notifier: h.notices, CoreVersion: o.coreVersion,
		Drifts: func(string) []Drift { return h.drifts },
	}
	if !o.noAdapter {
		wc.Adapter = func(context.Context, string) (adapters.AppAdapter, func(), error) { return h.app, func() {}, nil }
	}
	if o.queue {
		wc.Enqueuer = h.enq
	}
	if h.w, err = NewWatcher(wc); err != nil {
		t.Fatal(err)
	}
	return h
}

// stubProbes stands in for the runtime probes of M1-18 and M2-12: one passing
// check for each section the built-in checks do not implement, so that the
// faithful application's report is clear.
func stubProbes() []integrationaudit.Check {
	probe := func(section int, stage integrationaudit.Stage, id string) integrationaudit.Check {
		return integrationaudit.NewCheck(integrationaudit.Spec{
			ID: id, Title: integrationaudit.SectionTitle(section), Section: section, Stage: stage, Target: integrationaudit.TargetApplication,
		}, func(_ context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
			o := &integrationaudit.Outcome{}
			o.Covered = append(o.Covered, in.Package.SortedCapabilityIDs()...)
			o.Probes = []integrationaudit.ProbeResult{{Kind: integrationaudit.ProbeNegativeScope, Capability: "-", Subject: id, Expected: "refused", Observed: "refused", Passed: true}}
			return o, nil
		})
	}
	return []integrationaudit.Check{
		probe(integrationaudit.SectionDeterministic, integrationaudit.StageResults, "stub.deterministic_results"),
		probe(integrationaudit.SectionScope, integrationaudit.StageScope, "stub.scope"),
		probe(integrationaudit.SectionSafeExecution, integrationaudit.StageSafety, "stub.safe_execution"),
		probe(integrationaudit.SectionDegradation, integrationaudit.StageSafety, "stub.degradation"),
		probe(integrationaudit.SectionRuntimeSmoke, integrationaudit.StageBenchmark, "stub.smoke"),
	}
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

// goLive installs a package, audits it against the scripted application and
// activates it with every warning accepted, and requires that the registry then
// serves it. It returns the record as installed.
func (h *harness) goLive(dir string) apppkg.Record {
	h.t.Helper()
	rec := h.install(dir)
	run, err := h.runner.Audit(ctx, integrationaudit.RunRequest{ApplicationID: appID, ManifestHash: rec.ManifestHash, Adapter: h.app})
	if err != nil {
		h.t.Fatalf("Audit: %v", err)
	}
	h.acceptAll(run)
	if _, err := h.runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: appID, ManifestHash: rec.ManifestHash, OperatorID: "alice"}); err != nil {
		h.t.Fatalf("Activate: %v", err)
	}
	h.mustServe(rec.ManifestHash)
	return rec
}

func (h *harness) acceptAll(run *integrationaudit.Run) {
	h.t.Helper()
	for _, f := range run.Signed.Report.Findings {
		if f.Severity != contracts.SeverityWarning {
			continue
		}
		if _, err := h.runner.AcceptWarning(ctx, integrationaudit.AcceptRequest{
			ApplicationID: appID, ManifestHash: run.Record.ManifestHash, FindingKey: f.Key, OperatorID: "alice", Reason: "reviewed in the test",
		}); err != nil {
			h.t.Fatalf("AcceptWarning(%s): %v", f.CheckID, err)
		}
	}
}

// mustServe requires that the registry serves the package with the hash. It is
// also what warms the registry: the entry it loads is trusted for 30 s of the
// test's clock, which is what a quarantine must not wait for.
func (h *harness) mustServe(hash string) capabilities.Info {
	h.t.Helper()
	info, err := h.reg.Info(ctx, appID)
	if err != nil {
		h.t.Fatalf("the registry serves nothing, want %s: %v", hash[:12], err)
	}
	if info.ContentHash != hash {
		h.t.Fatalf("the registry serves %s, want %s", info.ContentHash[:12], hash[:12])
	}
	return info
}

// mustNotServe requires that the registry serves nothing for the application.
func (h *harness) mustNotServe(why string) {
	h.t.Helper()
	if info, err := h.reg.Info(ctx, appID); err == nil {
		h.t.Errorf("%s: the registry still serves %s", why, info.ContentHash[:12])
	}
	if apps := h.reg.Applications(); len(apps) != 0 {
		h.t.Errorf("%s: the registry holds %v", why, apps)
	}
}

func (h *harness) state(rec apppkg.Record) apppkg.State {
	h.t.Helper()
	got, err := h.inst.Get(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		h.t.Fatal(err)
	}
	return got.State
}

// runs counts the stored audit runs of a package.
func (h *harness) runs(hash string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM integrationaudit_runs WHERE application_id = ? AND manifest_hash = ?`, appID, hash).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) totalRuns() int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM integrationaudit_runs`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// events lists the event types of the application chain, in order.
func (h *harness) events() []string {
	h.t.Helper()
	rows, err := h.db.QueryContext(ctx,
		`SELECT event_type FROM audit_events WHERE application_id = ? AND tenant_id = '' ORDER BY chain_seq`, appID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, typ)
	}
	return out
}

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

// eventData returns the data of the last event of a type on the application chain.
func (h *harness) eventData(typ audit.EventType) eventData {
	h.t.Helper()
	var raw string
	err := h.db.QueryRowContext(ctx,
		`SELECT data FROM audit_events WHERE application_id = ? AND tenant_id = '' AND event_type = ? ORDER BY chain_seq DESC LIMIT 1`,
		appID, string(typ)).Scan(&raw)
	if err != nil {
		h.t.Fatalf("no %s event: %v", typ, err)
	}
	var d eventData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		h.t.Fatalf("event data %s: %v", raw, err)
	}
	return d
}

// ---- package variants ----

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

// variant is the demo library package with a new package version and the changes
// the edit function makes, signed again with the TEST-ONLY publisher key.
func variant(t testing.TB, version string, edit func(dir string)) string {
	t.Helper()
	return variantSigned(t, version, edit, devkeys.SigningKey())
}

type testKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func genKey(t testing.TB) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{pub, priv}
}

// variantSigned is variant signed with the given key.
func variantSigned(t testing.TB, version string, edit func(dir string), key trust.SigningKey) string {
	t.Helper()
	dir := copyDir(t, validMinimalDir)
	editFile(t, dir, "alixai.yaml", "version: 1.0.0\n  description: 'Initial", "version: "+version+"\n  description: 'Initial")
	if edit != nil {
		edit(dir)
	}
	if _, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: appID, PackageVersion: version},
		Keys:            []trust.SigningKey{key},
		Now:             time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2036, 10, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("sign %s: %v", dir, err)
	}
	return dir
}

// ---- recorders ----

type noticeLog struct {
	mu sync.Mutex
	ns []Notice
}

func (l *noticeLog) Notify(_ context.Context, n Notice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ns = append(l.ns, n)
}

func (l *noticeLog) all() []Notice {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Notice(nil), l.ns...)
}

// of returns the notices of an event type.
func (l *noticeLog) of(t audit.EventType) []Notice {
	var out []Notice
	for _, n := range l.all() {
		if n.Type == t {
			out = append(out, n)
		}
	}
	return out
}

type enqueued struct {
	scope   contracts.Scope
	key     string
	payload json.RawMessage
}

type fakeEnqueuer struct {
	mu   sync.Mutex
	jobs []enqueued
	err  error
}

func (f *fakeEnqueuer) EnqueueWatch(_ context.Context, scope contracts.Scope, key string, payload json.RawMessage) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	for _, j := range f.jobs {
		if j.key == key && j.scope.ApplicationID == scope.ApplicationID {
			return true, nil
		}
	}
	f.jobs = append(f.jobs, enqueued{scope, key, payload})
	return false, nil
}

func (f *fakeEnqueuer) all() []enqueued {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]enqueued(nil), f.jobs...)
}

// ---- a scripted application adapter ----

// stub is an adapters.AppAdapter whose Describe, Inspect and Health answers are
// scripted. It carries no behaviour of a real application: the audit's static
// and inspection checks need only these three.
type stub struct {
	desc   adapters.Description
	insp   adapters.Inspection
	health adapters.HealthReport
	// onDescribe runs at the start of every Describe, which the audit and the
	// watcher call in the middle of their work: a test uses it to see what the
	// registry serves while a re-audit is running.
	onDescribe func()
	// descErr makes Describe fail.
	descErr error
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

func newStub(t testing.TB) *stub {
	t.Helper()
	ins, err := adapters.NormaliseInspection([]byte(okInspection), adapters.InspectRequest{}, 0)
	if err != nil {
		t.Fatalf("inspection fixture: %v", err)
	}
	return &stub{desc: okDescription(), insp: ins, health: adapters.HealthReport{Status: adapters.HealthOK, ApplicationVersion: "2.4.0"}}
}

func (s *stub) Describe(context.Context) (adapters.Description, error) {
	if s.onDescribe != nil {
		s.onDescribe()
	}
	return s.desc, s.descErr
}
func (s *stub) Health(context.Context) (adapters.HealthReport, error) { return s.health, nil }
func (s *stub) Inspect(context.Context, contracts.Scope, adapters.InspectRequest) (adapters.Inspection, error) {
	return s.insp, nil
}

func (s *stub) refuse(op string) (actions.DeterministicResult, error) {
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
