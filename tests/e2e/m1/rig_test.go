package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// skewClock is the service's clock: real time plus an offset a test moves forward. The clinic
// application runs on wall time and mints its tokens with it, so the service has to tick with it;
// a test that needs an hour to pass moves both (rig.advance).
type skewClock struct{ off atomic.Int64 }

func (c *skewClock) Now() time.Time          { return time.Now().UTC().Add(time.Duration(c.off.Load())) }
func (c *skewClock) Advance(d time.Duration) { c.off.Add(int64(d)) }

// policyRecorder keeps every decision the policy gateway made, beside the audit log's own record
// of it, so a test can say what the gateway decided without reading the log back.
type policyRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (r *policyRecorder) Emit(_ context.Context, rec policy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

// denials counts the gateway's denials of one capability.
func (r *policyRecorder) denials(capabilityID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.recs {
		if rec.CapabilityID == capabilityID && rec.Outcome == policy.OutcomeDeny {
			n++
		}
	}
	return n
}

type teeSink []policy.Sink

func (t teeSink) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// syncBuffer is a log destination that several goroutines write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- the rig ------------------------------------------------------------------------------

// rig is the service under test: AlixAI as `alixai serve` composes it (see the package comment
// for the one swap), on the data directory the operator's commands acted on, serving the public
// API on a loopback port.
type rig struct {
	t   tb
	dep *deployment
	clk *skewClock

	db       *store.DB
	log      *slog.Logger
	logs     *syncBuffer
	audit    *audit.Log
	recs     *policyRecorder
	apps     *gateway.Applications
	registry *capabilities.Registry
	adapters *rigAdapters
	engine   *dialogue.Engine
	co       *actions.Coordinator

	model *pfake.Provider
	// scripts are the rules the test scripted; unscripted lists the model calls no rule answered.
	scripts    []*rigScript
	mu         sync.Mutex
	unscripted []string

	base    string
	stopAll func()
}

type rigScript struct {
	what string
	rule *pfake.Rule
	// times is how many calls the script is for.
	times int
	// optional marks a script that need not be used.
	optional bool
}

type rigOptions struct {
	templateOnly bool
}

type rigOption func(*rigOptions)

// withTemplateOnly makes every dialogue act answer with its deterministic template, so a test
// about the core's decisions reads what the core said and not the model's wording.
func withTemplateOnly() rigOption { return func(o *rigOptions) { o.templateOnly = true } }

// allActs lists every dialogue act, for dialogue.Config.TemplateOnly.
func allActs() []conversation.DialogueDirectiveAct {
	return []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActRequestConfirmation,
		conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure,
		conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActOfferHelp,
		conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
	}
}

