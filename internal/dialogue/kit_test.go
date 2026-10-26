package dialogue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The synthetic application of the tests: the two capabilities the in-memory host
// (internal/adapters/fake) offers, plus an admin capability the caller may never
// use. Identifiers that must never reach a prompt are distinctive so a leak is
// found by a substring search.
const (
	appID      = "demo-app"
	tenantA    = "tnt-acme-7781"
	tenantB    = "tnt-globex-5532"
	workspace1 = "wsp-north-2290"
	deployment = "dep-prod-3301"
	principal1 = "usr-jane-4410"
	session1   = "ses-xyz-5521"

	capGet    = fake.ReadCapability   // items.get
	capCreate = fake.ActionCapability // items.create
	capAdmin  = "admin.purge"
	capCount  = "items.count"
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

// ---- the typed package ----

// testPkg is an application package built in memory: the policy view from
// policy.NewPackage, the typed view beside it. It satisfies capabilities.Package
// and is how the engine is tested without a signed archive.
type testPkg struct {
	*policy.Package
	hash      string
	langs     []string
	specs     map[string]ap.Capability
	contracts map[[2]string]ap.ConversationContract
	terms     []ap.TerminologyTerm
}

func (p *testPkg) ContentHash() string          { return p.hash }
func (p *testPkg) SupportedLanguages() []string { return p.langs }
func (p *testPkg) Terminology() []ap.TerminologyTerm {
	return p.terms
}
func (p *testPkg) Spec(id string) (ap.Capability, bool) { s, ok := p.specs[id]; return s, ok }
func (p *testPkg) ContractFor(c, l string) (ap.ConversationContract, bool) {
	x, ok := p.contracts[[2]string{c, l}]
	return x, ok
}

var _ capabilities.Package = (*testPkg)(nil)

// level says how items.create is declared: the effect class fixes the platform risk
// floor, which fixes the confirmation it needs.
type level struct {
	effect  policy.EffectClass
	risk    contracts.RiskLevel
	confirm policy.ConfirmationMode
	typed   ap.CapabilityConfirmationMode
}

var (
	level1 = level{policy.EffectReversible, contracts.RiskReversible, policy.ConfirmNone, ap.CapabilityConfirmationModeNone}
	level2 = level{policy.EffectExternal, contracts.RiskConsequential, policy.ConfirmExplicit, ap.CapabilityConfirmationModeExplicit}
	level3 = level{policy.EffectPrivileged, contracts.RiskPrivileged, policy.ConfirmStepUp, ap.CapabilityConfirmationModeStepUp}
)

type pkgOpts struct {
	create level
	// resolveItems makes items.get's item_id an entity reference with a resolver.
	resolveItems bool
	// count adds items.count: a read that takes no fields.
	count bool
	// when adds a required datetime field "when" to items.create, for the tests of
	// "same time tomorrow".
	when  bool
	langs []string
}

func i64(n int64) *int64             { return &n }
func f64(n float64) *float64         { return &n }
func slugp(s string) *ap.Slug        { x := ap.Slug(s); return &x }
func boolp(b bool) *bool             { return &b }
func capID(s string) ap.CapabilityID { return ap.CapabilityID(s) }

func buildPackage(t testing.TB, o pkgOpts) *testPkg {
	t.Helper()
	if o.create.effect == "" {
		o.create = level2
	}
	if len(o.langs) == 0 {
		o.langs = []string{"en", "it"}
	}
	getFieldType := ap.CapabilityFieldTypeString
	var getEntity *ap.Slug
	entityTypes := map[string]ap.CapabilityEntityType{}
	if o.resolveItems {
		getFieldType, getEntity = ap.CapabilityFieldTypeEntityRef, slugp("item")
		entityTypes["item"] = ap.CapabilityEntityType{Description: "An item.", Resolve: &ap.CapabilityOperationBinding{Endpoint: "app"}}
	}
	specs := map[string]ap.Capability{
		capGet: {
			ID: capID(capGet), Audience: ap.CapabilityAudienceUser, Title: "Get an item",
			Purpose: "Look up one item by its ID and report its name. Use it when the user asks about an item.",
			Fields: []ap.CapabilityField{
				{Name: "item_id", Type: getFieldType, Required: true, Description: "The item to look up.", EntityType: getEntity},
			},
			EntityTypes: entityTypes,
			Ambiguity:   []ap.CapabilityAmbiguityRule{{ID: "item_multiple", Field: "item_id", When: ap.CapabilityAmbiguityRuleWhenMultipleMatches, Action: ap.CapabilityAmbiguityRuleActionAskUser, MaxChoices: i64(3)}},
			NextActions: []ap.CapabilityNextAction{{ID: "make_another", After: ap.CapabilityNextActionAfterSuccess, CapabilityID: capID(capCreate)}},
		},
		capCreate: {
			ID: capID(capCreate), Audience: ap.CapabilityAudienceUser, Title: "Create an item",
			Purpose: "Create a new item with a name and a quantity. The item is prepared first and created only after the user confirms.",
			Fields: []ap.CapabilityField{
				{Name: "name", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item name.", MinLength: i64(1), MaxLength: i64(64)},
				{Name: "quantity", Type: ap.CapabilityFieldTypeInteger, Required: true, Description: "How many.", Minimum: f64(1), Maximum: f64(99)},
				{Name: "notes", Type: ap.CapabilityFieldTypeString, Description: "A private note.", Sensitive: boolp(true), MaxLength: i64(300)},
			},
			EntityTypes: map[string]ap.CapabilityEntityType{},
		},
		capAdmin: {
			ID: capID(capAdmin), Audience: ap.CapabilityAudienceAdmin, Title: "Purge everything",
			Purpose: "Operators only: delete every item.", EntityTypes: map[string]ap.CapabilityEntityType{},
		},
	}
	if o.count {
		specs[capCount] = ap.Capability{
			ID: capID(capCount), Audience: ap.CapabilityAudienceUser, Title: "Count the items",
			Purpose: "Report how many items exist.", EntityTypes: map[string]ap.CapabilityEntityType{},
		}
	}
	getDecl := policy.FieldDecl{Name: "item_id", Required: true}
	decls := []policy.CapabilityDecl{
		{ID: capGet, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
			Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{getDecl}},
		{ID: capCreate, Audience: policy.AudienceUser, EffectClass: o.create.effect, RiskLevel: o.create.risk,
			Confirmation: o.create.confirm, Fields: []policy.FieldDecl{
				{Name: "name", Required: true}, {Name: "quantity", Required: true}, {Name: "notes", Sensitive: true},
			}},
		{ID: capAdmin, Audience: policy.AudienceAdmin, EffectClass: policy.EffectPrivileged, RiskLevel: contracts.RiskPrivileged, Confirmation: policy.ConfirmStepUp},
	}
	if o.count {
		decls = append(decls, policy.CapabilityDecl{ID: capCount, Audience: policy.AudienceUser, EffectClass: policy.EffectNone,
			RiskLevel: contracts.RiskRead, Confirmation: policy.ConfirmNone})
	}
	cr := specs[capCreate]
	if o.when {
		cr.Fields = append(cr.Fields, ap.CapabilityField{Name: "when", Type: ap.CapabilityFieldTypeDatetime, Required: true, Description: "When it is for."})
		for i := range decls {
			if decls[i].ID == capCreate {
				decls[i].Fields = append(decls[i].Fields, policy.FieldDecl{Name: "when", Required: true})
			}
		}
	}
	cr.EffectClass, cr.RiskLevel = ap.CapabilityEffectClass(o.create.effect), ap.RiskLevel(o.create.risk)
	cr.Confirmation = ap.CapabilityConfirmation{Mode: o.create.typed}
	specs[capCreate] = cr

	p := &testPkg{
		hash: "0000000000000000000000000000000000000000000000000000000000000abc", langs: o.langs,
		specs: specs, contracts: map[[2]string]ap.ConversationContract{},
	}
	label := func(en, it string) map[string]ap.TerminologyLabel {
		return map[string]ap.TerminologyLabel{"en": {Preferred: ap.Label(en)}, "it": {Preferred: ap.Label(it)}}
	}
	p.terms = []ap.TerminologyTerm{
		{ID: "create", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "create", Synonyms: []ap.Label{"add", "new"}}, "it": {Preferred: "crea", Synonyms: []ap.Label{"aggiungi", "nuovo"}}}},
		{ID: "item", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "item", Synonyms: []ap.Label{"thing", "article"}}, "it": {Preferred: "articolo", Synonyms: []ap.Label{"oggetto"}}}},
		{ID: "lookup", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "get", Synonyms: []ap.Label{"find", "show", "lookup"}}, "it": {Preferred: "trova", Synonyms: []ap.Label{"mostra", "cerca"}}}},
		{ID: "quantity", Labels: label("quantity", "quantità")},
		{ID: "name", Labels: label("name", "nome")},
	}
	if o.count {
		p.terms = append(p.terms, ap.TerminologyTerm{ID: "count", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "count", Synonyms: []ap.Label{"how many", "total"}}, "it": {Preferred: "conta", Synonyms: []ap.Label{"quanti", "totale"}}}})
		p.contracts[[2]string{capCount, "en"}] = ap.ConversationContract{CapabilityID: capID(capCount), Language: "en", Terminology: []ap.Slug{"count"}}
		p.contracts[[2]string{capCount, "it"}] = ap.ConversationContract{CapabilityID: capID(capCount), Language: "it", Terminology: []ap.Slug{"count"}}
	}
	sort.Slice(p.terms, func(i, j int) bool { return p.terms[i].ID < p.terms[j].ID })
	prompts := func(items ...[2]string) []ap.ConversationContractFieldPrompt {
		var out []ap.ConversationContractFieldPrompt
		for _, it := range items {
			out = append(out, ap.ConversationContractFieldPrompt{Field: common.FieldName(it[0]), Label: ap.Label(it[1]), Prompt: ap.Template("What is the " + it[1] + "?")})
		}
		return out
	}
	p.contracts[[2]string{capGet, "en"}] = ap.ConversationContract{CapabilityID: capID(capGet), Language: "en",
		Terminology: []ap.Slug{"lookup", "item"}, FieldPrompts: prompts([2]string{"item_id", "Item code"})}
	p.contracts[[2]string{capGet, "it"}] = ap.ConversationContract{CapabilityID: capID(capGet), Language: "it",
		Terminology: []ap.Slug{"lookup", "item"}, FieldPrompts: prompts([2]string{"item_id", "Codice articolo"})}
	p.contracts[[2]string{capCreate, "en"}] = ap.ConversationContract{CapabilityID: capID(capCreate), Language: "en",
		Terminology:  []ap.Slug{"create", "item", "quantity", "name"},
		FieldPrompts: prompts([2]string{"name", "Item name"}, [2]string{"quantity", "Quantity"})}
	p.contracts[[2]string{capCreate, "it"}] = ap.ConversationContract{CapabilityID: capID(capCreate), Language: "it",
		Terminology:  []ap.Slug{"create", "item", "quantity", "name"},
		FieldPrompts: prompts([2]string{"name", "Nome dell'articolo"}, [2]string{"quantity", "Quantità"})}

	memberCaps, visitorCaps := `"items.get", "items.create"`, `"items.get"`
	if o.count {
		memberCaps += `, "items.count"`
		visitorCaps += `, "items.count"`
	}
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": [` + memberCaps + `] },
    { "role": "visitor", "capabilities": [` + visitorCaps + `] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member", "visitor", "staff"}, Capabilities: decls, Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	p.Package = pp
	return p
}

