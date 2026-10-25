package injectionsec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
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
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	convgen "github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/tests/conversation"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// clinicPackage is the real, signed Application Integration Package of the clinic reference
// application, relative to the repository root.
const clinicPackage = "examples/reference-apps/clinic-booking/package"

// clinicApp is the application the package declares.
const clinicApp = "clinic-booking"

// clinicNow is "today" of every conversation: a Monday morning. The clinic's slots start two to
// four days later, inside the booking window.
var clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func ctxb() context.Context { return context.Background() }

// callCtx is the context of one call into the engine or the adapter: bounded, so a stuck call fails
// the test instead of hanging it.
func callCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// canary returns a random identifier-shaped string: it mixes letters and digits, so the response
// verifier reads it as an identifier, and it is unique to this run.
func canary(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "x" + hex.EncodeToString(b[:])
}

// ---- the package ----

// activePackage is the clinic package as the registry, the policy gateway, the action coordinator
// and the dialogue engine see it once the integration audit has activated it. A package fresh from
// the loader is quarantined and nothing in apppkg can activate it (only the audit does); the audit
// is not what these tests are about, so the one thing this wrapper changes is the activation state
// it reports. Everything else is the loaded package, byte for byte.
type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

func loadClinicPackage(t testing.TB) capabilities.Package {
	t.Helper()
	dir := filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(clinicPackage))
	res, err := apppkg.Load(ctxb(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	if res.Package == nil {
		t.Fatalf("package %s did not load: %v", dir, res.Findings)
	}
	return activePackage{capabilities.FromApppkg(res.Package)}
}

// ---- the application, behind a router ----

// request is one request the application received, as the router saw it.
type request struct {
	Tenant     string
	Principal  string
	Op         adapters.Operation
	Capability string
	Fields     map[string]any
}

// router is the adapter the engine reaches the application through. It serves one clinic host per
// tenant and records every request it forwards. The clinic host is bound to the clinic's own tenant
// identifier, so a request of another tenant is forwarded with that identifier in place of its own,
// to the host that holds that tenant's data: the application's behaviour is the same, only its data
// differs per tenant, as the data of a multi-tenant application does.
type router struct {
	own string // the tenant identifier the clinic host is bound to

	mu    sync.Mutex
	hosts map[string]adapters.AppAdapter
	log   []request
}

var _ adapters.AppAdapter = (*router)(nil)

func newRouter(own string) *router {
	return &router{own: own, hosts: map[string]adapters.AppAdapter{}}
}

// serve registers the host of a tenant.
func (r *router) serve(tenant string, h *conversation.ClinicHost) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[tenant] = h.Adapter()
}

func (r *router) route(sc contracts.Scope, op adapters.Operation, capability string, fields map[string]any) (adapters.AppAdapter, contracts.Scope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[sc.TenantID]
	if !ok {
		return nil, sc, contracts.New(contracts.ErrScopeViolation, "no application data is served for this tenant")
	}
	cp := make(map[string]any, len(fields))
	for k, v := range fields {
		cp[k] = v
	}
	r.log = append(r.log, request{Tenant: sc.TenantID, Principal: sc.PrincipalID, Op: op, Capability: capability, Fields: cp})
	fwd := sc
	fwd.TenantID = r.own
	return h, fwd, nil
}

// requests returns what the application received, oldest first.
func (r *router) requests() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.log...)
}

// count counts the requests of one operation, on one capability when it is not empty.
func (r *router) count(op adapters.Operation, capability string) int {
	n := 0
	for _, q := range r.requests() {
		if q.Op == op && (capability == "" || q.Capability == capability) {
			n++
		}
	}
	return n
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
	h, fwd, err := r.route(sc, adapters.OpInspect, "", nil)
	if err != nil {
		return adapters.Inspection{}, err
	}
	return h.Inspect(ctx, fwd, req)
}

func (r *router) Read(ctx context.Context, sc contracts.Scope, req adapters.ReadRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc, adapters.OpRead, req.CapabilityID, req.Fields)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Read(ctx, fwd, req)
}

func (r *router) Validate(ctx context.Context, sc contracts.Scope, req adapters.ValidateRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc, adapters.OpValidate, req.CapabilityID, req.Fields)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Validate(ctx, fwd, req)
}

func (r *router) Prepare(ctx context.Context, sc contracts.Scope, req adapters.PrepareRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc, adapters.OpPrepare, req.CapabilityID, req.Fields)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Prepare(ctx, fwd, req)
}

func (r *router) Commit(ctx context.Context, sc contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc, adapters.OpCommit, req.CapabilityID, nil)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Commit(ctx, fwd, req)
}