// newRig builds the service over the deployment's data directory and starts it. Everything it
// holds is closed when the test ends.
func newRig(t tb, dep *deployment, opts ...rigOption) *rig {
	t.Helper()
	var o rigOptions
	for _, f := range opts {
		f(&o)
	}
	r := &rig{t: t, dep: dep, clk: &skewClock{}, recs: &policyRecorder{}, logs: &syncBuffer{}}
	r.log = slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// The application's clock may be ahead of wall time already (an earlier test moved it): the
	// service starts at the application's time, so the tokens it mints are not from the future.
	r.clk.Advance(time.Duration(dep.clinic.stats(t).ClockOffsetSeconds) * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	var stops []func()
	r.stopAll = func() {
		cancel()
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	t.Cleanup(func() {
		r.stopAll()
		if t.Failed() {
			t.Logf("the service's warnings and errors:\n%s", r.logs.String())
		}
	})

	sopts := store.FromConfig()
	sopts.Path = filepath.Join(dep.op.dataDir, store.DefaultFileName)
	sopts.Logger = r.log
	db, err := store.Open(ctx, sopts)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	r.db = db
	stops = append(stops, func() { _ = db.Close() })

	// --- the same order as wireV1 ---
	verifier := trust.NewVerifier(trust.NewStore(db, r.clk), r.clk)
	r.audit, err = audit.New(db, audit.Options{Clock: r.clk, Registry: audit.DefaultRegistry, Logger: r.log, Verifier: verifier})
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	gw := policy.NewGateway(r.clk, policy.WithSink(teeSink{
		audit.NewRecordSink[policy.Record](r.audit, audit.EventPolicyDecision, func(rec policy.Record) (audit.Locator, error) {
			s := rec.Subject
			return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
		}),
		r.recs,
	}))
	core := apppkg.CoreVersionOrDev(version.Get().Version)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: r.clk, DataDir: dep.op.dataDir, Verifier: verifier, CoreVersion: core})
	if err != nil {
		t.Fatalf("installer: %v", err)
	}
	r.apps, err = gateway.NewApplications(inst, apppkg.Options{Verifier: verifier, CoreVersion: core}, r.clk, 0)
	if err != nil {
		t.Fatalf("applications: %v", err)
	}
	r.registry = capabilities.NewRegistry(r.apps.Source(), capabilities.WithClock(r.clk))
	r.adapters = &rigAdapters{apps: r.apps, inst: inst, dataDir: dep.op.dataDir, log: r.log, cache: map[string]rigCachedAdapter{}}
	stops = append(stops, r.adapters.close)

	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: r.audit, Clock: r.clk, Logger: r.log,
		Packages: actions.PackageSourceFunc(r.apps.ActivePackage), Adapters: r.adapters,
	})
	if err != nil {
		t.Fatalf("action coordinator: %v", err)
	}
	r.co = co
	sess, err := sessions.New(db, r.clk, sessions.Config{})
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	// The one swap: the model is the scripted fake, behind the same request budget as the local
	// runtime. A call no script answers fails, and the rig reports it when the test ends.
	r.model = pfake.New()
	r.model.On(pfake.Any()).ReplyFunc(func(c pfake.Call) (string, error) {
		last := ""
		if n := len(c.Request.Messages); n > 0 {
			last = c.Request.Messages[n-1].Content
		}
		if len(last) > 80 {
			last = last[:80] + "..."
		}
		r.mu.Lock()
		r.unscripted = append(r.unscripted, fmt.Sprintf("a %s call for tenant %s (last message %q)", c.Request.Output.Kind(), c.Scope.TenantID, last))
		r.mu.Unlock()
		return "", contracts.New(contracts.ErrInternal, "the gate's scripted model has no script for this call")
	})
	sel, err := dialogue.LocalSelector(r.model, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("provider selector: %v", err)
	}
	cfg := dialogue.Config{}
	if o.templateOnly {
		cfg.TemplateOnly = allActs()
	}
	r.engine, err = dialogue.New(dialogue.Deps{
		Sessions: sess, Registry: r.registry, Packages: r.apps, Policy: gw, Actions: co, Adapters: r.adapters,
		Providers: sel, Audit: r.audit, Clock: r.clk, Logger: r.log, Config: cfg,
		Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{},
	})
	if err != nil {
		t.Fatalf("dialogue engine: %v", err)
	}

	// The areas that share a service with the ones this gate uses are provided as well, as
	// alixai serve provides them: a server refuses to start with an area half wired.
	filesSvc, err := files.New(files.Options{DB: db, Clock: r.clk, DataDir: dep.op.dataDir, Log: r.log})
	if err != nil {
		t.Fatalf("files service: %v", err)
	}
	historySvc, err := history.New(history.Deps{DB: db, Transcripts: sess, Audit: r.audit, Clock: r.clk, Logger: r.log})
	if err != nil {
		t.Fatalf("history service: %v", err)
	}
	services := gateway.NewServices()
	for _, perr := range []error{
		gateway.Provide[gateway.FilesService](services, filesSvc),
		gateway.Provide(services, historySvc),
		gateway.Provide(services, sess),
		gateway.Provide(services, r.engine),
		gateway.Provide[gateway.ActionService](services, co),
		gateway.Provide[gateway.ApplicationService](services, r.apps),
		gateway.Provide[gateway.RAGSources](services, rigRAGSources{}),
		gateway.Provide[gateway.EventsIntakes](services, rigEventsIntakes{}),
		gateway.Provide(services, gateway.NewEventsFeed(gateway.EventsFeedConfig{Clock: r.clk})),
		gateway.Provide(services, gw),
		gateway.Provide[actions.AdapterSource](services, r.adapters),
		gateway.Provide[audit.Appender](services, r.audit),
		gateway.Provide(services, stream.NewHub(stream.Config{Clock: r.clk, Logger: r.log})),
	} {
		if perr != nil {
			t.Fatalf("provide a service: %v", perr)
		}
	}

	// The /v1 API verifies tokens against the issuer keys the operator provisioned, once per use.
	keys := identity.NewIssuerKeyStore(db, r.clk, identity.IssuerKeyStoreOptions{Logger: r.log})
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: deploymentID, Audience: audience, MaxLifetime: identity.MaxLifetime,
		Keys: keys, Replay: identity.NewSQLiteReplayCache(db, r.clk, r.log), Clock: r.clk, Logger: r.log,
	})
	if err != nil {
		t.Fatalf("identity verifier: %v", err)
	}
	srv, err := gateway.NewServer(gateway.Options{
		Logger: r.log, Clock: r.clk, Providers: providers.NewRegistry(r.clk),
		V1Auth: identity.Middleware(v, identity.WithLogger(r.log)), Services: services,
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	if un := srv.Unmounted(); len(un) > 0 {
		// only the areas this gate does not use may be missing
		for area := range un {
			switch area {
			case "v1_sessions", "v1_actions", "v1_apps", "v1_turnevents":
				t.Fatalf("the public API area %s is not mounted: it needs %v", area, un[area])
			}
		}
	}
	ln, err := gateway.Listen("127.0.0.1:0", srv.Policy())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()
	stops = append(stops, func() {
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Errorf("the service did not stop")
		}
	})
	r.base = "http://" + ln.Addr().String()

	// The operator's admin interface, on the data directory, so that `alixai audit quarantine`
	// and an activation tell this service at once, as they tell a running `alixai serve`.
	stopAdmin, err := r.startAdmin(ctx, dep.op.dataDir)
	if err != nil {
		t.Fatalf("the admin interface: %v", err)
	}
	stops = append(stops, stopAdmin)

	t.Cleanup(r.checkScripts)
	return r
}