// scopeFor returns a valid scope in tenant A for a role holding a token for the
// listed capabilities (nil means every capability of the application).
func scopeFor(role string, caps ...string) contracts.Scope {
	if len(caps) == 0 {
		caps = []string{capGet, capCreate, capAdmin}
	}
	return contracts.Scope{
		DeploymentID: deployment, ApplicationID: appID, TenantID: tenantA, WorkspaceID: workspace1,
		PrincipalID: principal1, Roles: []string{role}, CapabilityScopes: caps, SessionID: session1,
	}
}

func memberScope() contracts.Scope { return scopeFor("member") }

// ---- a scriptable adapter ----

// scripted delegates to the in-memory host and lets a test intercept an
// operation: to answer an entity resolution (the fake host has no search), to bring
// the host down at the right moment or to forge a reply. It sits INSIDE
// adapters.Wrap, so the wrapper still re-validates what it returns.
type scripted struct {
	adapters.AppAdapter

	mu        sync.Mutex
	onRead    func(req adapters.ReadRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	onCommit  func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	onPrepare func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error)
	reads     []adapters.ReadRequest
	prepared  []map[string]any
	commits   int
}

func (s *scripted) Read(ctx context.Context, sc contracts.Scope, req adapters.ReadRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	cp := adapters.ReadRequest{CapabilityID: req.CapabilityID, Fields: map[string]any{}}
	for k, v := range req.Fields {
		cp.Fields[k] = v
	}
	s.reads = append(s.reads, cp)
	hook := s.onRead
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Read(ctx, sc, req) }
	if hook != nil {
		return hook(req, next)
	}
	return next()
}