func (r *router) Status(ctx context.Context, sc contracts.Scope, req adapters.StatusRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc, adapters.OpStatus, req.CapabilityID, nil)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Status(ctx, fwd, req)
}

func (r *router) Cancel(ctx context.Context, sc contracts.Scope, req adapters.CancelRequest) (act.DeterministicResult, error) {
	h, fwd, err := r.route(sc, adapters.OpCancel, req.CapabilityID, nil)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return h.Cancel(ctx, fwd, req)
}

// ---- the world ----

type worldOpts struct {
	tenants        []string
	modelResponses bool
	app            func(w *world) adapters.AppAdapter
	profile        string
}

type worldOpt func(*worldOpts)

// withModelResponses lets the model phrase the response of every act (the default is the
// deterministic template for every act, which sends no host data to any model).
func withModelResponses() worldOpt { return func(o *worldOpts) { o.modelResponses = true } }

// withProfile selects the context profile of the deployment (the default is Lite).
func withProfile(name string) worldOpt { return func(o *worldOpts) { o.profile = name } }

// withApp replaces the adapter the engine reaches the application through (the default is the router).
func withApp(f func(w *world) adapters.AppAdapter) worldOpt { return func(o *worldOpts) { o.app = f } }

// withTenants serves these tenants (the first is the clinic's own).
func withTenants(ids ...string) worldOpt { return func(o *worldOpts) { o.tenants = ids } }

// allActs lists every dialogue act, for Config.TemplateOnly.
func allActs() []convgen.DialogueDirectiveAct {
	return []convgen.DialogueDirectiveAct{
		convgen.DialogueDirectiveActAskMissing, convgen.DialogueDirectiveActAskInvalid,
		convgen.DialogueDirectiveActDisambiguate, convgen.DialogueDirectiveActRequestConfirmation,
		convgen.DialogueDirectiveActRequestStepUp, convgen.DialogueDirectiveActPresentResult,
		convgen.DialogueDirectiveActReportOutcome, convgen.DialogueDirectiveActReportFailure,
		convgen.DialogueDirectiveActRefuse, convgen.DialogueDirectiveActOfferHelp,
		convgen.DialogueDirectiveActAcknowledgeCancel, convgen.DialogueDirectiveActReportNoEvidence,
	}
}

// world is everything a conversation runs against, wired as a composition root wires it: a
// throwaway store, the real audit log and policy gateway, the capability registry over the real
// clinic package, the router behind adapters.Wrap, the action coordinator, the session store, the
// scripted model behind the local selector and the dialogue engine.
type world struct {
	t     testing.TB
	db    *store.DB
	clk   *clock.Fake
	rt    *router
	hosts map[string]*conversation.ClinicHost
	guard adapters.AppAdapter
	gw    *policy.Gateway
	co    *actions.Coordinator
	reg   *capabilities.Registry
	store *sessions.Store
	model *pfake.Provider
	eng   *dialogue.Engine
	pkg   capabilities.Package
	own   string
}