// rigAdminControl is what the admin interface acts on, as in `alixai serve`: every cache of the
// process that holds something about an application.
type rigAdminControl struct{ r *rig }

func (c rigAdminControl) Refresh(ctx context.Context, applicationID string) error {
	c.r.apps.AdminForget(applicationID)
	return c.r.registry.Refresh(ctx, applicationID)
}

func (c rigAdminControl) Deactivate(applicationID string) {
	c.r.registry.Deactivate(applicationID)
	c.r.apps.AdminForget(applicationID)
	c.r.adapters.forget(applicationID)
}

func (r *rig) startAdmin(ctx context.Context, dataDir string) (func(), error) {
	token, err := gateway.NewAdminToken()
	if err != nil {
		return nil, err
	}
	adm, err := gateway.NewAdminServer(gateway.AdminOptions{Control: rigAdminControl{r}, Token: token, Logger: r.log, Clock: r.clk})
	if err != nil {
		return nil, err
	}
	ln, err := gateway.AdminListen()
	if err != nil {
		return nil, err
	}
	if err := gateway.WriteAdminEndpoint(dataDir, gateway.AdminEndpoint{
		URL: "http://" + ln.Addr().String(), Token: token, PID: os.Getpid(), StartedAt: r.clk.Now(),
	}); err != nil {
		_ = ln.Close()
		return nil, err
	}
	actx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = adm.Serve(actx, ln)
	}()
	return func() {
		cancel()
		<-done
		gateway.RemoveAdminEndpoint(dataDir, token)
	}, nil
}

