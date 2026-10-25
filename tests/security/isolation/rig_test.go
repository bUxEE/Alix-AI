package isolationsec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/parse"
	fsprov "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	convgen "github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	"github.com/buxee/alix-ai/tests/conversation"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

const (
	clinicPackage = "examples/reference-apps/clinic-booking/package"
	clinicApp     = "clinic-booking"
	embedModel    = "test-embed-32"
)

var (
	// clinicNow is "today" of every conversation: a Monday morning. The clinic's slots start two to four
	// days later, inside the booking window.
	clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	// oldTime is the modification time of every document, far enough behind the fake clock that the
	// folder provider trusts its record for the size-and-time shortcut.
	oldTime = time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
)

// canary returns a random identifier-shaped string, unique to this run.
func canary(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "x" + hex.EncodeToString(b[:])
}

// ---- the package ----

// activePackage is the clinic package as the registry, the policy gateway, the action coordinator
// and the dialogue engine see it once the integration audit has activated it (the loader reports a
// package quarantined and only the audit activates it). It is the loaded package, byte for byte, with
// the activation state and the languages the settings service asks the package for.
type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

// SupportedLanguages implements settings.Package.
func (activePackage) SupportedLanguages() []string { return []string{"en", "it"} }

func loadClinicPackage(t testing.TB) activePackage {
	t.Helper()
	dir := filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(clinicPackage))
	res, err := apppkg.Load(bg, apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	if res.Package == nil {
		t.Fatalf("package %s did not load: %v", dir, res.Findings)
	}
	return activePackage{capabilities.FromApppkg(res.Package)}
}

// ---- the application, behind a router ----

// router is the adapter the engine reaches the application through. It serves one clinic host per
// tenant, so the data of a tenant is real data in a real host. The clinic host is bound to the
// clinic's own tenant identifier, so a request of another tenant is forwarded with that identifier in
// place of its own, to the host that holds that tenant's data.
type router struct {
	own string

	mu    sync.Mutex
	hosts map[string]adapters.AppAdapter
}

var _ adapters.AppAdapter = (*router)(nil)

func (r *router) serve(tenant string, h *conversation.ClinicHost) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[tenant] = h.Adapter()
}

func (r *router) route(sc contracts.Scope) (adapters.AppAdapter, contracts.Scope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[sc.TenantID]
	if !ok {
		return nil, sc, contracts.New(contracts.ErrScopeViolation, "no application data is served for this tenant")
	}
	fwd := sc
	fwd.TenantID = r.own
	return h, fwd, nil
}

func (r *router) any() adapters.AppAdapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hosts {
		return h
	}
	return nil
}

func (r *router) Describe(ctx context.Context) (adapters.Description, error) {
	return r.any().Describe(ctx)
}
func (r *router) Health(ctx context.Context) (adapters.HealthReport, error) {
	return r.any().Health(ctx)
}

func (r *router) Inspect(ctx context.Context, sc contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return adapters.Inspection{}, err
	}
	return h.Inspect(ctx, fwd, req)
}

func (r *router) Read(ctx context.Context, sc contracts.Scope, req adapters.ReadRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Read(ctx, fwd, req)
}

func (r *router) Validate(ctx context.Context, sc contracts.Scope, req adapters.ValidateRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Validate(ctx, fwd, req)
}

func (r *router) Prepare(ctx context.Context, sc contracts.Scope, req adapters.PrepareRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Prepare(ctx, fwd, req)
}

func (r *router) Commit(ctx context.Context, sc contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Commit(ctx, fwd, req)
}

func (r *router) Status(ctx context.Context, sc contracts.Scope, req adapters.StatusRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Status(ctx, fwd, req)
}

func (r *router) Cancel(ctx context.Context, sc contracts.Scope, req adapters.CancelRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Cancel(ctx, fwd, req)
}

// ---- the whole world ----

