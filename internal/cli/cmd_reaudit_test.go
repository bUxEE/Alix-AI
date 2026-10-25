package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/triggers"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	reauditTestApp   = "demo-library"
	reauditTestCore  = "0.1.0"
	reauditTestKeyID = "reaudit-test-signer"
	// reauditTestPackage is the committed fixture package, signed with the published TEST-ONLY
	// publisher key.
	reauditTestPackage = "../../examples/packages/valid-minimal"
)

var reauditTestT0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// ---- a scripted application ---------------------------------------------------------------

// reauditTestInstance is one adapter the transport built: an application that describes exactly
// what valid-minimal maps, and an MCP adapter's memory of drift, which is sticky for the life of
// the instance (adapters/mcp).
type reauditTestInstance struct {
	adapters.AppAdapter

	mu     sync.Mutex
	drifts []mcptransport.Drift
	closed bool
}

var _ interface{ Drifts() []mcptransport.Drift } = (*reauditTestInstance)(nil)

func (i *reauditTestInstance) Drifts() []mcptransport.Drift {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]mcptransport.Drift(nil), i.drifts...)
}

func (i *reauditTestInstance) Close() error {
	i.mu.Lock()
	i.closed = true
	i.mu.Unlock()
	return nil
}

func (i *reauditTestInstance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

// drift is what an MCP adapter does when a mapped tool no longer matches its pin: it remembers
// it for good and, with notify, tells the observers through the path mcp.Config.OnDrift takes
// (mcpTransportNotify).
func (i *reauditTestInstance) drift(applicationID, tool string, notify bool) mcptransport.Drift {
	d := mcptransport.Drift{
		Endpoint: "library-mcp", Tool: tool, Kind: mcptransport.DriftSchemaChanged,
		Pinned: strings.Repeat("a", 64), Observed: strings.Repeat("b", 64),
	}
	i.mu.Lock()
	i.drifts = append(i.drifts, d)
	i.mu.Unlock()
	if notify {
		mcpTransportNotify(applicationID, d)
	}
	return d
}

type reauditTestStub struct {
	desc   adapters.Description
	insp   adapters.Inspection
	health adapters.HealthReport
}

const reauditTestInspection = `{"reports":[
 {"aspect":"versions","state":"reported","facts":[{"field":"application_version","value":{"kind":"text","value":"2.4.0"}}]},
 {"aspect":"health","state":"reported","facts":[{"field":"status","value":{"kind":"text","value":"ok"}}]},
 {"aspect":"limits","state":"reported","facts":[{"field":"max_title_length","value":{"kind":"integer","value":128}}]}
]}`

func reauditTestNewStub(t testing.TB, version string) *reauditTestStub {
	t.Helper()
	ins, err := adapters.NormaliseInspection([]byte(strings.Replace(reauditTestInspection, "2.4.0", version, 1)), adapters.InspectRequest{}, 0)
	if err != nil {
		t.Fatalf("inspection fixture: %v", err)
	}
	return &reauditTestStub{
		desc: adapters.Description{
			Transport: "fake", ContractVersions: []int{adapters.ContractVersion}, ApplicationVersion: version,
			Capabilities: []adapters.CapabilityOps{
				{CapabilityID: "catalog.find_title", Operations: []adapters.Operation{adapters.OpRead}},
				{CapabilityID: "reservations.create", Operations: []adapters.Operation{
					adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus, adapters.OpCancel}},
			},
		},
		insp:   ins,
		health: adapters.HealthReport{Status: adapters.HealthOK, ApplicationVersion: version},
	}
}

func (s *reauditTestStub) Describe(context.Context) (adapters.Description, error) { return s.desc, nil }
func (s *reauditTestStub) Health(context.Context) (adapters.HealthReport, error) {
	return s.health, nil
}
func (s *reauditTestStub) Inspect(context.Context, contracts.Scope, adapters.InspectRequest) (adapters.Inspection, error) {
	return s.insp, nil
}
func (s *reauditTestStub) refuse() (actions.DeterministicResult, error) {
	return actions.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "the scripted application answers no operation")
}
func (s *reauditTestStub) Read(context.Context, contracts.Scope, adapters.ReadRequest) (actions.DeterministicResult, error) {
	return s.refuse()
}
func (s *reauditTestStub) Validate(context.Context, contracts.Scope, adapters.ValidateRequest) (actions.DeterministicResult, error) {
	return s.refuse()
}
func (s *reauditTestStub) Prepare(context.Context, contracts.Scope, adapters.PrepareRequest) (actions.DeterministicResult, error) {
	return s.refuse()
}
func (s *reauditTestStub) Commit(context.Context, contracts.Scope, adapters.CommitRequest) (actions.DeterministicResult, error) {
	return s.refuse()
}
func (s *reauditTestStub) Status(context.Context, contracts.Scope, adapters.StatusRequest) (actions.DeterministicResult, error) {
	return s.refuse()
}
func (s *reauditTestStub) Cancel(context.Context, contracts.Scope, adapters.CancelRequest) (actions.DeterministicResult, error) {
	return s.refuse()
}

// ---- what the watcher did, in order -------------------------------------------------------

// reauditTimeline records, in order, every call the watcher makes to its parts.
type reauditTimeline struct {
	mu    sync.Mutex
	steps []string
}

func (l *reauditTimeline) mark(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
}

func (l *reauditTimeline) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

func (l *reauditTimeline) index(step string) int { return slices.Index(l.all(), step) }

type reauditTestAuditor struct {
	triggers.Auditor
	rig *reauditRig
}

func (a reauditTestAuditor) Audit(ctx context.Context, req integrationaudit.RunRequest) (*integrationaudit.Run, error) {
	a.rig.tl.mark("audit")
	if a.rig.stopExpected.Load() {
		a.rig.requireStopped("an audit began")
	}
	return a.Auditor.Audit(ctx, req)
}