// advance moves time forward for the service and for the application together.
func (r *rig) advance(d time.Duration) {
	r.t.Helper()
	r.clk.Advance(d)
	r.dep.clinic.advanceClock(r.t, d)
}

// ---- the adapter, from the installed package ------------------------------------------------

type rigCachedAdapter struct {
	hash string
	ad   adapters.AppAdapter
}

// rigAdapters is the actions.AdapterSource of the service: the application's adapter, built for
// the package that is active now from that package's own endpoint, adapter configuration,
// operation bindings and egress allowlist, and wrapped with adapters.Wrap, so the scope check,
// the bounds and the result re-validation hold. It mirrors cmd_serve.go's adapterSource over
// transport_mcp.go's mapping of a package to the MCP adapter, because those are unexported.
type rigAdapters struct {
	apps    *gateway.Applications
	inst    *apppkg.Installer
	dataDir string
	log     *slog.Logger

	mu    sync.Mutex
	cache map[string]rigCachedAdapter
	build sync.Mutex
}

func (s *rigAdapters) Adapter(ctx context.Context, applicationID string) (adapters.AppAdapter, error) {
	pkg, err := s.apps.Package(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	hash := pkg.ContentHash()
	s.mu.Lock()
	c, ok := s.cache[applicationID]
	s.mu.Unlock()
	if ok && c.hash == hash {
		return c.ad, nil
	}
	s.build.Lock()
	defer s.build.Unlock()
	s.mu.Lock()
	c, ok = s.cache[applicationID]
	s.mu.Unlock()
	if ok && c.hash == hash {
		return c.ad, nil
	}
	opened, rec, err := s.inst.Open(ctx, applicationID, hash)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the installed package could not be opened to build its adapter")
	}
	cfg, err := rigMCPConfig(opened, filepath.Join(s.dataDir, filepath.FromSlash(rec.Path)), s.log)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the application's adapter could not be configured")
	}
	cfg.ApplicationID = applicationID
	inner, err := mcptransport.New(cfg)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the application's adapter could not be opened")
	}
	ad, err := adapters.Wrap(inner, adapters.Options{ApplicationID: applicationID})
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	wrapped := rigClosing{AppAdapter: ad, inner: inner}
	s.mu.Lock()
	old, had := s.cache[applicationID]
	s.cache[applicationID] = rigCachedAdapter{hash: hash, ad: wrapped}
	s.mu.Unlock()
	if had {
		rigClose(old.ad)
	}
	return wrapped, nil
}

func (s *rigAdapters) forget(applicationID string) {
	s.mu.Lock()
	old, had := s.cache[applicationID]
	delete(s.cache, applicationID)
	s.mu.Unlock()
	if had {
		rigClose(old.ad)
	}
}

func (s *rigAdapters) close() {
	s.mu.Lock()
	all := s.cache
	s.cache = map[string]rigCachedAdapter{}
	s.mu.Unlock()
	for _, c := range all {
		rigClose(c.ad)
	}
}

type rigClosing struct {
	adapters.AppAdapter
	inner io.Closer
}

func (c rigClosing) Close() error { return c.inner.Close() }

func rigClose(ad adapters.AppAdapter) {
	if c, ok := ad.(io.Closer); ok {
		_ = c.Close()
	}
}