// world is one engine, one session store, one database, one index and one pair of models serving every
// tenant of a test. Nothing is per tenant but the tenant's data and the scope its caller presents.
type world struct {
	t     *testing.T
	clk   *clock.Fake
	db    *store.DB
	alog  *audit.Log
	gw    *policy.Gateway
	pkg   activePackage
	rt    *router
	hosts map[string]*conversation.ClinicHost
	own   string

	store *sessions.Store
	eng   *dialogue.Engine
	// dlg is the model of the dialogue engine; rag is the model of retrieval and answering (embedding,
	// reranking and the grounded answer). Both record every call with the scope it was made under.
	dlg *fake.Provider
	rag *fake.Provider

	// the knowledge stack, when the world has one
	withRAG bool
	q       *jobs.Queue
	pool    *jobs.Pool
	ix      *index.Index
	svc     *ingest.Service
	reg     *sourceRegistry
	cat     *catalog
	base    string
	syncs   int

	settings *settings.Service
}

type worldOpts struct {
	rag bool
}

type worldOpt func(*worldOpts)

// withRAG adds the knowledge stack: the folder source provider, the sandboxed parser worker, the
// chunker, the FTS5 and flat vector indexes, the job queue and ingestion service, the query pipeline
// and the grounded answerer, wired to the engine as the composition root wires them.
func withRAG() worldOpt { return func(o *worldOpts) { o.rag = true } }