func (a reauditTestAuditor) Quarantine(ctx context.Context, app, hash, reason, operator string) error {
	a.rig.tl.mark("quarantine")
	if a.rig.failQuarantine != nil {
		return a.rig.failQuarantine
	}
	err := a.Auditor.Quarantine(ctx, app, hash, reason, operator)
	if err == nil {
		a.rig.stopExpected.Store(true)
	}
	return err
}

type reauditTestRegistry struct {
	triggers.Registry
	rig *reauditRig
}

func (r reauditTestRegistry) Refresh(ctx context.Context, app string) error {
	r.rig.tl.mark("refresh")
	return r.Registry.Refresh(ctx, app)
}

func (r reauditTestRegistry) Deactivate(app string) {
	r.rig.tl.mark("deactivate")
	r.Registry.Deactivate(app)
}

func (r reauditTestRegistry) Info(ctx context.Context, app string) (capabilities.Info, error) {
	r.rig.tl.mark("info")
	if r.rig.keepServing {
		return capabilities.Info{ApplicationID: app}, nil // a registry that did not let go
	}
	return r.Registry.Info(ctx, app)
}

type reauditTestNotifier struct{ rig *reauditRig }

func (n reauditTestNotifier) Notify(_ context.Context, notice triggers.Notice) {
	n.rig.tl.mark("notice:" + string(notice.Type))
	n.rig.mu.Lock()
	n.rig.notices = append(n.rig.notices, notice)
	n.rig.mu.Unlock()
	if notice.Type != "reaudit.failed" && n.rig.stopExpected.Load() {
		n.rig.requireStopped("the operator was told (" + string(notice.Type) + ")")
	}
}

type reauditReport struct {
	hook string
	res  triggers.Result
	err  error
}

// ---- the rig ------------------------------------------------------------------------------

// reauditRig is the watcher of serve over a throwaway store, built from the same parts wireV1 builds
// it from: the installer, the capability registry, the package source and the adapter source of the
// process, with an adapter transport that builds scripted MCP-like adapters. It also holds what an
// operator's own commands do (audit, accept, activate), which the watcher cannot.
type reauditRig struct {
	t        *testing.T
	clk      *clock.Fake
	db       *store.DB
	dataDir  string
	verifier *trust.Verifier
	inst     *apppkg.Installer
	apps     *gateway.Applications
	reg      *capabilities.Registry
	ads      *adapterSource
	log      *audit.Log
	operator *integrationaudit.Runner
	queue    *jobs.Queue
	platform trust.SigningKey
	st       *reauditStack
	tl       *reauditTimeline

	// keepServing makes the watcher's registry answer that it still serves the application;
	// failQuarantine makes the quarantine fail.
	keepServing bool
	// stopExpected is true from a quarantine the watcher made until the operator activates a package
	// again: while it is, the taps require that nothing of this process serves the application.
	stopExpected   atomic.Bool
	failQuarantine error

	mu      sync.Mutex
	built   []*reauditTestInstance
	notices []triggers.Notice
	reports []reauditReport
}

func newReauditRig(t *testing.T, mutate ...func(*reauditRig)) *reauditRig {
	t.Helper()
	// The audits of these tests are about what follows an audit, not the runtime probes.
	auditChecksProbesBuiltinChecksOnly(t)
	r := &reauditRig{t: t, clk: clock.NewFake(reauditTestT0), dataDir: t.TempDir(), tl: &reauditTimeline{}}
	for _, m := range mutate {
		m(r)
	}
	ctx := context.Background()
	r.db = storetest.Open(t, nil)
	ts := trust.NewStore(r.db, r.clk, trust.AllowTestOnlyKeys())
	if _, err := ts.RegisterKey(ctx, reauditTestApp, devkeys.KeyID, devkeys.PublicKey(), "test publisher"); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, reauditTestKeyID, pub, "audit report signer"); err != nil {
		t.Fatal(err)
	}
	r.platform = trust.SigningKey{KeyID: reauditTestKeyID, Key: priv}
	r.verifier = trust.NewVerifier(ts, r.clk)
	if r.inst, err = apppkg.NewInstaller(apppkg.InstallerOptions{DB: r.db, Clock: r.clk, DataDir: r.dataDir, Verifier: r.verifier, CoreVersion: reauditTestCore}); err != nil {
		t.Fatal(err)
	}
	if r.apps, err = gateway.NewApplications(r.inst, apppkg.Options{Verifier: r.verifier, CoreVersion: reauditTestCore}, r.clk, 0); err != nil {
		t.Fatal(err)
	}
	r.reg = capabilities.NewRegistry(r.apps.Source(), capabilities.WithClock(r.clk))
	r.ads = &adapterSource{
		apps: r.apps, log: discardLog(), cache: map[string]cachedAdapter{},
		transports: transportsSource{installer: r.inst, dataDir: r.dataDir},
	}
	if r.log, err = audit.New(r.db, audit.Options{Clock: r.clk}); err != nil {
		t.Fatal(err)
	}
	// The transport of the process: every adapter it builds is remembered, newest last.
	transportsSwapDefault(t).register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		i := &reauditTestInstance{AppAdapter: reauditTestNewStub(t, "2.4.0")}
		r.mu.Lock()
		r.built = append(r.built, i)
		r.mu.Unlock()
		return i, nil
	})

	// What an operator's `alixai audit run`, `accept-warning` and activation do: the Runner of the CLI,
	// told about an activation or a quarantine through the same in-process control.
	r.operator, err = integrationaudit.NewRunner(integrationaudit.Config{
		DB: r.db, Clock: r.clk, Installer: r.inst, DataDir: r.dataDir, Verifier: r.verifier, CoreVersion: reauditTestCore,
		Signing: &r.platform, Log: r.log, Authorizer: integrationaudit.AllowOperators{"alice"},
		Capabilities: serveAdminControl{registry: r.reg, apps: r.apps, adapters: r.ads},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.queue, err = jobs.NewQueue(r.db, jobs.Options{Config: jobs.DefaultConfig(), Clock: r.clk}); err != nil {
		t.Fatal(err)
	}
	if err := r.queue.Register(reauditJobSpec()); err != nil {
		t.Fatal(err)
	}
	r.st = r.newStack(nil)
	// The drift observer of serve (reauditStack.start registers the same one).
	remove := RegisterMCPDriftObserver(r.st.onDrift)
	t.Cleanup(func() {
		remove()
		r.st.drain()
	})
	return r
}

// newStack builds one watcher over the rig's parts, with the taps that record what it does.
func (r *reauditRig) newStack(mutate func(*reauditInput)) *reauditStack {
	r.t.Helper()
	in := reauditInput{
		DB: r.db, Clock: r.clk, Log: discardLog(), Audit: r.log, DataDir: r.dataDir, Installer: r.inst, Verifier: r.verifier, Core: reauditTestCore,
		Registry: r.reg, Apps: r.apps, Adapters: r.ads, Signing: &r.platform, SweepEvery: time.Hour,
		Notifier:     reauditTestNotifier{r},
		WrapAuditor:  func(a triggers.Auditor) triggers.Auditor { return reauditTestAuditor{a, r} },
		WrapRegistry: func(g triggers.Registry) triggers.Registry { return reauditTestRegistry{g, r} },
		OnReport: func(hook, _ string, res triggers.Result, err error) {
			r.mu.Lock()
			r.reports = append(r.reports, reauditReport{hook, res, err})
			r.mu.Unlock()
		},
	}
	if mutate != nil {
		mutate(&in)
	}
	st, err := reauditCompose(in)
	if err != nil {
		r.t.Fatal(err)
	}
	st.enq.bind(r.queue)
	return st
}

// requireStopped says, from wherever it is called, that nothing of this process serves the
// application: not the registry, not the package source the engine and the coordinator decide
// against, not the adapter cache, and the package is quarantined in the store.
func (r *reauditRig) requireStopped(when string) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.reg.Info(ctx, reauditTestApp); err == nil {
		r.t.Errorf("%s: the capability registry still serves the application", when)
	}
	if p, err := r.apps.Package(ctx, reauditTestApp); err == nil && p != nil {
		r.t.Errorf("%s: the package source still has an active package", when)
	}
	if _, err := r.ads.Adapter(ctx, reauditTestApp); err == nil {
		r.t.Errorf("%s: an adapter still serves the application", when)
	}
	recs, err := r.inst.List(ctx, reauditTestApp)
	if err != nil {
		r.t.Errorf("%s: %v", when, err)
	}
	for _, rec := range recs {
		if rec.State == apppkg.StateActive {
			r.t.Errorf("%s: package %s is still active in the store", when, rec.ManifestHash[:12])
		}
	}
}