func newWorld(t testing.TB, opts ...worldOpt) *world {
	t.Helper()
	own := conversation.RoleScope(conversation.RoleReceptionist).TenantID
	o := worldOpts{tenants: []string{own}}
	for _, f := range opts {
		f(&o)
	}
	w := &world{t: t, clk: clock.NewFake(clinicNow), own: own, hosts: map[string]*conversation.ClinicHost{}}
	cfg := dialogue.Config{}
	if o.profile != "" {
		p, err := dctx.ProfileByName(o.profile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Profile = p
	}
	if !o.modelResponses {
		cfg.TemplateOnly = allActs()
	}
	w.db = storetest.Open(t, nil)
	alog, err := audit.New(w.db, audit.Options{Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	w.gw = policy.NewGateway(w.clk, policy.WithSink(sink))
	w.rt = newRouter(own)
	for _, tn := range o.tenants {
		h := conversation.NewClinicHost(w.clk)
		w.hosts[tn] = h
		w.rt.serve(tn, h)
	}
	var inner adapters.AppAdapter = w.rt
	if o.app != nil {
		inner = o.app(w)
	}
	if w.guard, err = adapters.Wrap(inner, adapters.Options{ApplicationID: clinicApp}); err != nil {
		t.Fatal(err)
	}
	w.pkg = loadClinicPackage(t)
	w.reg = capabilities.NewRegistry(nil, capabilities.WithClock(w.clk))
	if err := w.reg.Activate(w.pkg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	w.co, err = actions.New(actions.Deps{
		DB: w.db, Policy: w.gw, Audit: alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return w.pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return w.guard, nil }),
		Clock:    w.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(w.db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	w.model = pfake.New(pfake.WithClock(w.clk))
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	sel, err := dialogue.LocalSelector(w.model, budget)
	if err != nil {
		t.Fatal(err)
	}
	w.eng, err = dialogue.New(dialogue.Deps{
		Sessions: w.store, Registry: w.reg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil }),
		Policy:   w.gw, Actions: w.co,
		Adapters:  actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return w.guard, nil }),
		Providers: sel, Audit: alog, Clock: w.clk, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// scopeOf is the verified scope of the clinic's principal for a role, in a tenant.
func scopeOf(tenant, role string) contracts.Scope {
	sc := conversation.RoleScope(role)
	sc.TenantID = tenant
	return sc
}

// plant puts a patient into the host of a tenant. The identifier is the application's own
// (pat_ and twelve characters), so a record found by name is a record the application knows.
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

// ---- scripting the model ----

// proposalJSON builds a model output for a TaskProposal.
func proposalJSON(capability, intent string, fields map[string]any) string {
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

// propose scripts the model's next TaskProposal (a JSON-schema constrained request) for one use: a
// turn that makes a call the test did not script has no script and fails.
func (w *world) propose(capability, intent string, fields map[string]any) {
	w.t.Helper()
	w.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(capability, intent, fields)).Times(1)
}

// phrase scripts the next response text (an unconstrained request) for one use.
func (w *world) phrase(text string) {
	w.t.Helper()
	w.model.On(pfake.Constrained(providers.ConstraintNone)).Reply(text).Times(1)
}

// ---- running turns ----

// turn runs one text turn and fails the test on an error: an error means the turn could not be run
// at all, and every refusal or failure is a Result with a typed code.
func (w *world) turn(sc contracts.Scope, in dialogue.TurnInput) dialogue.Result {
	w.t.Helper()
	res, err := w.eng.Turn(callCtx(w.t), sc, in)
	if err != nil {
		w.t.Fatalf("Turn: %v", err)
	}
	if strings.TrimSpace(res.Response.Text) == "" {
		w.t.Fatalf("empty response for act %s", res.Directive.Act)
	}
	return res
}

func (w *world) say(sc contracts.Scope, lang, text string) dialogue.Result {
	w.t.Helper()
	return w.turn(sc, dialogue.TurnInput{Text: text, Language: lang})
}

func wantAct(t testing.TB, res dialogue.Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q, response %q), want %s", res.Directive.Act, res.Code, res.Response.Text, a)
	}
}

func wantCode(t testing.TB, res dialogue.Result, code contracts.ErrCode) {
	t.Helper()
	if res.Code != code {
		t.Fatalf("code = %q, want %q (act %s, response %q)", res.Code, code, res.Directive.Act, res.Response.Text)
	}
}

// ---- what the model was sent ----

// sent is one call as it reached the model, flattened for scanning.
type sent struct {
	Seq    int
	Tenant string
	Kind   string // proposal or response
	Texts  []string
	Prompt pfake.Prompt
}

// sentPrompts lists every chat call the model received, oldest first.
func sentPrompts(m *pfake.Provider) []sent {
	var out []sent
	for _, p := range m.Prompts() {
		kind := "other"
		switch p.Chat.Output.Kind() {
		case providers.ConstraintJSONSchema:
			kind = "proposal"
		case providers.ConstraintNone:
			kind = "response"
		}
		out = append(out, sent{Seq: p.Seq, Tenant: p.Scope.TenantID, Kind: kind, Texts: p.Texts(), Prompt: p})
	}
	return out
}

// find returns what the scanner found of the needles in every text of the calls: a description of
// each place a needle was seen. An empty needle is a bug in the test, not a match.
func find(calls []sent, needles map[string]string) []string {
	var hits []string
	for _, c := range calls {
		for i, text := range c.Texts {
			for what, needle := range needles {
				if needle == "" {
					panic("injectionsec: empty needle for " + what)
				}
				if strings.Contains(text, needle) {
					hits = append(hits, fmt.Sprintf("%s in text %d of %s call %d (tenant %s)", what, i, c.Kind, c.Seq, c.Tenant))
				}
			}
		}
	}
	return hits
}

// stateOf returns the display (or canonical) text of the first fact of the directive named field.
func stateOf(res dialogue.Result, field string) (string, bool) {
	for _, f := range res.Directive.Facts {
		if string(f.Field) != field {
			continue
		}
		b, _ := json.Marshal(f)
		var m struct {
			Value struct {
				Value any `json:"value"`
			} `json:"value"`
			Display string `json:"display"`
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		_ = dec.Decode(&m)
		if m.Display != "" {
			return m.Display, true
		}
		return fmt.Sprint(m.Value.Value), true
	}
	return "", false
}