func (s *scripted) Prepare(ctx context.Context, sc contracts.Scope, req adapters.PrepareRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	cp := map[string]any{}
	for k, v := range req.Fields {
		cp[k] = v
	}
	s.prepared = append(s.prepared, cp)
	hook := s.onPrepare
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Prepare(ctx, sc, req) }
	if hook != nil {
		return hook(next)
	}
	return next()
}

func (s *scripted) Commit(ctx context.Context, sc contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	s.commits++
	hook := s.onCommit
	s.mu.Unlock()
	next := func() (act.DeterministicResult, error) { return s.AppAdapter.Commit(ctx, sc, req) }
	if hook != nil {
		return hook(next)
	}
	return next()
}

func (s *scripted) set(f func(*scripted)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// resolveResult builds a successful resolution reply for items.get: the candidate
// IDs, their labels and a data version.
func resolveResult(ids []string, labels []string) act.DeterministicResult {
	res := act.DeterministicResult{
		Operation: act.DeterministicResultOperationRead, CapabilityID: common.CapabilityID(capGet), Status: act.DeterministicResultStatusOk,
		Identifiers: []act.RecordRef{}, Values: []act.Fact{}, Errors: []act.Issue{}, AllowedNextActions: []act.AllowedNextAction{},
		Freshness: &act.Freshness{AsOf: "2026-10-02T09:30:00Z", Version: "v7"},
	}
	for _, id := range ids {
		res.Identifiers = append(res.Identifiers, act.RecordRef{EntityType: "item", EntityID: common.Identifier(id)})
	}
	for _, l := range labels {
		res.Values = append(res.Values, textFact("label", l))
	}
	return res
}

// ---- the environment ----

// policyRecorder keeps every decision the gateway made.
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

func (r *policyRecorder) denied(capabilityID string, phase policy.Phase) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.recs {
		if rec.CapabilityID == capabilityID && rec.Phase == phase && rec.Outcome == policy.OutcomeDeny {
			n++
		}
	}
	return n
}