// rigMCPConfig is what a verified package becomes as the MCP adapter's configuration: every mcp
// endpoint of the manifest as one server at its own URL, the operations of every capability and
// the resolve operation of every entity type as the tools they name (the only tools the adapter
// will ever call), the endpoint's adapter configuration's allow list, and an egress allowlist of
// exactly the endpoints' own hosts and ports. It mirrors internal/cli/transport_mcp.go.
func rigMCPConfig(pkg *apppkg.Package, dir string, log *slog.Logger) (mcptransport.Config, error) {
	cfg := mcptransport.Config{Logger: log, ClientName: "alixai", ClientVersion: version.Get().Version}
	endpointIDs := map[string]bool{}
	var dests []egress.Destination
	ids := make([]string, 0, len(pkg.Manifest.Endpoints))
	for id := range pkg.Manifest.Endpoints {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v, ok := pkg.Manifest.Endpoints[id].Value.(ap.ManifestMCPEndpoint)
		if !ok {
			continue // not an operation endpoint of this transport
		}
		endpointIDs[id] = true
		ep := mcptransport.Endpoint{ID: id, URL: string(v.URL)}
		if v.ContractSHA256 != nil {
			ep.ContractSHA256 = strings.ToLower(string(*v.ContractSHA256))
		}
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(string(v.AdapterConfig))))
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: adapter_config: %w", id, err)
		}
		ac, err := mcptransport.ParseAdapterConfig(raw, id)
		if err != nil {
			return cfg, err
		}
		ep.Allow = ac.Tools.Allow
		cfg.Endpoints = append(cfg.Endpoints, ep)

		host, port, err := hostPortOf(string(v.URL))
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: %w", id, err)
		}
		dests = append(dests, egress.Destination{Host: host, Ports: []int{port}, Scheme: "http"})
	}
	allow, err := egress.NewAllowlist(dests...)
	if err != nil {
		return cfg, err
	}
	cfg.Dialer = egress.NewDialer(allow)

	capIDs := make([]string, 0, len(pkg.Capabilities))
	for id := range pkg.Capabilities {
		capIDs = append(capIDs, id)
	}
	sort.Strings(capIDs)
	bind := func(capability, what string, b ap.CapabilityOperationBinding) (mcptransport.Binding, error) {
		if !endpointIDs[string(b.Endpoint)] || b.MCPTool == nil {
			return mcptransport.Binding{}, fmt.Errorf("capability %q: %s is not bound to an mcp tool of an mcp endpoint", capability, what)
		}
		out := mcptransport.Binding{Endpoint: string(b.Endpoint), Tool: string(*b.MCPTool)}
		if b.TimeoutMS != nil {
			out.Timeout = time.Duration(*b.TimeoutMS) * time.Millisecond
		}
		if len(b.Arguments) > 0 {
			out.Arguments = make(map[string]string, len(b.Arguments))
			for from, to := range b.Arguments {
				out.Arguments[from] = string(to)
			}
		}
		return out, nil
	}
	for _, id := range capIDs {
		c := pkg.Capabilities[id]
		mc := mcptransport.Capability{ID: id, Operations: map[adapters.Operation]mcptransport.Binding{}, Resolve: map[string]mcptransport.Binding{}}
		for op, b := range map[adapters.Operation]*ap.CapabilityOperationBinding{
			adapters.OpRead: c.Operations.Read, adapters.OpValidate: c.Operations.Validate, adapters.OpPrepare: c.Operations.Prepare,
			adapters.OpCommit: c.Operations.Commit, adapters.OpStatus: c.Operations.Status,
		} {
			if b == nil {
				continue
			}
			mb, err := bind(id, string(op), *b)
			if err != nil {
				return cfg, err
			}
			mc.Operations[op] = mb
		}
		for typ, et := range c.EntityTypes {
			if et.Resolve == nil {
				continue
			}
			mb, err := bind(id, "entity_types."+typ+".resolve", *et.Resolve)
			if err != nil {
				return cfg, err
			}
			mc.Resolve[typ] = mb
		}
		cfg.Capabilities = append(cfg.Capabilities, mc)
	}
	return cfg, nil
}

func hostPortOf(rawURL string) (string, int, error) {
	hostport := strings.TrimPrefix(rawURL, "http://")
	hostport, _, _ = strings.Cut(hostport, "/")
	host, portText, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", 0, fmt.Errorf("the url %q has no host and port: %w", rawURL, err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("the url %q has no usable port", rawURL)
	}
	return host, port, nil
}

// ---- scripting the model ------------------------------------------------------------------

// script adds a rule that answers up to times calls matching m, and fails the test at its end if
// the calls never came: a script that was not used means the test's idea of what the model is
// asked is wrong.
func (r *rig) script(what string, times int, m pfake.Matcher, reply string) *rigScript {
	r.t.Helper()
	rule := r.model.On(m).Reply(reply).Times(times)
	s := &rigScript{what: what, rule: rule, times: times}
	r.scripts = append(r.scripts, s)
	return s
}