func (r *reauditRig) install(dir string) apppkg.Record {
	r.t.Helper()
	res, err := r.inst.Install(context.Background(), apppkg.FromDir(dir))
	if err != nil {
		r.t.Fatalf("Install(%s): %v", dir, err)
	}
	return res.Record
}

// goLive installs a package, audits it against the scripted application, accepts every warning as
// the operator "alice" and activates it, and requires that the process then serves it.
func (r *reauditRig) goLive(dir string) apppkg.Record {
	r.t.Helper()
	rec := r.install(dir)
	if _, err := r.operator.Audit(context.Background(), integrationaudit.RunRequest{
		ApplicationID: reauditTestApp, ManifestHash: rec.ManifestHash, Adapter: reauditTestNewStub(r.t, "2.4.0"),
	}); err != nil {
		r.t.Fatalf("Audit: %v", err)
	}
	r.activate(rec.ManifestHash)
	return rec
}

// activate is what an operator does after a report: accept the warnings of the package's latest
// report and activate it. The process must serve it afterwards.
func (r *reauditRig) activate(hash string) {
	r.t.Helper()
	ctx := context.Background()
	run, ok, err := r.operator.LatestRun(ctx, reauditTestApp, hash)
	if err != nil || !ok {
		r.t.Fatalf("no report of %s: %v", hash[:12], err)
	}
	for _, f := range run.Signed.Report.Findings {
		if f.Severity != contracts.SeverityWarning {
			continue
		}
		if _, err := r.operator.AcceptWarning(ctx, integrationaudit.AcceptRequest{
			ApplicationID: reauditTestApp, ManifestHash: hash, FindingKey: f.Key, OperatorID: "alice", Reason: "reviewed in the test",
		}); err != nil {
			r.t.Fatalf("AcceptWarning(%s): %v", f.CheckID, err)
		}
	}
	if _, err := r.operator.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: reauditTestApp, ManifestHash: hash, OperatorID: "alice"}); err != nil {
		r.t.Fatalf("Activate: %v", err)
	}
	r.stopExpected.Store(false)
	r.mustServe(hash)
}

func (r *reauditRig) mustServe(hash string) {
	r.t.Helper()
	info, err := r.reg.Info(context.Background(), reauditTestApp)
	if err != nil {
		r.t.Fatalf("the registry serves nothing, want %s: %v", hash[:12], err)
	}
	if info.ContentHash != hash {
		r.t.Fatalf("the registry serves %s, want %s", info.ContentHash[:12], hash[:12])
	}
}

// serving is the adapter instance that serves the active package now (the one adapterSource
// builds and caches), the way a request reaches it.
func (r *reauditRig) serving() *reauditTestInstance {
	r.t.Helper()
	ad, err := r.ads.Adapter(context.Background(), reauditTestApp)
	if err != nil {
		r.t.Fatalf("no adapter serves the application: %v", err)
	}
	c, ok := ad.(transportsClosing)
	if !ok {
		r.t.Fatalf("the adapter is %T", ad)
	}
	i, ok := c.inner.(*reauditTestInstance)
	if !ok {
		r.t.Fatalf("the transport adapter is %T", c.inner)
	}
	return i
}