type tee []policy.Sink

func (t tee) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

type env struct {
	t      testing.TB
	db     *store.DB
	clk    *clock.Fake
	alog   *audit.Log
	app    *fake.App
	gw     *policy.Gateway
	recs   *policyRecorder
	sc     *scripted
	guard  adapters.AppAdapter
	co     *actions.Coordinator
	reg    *capabilities.Registry
	store  *sessions.Store
	model  *pfake.Provider
	eng    *Engine
	pkg    *testPkg
	budget providers.Budget
	logbuf *strings.Builder

	mu sync.Mutex
}

type envOpts struct {
	pkg      pkgOpts
	cfg      Config
	deps     func(*Deps)
	budget   providers.Budget
	sessCfg  sessions.Config
	provider *pfake.Provider
}

type envOpt func(*envOpts)

func withLevel(l level) envOpt              { return func(o *envOpts) { o.pkg.create = l } }
func withResolver() envOpt                  { return func(o *envOpts) { o.pkg.resolveItems = true } }
func withCount() envOpt                     { return func(o *envOpts) { o.pkg.count = true } }
func withWhen() envOpt                      { return func(o *envOpts) { o.pkg.when = true } }
func withConfig(c Config) envOpt            { return func(o *envOpts) { o.cfg = c } }
func withDeps(f func(*Deps)) envOpt         { return func(o *envOpts) { o.deps = f } }
func withLangs(l ...string) envOpt          { return func(o *envOpts) { o.pkg.langs = l } }
func withSessions(c sessions.Config) envOpt { return func(o *envOpts) { o.sessCfg = c } }