func newWorld(t *testing.T, tenants []string, opts ...worldOpt) *world {
	t.Helper()
	var o worldOpts
	for _, f := range opts {
		f(&o)
	}
	own := conversation.RoleScope(conversation.RoleReceptionist).TenantID
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, clk: clock.NewFake(clinicNow), own: own, hosts: map[string]*conversation.ClinicHost{}, base: base, withRAG: o.rag}
	w.db = storetest.Open(t, nil) // store.Default holds the migrations of every imported module
	if w.alog, err = audit.New(w.db, audit.Options{Clock: w.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](w.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	w.gw = policy.NewGateway(w.clk, policy.WithSink(sink))
	w.pkg = loadClinicPackage(t)

	w.rt = &router{own: own, hosts: map[string]adapters.AppAdapter{}}
	for _, tn := range tenants {
		h := conversation.NewClinicHost(w.clk)
		w.hosts[tn] = h
		w.rt.serve(tn, h)
	}
	guard, err := adapters.Wrap(w.rt, adapters.Options{ApplicationID: clinicApp})
	if err != nil {
		t.Fatal(err)
	}
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(w.clk))
	if err := reg.Activate(w.pkg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	co, err := actions.New(actions.Deps{
		DB: w.db, Policy: w.gw, Audit: w.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return w.pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
		Clock:    w.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(w.db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}

	w.dlg = fake.New(fake.WithClock(w.clk))
	w.installDialogueModel()
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	sel, err := dialogue.LocalSelector(w.dlg, budget)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := dctx.ProfileByName(dctx.ProfileBalanced)
	if err != nil {
		t.Fatal(err)
	}
	deps := dialogue.Deps{
		Sessions: w.store, Registry: reg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil }),
		Policy:   w.gw, Actions: co,
		Adapters:  actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
		Providers: sel, Audit: w.alog, Clock: w.clk, Config: dialogue.Config{Profile: prof},
		UITools: dialogue.StaticUITools(map[string][]string{clinicApp: {"open_panel"}}),
	}
	if o.rag {
		deps.Knowledge = w.buildRAG()
	}
	if w.eng, err = dialogue.New(deps); err != nil {
		t.Fatal(err)
	}

	if w.settings, err = settings.New(settings.Deps{
		DB: w.db, Clock: w.clk, Audit: w.alog, Policy: w.gw, Limits: settings.DefaultLimits(),
		Packages: settingsPackages{w},
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

type settingsPackages struct{ w *world }

func (s settingsPackages) Package(context.Context, string) (settings.Package, error) {
	return s.w.pkg, nil
}

// ---- the dialogue model ----

var (
	userMessageRE = regexp.MustCompile(`(?s)<user_message>\n(.*?)\n</user_message>`)
	findRE        = regexp.MustCompile(`Find the patient (\w+)`)
	reasonRE      = regexp.MustCompile(`reason: (\S+)`)
	dateRE        = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

// installDialogueModel scripts the controller of the dialogue engine as a function of the user's
// message of the call, so one rule serves every turn of every tenant whatever their order: the model
// maps "Find the patient X", "Book Luca Ferri ... reason: R" and "Which appointments are free on D" to
// the proposal they name and every other request to out_of_scope, and phrases every response as one
// sentence that states no fact (the verifier replaces it with the template).
func (w *world) installDialogueModel() {
	w.dlg.On(fake.Constrained(providers.ConstraintJSONSchema)).ReplyFunc(func(c fake.Call) (string, error) {
		user := ""
		for _, m := range c.Request.Messages {
			if m.Role == providers.MsgUser {
				if g := userMessageRE.FindStringSubmatch(m.Content); g != nil {
					user = g[1]
				}
			}
		}
		switch {
		case findRE.MatchString(user):
			return proposal("patients.find", "task", map[string]any{"query": findRE.FindStringSubmatch(user)[1]}), nil
		case strings.Contains(user, "Book Luca Ferri"):
			f := map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"}
			if g := reasonRE.FindStringSubmatch(user); g != nil {
				f["reason"] = g[1]
			}
			return proposal("appointments.book", "task", f), nil
		case strings.Contains(user, "Which appointments are free on "):
			return proposal("availability.list", "task", map[string]any{"date": dateRE.FindString(user)}), nil
		}
		return proposal("", "out_of_scope", nil), nil
	})
	w.dlg.On(fake.Constrained(providers.ConstraintNone)).Reply("Here is what I found for you.")
}

func proposal(capability, intent string, fields map[string]any) string {
	m := map[string]any{
		"capability_id": nil, "fields": map[string]any{}, "entity_refs": []any{}, "intent": intent,
		"uncertainty": map[string]any{"overall": 0.9},
	}
	if capability != "" {
		m["capability_id"] = capability
	}
	if fields != nil {
		m["fields"] = fields
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---- scopes and turns ----

// caller is the verified scope of the clinic's receptionist in a tenant. Every caller of a test is the
// same principal in the same workspace and session: they differ in the tenant and nothing else, which
// is the shape in which a key that forgets the tenant would show. Capabilities lists the capability
// scopes of the token (the clinic's, plus the retrieval scopes).
func caller(tenant, session string, without ...string) contracts.Scope {
	sc := conversation.RoleScope(conversation.RoleReceptionist)
	sc.TenantID, sc.SessionID = tenant, session
	var caps []string
	for _, c := range append(sc.CapabilityScopes, "rag.query", "rag.sync") {
		keep := true
		for _, w := range without {
			keep = keep && c != w
		}
		if keep {
			caps = append(caps, c)
		}
	}
	sc.CapabilityScopes = caps
	return sc
}

func (w *world) plant(tenant, id, given, family string) {
	w.t.Helper()
	h, ok := w.hosts[tenant]
	if !ok {
		w.t.Fatalf("no host for tenant %q", tenant)
	}
	if err := h.Apply(conversation.HostEvent{Kind: "add_patient", PatientID: id, Given: given, Family: family, Born: "1990-01-01"}); err != nil {
		w.t.Fatal(err)
	}
}

// client is what a client supplies with a turn: readable context lines and the UI tools it declared.
type client struct {
	lines []string
	tools []dialogue.UIToolDeclaration
}

func (w *world) say(sc contracts.Scope, c client, text string) dialogue.Result {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	if len(c.lines) > 0 {
		ctx = dialogue.WithClientContext(ctx, dialogue.ClientContext{Lines: c.lines})
	}
	if len(c.tools) > 0 {
		ctx = dialogue.WithUITools(ctx, c.tools)
	}
	res, err := w.eng.Turn(ctx, sc, dialogue.TurnInput{Text: text, Language: "en"})
	if err != nil {
		w.t.Fatalf("Turn(%q): %v", text, err)
	}
	if strings.TrimSpace(res.Response.Text) == "" {
		w.t.Fatalf("empty response for act %s", res.Directive.Act)
	}
	return res
}

func wantAct(t testing.TB, res dialogue.Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q, response %q), want %s", res.Directive.Act, res.Code, res.Response.Text, a)
	}
}

// ---- what the models were sent ----

// sent is one call as it reached a model, flattened for scanning.
type sent struct {
	Model  string // dialogue or rag
	Seq    int
	Op     fake.Op
	Tenant string
	Kind   string
	Texts  []string
	Prompt fake.Prompt
}

func (w *world) sent() []sent {
	var out []sent
	for _, m := range []struct {
		name string
		p    *fake.Provider
	}{{"dialogue", w.dlg}, {"rag", w.rag}} {
		if m.p == nil {
			continue
		}
		for _, p := range m.p.Prompts() {
			kind := string(p.Op)
			if p.Op == fake.OpChat || p.Op == fake.OpChatStream {
				switch p.Chat.Output.Kind() {
				case providers.ConstraintJSONSchema:
					kind = "chat/constrained"
				default:
					kind = "chat/text"
				}
			}
			out = append(out, sent{Model: m.name, Seq: p.Seq, Op: p.Op, Tenant: p.Scope.TenantID, Kind: kind, Texts: p.Texts(), Prompt: p})
		}
	}
	return out
}

// scan returns a description of every place a needle was found in the calls.
func scan(calls []sent, needles map[string]string) []string {
	var hits []string
	for _, c := range calls {
		for i, text := range c.Texts {
			for what, needle := range needles {
				if needle == "" {
					panic("isolationsec: empty needle for " + what)
				}
				if strings.Contains(text, needle) {
					hits = append(hits, fmt.Sprintf("%s in text %d of %s call %d of the %s model (tenant %s)", what, i, c.Kind, c.Seq, c.Model, c.Tenant))
				}
			}
		}
	}
	return hits
}

func forTenant(calls []sent, tenant string) []sent {
	var out []sent
	for _, c := range calls {
		if c.Tenant == tenant {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------- the knowledge stack

type allowScopes struct{}

func (allowScopes) CheckScope(context.Context, contracts.Scope) error { return nil }

// src is a registered folder source: the folder, the scope it serves and its definition.
type src struct {
	w     *world
	id    string
	name  string
	scope contracts.Scope
	root  string
	def   source.Definition
}

// sourceRegistry resolves a source id to its definition and the production folder provider, and
// refuses a job whose scope is not the one the definition serves.
type sourceRegistry struct {
	mu   sync.Mutex
	prov source.Provider
	defs map[string]source.Definition
}

func (r *sourceRegistry) Resolve(_ context.Context, scope contracts.Scope, id string) (ingest.Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[id]
	if !ok || d.CheckScope(scope) != nil {
		return ingest.Resolved{}, ingest.ErrSourceGone
	}
	return ingest.Resolved{Def: d, Provider: r.prov}, nil
}

// catalog is the deterministic authority on sources: a source is readable only by the application,
// tenant and workspace it was registered under, and its state is read from the ingestion status.
type catalog struct {
	w       *world
	mu      sync.Mutex
	sources map[string]*src
}

func (c *catalog) Sources(ctx context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		c.mu.Lock()
		s := c.sources[id]
		c.mu.Unlock()
		if s == nil || s.scope.ApplicationID != scope.ApplicationID || s.scope.TenantID != scope.TenantID || s.scope.WorkspaceID != scope.WorkspaceID {
			continue // not readable by this scope: indistinguishable from not existing
		}
		info, err := c.w.svc.Status(ctx, scope, id)
		st := query.SourceState{DisplayName: s.def.Title, Authorised: true}
		switch {
		case err == nil && info.Available:
			st.Availability = convgen.CitationAvailabilityAvailable
			st.Staleness = convgen.CitationStalenessUnknown
		case err == nil:
			st.Availability = convgen.CitationAvailabilityUnavailable
		case contracts.CodeOf(err) == contracts.ErrValidationFailed:
		default:
			return nil, err
		}
		out[id] = st
	}
	return out, nil
}

// buildRAG builds the knowledge stack and returns the Knowledge the engine is given.
func (w *world) buildRAG() dialogue.Knowledge {
	t := w.t
	var err error
	pause := ingest.NewPause(ingest.DiskPressureFunc(func(context.Context) (bool, error) { return false, nil }), w.clk, 30*time.Second, nil)
	if w.q, err = jobs.NewQueue(w.db, jobs.Options{Config: jobs.DefaultConfig(), Clock: w.clk, Policy: pause.Policy(nil), Jitter: func() float64 { return 0 }}); err != nil {
		t.Fatal(err)
	}
	if w.ix, err = index.New(w.db, w.clk, index.Config{}); err != nil {
		t.Fatal(err)
	}
	vc := vector.DefaultConfig()
	vc.CacheMiB = 8
	flat, err := vector.NewFlat(w.db, vector.FlatOptions{Config: vc, Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	// The real parser worker, one disposable process per document.
	pcfg := parse.DefaultConfig()
	pcfg.WorkerPath = parserWorker
	pcfg.StartupTimeout = 30 * time.Second
	pcfg.WallTimeout = 30 * time.Second
	runr, err := parse.NewRunner(pcfg)
	if err != nil {
		t.Fatalf("parse.NewRunner: %v", err)
	}
	t.Cleanup(func() { runr.Close() })

	w.rag = fake.New(fake.WithModel(embedModel))
	installAnswerModel(w.rag)
	w.reg = &sourceRegistry{prov: fsprov.New(fsprov.Config{Clock: w.clk}), defs: map[string]source.Definition{}}
	w.cat = &catalog{w: w, sources: map[string]*src{}}

	w.svc, err = ingest.New(ingest.Deps{
		DB: w.db, Clock: w.clk, Queue: w.q, Index: w.ix, Vectors: flat,
		Embedding: &ingest.Embedding{Embedder: w.rag, Model: embedModel, Component: rag.ComponentVersion{Name: "fake-embed", Version: "1"}},
		Registry:  w.reg, Parser: ingest.NewRunnerParser(runr), Chunker: chk, Audit: w.alog, Pause: pause, Config: ingest.DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	if w.pool, err = jobs.NewPool(w.q, jobs.PoolOptions{ScopeChecker: allowScopes{}}); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Handle(w.pool); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.pool.Shutdown(sctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})

	roles := providers.NewRegistry(w.clk)
	if err := roles.Register(providers.RoleEmbedder, w.rag); err != nil {
		t.Fatal(err)
	}
	pipe, err := query.New(query.Deps{
		Lexical: w.ix, Hits: w.ix, Catalog: w.cat, Providers: roles, EmbeddingModel: embedModel, Clock: w.clk, Vectors: flat,
	}, query.DefaultConfig())
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	ans, err := answer.New(answer.Deps{Generator: w.rag, Catalog: w.cat, Clock: w.clk}, answer.DefaultConfig())
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}
	queries, err := gateway.NewRAGQueries(pipe, ans)
	if err != nil {
		t.Fatal(err)
	}
	return gateway.RAGQueriesScoped(queries)
}

// declaration is the package declaration of a folder source that accepts the text formats.
func declaration(root string) ap.RAGSource {
	return ap.RAGSource{
		ID:    "docs",
		Type:  ap.RAGSourceTypeLocalFolder,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: clinicApp,
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
		},
		Root:     ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}},
		Include:  []ap.GlobPattern{"**"},
		Exclude:  []ap.GlobPattern{},
		Formats:  []ap.MimeType{source.MIMEText, source.MIMEMarkdown},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 8 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 10000},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodManual},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 24, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge},
	}
}

// addSource registers a folder source of the scope's tenant and workspace holding the files, and
// ingests it: every document parsed, indexed and embedded.
func (w *world) addSource(scope contracts.Scope, name string, docs map[string]string) *src {
	w.t.Helper()
	root := filepath.Join(w.base, name, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		w.t.Fatal(err)
	}
	for rel, body := range docs {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			w.t.Fatal(err)
		}
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			w.t.Fatal(err)
		}
	}
	decl := declaration(root)
	title := ap.Label(name)
	decl.Title = &title
	s := &src{w: w, id: ids.New(source.IDPrefix), name: name, scope: scope, root: root}
	def, err := source.FromDeclaration(decl, source.Binding{SourceID: s.id, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID})
	if err != nil {
		w.t.Fatal(err)
	}
	s.def = def
	w.reg.mu.Lock()
	w.reg.defs[def.SourceID] = def
	w.reg.mu.Unlock()
	w.cat.mu.Lock()
	w.cat.sources[s.id] = s
	w.cat.mu.Unlock()

	w.sync(s)
	return s
}

func (w *world) running() int {
	n := 0
	for _, c := range jobs.Classes() {
		n += w.pool.Running(c)
	}
	return n
}

func (w *world) pending() int {
	var n int
	row := w.db.QueryRowContext(bg, `SELECT count(*) FROM jobs WHERE state = 'running' OR (state = 'pending' AND (attempts > 0 OR run_at <= ?))`, w.clk.Now().UnixMilli())
	if err := row.Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) poll() int {
	w.t.Helper()
	n, err := w.pool.Poll(bg)
	if err != nil {
		w.t.Fatalf("Poll: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for w.running() > 0 {
		if time.Now().After(deadline) {
			w.t.Fatal("job handlers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	return n
}

// drain polls until the queue is empty. A job in its retry backoff is waited out by moving the fake
// clock.
func (w *world) drain() {
	w.t.Helper()
	stuck := 0
	for i := 0; i < 5000; i++ {
		n := w.poll()
		if w.pending() == 0 {
			return
		}
		if n == 0 {
			stuck++
			if stuck > 40 {
				w.t.Fatalf("the queue did not drain: %d jobs left\n%s", w.pending(), w.dumpJobs())
			}
			w.clk.Advance(30 * time.Second)
			continue
		}
		stuck = 0
	}
	w.t.Fatal("drain did not finish")
}

func (w *world) dumpJobs() string {
	rows, err := w.db.QueryContext(bg, `SELECT type, state, attempts, coalesce(error_code, ''), coalesce(error_message, '') FROM jobs ORDER BY seq`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, state, code, msg string
		var attempts int
		if err := rows.Scan(&typ, &state, &attempts, &code, &msg); err != nil {
			return err.Error()
		}
		fmt.Fprintf(&b, "  %-24s %-9s attempts=%d %s %s\n", typ, state, attempts, code, msg)
	}
	return b.String()
}

// ---- the scripted grounded answer ----

var (
	evidenceRE = regexp.MustCompile(`(?s)<evidence id="(e\d+)">\n(.*?)\n</evidence>`)
	sentenceRE = regexp.MustCompile(`[^.!?]+[.!?]+`)
)

// installAnswerModel scripts the grounded answer of the retrieval model: a faithful one, which states
// the first sentence of every passage it was given and cites it.
func installAnswerModel(fp *fake.Provider) {
	fp.On(fake.ForRole(providers.RoleController)).ReplyFunc(func(c fake.Call) (string, error) {
		var user string
		for _, m := range c.Request.Messages {
			if m.Role == providers.MsgUser {
				user = m.Content
			}
		}
		type claim struct {
			Text string   `json:"text"`
			IDs  []string `json:"evidence_ids"`
		}
		claims := []claim{}
		for _, m := range evidenceRE.FindAllStringSubmatch(user, -1) {
			for _, line := range strings.Split(m[2], "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if s := strings.TrimSpace(sentenceRE.FindString(line)); len(strings.Fields(s)) >= 4 {
					claims = append(claims, claim{Text: s, IDs: []string{m[1]}})
					break
				}
			}
		}
		b, err := json.Marshal(map[string]any{"claims": claims})
		return string(b), err
	})
}

// sync queues a sync of the source and drains the queue until the pipeline is idle: every document
// parsed, indexed and embedded. The sync must have succeeded.
func (w *world) sync(s *src) {
	w.t.Helper()
	w.syncs++
	res, err := w.svc.RequestSync(bg, s.scope, s.id, ingest.SyncOptions{Key: fmt.Sprintf("sec:%d", w.syncs)})
	if err != nil {
		w.t.Fatalf("RequestSync: %v", err)
	}
	w.drain()
	got, err := w.q.Get(bg, s.scope, res.Job.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	if got.State != jobs.StateSucceeded {
		w.t.Fatalf("sync of %s ended %s: %s\n%s", s.name, got.State, got.ErrorMessage, w.dumpJobs())
	}
}

// addFile writes a document into a source and ingests it.
func (w *world) addFile(s *src, rel, body string) {
	w.t.Helper()
	p := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Chtimes(p, oldTime, oldTime); err != nil {
		w.t.Fatal(err)
	}
	w.sync(s)
}

// mark remembers how many calls each model has received, so a test can scan only what was sent after.
func (w *world) mark() map[string]int {
	m := map[string]int{}
	for _, c := range w.sent() {
		if c.Seq > m[c.Model] {
			m[c.Model] = c.Seq
		}
	}
	return m
}

// since returns the calls sent after a mark, for a tenant.
func (w *world) since(mark map[string]int, tenant string) []sent {
	var out []sent
	for _, c := range w.sent() {
		if c.Seq > mark[c.Model] && c.Tenant == tenant {
			out = append(out, c)
		}
	}
	return out
}