// proposal scripts the model's next TaskProposal for a tenant (model call 1, a JSON-schema
// constrained request): the capability the model picks, the intent, and the field values it read
// out of the user's words.
func (r *rig) proposal(tenant, capability, intent string, fields map[string]any) *rigScript {
	r.t.Helper()
	return r.script("a task proposal for "+capability, 1,
		pfake.All(pfake.ForTenant(tenant), pfake.Constrained(providers.ConstraintJSONSchema)),
		proposalJSON(capability, intent, fields))
}

// rawProposal scripts a model output that is not necessarily a TaskProposal at all.
func (r *rig) rawProposal(tenant, what, output string) *rigScript {
	r.t.Helper()
	return r.script(what, 1, pfake.All(pfake.ForTenant(tenant), pfake.Constrained(providers.ConstraintJSONSchema)), output)
}

// phrase scripts the text the model writes for the next response (model call 2, an unconstrained
// request). The verifier decides whether it is used.
func (r *rig) phrase(tenant, text string) *rigScript {
	r.t.Helper()
	return r.script("a response phrasing", 1, pfake.All(pfake.ForTenant(tenant), pfake.Constrained(providers.ConstraintNone)), text)
}

// phraseFunc scripts a response phrasing that is written when the call comes, from what the model
// would then have in front of it: the receipt of a commit that has just happened, say.
func (r *rig) phraseFunc(tenant, what string, f func(pfake.Call) (string, error)) *rigScript {
	r.t.Helper()
	rule := r.model.On(pfake.All(pfake.ForTenant(tenant), pfake.Constrained(providers.ConstraintNone))).ReplyFunc(f).Times(1)
	s := &rigScript{what: what, rule: rule, times: 1}
	r.scripts = append(r.scripts, s)
	return s
}

// modelCalls counts the calls that reached the model: proposals and phrasings.
func (r *rig) modelCalls(kind providers.ConstraintKind) int {
	n := 0
	for _, p := range r.model.Prompts() {
		if p.Chat.Output.Kind() == kind {
			n++
		}
	}
	return n
}

// checkScripts fails the test when the model was asked something nobody scripted, or a script
// was never used.
func (r *rig) checkScripts() {
	r.mu.Lock()
	un := append([]string(nil), r.unscripted...)
	r.mu.Unlock()
	for _, u := range un {
		r.t.Errorf("the model was called and nothing scripted an answer: %s", u)
	}
	for _, s := range r.scripts {
		if h := s.rule.Hits(); !s.optional && h < s.times {
			r.t.Errorf("a scripted model answer was never asked for (%s): used %d of %d", s.what, h, s.times)
		}
	}
}

// ---- the HTTP client ---------------------------------------------------------------------

// apiReply is one response of the public API.
type apiReply struct {
	Status int
	Header http.Header
	Body   []byte
}

// code is the ErrCode of an error envelope, or "" for any other body.
func (a apiReply) code() contracts.ErrCode {
	var e contracts.Error
	if a.Status < 400 || json.Unmarshal(a.Body, &e) != nil {
		return ""
	}
	return e.Code
}

func (a apiReply) String() string {
	return fmt.Sprintf("HTTP %d: %s", a.Status, strings.TrimSpace(string(a.Body)))
}

var apiClient = &http.Client{Timeout: 3 * time.Minute}

// call sends one request with the given bearer token ("" sends none).
func (r *rig) call(token, method, path string, body any, headers ...string) apiReply {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		var b []byte
		switch v := body.(type) {
		case string:
			b = []byte(v)
		case []byte:
			b = v
		default:
			var err error
			if b, err = json.Marshal(v); err != nil {
				r.t.Fatalf("encode the body of %s %s: %v", method, path, err)
			}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, r.base+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("%s %s: read the response: %v", method, path, err)
	}
	return apiReply{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}