func defaultBudget() providers.Budget {
	return providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
}

// newEnv wires an engine exactly as a composition root would: a throwaway store,
// the real audit log and policy gateway, the capability registry, the in-memory host
// behind adapters.Wrap, the action coordinator and the deterministic provider fake
// behind the local selector.
func newEnv(t testing.TB, opts ...envOpt) *env {
	t.Helper()
	o := envOpts{budget: defaultBudget()}
	for _, f := range opts {
		f(&o)
	}
	e := &env{t: t, clk: clock.NewFake(t0), recs: &policyRecorder{}, budget: o.budget}
	e.db = storetest.Open(t, nil)
	var err error
	if e.alog, err = audit.New(e.db, audit.Options{Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](e.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	e.gw = policy.NewGateway(e.clk, policy.WithSink(tee{sink, e.recs}))
	e.app = fake.NewApp(fake.WithClock(e.clk))
	e.sc = &scripted{AppAdapter: fake.NewAdapter(e.app)}
	if e.guard, err = adapters.Wrap(e.sc, adapters.Options{ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	e.pkg = buildPackage(t, o.pkg)
	e.reg = capabilities.NewRegistry(nil, capabilities.WithClock(e.clk))
	if err := e.reg.Activate(e.pkg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	e.co, err = actions.New(actions.Deps{
		DB: e.db, Policy: e.gw, Audit: e.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return e.currentPkg(), nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return e.guard, nil }),
		Clock:    e.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.store, err = sessions.New(e.db, e.clk, o.sessCfg); err != nil {
		t.Fatal(err)
	}
	e.model = o.provider
	if e.model == nil {
		e.model = pfake.New(pfake.WithClock(e.clk))
	}
	sel, err := LocalSelector(e.model, e.budget)
	if err != nil {
		t.Fatal(err)
	}
	e.logbuf = &strings.Builder{}
	d := Deps{
		Sessions: e.store, Registry: e.reg,
		Packages: PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return e.currentPkg(), nil }),
		Policy:   e.gw, Actions: e.co,
		Adapters:  actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return e.guard, nil }),
		Providers: sel, Audit: e.alog, Clock: e.clk, Config: o.cfg,
	}
	if o.deps != nil {
		o.deps(&d)
	}
	if e.eng, err = New(d); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) currentPkg() *testPkg {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pkg
}

// ---- scripting the model ----

// proposalJSON builds a model output for a TaskProposal.
func proposalJSON(capability string, intent string, fields map[string]any, refs ...map[string]any) string {
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
	if len(refs) > 0 {
		list := make([]any, len(refs))
		for i, r := range refs {
			list[i] = r
		}
		m["entity_refs"] = list
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// onProposal scripts the reply to model call 1 (a JSON-schema constrained request).
func (e *env) onProposal(reply string) *pfake.Rule {
	return e.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(reply)
}

// onResponse scripts the reply to model call 2 (an unconstrained request).
func (e *env) onResponse(text string) *pfake.Rule {
	return e.model.On(pfake.Constrained(providers.ConstraintNone)).Reply(text)
}

// prompts returns the calls that reached the model, oldest first.
func (e *env) prompts() []pfake.Prompt { return e.model.Prompts() }

// ---- running turns ----

func ctxb() context.Context { return context.Background() }

// turn runs a turn and fails the test on an error. It checks what every turn must
// satisfy: the directive is valid against the published schema, the response is
// never empty, and no confirmation secret leaks into the structured result.
func (e *env) turn(sc contracts.Scope, in TurnInput) Result {
	e.t.Helper()
	res, err := e.eng.Turn(ctxb(), sc, in)
	if err != nil {
		e.t.Fatalf("Turn: %v", err)
	}
	e.checkResult(res)
	return res
}

func (e *env) say(sc contracts.Scope, text string) Result {
	e.t.Helper()
	return e.turn(sc, TurnInput{Text: text, Language: "en"})
}

func (e *env) checkResult(res Result) {
	e.t.Helper()
	validateAgainst(e.t, schemaDirective, res.Directive)
	if strings.TrimSpace(res.Response.Text) == "" {
		e.t.Fatalf("empty response for act %s", res.Directive.Act)
	}
	if b, err := json.Marshal(res); err == nil && strings.Contains(string(b), "cfm_") {
		e.t.Fatalf("a confirmation token leaked into the marshalled result: %s", b)
	}
}

func (e *env) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range e.app.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

// noHostWrites asserts the application saw no Prepare and no Commit.
func (e *env) noHostWrites() {
	e.t.Helper()
	if n := e.hostCalls(adapters.OpPrepare); n != 0 {
		e.t.Fatalf("the host saw %d Prepare calls, want 0", n)
	}
	if n := e.hostCalls(adapters.OpCommit); n != 0 {
		e.t.Fatalf("the host saw %d Commit calls, want 0", n)
	}
	if n := e.app.Effects(); n != 0 {
		e.t.Fatalf("the host applied %d effects, want 0", n)
	}
}

func (e *env) session(sc contracts.Scope) sessions.Session {
	e.t.Helper()
	s, err := e.store.Open(ctxb(), sc)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// allPromptText concatenates everything every call sent to the model.
func (e *env) allPromptText() string {
	var sb strings.Builder
	for _, p := range e.prompts() {
		for _, s := range p.Texts() {
			sb.WriteString(s)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

func wantAct(t testing.TB, res Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q), want %s", res.Directive.Act, res.Code, a)
	}
}

func mustContain(t testing.TB, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s does not contain %q:\n%s", what, needle, haystack)
	}
}

func mustNotContain(t testing.TB, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s contains %q:\n%s", what, needle, haystack)
	}
}

// ---- schema validation of what the pipeline produces ----

var schemaRoot = filepath.Join("..", "..", "schemas")

const schemaDirective = "urn:alixai:schema:conversation:dialogue-directive:v1"

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
)

func schemaCompiler(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions", "conversation", "memory"} {
			files, err := filepath.Glob(filepath.Join(schemaRoot, area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				schemaErr = err
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					schemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
				if err != nil {
					schemaErr = err
					return
				}
				if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
					schemaErr = err
					return
				}
			}
		}
		schemaComp = c
	})
	if schemaErr != nil || schemaComp == nil {
		t.Fatalf("schemas unavailable: %v", schemaErr)
	}
	return schemaComp
}

// validateAgainst checks v, as JSON, against the published schema id.
func validateAgainst(t testing.TB, schemaID string, v any) {
	t.Helper()
	s, err := schemaCompiler(t).Compile(schemaID)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaID, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s rejects %s: %v", schemaID, b, err)
	}
}

// renderFor is the deterministic template of a result's directive, for comparing a
// response against.
func renderFor(t testing.TB, res Result, opts verify.Options) string {
	t.Helper()
	txt, err := verify.Render(&res.Directive, res.Receipt, opts)
	if err != nil {
		t.Fatal(err)
	}
	return txt
}

// labelOptions are the verifier options the engine uses for a capability: the
// application's own labels for its fields in the language.
func (e *env) labelOptions(capabilityID string, lang verify.Language) verify.Options {
	t := &turn{pkg: e.currentPkg(), lang: lang}
	return verify.Options{Language: lang, FieldLabels: t.labelsFor(capabilityID)}
}