func (r *reauditRig) state(hash string) apppkg.State {
	r.t.Helper()
	rec, err := r.inst.Get(context.Background(), reauditTestApp, hash)
	if err != nil {
		r.t.Fatal(err)
	}
	return rec.State
}

func (r *reauditRig) events() []string {
	r.t.Helper()
	rows, err := r.db.QueryContext(context.Background(),
		`SELECT event_type FROM audit_events WHERE application_id = ? AND tenant_id = '' ORDER BY chain_seq`, reauditTestApp)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// watchJobKeys are the idempotency keys of the watch jobs in the queue.
func (r *reauditRig) watchJobKeys() []string {
	r.t.Helper()
	rows, err := r.db.QueryContext(context.Background(),
		`SELECT idempotency_key FROM jobs WHERE type = ? AND service_identity = ? ORDER BY created_at`, triggers.JobType, triggers.ServiceIdentity)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func (r *reauditRig) noticesOf(typ string) []triggers.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []triggers.Notice
	for _, n := range r.notices {
		if string(n.Type) == typ {
			out = append(out, n)
		}
	}
	return out
}

func (r *reauditRig) lastReport() reauditReport {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		r.t.Fatal("the hook reported nothing")
	}
	return r.reports[len(r.reports)-1]
}

func reauditTestCount(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

// ---- 1. the order of a stop -----------------------------------------------------------------

// SEC-049, master plan 4.5: a tool that changed after it was approved takes the live package out of
// service before anything else happens. The order is the point, and each of these checks fails if it
// is mutated: the stop is the first thing done, the registry, the package source, the adapter cache
// and the store all say "nothing is served" by the time the operator is told or anything is
// audited, and a stop that cannot be confirmed is reaudit.failed, never a quiet success.
func TestMCPDriftQuarantinesAndStopsServingBeforeAnythingElse(t *testing.T) {
	t.Run("the stop comes first and is complete before the operator is told", func(t *testing.T) {
		r := newReauditRig(t)
		live := r.goLive(reauditTestPackage)
		old := r.serving()
		r.clk.Advance(2 * time.Second)

		old.drift(reauditTestApp, "find_title", true)
		r.st.idle()

		steps := r.tl.all()
		want := []string{"quarantine", "refresh", "deactivate", "info", "notice:reaudit.quarantined"}
		if !slices.Equal(steps, want) {
			t.Fatalf("what the watcher did, in order = %v, want %v", steps, want)
		}
		if r.state(live.ManifestHash) != apppkg.StateQuarantined {
			t.Errorf("the package is %s, want quarantined", r.state(live.ManifestHash))
		}
		rep := r.lastReport()
		if rep.hook != "drift" || rep.err != nil || !slices.Equal(rep.res.Quarantined, []string{live.ManifestHash}) {
			t.Errorf("the hook reported %+v", rep)
		}
		// Everything this process held about the application is gone, the drifted instance included.
		if !old.isClosed() {
			t.Error("the drifted adapter instance was left open")
		}
		if _, ok := r.ads.cached(reauditTestApp, live.ManifestHash); ok {
			t.Error("the drifted adapter instance is still cached: it would serve again the moment the package is reactivated")
		}
		// The notice and the audit log say what happened, and nothing was audited yet: the audit is a
		// job, queued, never run on the call path of the adapter.
		if n := r.noticesOf("reaudit.quarantined"); len(n) != 1 || n[0].Severity != triggers.SeverityCritical {
			t.Errorf("notices = %+v", n)
		}
		ev := r.events()
		if reauditTestCount(ev, "reaudit.quarantined") != 1 || reauditTestCount(ev, "reaudit.completed") != 0 || reauditTestCount(ev, "package.quarantined") != 1 {
			t.Errorf("audit log = %v", ev)
		}
		keys := r.watchJobKeys()
		if len(keys) != 1 || !strings.HasPrefix(keys[0], "drift:"+live.ManifestHash+":") {
			t.Errorf("the re-audit must be queued as one watch job, got %v", keys)
		}
		var runs int
		if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM integrationaudit_runs WHERE manifest_hash = ?`, live.ManifestHash).Scan(&runs); err != nil || runs != 1 {
			t.Errorf("only the report that activated the package exists: %d runs, %v", runs, err)
		}
	})

	t.Run("a registry that keeps serving is a failed stop, told as critical, retried, and nothing else is done", func(t *testing.T) {
		r := newReauditRig(t, func(r *reauditRig) { r.keepServing = true })
		live := r.goLive(reauditTestPackage)
		old := r.serving()
		r.clk.Advance(2 * time.Second)

		old.drift(reauditTestApp, "find_title", true)
		r.st.idle()

		steps := r.tl.all()
		want := []string{"quarantine", "refresh", "deactivate", "info", "notice:reaudit.failed"}
		if !slices.Equal(steps, want) {
			t.Fatalf("what the watcher did, in order = %v, want %v", steps, want)
		}
		rep := r.lastReport()
		if rep.err == nil || contracts.CodeOf(rep.err) != contracts.ErrUnavailable || !contracts.CodeOf(rep.err).Retryable() {
			t.Errorf("the hook reported %v, want a retryable unavailable error", rep.err)
		}
		if n := r.noticesOf("reaudit.failed"); len(n) != 1 || n[0].Severity != triggers.SeverityCritical {
			t.Errorf("notices = %+v", n)
		}
		if len(r.noticesOf("reaudit.quarantined")) != 0 || reauditTestCount(r.events(), "reaudit.quarantined") != 0 {
			t.Error("a stop nobody confirmed was reported as one")
		}
		if keys := r.watchJobKeys(); len(keys) != 1 || !strings.HasPrefix(keys[0], "retry:drift:") {
			t.Errorf("an unconfirmed stop must queue a retry, got %v", keys)
		}
		if r.state(live.ManifestHash) != apppkg.StateQuarantined {
			t.Errorf("the quarantine itself happened first: %s", r.state(live.ManifestHash))
		}
	})

	t.Run("a quarantine that cannot be made does nothing else", func(t *testing.T) {
		r := newReauditRig(t, func(r *reauditRig) { r.failQuarantine = contracts.New(contracts.ErrUnavailable, "the store is busy") })
		live := r.goLive(reauditTestPackage)
		old := r.serving()

		old.drift(reauditTestApp, "find_title", true)
		r.st.idle()

		if steps, want := r.tl.all(), []string{"quarantine", "notice:reaudit.failed"}; !slices.Equal(steps, want) {
			t.Fatalf("what the watcher did, in order = %v, want %v", steps, want)
		}
		if rep := r.lastReport(); contracts.CodeOf(rep.err) != contracts.ErrUnavailable {
			t.Errorf("the hook reported %v", rep.err)
		}
		if keys := r.watchJobKeys(); len(keys) != 1 || !strings.HasPrefix(keys[0], "retry:drift:") {
			t.Errorf("a stop that failed must queue a retry, got %v", keys)
		}
		if r.state(live.ManifestHash) != apppkg.StateActive {
			t.Errorf("the package is %s: nothing may be claimed that was not done", r.state(live.ManifestHash))
		}
	})
}

// ---- 2. an old instance's drift -----------------------------------------------------------------

// MCP drift is sticky for the life of an adapter instance. The poll of every look asks the instance
// that serves the active package now; reading an instance the process dropped would quarantine a
// package again right after an operator reactivated it. The poll is also what stops traffic when the
// push was missed, so it must see the current instance.
func TestOldAdapterInstanceDriftDoesNotRequarantineAfterReactivation(t *testing.T) {
	r := newReauditRig(t)
	live := r.goLive(reauditTestPackage)
	a := r.serving()
	r.clk.Advance(2 * time.Second)

	// Instance A drifts: the push quarantines the package and drops A.
	a.drift(reauditTestApp, "find_title", true)
	r.st.idle()
	if r.state(live.ManifestHash) != apppkg.StateQuarantined {
		t.Fatalf("fixture: the drift did not quarantine the package (%s)", r.state(live.ManifestHash))
	}

	// The operator's way back: the watcher audits the quarantined package again so that a report
	// postdates the quarantine, the operator accepts what is accepted and activates.
	r.clk.Advance(2 * time.Second)
	if _, err := r.st.watcher.Check(context.Background(), reauditTestApp); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(2 * time.Second)
	r.activate(live.ManifestHash)

	// Instance A is gone but still remembers its drift for good; the new instance B does not.
	if len(a.Drifts()) != 1 || !a.isClosed() {
		t.Fatalf("fixture: the old instance must be closed and still report its drift: %v closed=%v", a.Drifts(), a.isClosed())
	}
	quarantinedBefore := len(r.noticesOf("reaudit.quarantined"))
	check := func(when string) triggers.Result {
		t.Helper()
		res, err := r.st.watcher.Check(context.Background(), reauditTestApp)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		return res
	}
	if res := check("before the new instance exists"); len(res.Quarantined) != 0 || r.state(live.ManifestHash) != apppkg.StateActive {
		t.Fatalf("the reactivated package was quarantined again by nothing: %+v", res)
	}
	b := r.serving()
	if b == a || len(b.Drifts()) != 0 {
		t.Fatalf("fixture: the reactivated package must be served by a new, clean instance")
	}
	if res := check("with the new instance serving"); len(res.Quarantined) != 0 || len(res.Changes) != 0 || r.state(live.ManifestHash) != apppkg.StateActive {
		t.Fatalf("the drift of the old instance quarantined the reactivated package: %+v", res)
	}
	r.mustServe(live.ManifestHash)
	if got := len(r.noticesOf("reaudit.quarantined")); got != quarantinedBefore {
		t.Errorf("%d quarantine notices after the reactivation, %d before", got, quarantinedBefore)
	}

	// The mutation this guards against: a poll that reads the old instance. The same drift, polled
	// that way, does quarantine the reactivated package; so the test above is not vacuous.
	mutant := r.newStack(func(in *reauditInput) {
		in.Drifts = func(string) []triggers.Drift {
			var out []triggers.Drift
			for _, d := range a.Drifts() {
				out = append(out, reauditDriftOf(d))
			}
			return out
		}
	})
	res, err := mutant.watcher.Check(context.Background(), reauditTestApp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quarantined) != 1 || r.state(live.ManifestHash) != apppkg.StateQuarantined {
		t.Fatalf("fixture: polling the old instance must re-quarantine (%+v): the guard is not tested", res)
	}

	// And the poll of the CURRENT instance does see its drift (the push is not needed): bring the
	// package back once more, let instance C drift without telling anyone, and look.
	r.clk.Advance(2 * time.Second)
	if _, err := r.st.watcher.Check(context.Background(), reauditTestApp); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(2 * time.Second)
	r.activate(live.ManifestHash)
	c := r.serving()
	if c == a || c == b || len(c.Drifts()) != 0 {
		t.Fatal("fixture: a third, clean instance must serve")
	}
	c.drift(reauditTestApp, "find_title", false)
	res = check("the current instance drifted")
	if len(res.Quarantined) != 1 || r.state(live.ManifestHash) != apppkg.StateQuarantined {
		t.Errorf("the drift of the current instance was not seen by the poll: %+v", res)
	}
	r.requireStopped("after the poll found the drift")
}

// ---- 3. a stop and the limitation that follows it ----------------------------------------------------

// A known limitation (M1-17), stated as it is: AcceptWarning works only on a quarantined package. A
// re-audit of a package that is live and has no critical problem leaves its activation in force and
// stores a report, but its warnings cannot be accepted while it serves; the operator has to
// quarantine it first. The watcher does not work around that, and cannot accept anything itself.
func TestReauditLimitationNewWarningsOfALivePackage(t *testing.T) {
	r := newReauditRig(t)
	live := r.goLive(reauditTestPackage)
	r.clk.Advance(2 * time.Second)

	// A new application version is a trigger, not a critical change: re-audit, keep serving.
	r.mu.Lock()
	r.built = nil
	r.mu.Unlock()
	transportsSwapDefault(t).register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		return &reauditTestInstance{AppAdapter: reauditTestNewStub(t, "2.5.0")}, nil
	})
	res, err := r.st.watcher.Check(context.Background(), reauditTestApp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Audits) != 1 || len(res.Quarantined) != 0 {
		t.Fatalf("a new application version must re-audit the live package and stop nothing: %+v", res)
	}
	r.mustServe(live.ManifestHash)
	run, ok, err := r.operator.LatestRun(context.Background(), reauditTestApp, live.ManifestHash)
	if err != nil || !ok || run.RunID != res.Audits[0].RunID {
		t.Fatalf("the re-audit stored no report: %v", err)
	}

	var key string
	for _, f := range run.Signed.Report.Findings {
		if f.Severity == contracts.SeverityWarning {
			key = f.Key
			break
		}
	}
	if key == "" {
		t.Fatal("fixture: the report has no warning")
	}
	accept := func() error {
		_, err := r.operator.AcceptWarning(context.Background(), integrationaudit.AcceptRequest{
			ApplicationID: reauditTestApp, ManifestHash: live.ManifestHash, FindingKey: key, OperatorID: "alice", Reason: "reviewed in the test",
		})
		return err
	}
	if err := accept(); contracts.CodeOf(err) != contracts.ErrConflict || !strings.Contains(err.Error(), "only a quarantined package") {
		t.Fatalf("a warning of a live package was accepted, or refused for another reason: %v", err)
	}
	// The way through is the operator's: quarantine, accept, activate.
	if err := r.operator.Quarantine(context.Background(), reauditTestApp, live.ManifestHash, "operator review of the re-audit", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := accept(); err != nil {
		t.Fatalf("after the quarantine the warning must be acceptable: %v", err)
	}
}

// ---- 4. the pool's authority and the queue ------------------------------------------------------------

func reauditTestJob(t *testing.T, identity, jobType, payload string, mutate func(*contracts.Scope)) jobs.Job {
	t.Helper()
	var p triggers.JobPayload
	_ = json.Unmarshal([]byte(payload), &p)
	scope := triggers.JobScope("dep_cli", p.ApplicationID)
	if p.ApplicationID == "" {
		scope = triggers.JobScope("dep_cli", reauditTestApp)
	}
	if mutate != nil {
		mutate(&scope)
	}
	return jobs.Job{Type: jobType, Scope: scope, Payload: json.RawMessage(payload), ServiceIdentity: identity}
}

// The pool accepts the watcher's identity for the watcher's job, for the watcher's own scope of the
// very application the payload names, and for nothing else.
func TestReauditAuthorityAcceptsOnlyTheWatchJob(t *testing.T) {
	good := `{"application_id":"demo-library"}`
	for _, tc := range []struct {
		name string
		job  jobs.Job
		ok   bool
	}{
		{"the watch job", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, good, nil), true},
		{"another identity", reauditTestJob(t, ragServiceIdentity, triggers.JobType, good, nil), false},
		{"no identity", reauditTestJob(t, "", triggers.JobType, good, nil), false},
		{"another job type", reauditTestJob(t, triggers.ServiceIdentity, "history.retention", good, nil), false},
		{"a payload for another application than the scope's", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, good,
			func(s *contracts.Scope) { s.ApplicationID = "someone-else" }), false},
		{"an unknown payload member", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, `{"application_id":"demo-library","force":true}`, nil), false},
		{"no application in the payload", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, `{"application_id":""}`, nil), false},
		{"a payload that is not an object", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, `"demo-library"`, nil), false},
		{"data after the payload", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, good+` {}`, nil), false},
		{"a user's scope", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, good,
			func(s *contracts.Scope) { s.PrincipalID, s.Roles = "usr_alice", []string{"member"} }), false},
		{"a scope that reaches a tenant", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, good,
			func(s *contracts.Scope) { s.TenantID = "ten_acme" }), false},
		{"an incomplete scope", reauditTestJob(t, triggers.ServiceIdentity, triggers.JobType, good,
			func(s *contracts.Scope) { s.SessionID = "" }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := reauditAuthority{}.AuthorizeService(context.Background(), tc.job.ServiceIdentity, tc.job)
			switch {
			case tc.ok && err != nil:
				t.Errorf("denied: %v", err)
			case !tc.ok && contracts.CodeOf(err) != contracts.ErrPermissionDenied:
				t.Errorf("= %v, want permission_denied", err)
			}
		})
	}
}

// The job type is class 6, maintenance, as the triggers package declares it.
func TestReauditJobIsMaintenance(t *testing.T) {
	if spec := reauditJobSpec(); spec.Name != triggers.JobType || int(spec.Class) != triggers.JobClass || spec.Class != jobs.ClassMaintenance {
		t.Errorf("job spec = %+v, want %s in class %d", spec, triggers.JobType, triggers.JobClass)
	}
}

// The enqueuer queues under the service identity, reports a duplicate for the same key, hands a key
// the queue would refuse (a drift names an endpoint, a tool and a kind) over as a digest, and is
// unavailable, not silent, before the queue is bound.
func TestReauditEnqueuerQueuesUnderTheServiceIdentity(t *testing.T) {
	r := newReauditRig(t)
	ctx := context.Background()
	scope := triggers.JobScope("dep_cli", reauditTestApp)
	payload := json.RawMessage(`{"application_id":"demo-library"}`)

	unbound := &reauditEnqueuer{}
	if _, err := unbound.EnqueueWatch(ctx, scope, "k", payload); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("an enqueuer with no queue = %v, want unavailable", err)
	}

	long := "drift:" + strings.Repeat("x", 400)
	for i, want := range []bool{false, true} {
		dup, err := r.st.enq.EnqueueWatch(ctx, scope, long, payload)
		if err != nil || dup != want {
			t.Fatalf("enqueue %d: duplicate=%v err=%v, want duplicate=%v", i, dup, err, want)
		}
	}
	keys := r.watchJobKeys()
	if len(keys) != 1 || len(keys[0]) > reauditKeyMax+len("sha256:") || strings.HasPrefix(keys[0], "drift:") {
		t.Errorf("keys = %v, want one digest key", keys)
	}
	var class int
	if err := r.db.QueryRowContext(ctx, `SELECT class FROM jobs WHERE type = ?`, triggers.JobType).Scan(&class); err != nil || class != triggers.JobClass {
		t.Errorf("class = %d (%v), want %d", class, err, triggers.JobClass)
	}
	if reauditJobKey("watch:20261002T090000Z") != "watch:20261002T090000Z" || reauditJobKey("a\x00b") == "a\x00b" {
		t.Error("a plain key is kept, a key with a control character is replaced")
	}
}

// ---- 5. serve ------------------------------------------------------------------------------------------

// reauditTestVariant is the demo package with a new package version, signed again with the
// TEST-ONLY publisher key: a package an application uploads after its first one went live.
func reauditTestVariant(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.Walk(reauditTestPackage, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(reauditTestPackage, p)
		dst := filepath.Join(dir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "alixai.yaml")
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	const from = "version: 1.0.0\n  description: 'Initial"
	if !strings.Contains(string(b), from) {
		t.Fatalf("fixture: the manifest has no %q", from)
	}
	if err := os.WriteFile(manifest, []byte(strings.Replace(string(b), from, "version: "+version+"\n  description: 'Initial", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: reauditTestApp, PackageVersion: version},
		Keys:            []trust.SigningKey{devkeys.SigningKey()},
		Now:             time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2036, 10, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("sign %s: %v", dir, err)
	}
	return dir
}

// reauditTestArchive is a package directory as the .tar.gz an application uploads.
func reauditTestArchive(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil || info.IsDir() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// reauditTestSeed puts a live application into the store serve will open: the publisher's and the
// platform's keys, the demo package audited, its warnings accepted and activated, with a report
// that a later re-audit can compare with.
func reauditTestSeed(t *testing.T, dataDir string, platformPub ed25519.PublicKey, platform trust.SigningKey) apppkg.Record {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clk := clock.Real{}
	ts := trust.NewStore(db, clk, trust.AllowTestOnlyKeys())
	if _, err := ts.RegisterKey(ctx, reauditTestApp, devkeys.KeyID, devkeys.PublicKey(), "test publisher"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, platform.KeyID, platformPub, "audit report signer"); err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewVerifier(ts, clk)
	core := apppkg.CoreVersionOrDev(version.Get().Version)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: verifier, CoreVersion: core})
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := integrationaudit.NewRunner(integrationaudit.Config{
		DB: db, Clock: clk, Installer: inst, DataDir: dataDir, Verifier: verifier, CoreVersion: core,
		Signing: &platform, Log: al, Authorizer: integrationaudit.AllowOperators{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := inst.Install(ctx, apppkg.FromDir(reauditTestPackage))
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.Audit(ctx, integrationaudit.RunRequest{ApplicationID: reauditTestApp, ManifestHash: res.Record.ManifestHash, OperatorID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range run.Signed.Report.Findings {
		if f.Severity != contracts.SeverityWarning {
			continue
		}
		if _, err := runner.AcceptWarning(ctx, integrationaudit.AcceptRequest{
			ApplicationID: reauditTestApp, ManifestHash: res.Record.ManifestHash, FindingKey: f.Key, OperatorID: "alice", Reason: "reviewed in the test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: reauditTestApp, ManifestHash: res.Record.ManifestHash, OperatorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	return res.Record
}

// serve runs the re-audit watcher: the watch job is registered on the one pool the RAG stack built, is
// class 6 and runs under the watcher's own service identity (a sweep queues it at start); the pool
// refuses the same identity with another application's payload and any other identity; and an
// upload of a new package through POST /v1/applications is shown to the watcher before the API
// answers, which queues the audit of the upload and lets the previous activation serve meanwhile.
func TestServeRunsTheReauditWatcherAndAnInstallTriggersIt(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t)
	transportsSwapDefault(t) // this build has no adapter for the application: the audit records it as unverified
	oldSweep, oldOnCompose, oldRag, oldAudit := reauditSweepEvery, reauditOnCompose, ragOnCompose, auditSection
	reauditSweepEvery = time.Hour // the sweep of the start is the only one of this test
	stacks, rags := make(chan *reauditStack, 1), make(chan *ragStack, 1)
	reauditOnCompose = func(s *reauditStack) { stacks <- s }
	ragOnCompose = func(s *ragStack) { rags <- s }
	t.Cleanup(func() {
		reauditSweepEvery, reauditOnCompose, ragOnCompose, auditSection = oldSweep, oldOnCompose, oldRag, oldAudit
	})

	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	pub, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath, _, err := trust.WriteKeyFiles(t.TempDir(), "reaudit-signer", priv)
	if err != nil {
		t.Fatal(err)
	}
	extra := identityYAML + fmt.Sprintf("integration_audit:\n  signing_key_id: reaudit-signer\n  signing_key_file: %s\n", yamlPath(keyPath))
	cfg, dataDir := serveConfig(t, f, extra)
	provisionIssuer(t, cfg, iss)
	live := reauditTestSeed(t, dataDir, pub, trust.SigningKey{KeyID: "reaudit-signer", Key: priv})

	srv := startServe(t, cfg)
	var st *reauditStack
	var rs *ragStack
	for st == nil || rs == nil {
		select {
		case st = <-stacks:
		case rs = <-rags:
		case <-time.After(15 * time.Second):
			t.Fatal("serve did not build the re-audit watcher and the RAG stack")
		}
	}
	db := st.in.DB
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	// The sweep of the start queues the watch job and the shared pool runs it, under its identity.
	pollUntil(t, 30*time.Second, "the sweep's watch job to run on the pool", func() bool {
		return count(`SELECT count(*) FROM jobs WHERE type = ? AND state = 'succeeded' AND service_identity = ? AND idempotency_key LIKE 'watch:%' AND class = ?`,
			triggers.JobType, triggers.ServiceIdentity, int(jobs.ClassMaintenance)) > 0
	})
	if n := count(`SELECT count(*) FROM integrationaudit_runs WHERE application_id = ?`, reauditTestApp); n != 1 {
		t.Errorf("an application with nothing changed was audited again: %d runs", n)
	}

	// The pool's authority: the identity with another application's payload, and another identity, are
	// denied and fail at once.
	denied := func(key, identity, payload string, scope contracts.Scope) {
		t.Helper()
		if _, err := rs.queue.Enqueue(context.Background(), scope, jobs.EnqueueRequest{
			Type: triggers.JobType, Payload: json.RawMessage(payload), IdempotencyKey: key, ServiceIdentity: identity,
		}); err != nil {
			t.Fatal(err)
		}
		pollUntil(t, 30*time.Second, "the pool to deny "+key, func() bool {
			return count(`SELECT count(*) FROM jobs WHERE idempotency_key = ? AND state = 'failed' AND error_code = ?`, key, string(contracts.ErrPermissionDenied)) > 0
		})
	}
	scope := triggers.JobScope("dep_cli", reauditTestApp)
	denied("denied-payload", triggers.ServiceIdentity, `{"application_id":"someone-else"}`, scope)
	denied("denied-identity", ragServiceIdentity, `{"application_id":"demo-library"}`, scope)

	// An application uploads a new package version: the install hook queues the audit of the upload,
	// the job runs it, and the activation that was in force serves throughout.
	archive := reauditTestArchive(t, reauditTestVariant(t, "1.0.1"))
	token := iss.Token(func(d *identitytest.Draft) {
		d.Set(identity.ClaimCapabilityScopes, []string{gateway.CapabilityRegisterApplication})
	})
	req, err := http.NewRequest(http.MethodPost, srv.base+"/v1/applications", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var reg gateway.RegistrationBody
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(body, &reg) != nil || !reg.Created || reg.Package.ManifestHash == "" {
		t.Fatalf("POST /v1/applications = %d: %s", resp.StatusCode, body)
	}
	up := reg.Package.ManifestHash
	pollUntil(t, 30*time.Second, "the job queued by the install to audit the upload", func() bool {
		return count(`SELECT count(*) FROM jobs WHERE type = ? AND state = 'succeeded' AND idempotency_key = ?`, triggers.JobType, "installed:"+up) > 0
	})
	if n := count(`SELECT count(*) FROM integrationaudit_runs WHERE application_id = ? AND manifest_hash = ?`, reauditTestApp, up); n != 1 {
		t.Errorf("the upload was audited %d times, want once", n)
	}
	recs, err := st.in.Installer.List(context.Background(), reauditTestApp)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]apppkg.State{}
	for _, r := range recs {
		states[r.ManifestHash] = r.State
	}
	if states[live.ManifestHash] != apppkg.StateActive || states[up] != apppkg.StateQuarantined {
		t.Errorf("states = %v: the live package keeps serving, the upload waits for an operator", states)
	}
	if _, err := st.in.Registry.Info(context.Background(), reauditTestApp); err != nil {
		t.Errorf("the registry stopped serving the application: %v", err)
	}
	for _, typ := range []string{"reaudit.triggered", "reaudit.completed"} {
		if count(`SELECT count(*) FROM audit_events WHERE application_id = ? AND event_type = ?`, reauditTestApp, typ) == 0 {
			t.Errorf("no %s event in the audit log", typ)
		}
	}
	if count(`SELECT count(*) FROM audit_events WHERE application_id = ? AND event_type = 'reaudit.quarantined'`, reauditTestApp) != 0 {
		t.Error("a package of a new version quarantined the live one")
	}
	if err := srv.stop(); err != nil {
		t.Fatalf("serve stopped with %v", err)
	}
}

// The poll reads the cache entry of the ACTIVE package only: an instance that was built for another
// package hash (the process has not swapped it yet) is not a drift of the package that serves now.
func TestReauditDriftPollIgnoresAnInstanceOfAnotherPackage(t *testing.T) {
	r := newReauditRig(t)
	live := r.goLive(reauditTestPackage)
	cur := r.serving()
	cur.drift(reauditTestApp, "find_title", false) // sticky, and nobody was told

	if got := r.st.drifts(reauditTestApp); len(got) != 1 || got[0].Tool != "find_title" || got[0].Kind != string(mcptransport.DriftSchemaChanged) {
		t.Fatalf("the drift of the serving instance = %+v, want its one drift", got)
	}
	stale := &reauditTestInstance{AppAdapter: reauditTestNewStub(t, "2.4.0")}
	stale.drift(reauditTestApp, "find_title", false)
	r.ads.mu.Lock()
	r.ads.cache[reauditTestApp] = cachedAdapter{hash: strings.Repeat("0", 64), ad: transportsClosing{inner: stale}}
	r.ads.mu.Unlock()
	if got := r.st.drifts(reauditTestApp); len(got) != 0 {
		t.Errorf("the drift of an instance built for another package = %+v, want none (the active package is %s)", got, live.ManifestHash[:12])
	}
	r.ads.forget(reauditTestApp)
	if got := r.st.drifts(reauditTestApp); len(got) != 0 {
		t.Errorf("with no instance cached the poll found drift: %+v", got)
	}
}
